package bailian

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

const (
	baseBackoffDuration = 60 * time.Second
	maxBackoffDuration  = 300 * time.Second
	maxTrialPoolModels  = 3
)

var _ domain.QuotaProvider = (*Provider)(nil)

// Provider implements domain.QuotaProvider via the Node bailian-cli bridge and
// Bailian workspace limits HTTP API.
type Provider struct {
	Bridge *BridgeClient
	Limits *LimitsClient
}

// NewProvider constructs a Provider wired from BailianTrialConfig and an optional API key.
func NewProvider(cfg domain.BailianTrialConfig, apiKey string) *Provider {
	return &Provider{
		Bridge: &BridgeClient{NodeBin: cfg.NodeBin, ScriptPath: cfg.BridgeScript, ConfigDir: cfg.BailianConfigDir},
		Limits: &LimitsClient{WorkspaceID: cfg.WorkspaceID, APIKey: apiKey, BaseURL: cfg.LimitsBaseURL},
	}
}

// QueryFreeQuotas queries Bailian free-tier quotas via the bailian-cli bridge.
func (p *Provider) QueryFreeQuotas(ctx context.Context) ([]domain.QuotaObservation, error) {
	if p == nil || p.Bridge == nil {
		return nil, fmt.Errorf("bailian quota bridge is not configured")
	}
	return p.Bridge.QueryFreeQuotas(ctx)
}

// QueryModelLimits queries workspace and model RPM/TPM limits for model.
func (p *Provider) QueryModelLimits(ctx context.Context, model string) (domain.RateLimits, error) {
	if p == nil || p.Limits == nil {
		return domain.RateLimits{}, fmt.Errorf("bailian limits client is not configured")
	}
	client := *p.Limits
	if strings.TrimSpace(client.APIKey) == "" && p.Bridge != nil {
		loadCreds := p.Bridge.LoadCredsFn
		if loadCreds == nil {
			loadCreds = LoadBailianCredentials
		}
		if creds, err := loadCreds(p.Bridge.ConfigDir); err == nil && creds.APIKey != "" {
			client.APIKey = creds.APIKey
		}
	}
	return client.QueryModelLimits(ctx, model)
}

// FilterAndSortQuotas keeps active, non-excluded Text quotas strictly expiring after
// beijingToday and orders them by (Expires ASC, Remaining DESC, Model ASC).
func FilterAndSortQuotas(obs []domain.QuotaObservation, excluded []string, beijingToday string) []domain.QuotaObservation {
	excludedSet := make(map[string]bool, len(excluded))
	for _, m := range excluded {
		if trimmed := strings.TrimSpace(m); trimmed != "" {
			excludedSet[trimmed] = true
		}
	}
	filtered := make([]domain.QuotaObservation, 0, len(obs))
	for _, o := range obs {
		if isQuotaEligible(o, excludedSet, beijingToday) {
			filtered = append(filtered, o)
		}
	}
	slices.SortFunc(filtered, func(a, b domain.QuotaObservation) int {
		if c := cmp.Compare(a.Expires, b.Expires); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Remaining, a.Remaining); c != 0 {
			return c
		}
		return cmp.Compare(a.Model, b.Model)
	})
	return filtered
}

func isQuotaEligible(o domain.QuotaObservation, excluded map[string]bool, beijingToday string) bool {
	if o.Model == "" || excluded[o.Model] || !strings.EqualFold(strings.TrimSpace(o.Type), "Text") {
		return false
	}
	if math.IsNaN(o.Remaining) || math.IsNaN(o.Total) || o.Remaining <= 0 || o.Remaining > o.Total {
		return false
	}
	if o.RemainingPercent == nil || math.IsNaN(*o.RemainingPercent) || math.IsInf(*o.RemainingPercent, 0) || *o.RemainingPercent < 0 || *o.RemainingPercent > 100 {
		return false
	}
	if len(o.Expires) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", o.Expires)
	return err == nil && o.Expires > beijingToday
}

// BackoffDuration returns exponential backoff (60s -> 120s -> 240s -> 300s cap) per SPEC §7.2.
func BackoffDuration(consecutiveErrors int) time.Duration {
	if consecutiveErrors <= 0 {
		return 0
	}
	d := baseBackoffDuration
	for i := 1; i < consecutiveErrors; i++ {
		d *= 2
		if d >= maxBackoffDuration {
			return maxBackoffDuration
		}
	}
	return min(d, maxBackoffDuration)
}

