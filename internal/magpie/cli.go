package magpie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

const (
	defaultCLITimeout = 5 * time.Second
	refreshCLITimeout = 30 * time.Second
	maxModelsBodySize = 16 << 20
)

// ErrServedModelsTruncated indicates that /v1/models returned has_more=true.
var ErrServedModelsTruncated = errors.New("magpie /v1/models response truncated (has_more=true)")

var _ domain.MagpieGateway = (*Gateway)(nil)

var (
	keyParamRe    = regexp.MustCompile(`(?i)(\b(?:api_?key|key)=)(?:"[^"]*"|'[^']*'|\S+)`)
	bearerTokenRe = regexp.MustCompile(`(?i)(bearer\s+)\S+`)
)

// Gateway implements domain.MagpieGateway against a portable Magpie directory and HTTP server.
type Gateway struct {
	magpieDir      string
	magpieURL      string
	magpieKey      string
	backupDir      string
	httpClient     *http.Client
	defaultTimeout time.Duration
	refreshTimeout time.Duration
}

// NewGateway creates a Gateway targeting magpieDir and magpieURL.
func NewGateway(magpieDir, magpieURL, magpieKey, backupDir string, httpClient *http.Client) *Gateway {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Gateway{
		magpieDir:      magpieDir,
		magpieURL:      magpieURL,
		magpieKey:      magpieKey,
		backupDir:      backupDir,
		httpClient:     httpClient,
		defaultTimeout: defaultCLITimeout,
		refreshTimeout: refreshCLITimeout,
	}
}

// BackupRegistry copies <magpie_dir>/data/providers.json to a timestamped backup directory,
// syncing and closing the destination file before returning its path.
func (g *Gateway) BackupRegistry(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(g.magpieDir) == "" {
		return "", errors.New("magpie_dir is empty")
	}

	srcPath := filepath.Join(g.magpieDir, "data", "providers.json")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read providers.json for backup: %w", err)
	}

	root := strings.TrimSpace(g.backupDir)
	if root == "" {
		root = filepath.Join(g.magpieDir, "data", "backups")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create backup root %q: %w", root, err)
	}

	dstDir, err := allocateBackupDir(root, time.Now())
	if err != nil {
		return "", err
	}

	dstPath := filepath.Join(dstDir, "providers.json")
	f, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create backup file %q: %w", dstPath, err)
	}

	if err := writeAllAndSync(f, data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write backup file %q: %w", dstPath, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close backup file %q: %w", dstPath, err)
	}
	return dstPath, nil
}

// ExecCLI executes <magpie_dir>/magpie.com with a hidden console window and strict timeout,
// redacting any secret values (such as key=...) from returned errors.
func (g *Gateway) ExecCLI(ctx context.Context, args ...string) error {
	if strings.TrimSpace(g.magpieDir) == "" {
		return errors.New("magpie_dir is empty")
	}
	if len(args) == 0 {
		return errors.New("magpie CLI requires at least one argument")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	timeout := g.cliTimeout(args)
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := filepath.Join(g.magpieDir, "magpie.com")
	cmd := exec.CommandContext(cmdCtx, bin, args...)
	hideWindow(cmd)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err == nil {
		return nil
	}

	secrets := collectSecrets(args, g.magpieKey)
	safeArgs := strings.Join(redactArgs(args), " ")
	safeOut := redactSecrets(strings.TrimSpace(out.String()), secrets)
	safeErr := redactSecrets(err.Error(), secrets)

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		if safeOut != "" {
			return fmt.Errorf("magpie.com [%s] timed out after %s: %s (output: %s)", safeArgs, timeout, safeErr, safeOut)
		}
		return fmt.Errorf("magpie.com [%s] timed out after %s: %s", safeArgs, timeout, safeErr)
	}
	if safeOut != "" {
		return fmt.Errorf("magpie.com [%s] failed: %s (output: %s)", safeArgs, safeErr, safeOut)
	}
	return fmt.Errorf("magpie.com [%s] failed: %s", safeArgs, safeErr)
}

// ListServedModels queries GET <magpie_url>/models and returns all exposed model IDs.
// It returns ErrServedModelsTruncated if the response reports has_more=true.
func (g *Gateway) ListServedModels(ctx context.Context) ([]string, error) {
	endpoint, err := modelsEndpointURL(g.magpieURL)
	if err != nil {
		return nil, fmt.Errorf("build models endpoint URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(g.magpieKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %s", endpoint, redactSecrets(err.Error(), []string{g.magpieKey}))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodySize))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := redactSecrets(strings.TrimSpace(string(body)), []string{g.magpieKey})
		if len(snippet) > 512 {
			snippet = snippet[:512] + "..."
		}
		return nil, fmt.Errorf("GET %s returned HTTP %d: %s", endpoint, resp.StatusCode, snippet)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stripBOM(body)), &payload); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	if payload.HasMore {
		return nil, ErrServedModelsTruncated
	}

	ids := make([]string, 0, len(payload.Data))
	seen := make(map[string]bool, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" && !seen[item.ID] {
			seen[item.ID] = true
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

func (g *Gateway) cliTimeout(args []string) time.Duration {
	if len(args) >= 2 && args[0] == "provider" && args[1] == "refresh" {
		if g.refreshTimeout > 0 {
			return g.refreshTimeout
		}
		return refreshCLITimeout
	}
	if g.defaultTimeout > 0 {
		return g.defaultTimeout
	}
	return defaultCLITimeout
}

func modelsEndpointURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("magpie_url must include scheme and host")
	}
	p := strings.TrimRight(u.Path, "/")
	switch {
	case p == "":
		u.Path = "/v1/models"
	case strings.HasSuffix(p, "/models"):
		u.Path = p
	default:
		u.Path = p + "/models"
	}
	return u.String(), nil
}

func allocateBackupDir(root string, now time.Time) (string, error) {
	for i := 0; i < 1000; i++ {
		dir := filepath.Join(root, now.Add(time.Duration(i)*time.Millisecond).Format("20060102-150405.000"))
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create backup directory %q: %w", dir, err)
		}
	}
	return "", fmt.Errorf("failed to allocate unique backup directory under %q", root)
}

func writeAllAndSync(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return f.Sync()
}

func splitSecretArg(arg string) (prefixEq, val string, ok bool) {
	idx := strings.IndexByte(arg, '=')
	if idx <= 0 {
		return "", "", false
	}
	name := strings.ToLower(strings.TrimLeft(arg[:idx], "-"))
	if name != "key" && name != "api_key" && name != "apikey" {
		return "", "", false
	}
	return arg[:idx+1], strings.Trim(strings.TrimSpace(arg[idx+1:]), `"'`), true
}

func collectSecrets(args []string, extra ...string) []string {
	var secrets []string
	for _, arg := range args {
		if _, val, ok := splitSecretArg(arg); ok && val != "" {
			secrets = append(secrets, val)
		}
	}
	for _, s := range extra {
		if s = strings.TrimSpace(s); s != "" {
			secrets = append(secrets, s)
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	return secrets
}

func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		if prefixEq, _, ok := splitSecretArg(arg); ok {
			out[i] = prefixEq + "[REDACTED]"
		} else {
			out[i] = arg
		}
	}
	return out
}

func redactSecrets(text string, secrets []string) string {
	if text == "" {
		return ""
	}
	out := keyParamRe.ReplaceAllString(text, "${1}[REDACTED]")
	out = bearerTokenRe.ReplaceAllString(out, "${1}[REDACTED]")
	for _, s := range secrets {
		if len(s) >= 4 {
			out = strings.ReplaceAll(out, s, "[REDACTED]")
		}
	}
	return out
}
