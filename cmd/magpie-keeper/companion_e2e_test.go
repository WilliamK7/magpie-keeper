package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"magpie-keeper/internal/config"
	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/state"
)

func TestWatchCompanionOfflineClosedLoop(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node binary not found in PATH")
	}

	root := filepath.Join(t.TempDir(), "magpie companion e2e")
	magpieDir := filepath.Join(root, "magpie portable")
	dataDir := filepath.Join(magpieDir, "data")
	routingDir := filepath.Join(dataDir, "routing")
	keeperData := filepath.Join(root, "keeper data")
	backupDir := filepath.Join(keeperData, "backups")
	bailianCfgDir := filepath.Join(root, "bailian cfg")
	npmBinDir := filepath.Join(root, "npm global bin")
	bridgeDir := filepath.Join(root, "bridge scripts")
	binDir := filepath.Join(root, "keeper bin")

	for _, d := range []string{routingDir, keeperData, backupDir, bailianCfgDir, filepath.Join(npmBinDir, "node_modules", "bailian-cli"), bridgeDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var v1ModelsCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			v1ModelsCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"transcribe-bailian/qwen3.8-max"}],"has_more":false}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			serveMockAdmissionSSE(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/models/limits":
			model := r.URL.Query().Get("model")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"output": map[string]any{
					"quotas": []any{
						map[string]any{
							"model": model,
							"workspace_limit": map[string]any{
								"request_limit":        120,
								"request_limit_period": 60,
								"usage_limit":          36864 * 200,
								"usage_limit_period":   60,
							},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	parsedSrv, _ := url.Parse(srv.URL)
	srvPort, _ := strconv.Atoi(parsedSrv.Port())

	if err := os.WriteFile(filepath.Join(magpieDir, ".portable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dataDir, "settings.json"), map[string]any{"port": srvPort})
	providersPath := filepath.Join(dataDir, "providers.json")
	writeJSONFile(t, providersPath, map[string]any{
		"providers": map[string]any{
			"transcribe-bailian-trial": map[string]any{
				"chat": "https://ws-e2e.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
				"key":  "sk-trial-secret",
				"off":  true,
			},
			"transcribe-bailian": map[string]any{
				"chat":   "https://dashscope.aliyuncs.com/compatible-mode/v1",
				"key":    "sk-comm-secret",
				"models": []string{"qwen3.8-max"},
			},
		},
		"groups": map[string]any{
			"transcribe-free": map[string]any{
				"models":  []string{"group/transcribe-openrouter-free"},
				"routing": "weight",
				"stays":   "off",
			},
			"transcribe-paid": map[string]any{
				"models":  []string{"transcribe-bailian/qwen3.8-max"},
				"routing": "weight",
				"stays":   "off",
			},
			"transcribe": map[string]any{
				"models":  []string{"group/transcribe-free", "group/transcribe-paid"},
				"routing": "order",
				"stays":   "off",
			},
		},
	})

	// Build stub magpie.com and real magpie-keeper.exe into space-containing directories.
	buildStubMagpieCom(t, filepath.Join(magpieDir, "magpie.com"))
	keeperExe := filepath.Join(binDir, "magpie-keeper.exe")
	buildBinary(t, ".", keeperExe)

	// Fake bailian-cli 2.7.2 package + bl.cmd + ~/.bailian/config.json.
	if err := os.WriteFile(filepath.Join(npmBinDir, "bl.cmd"), []byte("@echo off\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(npmBinDir, "node_modules", "bailian-cli", "package.json"), map[string]any{
		"name":    "bailian-cli",
		"version": "2.7.2",
	})
	writeJSONFile(t, filepath.Join(bailianCfgDir, "config.json"), map[string]any{
		"active_config": "work",
		"work": map[string]any{
			"access_key_id":     "ak-e2e-test",
			"access_key_secret": "sk-e2e-test",
			"api_key":           "dash-e2e-key",
		},
	})

	// Mock Node quota bridge returning 4 Text models expiring after Beijing today.
	nowBJ := time.Now().In(domain.BeijingZone)
	d1 := nowBJ.AddDate(0, 0, 5).Format("2006-01-02")
	d2 := nowBJ.AddDate(0, 0, 6).Format("2006-01-02")
	d3 := nowBJ.AddDate(0, 0, 7).Format("2006-01-02")
	d4 := nowBJ.AddDate(0, 0, 8).Format("2006-01-02")
	bridgeScript := filepath.Join(bridgeDir, "mock-bridge.mjs")
	bridgeSrc := fmt.Sprintf(`import fs from "node:fs";
const creds = JSON.parse(fs.readFileSync(0, "utf8"));
if (!creds.accessKeyId || !creds.accessKeySecret) process.exit(2);
console.log(JSON.stringify([
  { model: "m1", type: "Text", remaining: 1000, total: 2000, remainingPercent: 50, expires: %q },
  { model: "m2", type: "Text", remaining: 1000, total: 2000, remainingPercent: 50, expires: %q },
  { model: "m3", type: "Text", remaining: 1000, total: 2000, remainingPercent: 50, expires: %q },
  { model: "m4", type: "Text", remaining: 1000, total: 2000, remainingPercent: 50, expires: %q }
]));
`, d1, d2, d3, d4)
	if err := os.WriteFile(bridgeScript, []byte(bridgeSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-seed fresh LastChannelSyncAt so lazy refresh skips ordinary channel sync.
	statePath := filepath.Join(keeperData, "state.json")
	stStore := state.NewStore(statePath)
	st, err := stStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.LastChannelSyncAt = time.Now().Add(-10 * time.Minute)
	if err := stStore.Save(st); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(keeperData, "channels.json")
	cfg := domain.Config{
		MagpieDir:              magpieDir,
		MagpieURL:              srv.URL + "/v1",
		MagpieKey:              "magpie-e2e-key",
		Roles:                  []string{domain.RoleTranscribe},
		ChannelSyncInterval:    "6h",
		MaxEvaluationsPerCycle: 4,
		MaxEvaluationsPerDay:   10,
		CooldownSeconds:        3600,
		BailianTrial: domain.BailianTrialConfig{
			Enabled:            true,
			WorkspaceID:        "ws-e2e",
			TemplateProvider:   "transcribe-bailian-trial",
			CommercialProvider: "transcribe-bailian",
			MaxActiveModels:    3,
			RefreshInterval:    "60s",
			BridgeScript:       bridgeScript,
			BailianConfigDir:   bailianCfgDir,
			LimitsBaseURL:      srv.URL,
		},
		Providers: map[string]domain.ProviderRule{
			"transcribe-bailian": {
				Adapter: "registry",
				Billing: []string{domain.BillingCommercial},
			},
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, keeperExe,
		"watch", "--companion",
		"--config", cfgPath,
		"--state", statePath,
		"--backups", backupDir,
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+npmBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start magpie-keeper watch --companion failed: %v", err)
	}

	id1 := domain.TrialProviderID("m1")
	id2 := domain.TrialProviderID("m2")
	id3 := domain.TrialProviderID("m3")
	id4 := domain.TrialProviderID("m4")

	// Wait for initial trial sync to activate m1, m2, m3 in providers.json.
	waitUntil(t, 8*time.Second, func() bool {
		reg := readProvidersJSON(t, providersPath)
		grp := reg.Groups["transcribe-trial"]
		return slices.Contains(grp.Models, id1+"/m1") &&
			slices.Contains(grp.Models, id2+"/m2") &&
			slices.Contains(grp.Models, id3+"/m3") &&
			!reg.Providers[id1].Off &&
			reg.Providers[id1].RPM == 30 &&
			reg.Providers[id1].Headers[domain.TrialHeaderName] == "m1"
	}, "initial trial sync did not activate m1, m2, m3; stderr="+stderr.String())

	// Verify lazy refresh skipped ordinary channel sync (/v1/models should not have been called).
	if got := v1ModelsCalls.Load(); got != 0 {
		t.Fatalf("expected lazy refresh to skip channel sync (0 /v1/models calls), got %d", got)
	}
	if !strings.Contains(stderr.String(), "lazy channel sync skipped") {
		t.Fatalf("expected lazy channel sync skipped log, stderr=%s", stderr.String())
	}

	// Emit free quota exhausted for m1 in routing/*.jsonl and assert rapid offline + m4 promotion.
	routeLog := filepath.Join(routingDir, "2026-11-20.jsonl")
	alertLine := fmt.Sprintf(`{"tries":[{"provider":"%s","model":"m1","status":429,"error":"Free quota exhausted for model m1"}]}`+"\n", id1)
	if err := os.WriteFile(routeLog, []byte(alertLine), 0o644); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 8*time.Second, func() bool {
		reg := readProvidersJSON(t, providersPath)
		grp := reg.Groups["transcribe-trial"]
		return reg.Providers[id1].Off &&
			!slices.Contains(grp.Models, id1+"/m1") &&
			slices.Contains(grp.Models, id2+"/m2") &&
			slices.Contains(grp.Models, id3+"/m3") &&
			slices.Contains(grp.Models, id4+"/m4") &&
			!reg.Providers[id4].Off
	}, "routing alert did not turn off m1 and promote m4; stderr="+stderr.String())

	// Verify backups were created before writes.
	backupEntries, err := os.ReadDir(backupDir)
	if err != nil || len(backupEntries) == 0 {
		t.Fatalf("expected backup directories under %s, got entries=%v err=%v", backupDir, backupEntries, err)
	}

	// Close stdin pipe -> companion mode must exit gracefully with code 0.
	if err := stdinPipe.Close(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("expected exit 0 on stdin EOF, got err=%v stderr=%s", err, stderr.String())
		}
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("magpie-keeper did not exit promptly after stdin EOF; stderr=%s", stderr.String())
	}
}

func TestConfigSubcommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cli cfg")
	cfgPath := filepath.Join(dir, "channels.json")
	statePath := filepath.Join(dir, "state.json")
	backupDir := filepath.Join(dir, "backups")

	cfg := domain.Config{
		MagpieDir: `D:\NonExistent\magpie`,
		MagpieURL: "http://127.0.0.1:3425/v1",
		BailianTrial: domain.BailianTrialConfig{
			Enabled:     true,
			WorkspaceID: "ws-cfg",
		},
		Providers: map[string]domain.ProviderRule{},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		var out bytes.Buffer
		opts := &appOptions{
			configPath: cfgPath,
			statePath:  statePath,
			backupDir:  backupDir,
			stdout:     &out,
		}
		root := newRootCommand(opts)
		root.SetArgs(args)
		root.SetOut(&out)
		root.SetErr(&out)
		if err := root.Execute(); err != nil {
			t.Fatalf("args %v failed: %v", args, err)
		}
		return out.String()
	}

	run("add-provider", "custom-or", "--adapter", "openrouter", "--billing", "free", "--include", "*:free", "--max-free", "3")
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Providers["custom-or"].Adapter != "openrouter" || loaded.Providers["custom-or"].MaxCandidates["free"] != 3 {
		t.Fatalf("unexpected added provider: %+v", loaded.Providers["custom-or"])
	}

	run("pause-trial")
	loaded, _ = config.Load(cfgPath)
	if !loaded.BailianTrial.Paused {
		t.Fatal("expected bailian_trial.paused=true")
	}

	run("resume-trial")
	loaded, _ = config.Load(cfgPath)
	if loaded.BailianTrial.Paused {
		t.Fatal("expected bailian_trial.paused=false")
	}

	run("remove-provider", "custom-or")
	loaded, _ = config.Load(cfgPath)
	if _, exists := loaded.Providers["custom-or"]; exists {
		t.Fatal("expected custom-or to be removed")
	}
}

func serveMockAdmissionSSE(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var userText string
	for _, m := range req.Messages {
		if m.Role == "user" {
			userText = m.Content
		}
	}

	var payload map[string]string
	switch {
	case strings.Contains(userText, "Do not disable the cache."):
		payload = map[string]string{
			"1": "请勿禁用缓存。",
			"2": "它存储了 12 条结果。",
		}
	case strings.Contains(userText, "35 degrees"):
		payload = map[string]string{
			"1": "如果温度超过 35 度，",
			"2": "请停止水泵。",
		}
	default:
		payload = map[string]string{
			"1": "先初始化 3 个工作线程。",
			"2": "等待 30 秒后再重试。",
			"3": "先不要清空缓存。",
			"4": "它会保留 12 条最近记录。",
			"5": "把每次超时写入磁盘日志。",
			"6": "再次检查队列深度。",
			"7": "在高负载下对请求限流。",
			"8": "传输完成后校验和检查。",
			"9": "向监控器汇报完成状态。",
		}
	}
	b, _ := json.Marshal(payload)

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	chunk1, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": string(b)}}},
	})
	chunk2, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"completion_tokens_details": map[string]any{"reasoning_tokens": 0}},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", chunk1, chunk2)
}

func buildStubMagpieCom(t *testing.T, outExe string) {
	t.Helper()
	srcDir := t.TempDir()
	src := `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type prov struct {
	Chat    string            ` + "`json:\"chat,omitempty\"`" + `
	Key     string            ` + "`json:\"key,omitempty\"`" + `
	Models  []string          ` + "`json:\"models,omitempty\"`" + `
	Off     bool              ` + "`json:\"off,omitempty\"`" + `
	Free    bool              ` + "`json:\"free,omitempty\"`" + `
	List    string            ` + "`json:\"list,omitempty\"`" + `
	RPM     int               ` + "`json:\"rpm,omitempty\"`" + `
	Headers map[string]string ` + "`json:\"headers,omitempty\"`" + `
}
type grp struct {
	Models  []string       ` + "`json:\"models,omitempty\"`" + `
	Routing string         ` + "`json:\"routing,omitempty\"`" + `
	Stays   string         ` + "`json:\"stays,omitempty\"`" + `
	Weights map[string]int ` + "`json:\"weights,omitempty\"`" + `
}
type reg struct {
	Providers map[string]prov ` + "`json:\"providers\"`" + `
	Groups    map[string]grp  ` + "`json:\"groups\"`" + `
}

func main() {
	exe, err := os.Executable()
	if err != nil {
		fatal(err.Error())
	}
	pPath := filepath.Join(filepath.Dir(exe), "data", "providers.json")
	raw, err := os.ReadFile(pPath)
	if err != nil {
		fatal(err.Error())
	}
	var r reg
	if err := json.Unmarshal(raw, &r); err != nil {
		fatal(err.Error())
	}
	if r.Providers == nil {
		r.Providers = map[string]prov{}
	}
	if r.Groups == nil {
		r.Groups = map[string]grp{}
	}
	args := os.Args[1:]
	if len(args) < 2 {
		fatal("missing args")
	}
	switch args[0] {
	case "provider":
		id := args[2]
		switch args[1] {
		case "add":
			p := prov{Headers: map[string]string{}}
			for _, kv := range args[3:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "chat":
					p.Chat = v
				case "key":
					p.Key = v
				case "models":
					p.Models = splitCSV(v)
				case "free":
					p.Free = v == "yes"
				case "list":
					p.List = v
				default:
					if strings.HasPrefix(k, "header.") {
						p.Headers[strings.TrimPrefix(k, "header.")] = v
					}
				}
			}
			if strings.HasPrefix(id, "transcribe-bailian-trial-") && p.Headers["X-Transcribe-Trial-Model"] == "" {
				fatal("missing X-Transcribe-Trial-Model header")
			}
			r.Providers[id] = p
		case "on":
			p := r.Providers[id]
			p.Off = false
			r.Providers[id] = p
		case "off":
			p := r.Providers[id]
			p.Off = true
			r.Providers[id] = p
		case "rpm":
			n, _ := strconv.Atoi(args[3])
			p := r.Providers[id]
			p.RPM = n
			r.Providers[id] = p
		case "models":
			p := r.Providers[id]
			for _, m := range args[3:] {
				if strings.HasPrefix(m, "+") {
					name := strings.TrimPrefix(m, "+")
					if !slices.Contains(p.Models, name) {
						p.Models = append(p.Models, name)
					}
				}
			}
			r.Providers[id] = p
		}
	case "group":
		name := args[2]
		g := r.Groups[name]
		for _, kv := range args[3:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "models":
				g.Models = splitCSV(v)
			case "models+":
				for _, m := range splitCSV(v) {
					if !slices.Contains(g.Models, m) {
						g.Models = append(g.Models, m)
					}
				}
			case "models-":
				rm := splitCSV(v)
				var next []string
				for _, m := range g.Models {
					if !slices.Contains(rm, m) {
						next = append(next, m)
					}
				}
				g.Models = next
			case "routing":
				g.Routing = v
			case "stays":
				g.Stays = v
			case "weight":
				g.Weights = map[string]int{}
				for _, pair := range splitCSV(v) {
					m, wStr, ok := strings.Cut(pair, "=")
					if ok {
						wVal, _ := strconv.Atoi(wStr)
						g.Weights[m] = wVal
					}
				}
			}
		}
		if len(g.Models) == 0 {
			fatal("group " + name + " cannot be emptied")
		}
		r.Groups[name] = g
	}
	out, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(pPath, out, 0o644); err != nil {
		fatal(err.Error())
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}
`
	srcFile := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(srcFile, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", outExe, srcFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile stub magpie.com failed: %v\n%s", err, out)
	}
}

func buildBinary(t *testing.T, pkg, outExe string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", outExe, pkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile %s failed: %v\n%s", pkg, err, out)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

type stubRegFile struct {
	Providers map[string]domain.Provider `json:"providers"`
	Groups    map[string]domain.Group    `json:"groups"`
}

func readProvidersJSON(t *testing.T, path string) stubRegFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return stubRegFile{}
	}
	var out stubRegFile
	_ = json.Unmarshal(raw, &out)
	return out
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, failMsg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(failMsg)
}
