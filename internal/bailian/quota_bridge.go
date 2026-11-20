package bailian

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

const (
	expectedCLIPackageName    = "bailian-cli"
	expectedCLIPackageVersion = "2.7.2"
	defaultBridgeScriptPath   = "scripts/bailian-quota-bridge.mjs"
	defaultBridgeTimeout      = 30 * time.Second
)

// BailianCredentials holds DashScope/Bailian credentials loaded from ~/.bailian/config.json.
// Secret fields are intentionally masked in String/GoString and excluded from APIKey serialization.
type BailianCredentials struct {
	AccessKeyID        string `json:"accessKeyId"`
	AccessKeySecret    string `json:"accessKeySecret"`
	SecurityToken      string `json:"securityToken,omitempty"`
	APIKey             string `json:"-"`
	BaseURL            string `json:"baseUrl,omitempty"`
	ConsoleRegion      string `json:"consoleRegion,omitempty"`
	ConsoleSite        string `json:"consoleSite,omitempty"`
	ConsoleSwitchAgent *int   `json:"consoleSwitchAgent,omitempty"`
	Timeout            int    `json:"timeout,omitempty"`
}

// String masks secrets to prevent accidental exposure in logs.
func (c BailianCredentials) String() string {
	return fmt.Sprintf("BailianCredentials{accessKeyId:%s, baseUrl:%q, region:%q, site:%q}",
		maskSecret(c.AccessKeyID), c.BaseURL, c.ConsoleRegion, c.ConsoleSite)
}

// GoString masks secrets to prevent accidental exposure via %#v.
func (c BailianCredentials) GoString() string {
	return c.String()
}

type bailianProfileRaw struct {
	AccessKeyID        string `json:"access_key_id"`
	AccessKeySecret    string `json:"access_key_secret"`
	SecurityToken      string `json:"security_token"`
	APIKey             string `json:"api_key"`
	BaseURL            string `json:"base_url"`
	ConsoleRegion      string `json:"console_region"`
	ConsoleSite        string `json:"console_site"`
	ConsoleSwitchAgent *int   `json:"console_switch_agent"`
}

// LoadBailianCredentials resolves the active profile from <configDir>/config.json.
func LoadBailianCredentials(configDir string) (BailianCredentials, error) {
	dir, err := resolveConfigDir(configDir)
	if err != nil {
		return BailianCredentials{}, err
	}
	cfgPath := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return BailianCredentials{}, fmt.Errorf("read bailian config %q: %w", cfgPath, err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return BailianCredentials{}, fmt.Errorf("parse bailian config %q: %w", cfgPath, err)
	}

	var activeConfig string
	if rawActive, ok := top["active_config"]; ok && len(bytes.TrimSpace(rawActive)) > 0 && !bytes.Equal(bytes.TrimSpace(rawActive), []byte("null")) {
		if err := json.Unmarshal(rawActive, &activeConfig); err != nil {
			return BailianCredentials{}, fmt.Errorf("parse active_config in %q: %w", cfgPath, err)
		}
		activeConfig = strings.TrimSpace(activeConfig)
	}

	var prof bailianProfileRaw
	if activeConfig != "" && activeConfig != "default" {
		profRaw, ok := top[activeConfig]
		if !ok || len(bytes.TrimSpace(profRaw)) == 0 || bytes.Equal(bytes.TrimSpace(profRaw), []byte("null")) {
			return BailianCredentials{}, fmt.Errorf("active_config profile %q not found in %q", activeConfig, cfgPath)
		}
		if err := json.Unmarshal(profRaw, &prof); err != nil {
			return BailianCredentials{}, fmt.Errorf("parse profile %q in %q: %w", activeConfig, cfgPath, err)
		}
	} else {
		if err := json.Unmarshal(data, &prof); err != nil {
			return BailianCredentials{}, fmt.Errorf("parse top-level profile in %q: %w", cfgPath, err)
		}
	}

	return BailianCredentials{
		AccessKeyID:        strings.TrimSpace(prof.AccessKeyID),
		AccessKeySecret:    strings.TrimSpace(prof.AccessKeySecret),
		SecurityToken:      strings.TrimSpace(prof.SecurityToken),
		APIKey:             strings.TrimSpace(prof.APIKey),
		BaseURL:            strings.TrimSpace(prof.BaseURL),
		ConsoleRegion:      strings.TrimSpace(prof.ConsoleRegion),
		ConsoleSite:        strings.TrimSpace(prof.ConsoleSite),
		ConsoleSwitchAgent: prof.ConsoleSwitchAgent,
	}, nil
}

func resolveConfigDir(explicit string) (string, error) {
	if dir := strings.TrimSpace(explicit); dir != "" {
		return dir, nil
	}
	if dir := strings.TrimSpace(os.Getenv("BAILIAN_CONFIG_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for .bailian config: %w", err)
	}
	return filepath.Join(home, ".bailian"), nil
}

// LocateBailianCLIPackage finds bl.cmd (falling back to bl.ps1) on PATH and
// validates the sibling node_modules/bailian-cli package at version 2.7.2.
func LocateBailianCLIPackage() (string, error) {
	return locateBailianCLIPackage(exec.LookPath)
}

func locateBailianCLIPackage(lookPath func(string) (string, error)) (string, error) {
	binPath, err := lookPath("bl.cmd")
	if err != nil {
		binPath, err = lookPath("bl.ps1")
		if err != nil {
			return "", fmt.Errorf("locate bl.cmd or bl.ps1 on PATH: %w", err)
		}
	}
	pkgDir := filepath.Join(filepath.Dir(binPath), "node_modules", expectedCLIPackageName)
	if err := validateBailianCLIPackage(pkgDir); err != nil {
		return "", err
	}
	return pkgDir, nil
}

