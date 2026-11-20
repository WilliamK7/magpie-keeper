package magpie

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"magpie-keeper/internal/domain"
)

func setupPortableDir(t *testing.T, withSpaces bool) string {
	t.Helper()
	base := t.TempDir()
	if withSpaces {
		base = filepath.Join(base, "magpie dir")
	}
	for _, dir := range []string{
		base,
		filepath.Join(base, "data"),
		filepath.Join(base, "data", "cache", "models"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestFile(t, filepath.Join(base, ".portable"), "")
	writeTestFile(t, filepath.Join(base, "magpie.com"), "stub")
	writeTestFile(t, filepath.Join(base, "data", "providers.json"), "{}")
	return base
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func buildStubMagpieCLI(t *testing.T, magpieDir string) {
	t.Helper()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "main.go")
	src := `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if logPath := os.Getenv("STUB_ARGS_FILE"); logPath != "" {
		b, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(logPath, b, 0o600)
	}
	if msStr := os.Getenv("STUB_SLEEP_MS"); msStr != "" {
		if ms, err := strconv.Atoi(msStr); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	for _, a := range os.Args[1:] {
		if a == "--fail" {
			fmt.Fprintf(os.Stderr, "stub failure args=%v secret=%s\n", os.Args[1:], os.Getenv("STUB_LEAK_SECRET"))
			os.Exit(2)
		}
	}
}
`
	writeTestFile(t, srcPath, src)

	goExe := "go"
	if runtime.GOOS == "windows" {
		candidate := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
		if _, err := os.Stat(candidate); err == nil {
			goExe = candidate
		}
	} else {
		candidate := filepath.Join(runtime.GOROOT(), "bin", "go")
		if _, err := os.Stat(candidate); err == nil {
			goExe = candidate
		}
	}

	outBin := filepath.Join(magpieDir, "magpie.com")
	_ = os.Remove(outBin)
	cmd := exec.Command(goExe, "build", "-o", outBin, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile stub magpie.com: %v\n%s", err, string(out))
	}
}

func TestValidatePortableDir(t *testing.T) {
	t.Run("missing .portable", func(t *testing.T) {
		dir := setupPortableDir(t, true)
		_ = os.Remove(filepath.Join(dir, ".portable"))
		if _, err := ValidatePortableDir(dir, "http://127.0.0.1:3425/v1"); err == nil {
			t.Fatal("expected error when .portable missing")
		}
	})

	t.Run("missing magpie.com", func(t *testing.T) {
		dir := setupPortableDir(t, true)
		_ = os.Remove(filepath.Join(dir, "magpie.com"))
		if _, err := ValidatePortableDir(dir, "http://127.0.0.1:3425/v1"); err == nil {
			t.Fatal("expected error when magpie.com missing")
		}
	})

	t.Run("missing providers.json", func(t *testing.T) {
		dir := setupPortableDir(t, true)
		_ = os.Remove(filepath.Join(dir, "data", "providers.json"))
		if _, err := ValidatePortableDir(dir, "http://127.0.0.1:3425/v1"); err == nil {
			t.Fatal("expected error when providers.json missing")
		}
	})

	t.Run("default port 3425 when settings absent or zero", func(t *testing.T) {
		dir := setupPortableDir(t, true)
		port, err := ValidatePortableDir(dir, "http://127.0.0.1:3425/v1")
		if err != nil || port != 3425 {
			t.Fatalf("got (%d, %v), want (3425, nil)", port, err)
		}
		writeTestFile(t, filepath.Join(dir, "data", "settings.json"), `{"port": 0}`)
		port, err = ValidatePortableDir(dir, "http://127.0.0.1:3425/v1")
		if err != nil || port != 3425 {
			t.Fatalf("got (%d, %v), want (3425, nil)", port, err)
		}
	})

	t.Run("settings port mismatch and scheme defaults", func(t *testing.T) {
		dir := setupPortableDir(t, true)
		writeTestFile(t, filepath.Join(dir, "data", "settings.json"), `{"port": 4000}`)
		if _, err := ValidatePortableDir(dir, "http://127.0.0.1:3425/v1"); err == nil {
			t.Fatal("expected port mismatch error")
		}
		port, err := ValidatePortableDir(dir, "http://127.0.0.1:4000/v1")
		if err != nil || port != 4000 {
			t.Fatalf("got (%d, %v), want (4000, nil)", port, err)
		}

		writeTestFile(t, filepath.Join(dir, "data", "settings.json"), `{"port": 80}`)
		if port, err := ValidatePortableDir(dir, "http://localhost/v1"); err != nil || port != 80 {
			t.Fatalf("http default port check failed: (%d, %v)", port, err)
		}

		writeTestFile(t, filepath.Join(dir, "data", "settings.json"), `{"port": 443}`)
		if port, err := ValidatePortableDir(dir, "https://localhost/v1"); err != nil || port != 443 {
			t.Fatalf("https default port check failed: (%d, %v)", port, err)
		}
	})
}

func TestReadRegistryMergingAndCacheValidation(t *testing.T) {
	dir := setupPortableDir(t, true)
	writeTestFile(t, filepath.Join(dir, "data", "settings.json"), `{"port": 3425}`)
	writeTestFile(t, filepath.Join(dir, "data", "providers.json"), `{
		"providers": {
			"openrouter": {
				"kind": "openai",
				"chat": "https://openrouter.ai/api/v1/chat/completions",
				"key": "sk-or-1",
				"models": ["qwen/qwen3:free", "bad-model"],
				"modelsOff": ["bad-model"],
				"_models": {
					"qwen/qwen3:free": {
						"type": "language",
						"outputModalities": ["text"],
						"free": true
					}
				},
				"headers": {"X-Test": "1"},
				"list": "off",
				"rpm": 30
			},
			"stale-cache-provider": {
				"base": "https://api.new-endpoint.example/v1",
				"key": "sk-new",
				"models": ["kept-model"]
			},
			"disabled-nomodels": {
				"base": "https://example.com/v1",
				"noModels": true,
				"off": false
			}
		},
		"groups": {
			"transcribe": {
				"models": ["group/transcribe-free", "group/transcribe-paid"],
				"routing": "order",
				"stays": false,
				"hidden": false,
				"weight": {"group/transcribe-free": 2, "group/transcribe-paid": 1}
			}
		}
	}`)

	writeTestFile(t, filepath.Join(dir, "data", "plugin-providers.json"), `{
		"copilot": {
			"signedIn": false,
			"free": true,
			"base": "https://api.githubcopilot.com/",
			"models": ["gpt-4o"]
		},
		"antigravity": {
			"signedIn": true,
			"free": "yes",
			"models": ["gemini-2.5-flash"]
		}
	}`)

	// Matching base (normalized from ChatURL with uppercase host + trailing slash + default :443).
	writeTestFile(t, filepath.Join(dir, "data", "cache", "models", "openrouter.json"), `{
		"base": "HTTPS://OpenRouter.AI:443/api/v1/",
		"models": [
			{"id": "deepseek/r1:free", "type": "language", "outputModalities": ["text"], "free": true},
			{"id": "decider-model", "type": "language", "outputModalities": ["text"], "decides": true}
		]
	}`)

	// Mismatched base must NOT be merged.
	writeTestFile(t, filepath.Join(dir, "data", "cache", "models", "stale-cache-provider.json"), `{
		"base": "https://api.old-endpoint.example/v1",
		"models": [
			{"id": "should-not-appear", "type": "language", "outputModalities": ["text"]}
		]
	}`)

	gw := NewGateway(dir, "http://127.0.0.1:3425/v1", "k", "", nil)
	snap, err := gw.ReadRegistry(context.Background())
	if err != nil {
		t.Fatalf("ReadRegistry failed: %v", err)
	}

	if snap.Port != 3425 || len(snap.Providers) != 5 || len(snap.Groups) != 1 {
		t.Fatalf("unexpected snapshot counts: port=%d providers=%d groups=%d", snap.Port, len(snap.Providers), len(snap.Groups))
	}

	or := snap.Providers["openrouter"]
	if or.List != "off" || or.RPM != 30 || or.Headers["X-Test"] != "1" {
		t.Fatalf("unexpected openrouter fields: %+v", or)
	}
	if !domain.IsModelAvailable(or, "deepseek/r1:free") {
		t.Fatal("expected cached deepseek/r1:free to be merged and available")
	}
	if domain.IsModelAvailable(or, "decider-model") {
		t.Fatal("expected cached decider-model to be marked decides=true and unavailable")
	}
	if domain.IsModelAvailable(or, "bad-model") {
		t.Fatal("expected modelsOff bad-model to be unavailable")
	}

	stale := snap.Providers["stale-cache-provider"]
	if len(stale.Models) != 1 || stale.Models[0] != "kept-model" {
		t.Fatalf("mismatched cache should not merge, got models=%v", stale.Models)
	}

	copilot := snap.Providers["copilot"]
	if !copilot.Plugin || copilot.SignedIn == nil || *copilot.SignedIn {
		t.Fatalf("expected copilot Plugin=true and SignedIn=false pointer, got %+v", copilot)
	}
	if domain.IsProviderAvailable(copilot) {
		t.Fatal("expected copilot with signedIn=false to be unavailable")
	}

	ag := snap.Providers["antigravity"]
	if !domain.IsProviderAvailable(ag) || !ag.Free {
		t.Fatalf("expected antigravity available and free, got %+v", ag)
	}

	grp := snap.Groups["transcribe"]
	if grp.Stays != "off" || grp.Routing != "order" || grp.Weights["group/transcribe-free"] != 2 {
		t.Fatalf("unexpected group parsed: %+v", grp)
	}
}

func TestReadRegistrySlicesAndFlatMap(t *testing.T) {
	dir := setupPortableDir(t, true)
	writeTestFile(t, filepath.Join(dir, "data", "providers.json"), `{
		"providers": [
			{"id": "p1", "chat": "https://p1.example/v1/chat/completions", "models": ["m1"], "rpm": "60"}
		],
		"groups": [
			{"name": "group/transcribe", "models": ["p1/m1"], "routing": "weight", "stays": "", "hidden": true, "weights": {"p1/m1": 5}}
		]
	}`)
	gw := NewGateway(dir, "http://127.0.0.1:3425/v1", "", "", nil)
	snap, err := gw.ReadRegistry(context.Background())
	if err != nil {
		t.Fatalf("ReadRegistry slice failed: %v", err)
	}
	if snap.Providers["p1"].RPM != 60 {
		t.Fatalf("expected rpm=60, got %d", snap.Providers["p1"].RPM)
	}
	g, ok := snap.Groups["transcribe"]
	if !ok || !g.Hidden || g.Stays != "" || g.Weights["p1/m1"] != 5 {
		t.Fatalf("unexpected group from slice: %+v (ok=%v)", g, ok)
	}

	// Flat map format with group/ prefix and provider entry.
	writeTestFile(t, filepath.Join(dir, "data", "providers.json"), `{
		"p2": {"base": "https://p2.example/v1", "key": "secret", "off": true},
		"group/transcribe-free": {"models": ["p2/m2"], "routing": "fast", "stays": "off", "weight": "p2/m2=3,p3/m3=1"}
	}`)
	snap, err = gw.ReadRegistry(context.Background())
	if err != nil {
		t.Fatalf("ReadRegistry flat map failed: %v", err)
	}
	if !snap.Providers["p2"].Off {
		t.Fatal("expected p2 off=true")
	}
	gf, ok := snap.Groups["transcribe-free"]
	if !ok || gf.Routing != "fast" || gf.Stays != "off" || gf.Weights["p2/m2"] != 3 || gf.Weights["p3/m3"] != 1 {
		t.Fatalf("unexpected flat group: %+v (ok=%v)", gf, ok)
	}
}

func TestBackupRegistryAndExecCLI(t *testing.T) {
	dir := setupPortableDir(t, true)
	rawContent := `{"providers":{"p1":{"key":"sk-demo"}}}`
	writeTestFile(t, filepath.Join(dir, "data", "providers.json"), rawContent)

	customBackup := filepath.Join(t.TempDir(), "custom backups")
	gw := NewGateway(dir, "http://127.0.0.1:3425/v1", "magpie-secret-key", customBackup, nil)

	b1, err := gw.BackupRegistry(context.Background())
	if err != nil {
		t.Fatalf("BackupRegistry failed: %v", err)
	}
	b2, err := gw.BackupRegistry(context.Background())
	if err != nil {
		t.Fatalf("second BackupRegistry failed: %v", err)
	}
	if b1 == b2 {
		t.Fatalf("expected distinct backup paths, got %q", b1)
	}
	tsDirRe := regexp.MustCompile(`^\d{8}-\d{6}\.\d{3}$`)
	if !tsDirRe.MatchString(filepath.Base(filepath.Dir(b1))) {
		t.Fatalf("unexpected backup directory timestamp format: %q", b1)
	}
	gotBytes, err := os.ReadFile(b1)
	if err != nil || string(gotBytes) != rawContent {
		t.Fatalf("backup content mismatch: got=%q err=%v", string(gotBytes), err)
	}

	// Verify empty backupDir falls back to <magpie_dir>/data/backups/<ts>/providers.json.
	gwDefaultBackup := NewGateway(dir, "http://127.0.0.1:3425/v1", "magpie-secret-key", "", nil)
	bDefault, err := gwDefaultBackup.BackupRegistry(context.Background())
	if err != nil {
		t.Fatalf("default BackupRegistry failed: %v", err)
	}
	wantPrefix := filepath.Join(dir, "data", "backups") + string(os.PathSeparator)
	if !strings.HasPrefix(bDefault, wantPrefix) {
		t.Fatalf("default backup path %q does not live under %q", bDefault, wantPrefix)
	}

	// Build tiny Go stub magpie.com inside directory with spaces.
	buildStubMagpieCLI(t, dir)

	argsFile := filepath.Join(t.TempDir(), "captured args.json")
	t.Setenv("STUB_ARGS_FILE", argsFile)
	err = gw.ExecCLI(context.Background(), "provider", "add", "trial-1", "key=sk-top-secret-999", "models=qwen-plus")
	if err != nil {
		t.Fatalf("ExecCLI failed: %v", err)
	}
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	var gotArgs []string
	if err := json.Unmarshal(rawArgs, &gotArgs); err != nil {
		t.Fatalf("unmarshal captured args: %v", err)
	}
	if len(gotArgs) != 5 || gotArgs[3] != "key=sk-top-secret-999" {
		t.Fatalf("unexpected stub args: %v", gotArgs)
	}

	// Verify key=... and leaked raw secret are redacted from error messages.
	t.Setenv("STUB_LEAK_SECRET", "sk-top-secret-999")
	err = gw.ExecCLI(context.Background(), "provider", "add", "trial-1", "key=sk-top-secret-999", "--fail")
	if err == nil {
		t.Fatal("expected error from failing stub")
	}
	if strings.Contains(err.Error(), "sk-top-secret-999") {
		t.Fatalf("secret leaked in ExecCLI error: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "key=[REDACTED]") {
		t.Fatalf("expected redacted key placeholder in error: %s", err.Error())
	}

	// Verify 5s vs 30s timeout selection and actual timeout behavior.
	if got := gw.cliTimeout([]string{"group", "set", "transcribe"}); got != 5*time.Second {
		t.Fatalf("default timeout = %s, want 5s", got)
	}
	if got := gw.cliTimeout([]string{"provider", "refresh", "openrouter"}); got != 30*time.Second {
		t.Fatalf("refresh timeout = %s, want 30s", got)
	}

	gw.defaultTimeout = 150 * time.Millisecond
	gw.refreshTimeout = 1200 * time.Millisecond
	t.Setenv("STUB_LEAK_SECRET", "")
	t.Setenv("STUB_SLEEP_MS", "450")

	if err := gw.ExecCLI(context.Background(), "group", "set", "transcribe"); err == nil {
		t.Fatal("expected non-refresh CLI call to time out")
	}
	if err := gw.ExecCLI(context.Background(), "provider", "refresh", "openrouter"); err != nil {
		t.Fatalf("expected provider refresh to use longer refresh timeout, got: %v", err)
	}
}

func TestListServedModels(t *testing.T) {
	var seenAuth string
	var seenPath string
	hasMore := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "openrouter/qwen:free"},
				{"id": "bailian/qwen-max"},
				{"id": "openrouter/qwen:free"},
			},
			"has_more": hasMore,
		})
	}))
	defer srv.Close()

	for _, baseURL := range []string{srv.URL + "/v1/", srv.URL + "/v1", srv.URL} {
		gw := NewGateway("", baseURL, "magpie-secret", "", srv.Client())
		models, err := gw.ListServedModels(context.Background())
		if err != nil {
			t.Fatalf("ListServedModels(%q) failed: %v", baseURL, err)
		}
		if seenPath != "/v1/models" || seenAuth != "Bearer magpie-secret" {
			t.Fatalf("unexpected request for %q: path=%q auth=%q", baseURL, seenPath, seenAuth)
		}
		if len(models) != 2 || models[0] != "openrouter/qwen:free" || models[1] != "bailian/qwen-max" {
			t.Fatalf("unexpected served models: %v", models)
		}
	}

	gw := NewGateway("", srv.URL+"/v1/", "magpie-secret", "", srv.Client())
	hasMore = true
	if _, err := gw.ListServedModels(context.Background()); !errors.Is(err, ErrServedModelsTruncated) {
		t.Fatalf("expected ErrServedModelsTruncated, got %v", err)
	}
}

