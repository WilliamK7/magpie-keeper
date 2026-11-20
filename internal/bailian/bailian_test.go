package bailian

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"magpie-keeper/internal/domain"
)

func floatPtr(v float64) *float64 {
	return &v
}

func TestFilterAndSortQuotas(t *testing.T) {
	today := "2026-11-20"
	obs := []domain.QuotaObservation{
		{Model: "non-text", Type: "Vision", Remaining: 100, Total: 200, RemainingPercent: floatPtr(50), Expires: "2026-12-01"},
		{Model: "zero-rem", Type: "Text", Remaining: 0, Total: 200, RemainingPercent: floatPtr(0), Expires: "2026-12-01"},
		{Model: "neg-rem", Type: "Text", Remaining: -1, Total: 200, RemainingPercent: floatPtr(0), Expires: "2026-12-01"},
		{Model: "over-total", Type: "Text", Remaining: 201, Total: 200, RemainingPercent: floatPtr(100), Expires: "2026-12-01"},
		{Model: "nil-pct", Type: "Text", Remaining: 100, Total: 200, RemainingPercent: nil, Expires: "2026-12-01"},
		{Model: "nan-pct", Type: "Text", Remaining: 100, Total: 200, RemainingPercent: floatPtr(math.NaN()), Expires: "2026-12-01"},
		{Model: "bad-pct", Type: "Text", Remaining: 100, Total: 200, RemainingPercent: floatPtr(101), Expires: "2026-12-01"},
		{Model: "expires-today", Type: "Text", Remaining: 100, Total: 200, RemainingPercent: floatPtr(50), Expires: "2026-11-20"},
		{Model: "expired-past", Type: "Text", Remaining: 100, Total: 200, RemainingPercent: floatPtr(50), Expires: "2026-11-19"},
		{Model: "bad-date", Type: "Text", Remaining: 100, Total: 200, RemainingPercent: floatPtr(50), Expires: "2026-02-30"},
		{Model: "excluded-model", Type: "Text", Remaining: 999, Total: 1000, RemainingPercent: floatPtr(99.9), Expires: "2026-11-21"},
		{Model: "qwen-later", Type: "Text", Remaining: 900, Total: 1000, RemainingPercent: floatPtr(90), Expires: "2026-12-05"},
		{Model: "qwen-tie-b", Type: "text", Remaining: 500, Total: 1000, RemainingPercent: floatPtr(50), Expires: "2026-11-25"},
		{Model: "qwen-tie-a", Type: "Text", Remaining: 500, Total: 1000, RemainingPercent: floatPtr(50), Expires: "2026-11-25"},
		{Model: "qwen-more-rem", Type: "Text", Remaining: 800, Total: 1000, RemainingPercent: floatPtr(80), Expires: "2026-11-25"},
	}

	got := FilterAndSortQuotas(obs, []string{"excluded-model"}, today)
	wantOrder := []string{"qwen-more-rem", "qwen-tie-a", "qwen-tie-b", "qwen-later"}
	if len(got) != len(wantOrder) {
		t.Fatalf("FilterAndSortQuotas returned %d items (%+v), want %d", len(got), got, len(wantOrder))
	}
	for i, want := range wantOrder {
		if got[i].Model != want {
			t.Errorf("got[%d].Model = %q, want %q", i, got[i].Model, want)
		}
	}
}

func TestBackoffDuration(t *testing.T) {
	cases := []struct {
		errs int
		want time.Duration
	}{
		{0, 0},
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 240 * time.Second},
		{4, 300 * time.Second},
		{5, 300 * time.Second},
		{20, 300 * time.Second},
	}
	for _, tc := range cases {
		if got := BackoffDuration(tc.errs); got != tc.want {
			t.Errorf("BackoffDuration(%d) = %s, want %s", tc.errs, got, tc.want)
		}
	}
}

