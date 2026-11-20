package coordinator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"magpie-keeper/internal/admit"
	"magpie-keeper/internal/bailian"
	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/planner"
	"magpie-keeper/internal/state"
)

func (c *Coordinator) handleTrialSync(ctx context.Context, force bool) (TrialReport, error) {
	now := c.now()
	report := TrialReport{ExecutedAt: now}
	if !c.cfg.BailianTrial.Enabled {
		report.Skipped = true
		report.SkipReason = "bailian_trial disabled"
		return report, nil
	}

	st, err := c.store.Load()
	if err != nil {
		return report, err
	}
	storeClock := state.NewStoreWithClock(c.store.Path(), c.now)
	storeClock.PruneExpiredCooldowns(&st)

	snap, err := c.gateway.ReadRegistry(ctx)
	if err != nil {
		return report, err
	}
	holds := planner.DetectManualHolds(snap, st, []string{"transcribe-trial", "transcribe-free"}, now)
	for _, h := range holds {
		state.AddManualHold(&st, h)
	}

	creditBlocked := !st.TrialPool.AccountCreditBlockedUntil.IsZero() && st.TrialPool.AccountCreditBlockedUntil.After(now)
	var quotas []domain.QuotaObservation
	backedUp := false

	if !c.cfg.BailianTrial.Paused && !creditBlocked {
		if !force && !st.TrialPool.NextQuotaRetryAt.IsZero() && now.Before(st.TrialPool.NextQuotaRetryAt) {
			if len(c.lastQuotas) == 0 {
				report.Skipped = true
				report.SkipReason = fmt.Sprintf("quota query in backoff until %s", st.TrialPool.NextQuotaRetryAt.Format(time.RFC3339))
				return report, nil
			}
			quotas = c.lastQuotas
			report.Warnings = append(report.Warnings, "using cached quotas during backoff window")
		} else {
			if c.quota == nil {
				return report, fmt.Errorf("bailian quota provider is not configured")
			}
			fetched, qErr := c.quota.QueryFreeQuotas(ctx)
			if qErr != nil {
				st.TrialPool.ConsecutiveErrors++
				st.TrialPool.NextQuotaRetryAt = now.Add(bailian.BackoffDuration(st.TrialPool.ConsecutiveErrors))
				st.TrialPool.LastError = qErr.Error()
				_ = c.store.Save(st)
				return report, fmt.Errorf("query free quotas: %w", qErr)
			}
			st.TrialPool.ConsecutiveErrors = 0
			st.TrialPool.NextQuotaRetryAt = time.Time{}
			st.TrialPool.LastError = ""
			st.TrialPool.LastQuotaSyncAt = now
			c.lastQuotas = fetched
			quotas = fetched
		}

		var evalCount int
		var evalBackupPath string
		var evalWarnings []string
		quotas, evalCount, evalBackupPath, evalWarnings, err = c.filterAndAdmitTrialQuotas(ctx, storeClock, &st, &snap, quotas, now)
		report.Evaluated = evalCount
		report.Warnings = append(report.Warnings, evalWarnings...)
		if evalBackupPath != "" {
			backedUp = true
			report.BackupPath = evalBackupPath
		}
		if err != nil {
			_ = c.store.Save(st)
			return report, err
		}
	}

	limits := make(map[string]domain.RateLimits)
	var commLimits *domain.RateLimits
	if c.quota != nil && len(quotas) > 0 {
		for _, q := range quotas {
			lim, lErr := c.quota.QueryModelLimits(ctx, q.Model)
			if lErr != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("query limits %s: %v", q.Model, lErr))
				continue
			}
			limits[q.Model] = lim
		}
		if commID := c.cfg.BailianTrial.CommercialProvider; commID != "" {
			if commProv, ok := snap.Providers[commID]; ok && len(commProv.Models) > 0 {
				if lim, lErr := c.quota.QueryModelLimits(ctx, commProv.Models[0]); lErr == nil {
					commLimits = &lim
				}
			}
		}
	}

	decision := bailian.DecideTrialPool(bailian.PoolInput{
		Now:              now,
		Config:           c.cfg.BailianTrial,
		Snapshot:         snap,
		State:            st,
		Quotas:           quotas,
		Limits:           limits,
		CommercialLimits: commLimits,
		FallbackAPIKey:   c.fallbackAPIKey,
	})
	report.DesiredMembers = decision.DesiredMembers
	report.SharedRPM = decision.SharedRPM
	report.Warnings = append(report.Warnings, decision.Warnings...)

	plan := planner.PlanTrialGroup(planner.TrialPlanInput{
		Config:           c.cfg,
		Snapshot:         snap,
		State:            st,
		DesiredMembers:   decision.DesiredMembers,
		ProviderCommands: decision.ProviderCommands,
		Now:              now,
	})
	report.Commands = plan.Commands
	report.Warnings = append(report.Warnings, plan.Warnings...)

	if len(plan.Commands) > 0 {
		backupPath, execErr := c.executePlan(ctx, plan.Commands, backedUp)
		if backupPath != "" {
			report.BackupPath = backupPath
		}
		if execErr != nil {
			_ = c.store.Save(st)
			return report, execErr
		}
	}

	st.TrialPool.Channels = decision.NextChannels
	recordHoldsAndTargetGroups(&st, holds, plan.TargetGroups, "")
	if err := c.store.Save(st); err != nil {
		return report, err
	}
	return report, nil
}