// PoolInput provides all state required to compute the pure trial pool decision.
type PoolInput struct {
	Now              time.Time
	Config           domain.BailianTrialConfig
	Snapshot         domain.RegistrySnapshot
	State            domain.State
	Quotas           []domain.QuotaObservation
	Limits           map[string]domain.RateLimits
	CommercialLimits *domain.RateLimits
	FallbackAPIKey   string
}

// PoolDecision holds the Magpie CLI provider changes and updated state metadata.
type PoolDecision struct {
	ProviderCommands []domain.CLICommand
	DesiredMembers   []string
	NextChannels     map[string]domain.TrialChannelRecord
	SharedRPM        int
	Warnings         []string
}

// DecideTrialPool computes the desired active trial sub-channels (up to 3) and
// the exact magpie.com provider commands to reconcile them.
func DecideTrialPool(in PoolInput) PoolDecision {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	maxActive := min(max(in.Config.MaxActiveModels, 0), maxTrialPoolModels)
	if maxActive == 0 {
		maxActive = maxTrialPoolModels
	}
	templateID := firstNonEmpty(in.Config.TemplateProvider, "transcribe-bailian-trial")
	commercialID := firstNonEmpty(in.Config.CommercialProvider, "transcribe-bailian")

	decision := PoolDecision{NextChannels: make(map[string]domain.TrialChannelRecord)}
	chatURL, apiKey := resolveTrialEndpoint(in.Config.WorkspaceID, templateID, commercialID, in.Snapshot, in.FallbackAPIKey)
	runnable := selectRunnableModels(in, now, maxActive, chatURL, apiKey, &decision)
	if len(runnable) == 0 {
		deactivateRemainingChannels(now, nil, in.Snapshot, in.State, &decision)
		return decision
	}

	sharedRPM := resolvePoolSharedRPM(runnable, in.Limits, in.CommercialLimits)
	decision.SharedRPM = sharedRPM
	selectedIDs := make(map[string]bool, len(runnable))
	decision.DesiredMembers = make([]string, 0, len(runnable))
	for _, q := range runnable {
		id := domain.TrialProviderID(q.Model)
		member := domain.TrialMember(q.Model)
		selectedIDs[id] = true
		decision.DesiredMembers = append(decision.DesiredMembers, member)
		recordedRPM := reconcileTrialSubChannel(id, q.Model, chatURL, apiKey, sharedRPM, in.Snapshot.Providers, &decision)
		decision.NextChannels[id] = domain.TrialChannelRecord{
			ProviderID: id, Model: q.Model, Member: member, Active: true,
			RPM: recordedRPM, Expires: q.Expires, Remaining: q.Remaining, LastSyncedAt: now,
		}
	}
	if comm, ok := in.Snapshot.Providers[commercialID]; ok && sharedRPM > 0 && comm.RPM != sharedRPM {
		decision.ProviderCommands = append(decision.ProviderCommands, domain.CLICommand{
			Args:   []string{"provider", "rpm", commercialID, strconv.Itoa(sharedRPM)},
			Reason: fmt.Sprintf("sync shared RPM (%d) for commercial provider %s", sharedRPM, commercialID),
		})
	}
	deactivateRemainingChannels(now, selectedIDs, in.Snapshot, in.State, &decision)
	return decision
}

func selectRunnableModels(in PoolInput, now time.Time, maxActive int, chatURL, apiKey string, out *PoolDecision) []domain.QuotaObservation {
	creditBlocked := !in.State.TrialPool.AccountCreditBlockedUntil.IsZero() && in.State.TrialPool.AccountCreditBlockedUntil.After(now)
	if in.Config.Paused || creditBlocked {
		return nil
	}
	candidates := FilterAndSortQuotas(in.Quotas, in.Config.ExcludedModels, domain.BeijingDate(now))
	seen := make(map[string]bool, maxActive)
	var runnable []domain.QuotaObservation
	for _, q := range candidates {
		if seen[q.Model] || isTrialModelBlocked(q.Model, now, in.State) {
			continue
		}
		seen[q.Model] = true
		id := domain.TrialProviderID(q.Model)
		if _, exists := in.Snapshot.Providers[id]; !exists && (chatURL == "" || apiKey == "") {
			out.Warnings = append(out.Warnings, fmt.Sprintf("cannot add trial sub-channel %s for %s: missing chat URL or API key", id, q.Model))
			continue
		}
		runnable = append(runnable, q)
		if len(runnable) >= maxActive {
			break
		}
	}
	return runnable
}