func TestLimitsClientAndComputeSharedRPM(t *testing.T) {
	var seenAuth, seenModel, seenPageSize string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models/limits" {
			http.NotFound(w, r)
			return
		}
		seenAuth = r.Header.Get("Authorization")
		seenModel = r.URL.Query().Get("model")
		seenPageSize = r.URL.Query().Get("page_size")
		switch seenModel {
		case "qwen-plus":
			_, _ = io.WriteString(w, `{
				"output": {
					"quotas": [
						{
							"model": "qwen-plus-other",
							"workspace_limit": {"request_limit": 999, "request_limit_period": 60}
						},
						{
							"model": "qwen-plus",
							"model_limit": {"request_limit": 100, "request_limit_period": 30, "usage_limit": 4000000, "usage_limit_period": 0},
							"workspace_limit": {"request_limit": 120, "request_limit_period": 60, "usage_limit": 1474560, "usage_limit_period": 60}
						}
					]
				}
			}`)
		case "flat-model":
			_, _ = io.WriteString(w, `{
				"workspace_rpm": 80,
				"workspace_tpm": 2949120
			}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"bad model for key sk-secret-123"}`)
		}
	}))
	defer srv.Close()

	client := &LimitsClient{
		WorkspaceID: "ws-ignored-when-baseurl-set",
		APIKey:      "sk-secret-123",
		BaseURL:     srv.URL,
	}

	lim, err := client.QueryModelLimits(context.Background(), "qwen-plus")
	if err != nil {
		t.Fatalf("QueryModelLimits(qwen-plus) failed: %v", err)
	}
	if seenAuth != "Bearer sk-secret-123" || seenModel != "qwen-plus" || seenPageSize != "100" {
		t.Fatalf("unexpected request metadata: auth=%q model=%q page_size=%q", seenAuth, seenModel, seenPageSize)
	}
	want := domain.RateLimits{
		Model:        "qwen-plus",
		AccountRPM:   200,
		AccountTPM:   4000000,
		WorkspaceRPM: 120,
		WorkspaceTPM: 1474560,
	}
	if lim != want {
		t.Fatalf("limits = %+v, want %+v", lim, want)
	}
	// TPM bottleneck: 1474560 / 36864 = 40 < 120 -> 40 / 4 = 10
	if got := ComputeSharedRPM(lim); got != 10 {
		t.Fatalf("ComputeSharedRPM(TPM bottleneck) = %d, want 10", got)
	}

	flatLim, err := client.QueryModelLimits(context.Background(), "flat-model")
	if err != nil {
		t.Fatalf("QueryModelLimits(flat-model) failed: %v", err)
	}
	if flatLim.WorkspaceRPM != 80 || flatLim.WorkspaceTPM != 2949120 {
		t.Fatalf("flat limits = %+v, want workspace 80 / 2949120", flatLim)
	}
	// 2949120 / 36864 = 80 -> min(80, 80) / 4 = 20
	if got := ComputeSharedRPM(flatLim); got != 20 {
		t.Fatalf("ComputeSharedRPM(flat) = %d, want 20", got)
	}

	if _, err := client.QueryModelLimits(context.Background(), "missing"); err == nil || strings.Contains(err.Error(), "sk-secret-123") {
		t.Fatalf("expected non-2xx error without leaking API key, got: %v", err)
	}

	// Additional ComputeSharedRPM unit cases.
	if got := ComputeSharedRPM(domain.RateLimits{AccountRPM: 16, AccountTPM: 36864 * 100}); got != 4 {
		t.Fatalf("ComputeSharedRPM(account fallback RPM bottleneck) = %d, want 4", got)
	}
	if got := ComputeSharedRPM(domain.RateLimits{WorkspaceRPM: 2}); got != 1 {
		t.Fatalf("ComputeSharedRPM(clamp <1 to 1) = %d, want 1", got)
	}
	if got := ComputeSharedRPM(domain.RateLimits{}); got != 0 {
		t.Fatalf("ComputeSharedRPM(empty) = %d, want 0", got)
	}
}