func (c *Coordinator) filterAndAdmitTrialQuotas(
	ctx context.Context,
	storeClock *state.Store,
	st *domain.State,
	snap *domain.RegistrySnapshot,
	raw []domain.QuotaObservation,
	now time.Time,
) ([]domain.QuotaObservation, int, string, []string, error) {
	sorted := bailian.FilterAndSortQuotas(raw, c.cfg.BailianTrial.ExcludedModels, domain.BeijingDate(now))
	maxActive := min(max(c.cfg.BailianTrial.MaxActiveModels, 1), 3)
	chatURL, apiKey := c.resolveTrialEndpoint(*snap)
	cooldownDur := time.Duration(c.cfg.CooldownSeconds) * time.Second
	if cooldownDur <= 0 {
		cooldownDur = time.Hour
	}

	var ready []domain.QuotaObservation
	var warnings []string
	var backupPath string
	backedUp := false
	evalCount := 0

	for _, q := range sorted {
		if len(ready) >= maxActive {
			break
		}
		id := domain.TrialProviderID(q.Model)
		member := domain.TrialMember(q.Model)
		if isTrialBlocked(*st, id, member, q.Model, now) {
			continue
		}
		epHash := domain.EndpointKeyHash(chatURL, apiKey, map[string]string{domain.TrialHeaderName: q.Model})
		if _, cached := state.GetCachedAdmission(*st, epHash, q.Model, admit.SuiteVersion); cached {
			ready = append(ready, q)
			continue
		}
		if evalCount >= c.cfg.MaxEvaluationsPerCycle {
			warnings = append(warnings, fmt.Sprintf("evaluation cycle limit (%d) reached; deferring trial %s", c.cfg.MaxEvaluationsPerCycle, q.Model))
			continue
		}
		if _, exists := snap.Providers[id]; !exists && (chatURL == "" || apiKey == "") {
			warnings = append(warnings, fmt.Sprintf("cannot admit trial %s: missing chatURL or API key", q.Model))
			continue
		}
		reserved, err := storeClock.ReserveDailyBudget(st, c.cfg.MaxEvaluationsPerDay)
		if err != nil {
			return ready, evalCount, backupPath, warnings, fmt.Errorf("reserve daily budget for trial %s: %w", q.Model, err)
		}
		if !reserved {
			warnings = append(warnings, fmt.Sprintf("daily evaluation budget (%d) exhausted; deferring trial %s", c.cfg.MaxEvaluationsPerDay, q.Model))
			continue
		}

		// Ensure sub-channel exists and is enabled before testing via Magpie.
		if p, exists := snap.Providers[id]; !exists || p.Off {
			if !backedUp {
				bp, bErr := c.gateway.BackupRegistry(ctx)
				if bErr != nil {
					return ready, evalCount, backupPath, warnings, fmt.Errorf("backup before trial eval setup: %w", bErr)
				}
				backupPath = bp
				backedUp = true
			}
			var setupArgs []string
			if !exists {
				setupArgs = []string{
					"provider", "add", id, "id=" + id, "chat=" + chatURL, "key=" + apiKey,
					"models=" + q.Model, "free=yes", "list=off", "header." + domain.TrialHeaderName + "=" + q.Model,
				}
			} else {
				setupArgs = []string{"provider", "on", id}
			}
			if execErr := c.gateway.ExecCLI(ctx, setupArgs...); execErr != nil {
				return ready, evalCount, backupPath, warnings, fmt.Errorf("enable trial sub-channel %s for admission: %w", id, execErr)
			}
			if updatedSnap, rErr := c.gateway.ReadRegistry(ctx); rErr == nil {
				*snap = updatedSnap
			}
		}

		evalCount++
		res, evalErr := c.admitter.Evaluate(ctx, member)
		if evalErr != nil {
			_ = c.gateway.ExecCLI(ctx, "provider", "off", id)
			storeClock.SetCooldown(st, member, domain.TransientCooldown, evalErr.Error(), true)
			_ = c.store.Save(*st)
			warnings = append(warnings, fmt.Sprintf("evaluate trial %s error: %v", member, evalErr))
			continue
		}
		if res.EvaluatedAt.IsZero() {
			res.EvaluatedAt = now
		}
		res.EndpointKeyHash = epHash

		switch res.Status {
		case domain.AdmissionPassed:
			state.RecordAdmissionByModel(st, q.Model, res)
			storeClock.ClearCooldown(st, member)
			ready = append(ready, q)
		case domain.AdmissionTransient:
			_ = c.gateway.ExecCLI(ctx, "provider", "off", id)
			storeClock.SetCooldown(st, member, domain.TransientCooldown, res.Reason, true)
		default:
			_ = c.gateway.ExecCLI(ctx, "provider", "off", id)
			state.RecordAdmissionByModel(st, q.Model, res)
			storeClock.SetCooldown(st, member, cooldownDur, res.Reason, false)
		}
		if err := c.store.Save(*st); err != nil {
			return ready, evalCount, backupPath, warnings, fmt.Errorf("save state after trial eval: %w", err)
		}
	}
	return ready, evalCount, backupPath, warnings, nil
}