func reconcileTrialSubChannel(id, model, chatURL, apiKey string, sharedRPM int, providers map[string]domain.Provider, out *PoolDecision) int {
	existing, exists := providers[id]
	currentRPM := 0
	if !exists {
		args := []string{
			"provider", "add", id, "id=" + id, "chat=" + chatURL, "key=" + apiKey,
			"models=" + model, "free=yes", "list=off", "header." + domain.TrialHeaderName + "=" + model,
		}
		out.ProviderCommands = append(out.ProviderCommands, domain.CLICommand{Args: args, Reason: "create bailian trial sub-channel for " + model})
	} else {
		currentRPM = existing.RPM
		if existing.Off {
			out.ProviderCommands = append(out.ProviderCommands, domain.CLICommand{Args: []string{"provider", "on", id}, Reason: "enable bailian trial sub-channel for " + model})
		}
	}
	if sharedRPM > 0 && currentRPM != sharedRPM {
		out.ProviderCommands = append(out.ProviderCommands, domain.CLICommand{
			Args:   []string{"provider", "rpm", id, strconv.Itoa(sharedRPM)},
			Reason: fmt.Sprintf("sync shared RPM (%d) for trial sub-channel %s", sharedRPM, id),
		})
		return sharedRPM
	}
	return currentRPM
}

func isTrialModelBlocked(model string, now time.Time, st domain.State) bool {
	providerID, member := domain.TrialProviderID(model), domain.TrialMember(model)
	for _, key := range []string{member, providerID, model} {
		if cd, ok := st.Cooldowns[key]; ok && now.Before(cd.Until) {
			return true
		}
	}
	for _, hold := range st.ManualHolds {
		if hold.Member == member || hold.Member == providerID || hold.Member == model {
			return true
		}
	}
	return false
}

func resolveTrialEndpoint(workspaceID, templateID, commercialID string, snap domain.RegistrySnapshot, fallbackKey string) (string, string) {
	var chatURL, apiKey string
	if tmpl, ok := snap.Providers[templateID]; ok {
		chatURL = firstNonEmpty(tmpl.ChatURL, tmpl.BaseURL)
		apiKey = strings.TrimSpace(tmpl.APIKey)
	}
	if chatURL == "" && isValidWorkspaceID(strings.TrimSpace(workspaceID)) {
		chatURL = fmt.Sprintf("https://%s.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", strings.TrimSpace(workspaceID))
	}
	if comm, ok := snap.Providers[commercialID]; ok {
		chatURL = firstNonEmpty(chatURL, comm.ChatURL, comm.BaseURL)
		apiKey = firstNonEmpty(apiKey, comm.APIKey)
	}
	return chatURL, firstNonEmpty(apiKey, fallbackKey)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func resolvePoolSharedRPM(selected []domain.QuotaObservation, limits map[string]domain.RateLimits, commercial *domain.RateLimits) int {
	best := 0
	consider := func(lim domain.RateLimits) {
		if rpm := ComputeSharedRPM(lim); rpm > 0 && (best == 0 || rpm < best) {
			best = rpm
		}
	}
	for _, q := range selected {
		if lim, ok := limits[q.Model]; ok {
			consider(lim)
		}
	}
	if best == 0 {
		for _, lim := range limits {
			consider(lim)
		}
	}
	if commercial != nil {
		consider(*commercial)
	}
	return best
}

func deactivateRemainingChannels(now time.Time, selectedIDs map[string]bool, snap domain.RegistrySnapshot, st domain.State, out *PoolDecision) {
	allIDs := make(map[string]bool)
	for id := range snap.Providers {
		if strings.HasPrefix(id, domain.TrialProviderPrefix) && !selectedIDs[id] {
			allIDs[id] = true
		}
	}
	for key, rec := range st.TrialPool.Channels {
		if id := firstNonEmpty(rec.ProviderID, key); !selectedIDs[id] {
			allIDs[id] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(allIDs)) {
		turnedOff := false
		if p, ok := snap.Providers[id]; ok && !p.Off {
			out.ProviderCommands = append(out.ProviderCommands, domain.CLICommand{Args: []string{"provider", "off", id}, Reason: "disable inactive bailian trial sub-channel " + id})
			turnedOff = true
		}
		if prev, ok := findTrialRecord(st.TrialPool.Channels, id); ok {
			prev.ProviderID, prev.Active = id, false
			if turnedOff {
				prev.LastSyncedAt = now
			}
			out.NextChannels[id] = prev
		}
	}
}

func findTrialRecord(channels map[string]domain.TrialChannelRecord, providerID string) (domain.TrialChannelRecord, bool) {
	if rec, ok := channels[providerID]; ok {
		return rec, true
	}
	for _, rec := range channels {
		if rec.ProviderID == providerID {
			return rec, true
		}
	}
	return domain.TrialChannelRecord{}, false
}
