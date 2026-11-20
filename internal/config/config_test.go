package config

import (
	"path/filepath"
	"testing"
	"time"

	"magpie-keeper/internal/domain"
)

func TestLoadExampleConfig(t *testing.T) {
	examplePath := filepath.Join("..", "..", "data", "channels.example.json")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", examplePath, err)
	}
	if cfg.MagpieDir == "" || cfg.MagpieURL != "http://127.0.0.1:3425/v1" {
		t.Fatalf("unexpected magpie settings: %+v", cfg)
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0] != domain.RoleTranscribe {
		t.Fatalf("unexpected roles: %+v", cfg.Roles)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(cfg.Providers))
	}
	if !cfg.BailianTrial.Enabled || cfg.BailianTrial.MaxActiveModels != 3 {
		t.Fatalf("unexpected bailian_trial: %+v", cfg.BailianTrial)
	}
}

func TestDefaultsAndRoundTripSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub dir", "channels.json")
	cfg := domain.Config{
		MagpieDir: `D:\Program Files\magpie`,
		MagpieURL: "http://127.0.0.1:3425/v1",
		BailianTrial: domain.BailianTrialConfig{
			Enabled: false,
		},
		Providers: map[string]domain.ProviderRule{
			"or-free": {Adapter: "openrouter"},
			"vercel":  {Adapter: "vercel"},
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save failed: %v", err)
	}
	if loaded.ChannelSyncInterval != "6h" {
		t.Fatalf("ChannelSyncInterval = %q, want 6h", loaded.ChannelSyncInterval)
	}
	if loaded.MaxEvaluationsPerCycle != 2 || loaded.MaxEvaluationsPerDay != 6 {
		t.Fatalf("unexpected eval limits: cycle=%d day=%d", loaded.MaxEvaluationsPerCycle, loaded.MaxEvaluationsPerDay)
	}
	if loaded.CooldownSeconds != 3600 {
		t.Fatalf("CooldownSeconds = %d, want 3600", loaded.CooldownSeconds)
	}
	if loaded.Providers["vercel"].MaxInputUSDPerMillion != 2.0 || loaded.Providers["vercel"].MaxOutputUSDPerMillion != 10.0 {
		t.Fatalf("unexpected vercel default ceilings: %+v", loaded.Providers["vercel"])
	}
	if len(loaded.Providers["or-free"].Billing) != 1 || loaded.Providers["or-free"].Billing[0] != domain.BillingFree {
		t.Fatalf("unexpected openrouter default billing: %+v", loaded.Providers["or-free"].Billing)
	}
}

func TestValidationErrors(t *testing.T) {
	base := func() domain.Config {
		cfg := domain.Config{
			MagpieDir: `D:\Program Files\magpie`,
			MagpieURL: "http://127.0.0.1:3425/v1",
			BailianTrial: domain.BailianTrialConfig{
				Enabled:     true,
				WorkspaceID: "ws-1",
			},
			Providers: map[string]domain.ProviderRule{
				"p": {Adapter: "registry", Billing: []string{"commercial"}},
			},
		}
		ApplyDefaults(&cfg)
		return cfg
	}

	cases := []struct {
		name   string
		mutate func(*domain.Config)
	}{
		{"empty magpie_dir", func(c *domain.Config) { c.MagpieDir = "" }},
		{"bad magpie_url", func(c *domain.Config) { c.MagpieURL = "ftp://localhost:3425" }},
		{"bad role", func(c *domain.Config) { c.Roles = []string{"admin"} }},
		{"duplicate role", func(c *domain.Config) { c.Roles = []string{"transcribe", "transcribe"} }},
		{"bad sync interval", func(c *domain.Config) { c.ChannelSyncInterval = "-5s" }},
		{"bad max active trial", func(c *domain.Config) { c.BailianTrial.MaxActiveModels = 4 }},
		{"missing workspace id", func(c *domain.Config) { c.BailianTrial.WorkspaceID = "" }},
		{"bad adapter", func(c *domain.Config) { c.Providers["p"] = domain.ProviderRule{Adapter: "unknown"} }},
		{"bad billing", func(c *domain.Config) {
			c.Providers["p"] = domain.ProviderRule{Adapter: "registry", Billing: []string{"promo"}}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			if err := Validate(c); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}

	if d, err := ParseDuration("", 3*time.Minute); err != nil || d != 3*time.Minute {
		t.Fatalf("ParseDuration fallback failed: d=%v err=%v", d, err)
	}
}