func (c *Coordinator) resolveTrialEndpoint(snap domain.RegistrySnapshot) (string, string) {
	tmplID := c.cfg.BailianTrial.TemplateProvider
	if tmplID == "" {
		tmplID = "transcribe-bailian-trial"
	}
	commID := c.cfg.BailianTrial.CommercialProvider
	if commID == "" {
		commID = "transcribe-bailian"
	}
	var chatURL, apiKey string
	if p, ok := snap.Providers[tmplID]; ok {
		chatURL = firstStr(p.ChatURL, p.BaseURL)
		apiKey = strings.TrimSpace(p.APIKey)
	}
	if chatURL == "" && strings.TrimSpace(c.cfg.BailianTrial.WorkspaceID) != "" {
		chatURL = fmt.Sprintf("https://%s.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", strings.TrimSpace(c.cfg.BailianTrial.WorkspaceID))
	}
	if p, ok := snap.Providers[commID]; ok {
		chatURL = firstStr(chatURL, p.ChatURL, p.BaseURL)
		apiKey = firstStr(apiKey, p.APIKey)
	}
	return chatURL, firstStr(apiKey, c.fallbackAPIKey)
}

func isTrialBlocked(st domain.State, providerID, member, model string, now time.Time) bool {
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

func firstStr(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
