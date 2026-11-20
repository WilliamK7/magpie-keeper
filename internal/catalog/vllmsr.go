package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"magpie-keeper/internal/domain"
)

const (
	defaultVLLMSREndpoint = "https://app.vllm-sr.ai/pt/api/info"
	vllmSRAutoModel       = "vllm-sr/auto"
)

// VLLMSRAdapter discovers the dynamic vLLM-SR router model when active backend
// routes are reported by the info endpoint.
type VLLMSRAdapter struct {
	Endpoint   string
	HTTPClient *http.Client
}

// NewVLLMSRAdapter creates a VLLMSRAdapter with the default vLLM-SR info endpoint.
func NewVLLMSRAdapter(httpClient *http.Client) *VLLMSRAdapter {
	return &VLLMSRAdapter{
		Endpoint:   defaultVLLMSREndpoint,
		HTTPClient: ensureHTTPClient(httpClient),
	}
}

// Name returns the adapter identifier "vllm-sr".
func (a *VLLMSRAdapter) Name() string {
	return AdapterVLLMSR
}

type vllmSRInfoResponse struct {
	RoutesTo []json.RawMessage `json:"routes_to"`
	Data     *struct {
		RoutesTo []json.RawMessage `json:"routes_to"`
	} `json:"data"`
}

// Discover checks vLLM-SR's /api/info endpoint for non-empty routes_to and emits
// `vllm-sr/auto` as a free candidate restricted strictly to the transcribe role.
func (a *VLLMSRAdapter) Discover(ctx context.Context, provider domain.Provider, rule domain.ProviderRule, served map[string]bool) ([]domain.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.IsProviderAvailable(provider) || strings.TrimSpace(provider.ID) == "" {
		return nil, nil
	}
	if !HasBilling(rule, domain.BillingFree) {
		return nil, nil
	}
	if len(rule.Roles) > 0 && !containsFold(rule.Roles, domain.RoleTranscribe) {
		return nil, nil
	}
	if !domain.IsModelAvailable(provider, vllmSRAutoModel) || !FilterRule(vllmSRAutoModel, rule) {
		return nil, nil
	}

	endpoint := resolveEndpoint(rule.EndpointURL, a.Endpoint, defaultVLLMSREndpoint)
	var payload vllmSRInfoResponse
	if err := fetchJSON(ctx, a.HTTPClient, endpoint, &payload); err != nil {
		return nil, err
	}

	routes := extractRoutesTo(payload)
	if len(routes) == 0 {
		return nil, nil
	}

	candidate := newCandidate(
		provider,
		rule,
		vllmSRAutoModel,
		domain.BillingFree,
		served,
		0,
		0,
		false,
		[]string{domain.RoleTranscribe},
	)
	return RankAndCap([]domain.Candidate{candidate}, rule), nil
}

func extractRoutesTo(payload vllmSRInfoResponse) []string {
	rawItems := payload.RoutesTo
	if len(rawItems) == 0 && payload.Data != nil {
		rawItems = payload.Data.RoutesTo
	}
	if len(rawItems) == 0 {
		return nil
	}

	routes := make([]string, 0, len(rawItems))
	for _, raw := range rawItems {
		if route := parseRouteEntry(raw); route != "" {
			routes = append(routes, route)
		}
	}
	return routes
}

func parseRouteEntry(raw json.RawMessage) string {
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strings.TrimSpace(str)
	}
	var obj struct {
		Model string `json:"model"`
		ID    string `json:"id"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, candidate := range []string{obj.Model, obj.ID, obj.Name} {
			if trimmed := strings.TrimSpace(candidate); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
