package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

const (
	defaultSyncInterval     = "6h"
	defaultTrialInterval    = "60s"
	defaultTemplateProvider = "transcribe-bailian-trial"
	defaultCommercial       = "transcribe-bailian"
	defaultMaxCycleEvals    = 2
	defaultMaxDayEvals      = 6
	defaultCooldownSeconds  = 3600
	defaultMaxActiveTrial   = 3
	defaultMaxInputUSD      = 2.0
	defaultMaxOutputUSD     = 10.0
)

var validAdapters = map[string]bool{
	"registry":   true,
	"openrouter": true,
	"vercel":     true,
	"vllm-sr":    true,
}

var validRoles = map[string]bool{
	domain.RoleTranscribe: true,
	domain.RoleReview:     true,
}

var validBilling = map[string]bool{
	domain.BillingFree:       true,
	domain.BillingCommercial: true,
}

// Load reads, applies defaults to, and validates data/channels.json.
func Load(path string) (domain.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg domain.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return domain.Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	ApplyDefaults(&cfg)
	if err := Validate(cfg); err != nil {
		return domain.Config{}, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

// Save atomically persists cfg as indented JSON to path.
func Save(path string, cfg domain.Config) error {
	ApplyDefaults(&cfg)
	if err := Validate(cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data)
}

// ApplyDefaults populates omitted configuration fields with spec defaults.
func ApplyDefaults(cfg *domain.Config) {
	if len(cfg.Roles) == 0 {
		cfg.Roles = []string{domain.RoleTranscribe}
	}
	if strings.TrimSpace(cfg.ChannelSyncInterval) == "" {
		cfg.ChannelSyncInterval = defaultSyncInterval
	}
	if cfg.ExcludedProviders == nil {
		cfg.ExcludedProviders = []string{}
	}
	if cfg.MaxEvaluationsPerCycle <= 0 {
		cfg.MaxEvaluationsPerCycle = defaultMaxCycleEvals
	}
	if cfg.MaxEvaluationsPerDay <= 0 {
		cfg.MaxEvaluationsPerDay = defaultMaxDayEvals
	}
	if cfg.CooldownSeconds <= 0 {
		cfg.CooldownSeconds = defaultCooldownSeconds
	}
	if strings.TrimSpace(cfg.BailianTrial.TemplateProvider) == "" {
		cfg.BailianTrial.TemplateProvider = defaultTemplateProvider
	}
	if strings.TrimSpace(cfg.BailianTrial.CommercialProvider) == "" {
		cfg.BailianTrial.CommercialProvider = defaultCommercial
	}
	if cfg.BailianTrial.MaxActiveModels <= 0 {
		cfg.BailianTrial.MaxActiveModels = defaultMaxActiveTrial
	}
	if strings.TrimSpace(cfg.BailianTrial.RefreshInterval) == "" {
		cfg.BailianTrial.RefreshInterval = defaultTrialInterval
	}
	if cfg.BailianTrial.ExcludedModels == nil {
		cfg.BailianTrial.ExcludedModels = []string{}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]domain.ProviderRule{}
	}
	for id, rule := range cfg.Providers {
		if len(rule.Billing) == 0 {
			if rule.Adapter == "openrouter" || rule.Adapter == "vllm-sr" {
				rule.Billing = []string{domain.BillingFree}
			} else {
				rule.Billing = []string{domain.BillingFree, domain.BillingCommercial}
			}
		}
		if rule.Adapter == "vercel" && hasBilling(rule.Billing, domain.BillingCommercial) {
			if rule.MaxInputUSDPerMillion <= 0 {
				rule.MaxInputUSDPerMillion = defaultMaxInputUSD
			}
			if rule.MaxOutputUSDPerMillion <= 0 {
				rule.MaxOutputUSDPerMillion = defaultMaxOutputUSD
			}
		}
		cfg.Providers[id] = rule
	}
}

// Validate verifies structural and semantic constraints on cfg.
func Validate(cfg domain.Config) error {
	if strings.TrimSpace(cfg.MagpieDir) == "" {
		return fmt.Errorf("magpie_dir is required")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.MagpieURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("magpie_url must be a valid http/https URL, got %q", cfg.MagpieURL)
	}
	seenRoles := make(map[string]bool, len(cfg.Roles))
	for _, role := range cfg.Roles {
		if !validRoles[role] {
			return fmt.Errorf("unsupported role %q (want transcribe or review)", role)
		}
		if seenRoles[role] {
			return fmt.Errorf("duplicate role %q", role)
		}
		seenRoles[role] = true
	}
	if _, err := ParseDuration(cfg.ChannelSyncInterval, 0); err != nil {
		return fmt.Errorf("channel_sync_interval: %w", err)
	}
	if cfg.MaxEvaluationsPerCycle <= 0 {
		return fmt.Errorf("max_evaluations_per_cycle must be > 0")
	}
	if cfg.MaxEvaluationsPerDay <= 0 {
		return fmt.Errorf("max_evaluations_per_day must be > 0")
	}
	if cfg.CooldownSeconds <= 0 {
		return fmt.Errorf("cooldown_seconds must be > 0")
	}
	if cfg.BailianTrial.MaxActiveModels < 1 || cfg.BailianTrial.MaxActiveModels > 3 {
		return fmt.Errorf("bailian_trial.max_active_models must be between 1 and 3, got %d", cfg.BailianTrial.MaxActiveModels)
	}
	if _, err := ParseDuration(cfg.BailianTrial.RefreshInterval, 0); err != nil {
		return fmt.Errorf("bailian_trial.refresh_interval: %w", err)
	}
	if cfg.BailianTrial.Enabled && strings.TrimSpace(cfg.BailianTrial.WorkspaceID) == "" {
		return fmt.Errorf("bailian_trial.workspace_id is required when bailian_trial.enabled is true")
	}
	for id, rule := range cfg.Providers {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("provider id must not be empty")
		}
		if !validAdapters[rule.Adapter] {
			return fmt.Errorf("provider %q has unsupported adapter %q", id, rule.Adapter)
		}
		for _, b := range rule.Billing {
			if !validBilling[b] {
				return fmt.Errorf("provider %q has unsupported billing tier %q", id, b)
			}
		}
		for tier, capCount := range rule.MaxCandidates {
			if !validBilling[tier] {
				return fmt.Errorf("provider %q max_candidates has unsupported tier %q", id, tier)
			}
			if capCount < 0 {
				return fmt.Errorf("provider %q max_candidates[%q] must be >= 0", id, tier)
			}
		}
		for _, r := range rule.Roles {
			if !validRoles[r] {
				return fmt.Errorf("provider %q has unsupported role %q", id, r)
			}
		}
	}
	return nil
}

// ParseDuration parses a positive duration string, returning fallback if raw is empty
// and fallback > 0.
func ParseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if fallback > 0 {
			return fallback, nil
		}
		return 0, fmt.Errorf("duration is empty")
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be > 0", raw)
	}
	return d, nil
}

func hasBilling(tiers []string, target string) bool {
	for _, b := range tiers {
		if b == target {
			return true
		}
	}
	return false
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".cfg-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %q -> %q: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}