func validateBailianCLIPackage(pkgDir string) error {
	pkgPath := filepath.Join(pkgDir, "package.json")
	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		return fmt.Errorf("read bailian-cli package.json %q: %w", pkgPath, err)
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return fmt.Errorf("parse bailian-cli package.json %q: %w", pkgPath, err)
	}
	if pkg.Name != expectedCLIPackageName || pkg.Version != expectedCLIPackageVersion {
		return fmt.Errorf("unexpected bailian-cli package at %q: got %s@%s, want %s@%s",
			pkgDir, pkg.Name, pkg.Version, expectedCLIPackageName, expectedCLIPackageVersion)
	}
	return nil
}

// BridgeClient executes scripts/bailian-quota-bridge.mjs via Node with credentials
// passed strictly over stdin.
type BridgeClient struct {
	NodeBin     string
	ScriptPath  string
	PackageDir  string
	ConfigDir   string
	Timeout     time.Duration
	LoadCredsFn func(configDir string) (BailianCredentials, error)
	LocatePkgFn func() (string, error)
}

// QueryFreeQuotas runs the Node quota bridge and returns raw quota observations.
func (c *BridgeClient) QueryFreeQuotas(ctx context.Context) ([]domain.QuotaObservation, error) {
	if c == nil {
		return nil, fmt.Errorf("bridge client is nil")
	}
	loadCreds := c.LoadCredsFn
	if loadCreds == nil {
		loadCreds = LoadBailianCredentials
	}
	creds, err := loadCreds(c.ConfigDir)
	if err != nil {
		return nil, err
	}
	if creds.AccessKeyID == "" || creds.AccessKeySecret == "" {
		return nil, fmt.Errorf("bailian credentials missing access_key_id or access_key_secret")
	}

	pkgDir := strings.TrimSpace(c.PackageDir)
	if pkgDir == "" {
		locatePkg := c.LocatePkgFn
		if locatePkg == nil {
			locatePkg = LocateBailianCLIPackage
		}
		pkgDir, err = locatePkg()
		if err != nil {
			return nil, err
		}
	} else if err := validateBailianCLIPackage(pkgDir); err != nil {
		return nil, err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultBridgeTimeout
	}
	if creds.Timeout <= 0 {
		creds.Timeout = int(math.Ceil(timeout.Seconds()))
	}

	payload, err := json.Marshal(creds)
	if err != nil {
		return nil, fmt.Errorf("marshal bridge credentials: %w", err)
	}

	nodeBin := strings.TrimSpace(c.NodeBin)
	if nodeBin == "" {
		nodeBin = "node"
	}
	scriptPath := strings.TrimSpace(c.ScriptPath)
	if scriptPath == "" {
		scriptPath = defaultBridgeScriptPath
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, nodeBin, scriptPath, pkgDir)
	hideWindow(cmd)
	cmd.Env = withShanghaiTZ(os.Environ())
	cmd.Stdin = bytes.NewReader(payload)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := redactStrings(strings.TrimSpace(stderr.String()), creds.AccessKeyID, creds.AccessKeySecret, creds.SecurityToken, creds.APIKey)
		if errMsg != "" {
			return nil, fmt.Errorf("bailian quota bridge failed: %w: %s", err, errMsg)
		}
		return nil, fmt.Errorf("bailian quota bridge failed: %w", err)
	}

	return ParseUsageFreeOutput(stdout.Bytes())
}

// ParseUsageFreeOutput parses JSON emitted by bailian-cli usageFree (--output json).
func ParseUsageFreeOutput(raw []byte) ([]domain.QuotaObservation, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("bailian quota bridge returned empty output")
	}
	type rawQuotaItem struct {
		Model                 string   `json:"model"`
		Type                  string   `json:"type"`
		Remaining             *float64 `json:"remaining"`
		Total                 *float64 `json:"total"`
		UsagePercent          *float64 `json:"usagePercent"`
		UsagePercentSnake     *float64 `json:"usage_percent"`
		RemainingPercent      *float64 `json:"remainingPercent"`
		RemainingPercentSnake *float64 `json:"remaining_percent"`
		Expires               string   `json:"expires"`
		AutoStop              any      `json:"autoStop"`
		AutoStopSnake         any      `json:"auto_stop"`
	}

	var items []rawQuotaItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("parse usageFree JSON output: %w", err)
	}

	out := make([]domain.QuotaObservation, 0, len(items))
	for _, it := range items {
		obs := domain.QuotaObservation{
			Model:            strings.TrimSpace(it.Model),
			Type:             strings.TrimSpace(it.Type),
			UsagePercent:     firstPtr(it.UsagePercent, it.UsagePercentSnake),
			RemainingPercent: firstPtr(it.RemainingPercent, it.RemainingPercentSnake),
			Expires:          strings.TrimSpace(it.Expires),
			AutoStop:         firstNonNil(it.AutoStop, it.AutoStopSnake),
		}
		if it.Remaining != nil {
			obs.Remaining = *it.Remaining
		}
		if it.Total != nil {
			obs.Total = *it.Total
		}
		out = append(out, obs)
	}
	return out, nil
}

func withShanghaiTZ(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if len(entry) >= 3 && strings.EqualFold(entry[:3], "TZ=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "TZ=Asia/Shanghai")
}

func firstPtr(a, b *float64) *float64 {
	if a != nil {
		return a
	}
	return b
}

func firstNonNil(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

func maskSecret(s string) string {
	if len(s) <= 4 {
		return "***"
	}
	return s[:2] + "***" + s[len(s)-2:]
}

func redactStrings(text string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "[REDACTED]")
		}
	}
	return text
}
