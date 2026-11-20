package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"magpie-keeper/internal/domain"
)

const defaultVercelEndpoint = "https://ai-gateway.vercel.sh/v1/models"

// VercelAdapter discovers models from Vercel AI Gateway and intersects them
// with locally available models in Magpie.
type VercelAdapter struct {
	Endpoint   string
	HTTPClient *http.Client
}

// NewVercelAdapter creates a VercelAdapter with the default Vercel AI Gateway endpoint.
func NewVercelAdapter(httpClient *http.Client) *VercelAdapter {
	return &VercelAdapter{
		Endpoint:   defaultVercelEndpoint,
		HTTPClient: ensureHTTPClient(httpClient),
	}
}

// Name returns the adapter identifier "vercel".
func (a *VercelAdapter) Name() string {
	return AdapterVercel
}

type vercelModelsResponse struct {
	Data []vercelModel `json:"data"`
}

type vercelModel struct {
	ID      string        `json:"id"`
	Type    string        `json:"type"`
	Pricing vercelPricing `json:"pricing"`
}

type vercelPricing struct {
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	Prompt     json.RawMessage `json:"prompt"`
	Completion json.RawMessage `json:"completion"`
}

// Discover fetches Vercel AI Gateway's model catalog, intersects with locally
// available models for provider, converts per-token USD prices to per-million
// USD, enforces ceilings/price_exceptions, and ranks results.
func (a *VercelAdapter) Discover(ctx context.Context, provider domain.Provider, rule domain.ProviderRule, served map[string]bool) ([]domain.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.IsProviderAvailable(provider) || strings.TrimSpace(provider.ID) == "" {
		return nil, nil
	}

	local := localAvailableModels(provider, served)
	if len(local) == 0 {
		return nil, nil
	}

	endpoint := resolveEndpoint(rule.EndpointURL, a.Endpoint, defaultVercelEndpoint)
	var payload vercelModelsResponse
	if err := fetchJSON(ctx, a.HTTPClient, endpoint, &payload); err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(payload.Data))
	candidates := make([]domain.Candidate, 0, len(local))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] || !local[id] {
			continue
		}
		seen[id] = true

		if item.Type != "" && !strings.EqualFold(strings.TrimSpace(item.Type), "language") {
			continue
		}
		if !domain.IsModelAvailable(provider, id) || !FilterRule(id, rule) {
			continue
		}

		billing, allowed, pe, inUSD, outUSD := classifyVercelModel(id, item.Pricing, rule)
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

func localAvailableModels(provider domain.Provider, served map[string]bool) map[string]bool {
	local := make(map[string]bool, len(provider.Models))
	for _, raw := range provider.Models {
		if id := strings.TrimSpace(raw); id != "" {
			local[id] = true
		}
	}
	prefix := provider.ID + "/"
	for member, isServed := range served {
		if !isServed || !strings.HasPrefix(member, prefix) {
			continue
		}
		if id := strings.TrimSpace(strings.TrimPrefix(member, prefix)); id != "" {
			local[id] = true
		}
	}
	return local
}

func classifyVercelModel(id string, pricing vercelPricing, rule domain.ProviderRule) (billing string, allowed bool, priceException bool, inUSD float64, outUSD float64) {
	rawIn, hasIn, errIn := parseVercelPriceField(pricing.Input, pricing.Prompt)
	rawOut, hasOut, errOut := parseVercelPriceField(pricing.Output, pricing.Completion)
	if errIn != nil || errOut != nil || !hasIn || !hasOut {
		if HasBilling(rule, domain.BillingCommercial) && IsPriceException(rule, id) {
			return domain.BillingCommercial, true, true, 0, 0
		}
		return domain.BillingCommercial, false, IsPriceException(rule, id), 0, 0
	}

	inUSD = normalizeVercelUSDPerMillion(rawIn)
	outUSD = normalizeVercelUSDPerMillion(rawOut)
	billing, allowed, priceException = EvaluatePricePolicy(inUSD, outUSD, rule, id)
	if inUSD < 0 {
		inUSD = 0
	}
	if outUSD < 0 {
		outUSD = 0
	}
	return billing, allowed, priceException, inUSD, outUSD
}

func parseVercelPriceField(primary, fallback json.RawMessage) (float64, bool, error) {
	val, ok, err := parseFlexiblePrice(primary)
	if err != nil || ok {
		return val, ok, err
	}
	return parseFlexiblePrice(fallback)
}

func normalizeVercelUSDPerMillion(v float64) float64 {
	if v > 0 && v < perTokenCeilingMarker {
		return roundUSD(v * 1_000_000)
	}
	return roundUSD(v)
}