func TestLoadBailianCredentials(t *testing.T) {
	t.Run("default top-level via BAILIAN_CONFIG_DIR", func(t *testing.T) {
		dir := t.TempDir()
		cfg := `{
			"active_config": "default",
			"access_key_id": "ak-top",
			"access_key_secret": "sk-top",
			"security_token": "sts-top",
			"api_key": "dash-top",
			"base_url": "https://dashscope.aliyuncs.com",
			"console_region": "cn-beijing",
			"console_site": "domestic",
			"console_switch_agent": 42
		}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BAILIAN_CONFIG_DIR", dir)

		creds, err := LoadBailianCredentials("")
		if err != nil {
			t.Fatalf("LoadBailianCredentials failed: %v", err)
		}
		if creds.AccessKeyID != "ak-top" || creds.AccessKeySecret != "sk-top" || creds.SecurityToken != "sts-top" ||
			creds.APIKey != "dash-top" || creds.ConsoleRegion != "cn-beijing" || creds.ConsoleSite != "domestic" ||
			creds.ConsoleSwitchAgent == nil || *creds.ConsoleSwitchAgent != 42 {
			t.Fatalf("unexpected creds: %+v", creds)
		}
	})

	t.Run("named active_config profile and missing profile error", func(t *testing.T) {
		dir := t.TempDir()
		cfg := `{
			"active_config": "work",
			"access_key_id": "ak-ignored",
			"work": {
				"access_key_id": "ak-work",
				"access_key_secret": "sk-work",
				"api_key": "dash-work",
				"console_region": "ap-southeast-1",
				"console_site": "international"
			}
		}`
		cfgPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		creds, err := LoadBailianCredentials(dir)
		if err != nil {
			t.Fatalf("LoadBailianCredentials(work) failed: %v", err)
		}
		if creds.AccessKeyID != "ak-work" || creds.AccessKeySecret != "sk-work" || creds.APIKey != "dash-work" || creds.ConsoleRegion != "ap-southeast-1" {
			t.Fatalf("unexpected profile creds: %+v", creds)
		}

		badCfg := `{"active_config": "nonexistent"}`
		if err := os.WriteFile(cfgPath, []byte(badCfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBailianCredentials(dir); err == nil {
			t.Fatal("expected error for missing active_config profile")
		}
	})
}

func TestLocateBailianCLIPackage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "npm dir with spaces")
	pkgDir := filepath.Join(root, "node_modules", "bailian-cli")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"bailian-cli","version":"2.7.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := locateBailianCLIPackage(func(name string) (string, error) {
		if name == "bl.cmd" {
			return "", errors.New("missing bl.cmd")
		}
		if name == "bl.ps1" {
			return filepath.Join(root, "bl.ps1"), nil
		}
		return "", errors.New("unexpected binary")
	})
	if err != nil {
		t.Fatalf("locateBailianCLIPackage failed: %v", err)
	}
	if got != pkgDir {
		t.Fatalf("got %q, want %q", got, pkgDir)
	}

	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"bailian-cli","version":"2.7.1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := locateBailianCLIPackage(func(string) (string, error) {
		return filepath.Join(root, "bl.cmd"), nil
	}); err == nil {
		t.Fatal("expected version mismatch error for 2.7.1")
	}
}

func TestBridgeClientRealNodeMockScriptWithSpaces(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "path with spaces")
	pkgDir := filepath.Join(baseDir, "npm global", "node_modules", "bailian-cli")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"bailian-cli","version":"2.7.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mockScript := filepath.Join(baseDir, "mock bridge.mjs")
	scriptBody := `import fs from "node:fs";
const chunks = [];
for await (const c of process.stdin) chunks.push(c);
const stdinText = Buffer.concat(chunks).toString("utf8");
const creds = JSON.parse(stdinText);
if (process.env.TZ !== "Asia/Shanghai") {
  throw new Error("expected TZ=Asia/Shanghai, got " + process.env.TZ);
}
const envDump = JSON.stringify(process.env);
const argvDump = JSON.stringify(process.argv);
for (const secret of [creds.accessKeyId, creds.accessKeySecret, creds.securityToken, "SECRET_API_KEY"]) {
  if (envDump.includes(secret) || argvDump.includes(secret)) {
    throw new Error("secret leaked into argv or env");
  }
}
if (stdinText.includes("SECRET_API_KEY")) {
  throw new Error("apiKey must not be serialized onto bridge stdin");
}
if (process.argv[2] !== ` + "`" + strings.ReplaceAll(pkgDir, `\`, `\\`) + "`" + `) {
  throw new Error("unexpected pkgDir arg: " + process.argv[2]);
}
process.stdout.write(JSON.stringify([{
  model: "qwen-plus",
  type: "Text",
  remaining: 500000,
  total: 1000000,
  usagePercent: 50,
  remainingPercent: 50,
  expires: "2026-12-31",
  autoStop: true
}]));
`
	if err := os.WriteFile(mockScript, []byte(scriptBody), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &BridgeClient{
		ScriptPath: mockScript,
		PackageDir: pkgDir,
		Timeout:    10 * time.Second,
		LoadCredsFn: func(string) (BailianCredentials, error) {
			return BailianCredentials{
				AccessKeyID:     "SECRET_AK_ID",
				AccessKeySecret: "SECRET_AK_VAL",
				SecurityToken:   "SECRET_STS_VAL",
				APIKey:          "SECRET_API_KEY",
			}, nil
		},
	}

	obs, err := client.QueryFreeQuotas(context.Background())
	if err != nil {
		t.Fatalf("BridgeClient.QueryFreeQuotas failed: %v", err)
	}
	if len(obs) != 1 || obs[0].Model != "qwen-plus" || obs[0].RemainingPercent == nil || *obs[0].RemainingPercent != 50 {
		t.Fatalf("unexpected observations: %+v", obs)
	}

	// Verify BridgeClient enforces package.json 2.7.2 before running script.
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"bailian-cli","version":"2.6.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryFreeQuotas(context.Background()); err == nil {
		t.Fatal("expected error when bailian-cli package version is not 2.7.2")
	}
}

func TestActualBridgeScriptAgainstFakeBailianCLI(t *testing.T) {
	bridgeScript, err := filepath.Abs(filepath.Join("..", "..", "scripts", "bailian-quota-bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}

	pkgDir := filepath.Join(t.TempDir(), "fake bailian cli")
	coreDir := filepath.Join(pkgDir, "node_modules", "bailian-cli-core", "dist")
	cmdsDir := filepath.Join(pkgDir, "node_modules", "bailian-cli-commands", "dist")
	if err := os.MkdirAll(coreDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cmdsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"bailian-cli","version":"2.7.2","type":"module"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	coreMjs := `
export async function generateCLIAccessToken({ accessKeyId, accessKeySecret }) {
  if (!accessKeyId || !accessKeySecret) throw new Error("missing ak/sk");
  return { cliAccessToken: "ephemeral-token-xyz", Success: true };
}
export function resolveCLIAccessTokenTarget(settings, baseUrl) {
  return { region: settings.consoleRegion || "cn-beijing", site: settings.consoleSite || "domestic" };
}
export async function callConsoleGateway(target, timeoutSec, { api, data }) {
  if (target.token !== "ephemeral-token-xyz") throw new Error("missing ephemeral token");
  if (api === "zeldaHttp.dashscopeModel./zelda/api/v1/modelCenter/listFoundationModels") {
    return { models: [{ model: "qwen3-max", type: "Text" }] };
  }
  if (api === "zeldaEasy.bailian-commerce.freeTrial.queryFreeTierQuota") {
    return { quotas: [{ model: "qwen3-max", remaining: 1000, total: 2000 }] };
  }
  if (api === "zeldaEasy.bailian-commerce.freeTrial.queryFreeTierOnlyStatus") {
    return { statuses: [{ model: "qwen3-max", freeTierOnly: true }] };
  }
  throw new Error("unexpected gateway api: " + api);
}
`
	if err := os.WriteFile(filepath.Join(coreDir, "index.mjs"), []byte(coreMjs), 0o644); err != nil {
		t.Fatal(err)
	}

	cmdsMjs := `
export const usageFree = {
  async run(ctx) {
    if (ctx.settings.output !== "json" || !ctx.settings.outputExplicit) {
      throw new Error("expected json explicit output");
    }
    if (!ctx.flags.all || ctx.flags.sort !== "expires") {
      throw new Error("expected flags {all:true, sort:'expires'}");
    }
    if (ctx.settings.consoleRegion === "trigger-blocked-api") {
      await ctx.client.console("zeldaEasy.bailian-commerce.freeTrial.enableAutoStop", {});
      return;
    }
    await ctx.client.console("zeldaHttp.dashscopeModel./zelda/api/v1/modelCenter/listFoundationModels", {});
    await ctx.client.console("zeldaEasy.bailian-commerce.freeTrial.queryFreeTierQuota", { queryFreeTierQuotaRequest: { models: ["qwen3-max"] } });
    await ctx.client.console("zeldaEasy.bailian-commerce.freeTrial.queryFreeTierOnlyStatus", { queryFreeTierOnlyStatusRequest: { models: ["qwen3-max"] } });
    process.stdout.write(JSON.stringify([
      {
        model: "qwen3-max",
        type: "Text",
        remaining: 750000,
        total: 1000000,
        usagePercent: 25,
        remainingPercent: 75,
        expires: "2026-12-10",
        autoStop: true
      }
    ]) + "\n");
  }
};
`
	if err := os.WriteFile(filepath.Join(cmdsDir, "index.mjs"), []byte(cmdsMjs), 0o644); err != nil {
		t.Fatal(err)
	}

	// Success case across all 3 allowlisted APIs.
	client := &BridgeClient{
		ScriptPath: bridgeScript,
		PackageDir: pkgDir,
		Timeout:    10 * time.Second,
		LoadCredsFn: func(string) (BailianCredentials, error) {
			return BailianCredentials{
				AccessKeyID:     "TEST_AK_OK",
				AccessKeySecret: "TEST_SK_OK",
				ConsoleRegion:   "cn-beijing",
			}, nil
		},
	}
	obs, err := client.QueryFreeQuotas(context.Background())
	if err != nil {
		t.Fatalf("actual bridge script failed: %v", err)
	}
	if len(obs) != 1 || obs[0].Model != "qwen3-max" || obs[0].Remaining != 750000 || obs[0].RemainingPercent == nil || *obs[0].RemainingPercent != 75 {
		t.Fatalf("unexpected bridge output: %+v", obs)
	}

	// Allowlist rejection case.
	blockedClient := &BridgeClient{
		ScriptPath: bridgeScript,
		PackageDir: pkgDir,
		Timeout:    10 * time.Second,
		LoadCredsFn: func(string) (BailianCredentials, error) {
			return BailianCredentials{
				AccessKeyID:     "SECRET_AK_BLOCKED",
				AccessKeySecret: "SECRET_SK_BLOCKED",
				ConsoleRegion:   "trigger-blocked-api",
			}, nil
		},
	}
	_, err = blockedClient.QueryFreeQuotas(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Blocked non-allowlisted console API") {
		t.Fatalf("expected allowlist rejection error, got: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET_AK_BLOCKED") || strings.Contains(err.Error(), "SECRET_SK_BLOCKED") {
		t.Fatalf("error leaked credentials: %v", err)
	}

	// Verify script also rejects version mismatch when executed directly.
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"bailian-cli","version":"2.7.1","type":"module"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QueryFreeQuotas(context.Background()); err == nil {
		t.Fatal("expected error for bailian-cli version mismatch")
	}
}

func TestParseUsageFreeOutputAndProvider(t *testing.T) {
	if _, err := ParseUsageFreeOutput([]byte("   ")); err == nil {
		t.Fatal("expected error for empty usageFree output")
	}
	if _, err := ParseUsageFreeOutput([]byte("{not-array}")); err == nil {
		t.Fatal("expected error for non-array JSON")
	}
	empty, err := ParseUsageFreeOutput([]byte("[]"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty array parse failed: len=%d err=%v", len(empty), err)
	}
	parsed, err := ParseUsageFreeOutput([]byte(`[
		{"model":"m1","type":null,"remaining":null,"total":null,"usage_percent":10,"remaining_percent":90,"expires":null,"auto_stop":false}
	]`))
	if err != nil {
		t.Fatalf("snake_case parse failed: %v", err)
	}
	if len(parsed) != 1 || parsed[0].Model != "m1" || parsed[0].RemainingPercent == nil || *parsed[0].RemainingPercent != 90 {
		t.Fatalf("unexpected parsed result: %+v", parsed)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-from-config" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"workspace_rpm": 40, "workspace_tpm": 3686400}`)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"api_key":"sk-from-config"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := NewProvider(domain.BailianTrialConfig{
		WorkspaceID:      "ws-prov",
		BailianConfigDir: cfgDir,
		LimitsBaseURL:    srv.URL,
	}, "")
	lim, err := prov.QueryModelLimits(context.Background(), "qwen-plus")
	if err != nil || lim.WorkspaceRPM != 40 {
		t.Fatalf("Provider.QueryModelLimits lazy key fallback failed: lim=%+v err=%v", lim, err)
	}
}

func TestDecideTrialPool(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, domain.BeijingZone)
	oldModel := "qwen-old"
	oldID := domain.TrialProviderID(oldModel)
	existingModel := "qwen-keep"
	existingID := domain.TrialProviderID(existingModel)
	newModel := "qwen-new"
	newID := domain.TrialProviderID(newModel)
	cooldownModel := "qwen-cool"
	holdModel := "qwen-hold"

	baseSnap := domain.RegistrySnapshot{
		Providers: map[string]domain.Provider{
			"transcribe-bailian-trial": {
				ID:      "transcribe-bailian-trial",
				ChatURL: "https://ws-1.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
				APIKey:  "sk-trial-key",
				Off:     true,
			},
			"transcribe-bailian": {
				ID:     "transcribe-bailian",
				APIKey: "sk-comm-key",
				RPM:    5,
			},
			oldID: {
				ID:     oldID,
				Models: []string{oldModel},
				RPM:    10,
				Off:    false,
			},
			existingID: {
				ID:     existingID,
				Models: []string{existingModel},
				RPM:    5,
				Off:    true,
			},
		},
	}

	st := domain.State{
		Cooldowns: map[string]domain.CooldownEntry{
			domain.TrialMember(cooldownModel): {Member: domain.TrialMember(cooldownModel), Until: now.Add(10 * time.Minute)},
		},
		ManualHolds: map[string]domain.ManualHold{
			domain.ManualHoldKey("group/transcribe-trial", domain.TrialMember(holdModel)): {
				Group:  "group/transcribe-trial",
				Member: domain.TrialMember(holdModel),
			},
		},
		TrialPool: domain.TrialPoolState{
			Channels: map[string]domain.TrialChannelRecord{
				oldID: {ProviderID: oldID, Model: oldModel, Member: domain.TrialMember(oldModel), Active: true},
			},
		},
	}

	quotas := []domain.QuotaObservation{
		{Model: cooldownModel, Type: "Text", Remaining: 900, Total: 1000, RemainingPercent: floatPtr(90), Expires: "2026-11-22"},
		{Model: holdModel, Type: "Text", Remaining: 900, Total: 1000, RemainingPercent: floatPtr(90), Expires: "2026-11-23"},
		{Model: existingModel, Type: "Text", Remaining: 700, Total: 1000, RemainingPercent: floatPtr(70), Expires: "2026-11-24"},
		{Model: newModel, Type: "Text", Remaining: 600, Total: 1000, RemainingPercent: floatPtr(60), Expires: "2026-11-25"},
		{Model: "qwen-extra-1", Type: "Text", Remaining: 500, Total: 1000, RemainingPercent: floatPtr(50), Expires: "2026-11-26"},
		{Model: "qwen-extra-2", Type: "Text", Remaining: 400, Total: 1000, RemainingPercent: floatPtr(40), Expires: "2026-11-27"},
	}

	cfg := domain.BailianTrialConfig{
		Enabled:            true,
		WorkspaceID:        "ws-1",
		TemplateProvider:   "transcribe-bailian-trial",
		CommercialProvider: "transcribe-bailian",
		MaxActiveModels:    2,
	}

	dec := DecideTrialPool(PoolInput{
		Now:      now,
		Config:   cfg,
		Snapshot: baseSnap,
		State:    st,
		Quotas:   quotas,
		Limits: map[string]domain.RateLimits{
			existingModel: {Model: existingModel, WorkspaceRPM: 40, WorkspaceTPM: 36864 * 100}, // shared = 10
		},
	})

	wantMembers := []string{domain.TrialMember(existingModel), domain.TrialMember(newModel)}
	if !reflect.DeepEqual(dec.DesiredMembers, wantMembers) {
		t.Fatalf("DesiredMembers = %v, want %v", dec.DesiredMembers, wantMembers)
	}
	if dec.SharedRPM != 10 {
		t.Fatalf("SharedRPM = %d, want 10", dec.SharedRPM)
	}

	wantCmds := [][]string{
		{"provider", "on", existingID},
		{"provider", "rpm", existingID, "10"},
		{
			"provider", "add", newID,
			"id=" + newID,
			"chat=https://ws-1.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
			"key=sk-trial-key",
			"models=" + newModel,
			"free=yes",
			"list=off",
			"header." + domain.TrialHeaderName + "=" + newModel,
		},
		{"provider", "rpm", newID, "10"},
		{"provider", "rpm", "transcribe-bailian", "10"},
		{"provider", "off", oldID},
	}
	if len(dec.ProviderCommands) != len(wantCmds) {
		t.Fatalf("got %d commands (%+v), want %d (%+v)", len(dec.ProviderCommands), dec.ProviderCommands, len(wantCmds), wantCmds)
	}
	for i, want := range wantCmds {
		if !reflect.DeepEqual(dec.ProviderCommands[i].Args, want) {
			t.Errorf("cmd[%d] = %v, want %v", i, dec.ProviderCommands[i].Args, want)
		}
	}
	if !dec.NextChannels[existingID].Active || !dec.NextChannels[newID].Active || dec.NextChannels[oldID].Active {
		t.Fatalf("unexpected NextChannels active flags: %+v", dec.NextChannels)
	}

	// Verify Paused turns off all active trial sub-channels and clears DesiredMembers.
	pausedCfg := cfg
	pausedCfg.Paused = true
	pausedDec := DecideTrialPool(PoolInput{
		Now:      now,
		Config:   pausedCfg,
		Snapshot: baseSnap,
		State:    st,
		Quotas:   quotas,
	})
	if pausedDec.DesiredMembers != nil {
		t.Fatalf("paused DesiredMembers = %v, want nil", pausedDec.DesiredMembers)
	}
	if len(pausedDec.ProviderCommands) != 1 || !reflect.DeepEqual(pausedDec.ProviderCommands[0].Args, []string{"provider", "off", oldID}) {
		t.Fatalf("paused ProviderCommands = %+v, want [provider off %s]", pausedDec.ProviderCommands, oldID)
	}

	// Verify AccountCreditBlockedUntil in future turns off all active trial sub-channels.
	creditSt := st
	creditSt.TrialPool.AccountCreditBlockedUntil = now.Add(30 * time.Minute)
	creditDec := DecideTrialPool(PoolInput{
		Now:      now,
		Config:   cfg,
		Snapshot: baseSnap,
		State:    creditSt,
		Quotas:   quotas,
	})
	if creditDec.DesiredMembers != nil {
		t.Fatalf("credit-blocked DesiredMembers = %v, want nil", creditDec.DesiredMembers)
	}
	if len(creditDec.ProviderCommands) != 1 || !reflect.DeepEqual(creditDec.ProviderCommands[0].Args, []string{"provider", "off", oldID}) {
		t.Fatalf("credit-blocked ProviderCommands = %+v, want [provider off %s]", creditDec.ProviderCommands, oldID)
	}

	// Verify JSON serialization of BailianCredentials excludes APIKey.
	rawCreds, err := json.Marshal(BailianCredentials{AccessKeyID: "ak", AccessKeySecret: "sk", APIKey: "secret-api"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawCreds), "secret-api") {
		t.Fatalf("APIKey leaked into JSON marshaling: %s", string(rawCreds))
	}

	// Verify no eligible models turns off active trial sub-channels.
	emptyDec := DecideTrialPool(PoolInput{
		Now:      now,
		Config:   cfg,
		Snapshot: baseSnap,
		State:    st,
		Quotas:   []domain.QuotaObservation{{Model: "expired", Type: "Text", Remaining: 10, Total: 100, RemainingPercent: floatPtr(10), Expires: "2026-11-20"}},
	})
	if emptyDec.DesiredMembers != nil || len(emptyDec.ProviderCommands) != 1 || !reflect.DeepEqual(emptyDec.ProviderCommands[0].Args, []string{"provider", "off", oldID}) {
		t.Fatalf("empty eligible decision unexpected: %+v", emptyDec)
	}

	// Verify idempotent steady-state produces zero commands and workspace_id + commercial key fallback works.
	steadyID := domain.TrialProviderID(newModel)
	steadySnap := domain.RegistrySnapshot{
		Providers: map[string]domain.Provider{
			"transcribe-bailian": {ID: "transcribe-bailian", APIKey: "sk-comm-only", RPM: 10},
			steadyID:             {ID: steadyID, Models: []string{newModel}, RPM: 10, Off: false},
		},
	}
	steadyDec := DecideTrialPool(PoolInput{
		Now:      now,
		Config:   cfg,
		Snapshot: steadySnap,
		Quotas:   []domain.QuotaObservation{{Model: newModel, Type: "Text", Remaining: 100, Total: 200, RemainingPercent: floatPtr(50), Expires: "2026-12-01"}},
		Limits:   map[string]domain.RateLimits{newModel: {WorkspaceRPM: 40}},
	})
	if len(steadyDec.ProviderCommands) != 0 || !reflect.DeepEqual(steadyDec.DesiredMembers, []string{domain.TrialMember(newModel)}) {
		t.Fatalf("steady state decision should emit 0 commands, got %+v", steadyDec)
	}
}
