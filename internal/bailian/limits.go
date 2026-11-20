package bailian

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

const (
	defaultLimitsTimeout  = 15 * time.Second
	maxLimitsResponseBody = 2 << 20
	tpmPerRPMDivisor      = 36864
	trialPoolShareDivisor = 4
)

// LimitsClient queries Bailian workspace and model RPM/TPM limits.
type LimitsClient struct {
	WorkspaceID string
	APIKey      string
	BaseURL     string
	HTTPClient  *http.Client
}

// QueryModelLimits fetches /api/v1/models/limits for model and normalizes limits to per-minute units.
func (c *LimitsClient) QueryModelLimits(ctx context.Context, model string) (domain.RateLimits, error) {
	if c == nil {
		return domain.RateLimits{}, fmt.Errorf("limits client is nil")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return domain.RateLimits{}, fmt.Errorf("model is required")
	}
	apiKey := strings.TrimSpace(c.APIKey)
	if apiKey == "" {
		return domain.RateLimits{}, fmt.Errorf("bailian api key is required for model limits query")
	}

	baseURL, err := resolveLimitsBaseURL(c.BaseURL, c.WorkspaceID)
	if err != nil {
		return domain.RateLimits{}, err
	}

	query := url.Values{}
	query.Set("model", model)
	query.Set("page_size", "100")
	reqURL := strings.TrimRight(baseURL, "/") + "/api/v1/models/limits?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return domain.RateLimits{}, fmt.Errorf("build limits request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultLimitsTimeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return domain.RateLimits{}, fmt.Errorf("query bailian model limits: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLimitsResponseBody))
	if err != nil {
		return domain.RateLimits{}, fmt.Errorf("read limits response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := redactStrings(strings.TrimSpace(string(body)), apiKey)
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return domain.RateLimits{}, fmt.Errorf("bailian model limits returned HTTP %d: %s", resp.StatusCode, snippet)
	}

	return parseLimitsResponse(model, body)
}

func resolveLimitsBaseURL(explicitBase, workspaceID string) (string, error) {
	if trimmed := strings.TrimSpace(explicitBase); trimmed != "" {
		u, err := url.Parse(trimmed)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("invalid limits base URL %q", explicitBase)
		}
		return strings.TrimRight(trimmed, "/"), nil
	}
	ws := strings.TrimSpace(workspaceID)
	if !isValidWorkspaceID(ws) {
		return "", fmt.Errorf("invalid bailian workspace_id %q", workspaceID)
	}
	return fmt.Sprintf("https://%s.cn-beijing.maas.aliyuncs.com", ws), nil
}

func isValidWorkspaceID(ws string) bool {
	if ws == "" {
		return false
	}
	for _, r := range ws {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlphaNum && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

type limitPeriod struct {
	RequestLimit       float64 `json:"request_limit"`
	RequestLimitPeriod float64 `json:"request_limit_period"`
	UsageLimit         float64 `json:"usage_limit"`
	UsageLimitPeriod   float64 `json:"usage_limit_period"`
}

type limitQuotaItem struct {
	Model          string       `json:"model"`
	ModelLimit     *limitPeriod `json:"model_limit"`
	WorkspaceLimit *limitPeriod `json:"workspace_limit"`
	AccountRPM     int          `json:"account_rpm"`
	AccountTPM     int          `json:"account_tpm"`
	WorkspaceRPM   int          `json:"workspace_rpm"`
	WorkspaceTPM   int          `json:"workspace_tpm"`
}

type limitsAPIResponse struct {
	AccountRPM   int `json:"account_rpm"`
	AccountTPM   int `json:"account_tpm"`
	WorkspaceRPM int `json:"workspace_rpm"`
	WorkspaceTPM int `json:"workspace_tpm"`
	Output       struct {
		Quotas       []limitQuotaItem `json:"quotas"`
		AccountRPM   int              `json:"account_rpm"`
		AccountTPM   int              `json:"account_tpm"`
		WorkspaceRPM int              `json:"workspace_rpm"`
		WorkspaceTPM int              `json:"workspace_tpm"`
	} `json:"output"`
}

func parseLimitsResponse(model string, body []byte) (domain.RateLimits, error) {
	var raw limitsAPIResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return domain.RateLimits{}, fmt.Errorf("parse bailian limits response: %w", err)
	}

	out := domain.RateLimits{Model: model}
	if entry, ok := findMatchingQuota(raw.Output.Quotas, model); ok {
		if entry.ModelLimit != nil {
			out.AccountRPM = normalizeLimitPerMinute(entry.ModelLimit.RequestLimit, entry.ModelLimit.RequestLimitPeriod)
			out.AccountTPM = normalizeLimitPerMinute(entry.ModelLimit.UsageLimit, entry.ModelLimit.UsageLimitPeriod)
		}
		if entry.WorkspaceLimit != nil {
			out.WorkspaceRPM = normalizeLimitPerMinute(entry.WorkspaceLimit.RequestLimit, entry.WorkspaceLimit.RequestLimitPeriod)
			out.WorkspaceTPM = normalizeLimitPerMinute(entry.WorkspaceLimit.UsageLimit, entry.WorkspaceLimit.UsageLimitPeriod)
		}
		out.AccountRPM = firstPositive(out.AccountRPM, entry.AccountRPM)
		out.AccountTPM = firstPositive(out.AccountTPM, entry.AccountTPM)
		out.WorkspaceRPM = firstPositive(out.WorkspaceRPM, entry.WorkspaceRPM)
		out.WorkspaceTPM = firstPositive(out.WorkspaceTPM, entry.WorkspaceTPM)
	}

	out.AccountRPM = firstPositive(out.AccountRPM, raw.Output.AccountRPM, raw.AccountRPM)
	out.AccountTPM = firstPositive(out.AccountTPM, raw.Output.AccountTPM, raw.AccountTPM)
	out.WorkspaceRPM = firstPositive(out.WorkspaceRPM, raw.Output.WorkspaceRPM, raw.WorkspaceRPM)
	out.WorkspaceTPM = firstPositive(out.WorkspaceTPM, raw.Output.WorkspaceTPM, raw.WorkspaceTPM)
	return out, nil
}

func findMatchingQuota(quotas []limitQuotaItem, model string) (limitQuotaItem, bool) {
	for _, q := range quotas {
		if q.Model == model {
			return q, true
		}
	}
	for _, q := range quotas {
		if strings.EqualFold(strings.TrimSpace(q.Model), model) {
			return q, true
		}
	}
	if len(quotas) == 1 && strings.TrimSpace(quotas[0].Model) == "" {
		return quotas[0], true
	}
	return limitQuotaItem{}, false
}

func normalizeLimitPerMinute(limit, period float64) int {
	if limit <= 0 {
		return 0
	}
	if period <= 0 {
		period = 60
	}
	return int(limit * 60 / period)
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// ComputeSharedRPM computes the per-channel RPM cap by taking the positive minimum
// of workspace (fallback account) RPM and TPM/36864, then dividing by 4 (1 share
// for commercial, up to 3 shares for concurrent trial channels).
func ComputeSharedRPM(limits domain.RateLimits) int {
	rpm := firstPositive(limits.WorkspaceRPM, limits.AccountRPM)
	tpm := firstPositive(limits.WorkspaceTPM, limits.AccountTPM)
	tpmRPM := 0
	if tpm > 0 {
		tpmRPM = tpm / tpmPerRPMDivisor
	}

	bottleneck := positiveMin(rpm, tpmRPM)
	if bottleneck <= 0 {
		return 0
	}
	shared := bottleneck / trialPoolShareDivisor
	if shared < 1 {
		return 1
	}
	return shared
}

func positiveMin(a, b int) int {
	switch {
	case a > 0 && b > 0:
		if a < b {
			return a
		}
		return b
	case a > 0:
		return a
	case b > 0:
		return b
	default:
		return 0
	}
}