func TestReadRegistryRealMagpieFormats(t *testing.T) {
	dir := setupPortableDir(t, true)
	writeTestFile(t, filepath.Join(dir, "data", "settings.json"), `{"theme": "dark"}`)
	writeTestFile(t, filepath.Join(dir, "data", "providers.json"), `{
		"providers": [
			{
				"id": "transcribe-bailian",
				"chat": "https://ws.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions",
				"baseAPI": "https://ws.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
				"models": ["qwen3.8-max"],
				"listOff": true,
				"maxRPM": 45
			},
			{
				"id": "custom-resp",
				"responses": "https://resp.example/v1/responses",
				"models": ["gpt-6-luna"]
			}
		],
		"groups": [
			{
				"id": "transcribe",
				"name": "transcribe",
				"members": ["group/transcribe-free", "group/transcribe-paid"],
				"routing": "order",
				"affinity": "off",
				"off": ["group/transcribe-free"]
			}
		]
	}`)
	writeTestFile(t, filepath.Join(dir, "data", "plugin-providers.json"), `[
		{
			"id": "opencode-zen-free",
			"api": "https://opencode.ai/zen/v1/chat/completions",
			"signedIn": true,
			"models": [
				{
					"id": "deepseek-v4.1-flash-free",
					"free": true,
					"type": "language",
					"outputs": ["text"]
				},
				{
					"id": "router-decider",
					"decides": true,
					"modalities": {"output": ["text"]}
				}
			]
		}
	]`)

	gw := NewGateway(dir, "http://127.0.0.1:3425/v1", "magpie-transcribe", "", nil)
	snap, err := gw.ReadRegistry(context.Background())
	if err != nil {
		t.Fatalf("ReadRegistry failed on real Magpie formats: %v", err)
	}

	b := snap.Providers["transcribe-bailian"]
	if b.List != "off" || b.RPM != 45 || b.BaseURL != "https://ws.cn-beijing.maas.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("unexpected bailian provider parsed: %+v", b)
	}

	cr := snap.Providers["custom-resp"]
	if cr.ChatURL != "https://resp.example/v1/responses" {
		t.Fatalf("expected responses fallback for ChatURL, got %q", cr.ChatURL)
	}

	zen := snap.Providers["opencode-zen-free"]
	if !zen.Plugin || zen.ChatURL != "https://opencode.ai/zen/v1/chat/completions" {
		t.Fatalf("unexpected plugin provider parsed: %+v", zen)
	}
	if !domain.IsModelAvailable(zen, "deepseek-v4.1-flash-free") || !zen.ModelMeta["deepseek-v4.1-flash-free"].Free {
		t.Fatalf("expected deepseek-v4.1-flash-free available & free, meta=%+v", zen.ModelMeta["deepseek-v4.1-flash-free"])
	}
	if domain.IsModelAvailable(zen, "router-decider") {
		t.Fatal("expected router-decider with decides=true to be unavailable")
	}

	grp, ok := snap.Groups["transcribe"]
	if !ok || grp.Stays != "off" || grp.Off || len(grp.Models) != 2 || grp.Models[0] != "group/transcribe-free" {
		t.Fatalf("unexpected group parsed: %+v (ok=%v)", grp, ok)
	}
}
