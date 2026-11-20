package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"magpie-keeper/internal/domain"
)

const defaultOpenRouterEndpoint = "https://openrouter.ai/api/v1/models"

// OpenRouterAdapter discovers models from OpenRouter's /api/v1/models catalog.
type OpenRouterAdapter struct {
	Endpoint   string
	HTTPClient *http.Client
}

// NewOpenRouterAdapter creates an OpenRouterAdapter with the default endpoint.
func NewOpenRouterAdapter(httpClient *http.Client) *OpenRouterAdapter {
	return &OpenRouterAdapter{
		Endpoint:   defaultOpenRouterEndpoint,
		HTTPClient: ensureHTTPClient(httpClient),
	}
}

// Name returns the adapter identifier "openrouter".
func (a *OpenRouterAdapter) Name() string {
	return AdapterOpenRouter
}

type openRouterModelsResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID           string                 `json:"id"`
	Pricing      openRouterPricing      `json:"pricing"`
	Architecture openRouterArchitecture `json:"architecture"`
}

type openRouterPricing struct {
	Prompt     json.RawMessage `json:"prompt"`
	Completion json.RawMessage `json:"completion"`
}

type openRouterArchitecture struct {
	OutputModalities []string `json:"output_modalities"`
}

// Discover fetches OpenRouter's catalog, classifies `:free` zero-priced models
// as free and non-free models against commercial price ceilings, and ranks results.
func (a *OpenRouterAdapter) Discover(ctx context.Context, provider domain.Provider, rule domain.ProviderRule, served map[string]bool) ([]domain.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.IsProviderAvailable(provider) || strings.TrimSpace(provider.ID) == "" {
		return nil, nil
	}

	endpoint := resolveEndpoint(rule.EndpointURL, a.Endpoint, defaultOpenRouterEndpoint)
	var payload openRouterModelsResponse
	if err := fetchJSON(ctx, a.HTTPClient, endpoint, &payload); err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(payload.Data))
	candidates := make([]domain.Candidate, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		if len(item.Architecture.OutputModalities) > 0 && !containsFold(item.Architecture.OutputModalities, "text") {
			continue
		}
		if !domain.IsModelAvailable(provider, id) || !FilterRule(id, rule) {
			continue
		}

		billing, allowed, pe, inUSD, outUSD := classifyOpenRouterModel(id, item.Pricing, rule)
		if !allowed {
			continue
		}
		candidates = append(candidates, newCandidate(
			provider,
			rule,
			id,
			billing,
			served,
			inUSD,
			outUSD,
			pe,
			rule.Roles,
		))
	}
	return RankAndCap(candidates, rule), nil
}

func classifyOpenRouterModel(id string, pricing openRouterPricing, rule domain.ProviderRule) (billing string, allowed bool, priceException bool, inUSD float64, outUSD float64) {
	promptPerTok, hasPrompt, errP := parseFlexiblePrice(pricing.Prompt)
	compPerTok, hasComp, errC := parseFlexiblePrice(pricing.Completion)

	// Per SPEC §6.1: only `:free` models with explicit 0 prompt and completion prices qualify as free.
	if strings.HasSuffix(id, ":free") && errP == nil && errC == nil && hasPrompt && hasComp && promptPerTok == 0 && compPerTok == 0 {
		return domain.BillingFree, HasBilling(rule, domain.BillingFree), false, 0, 0
	}

	if !HasBilling(rule, domain.BillingCommercial) {
		return domain.BillingCommercial, false, IsPriceException(rule, id), 0, 0
	}

	if errP != nil || errC != nil || !hasPrompt || !hasComp {
		pe := IsPriceException(rule, id)
		return domain.BillingCommercial, pe, pe, 0, 0
	}

	inUSD = roundUSD(promptPerTok * 1_000_000)
	outUSD = roundUSD(compPerTok * 1_000_000)
	billing, allowed, priceException = evaluateCommercialPricePolicy(inUSD, outUSD, rule, id)
	if inUSD < 0 {
		inUSD = 0
	}
	if outUSD < 0 {
		outUSD = 0
	}
	return billing, allowed, priceException, inUSD, outUSD
}
