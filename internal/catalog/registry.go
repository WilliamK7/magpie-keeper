package catalog

import (
	"context"
	"sort"
	"strings"

	"magpie-keeper/internal/domain"
)

// RegistryAdapter discovers models from Magpie's merged local/plugin registry
// and currently served gateway endpoints.
type RegistryAdapter struct{}

// NewRegistryAdapter constructs a RegistryAdapter.
func NewRegistryAdapter() *RegistryAdapter {
	return &RegistryAdapter{}
}

// Name returns the adapter identifier "registry".
func (a *RegistryAdapter) Name() string {
	return AdapterRegistry
}

// Discover gathers locally configured and served models for provider, classifies
// billing tiers, enforces price ceilings when metadata is present, and ranks results.
func (a *RegistryAdapter) Discover(ctx context.Context, provider domain.Provider, rule domain.ProviderRule, served map[string]bool) ([]domain.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.IsProviderAvailable(provider) || strings.TrimSpace(provider.ID) == "" {
		return nil, nil
	}

	models := collectRegistryModels(provider, served)
	if len(models) == 0 {
		return nil, nil
	}

	candidates := make([]domain.Candidate, 0, len(models))
	for _, model := range models {
		if !domain.IsModelAvailable(provider, model) || !FilterRule(model, rule) {
			continue
		}
		billing, allowed, priceException, inUSD, outUSD := classifyRegistryModel(provider, rule, model)
		if !allowed {
			continue
		}
		candidates = append(candidates, newCandidate(
			provider,
			rule,
			model,
			billing,
			served,
			inUSD,
			outUSD,
			priceException,
			rule.Roles,
		))
	}
	return RankAndCap(candidates, rule), nil
}

func collectRegistryModels(provider domain.Provider, served map[string]bool) []string {
	seen := make(map[string]bool, len(provider.Models))
	models := make([]string, 0, len(provider.Models))

	for _, raw := range provider.Models {
		model := strings.TrimSpace(raw)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}

	prefix := provider.ID + "/"
	var servedModels []string
	for member, isServed := range served {
		if !isServed || !strings.HasPrefix(member, prefix) {
			continue
		}
		model := strings.TrimSpace(strings.TrimPrefix(member, prefix))
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		servedModels = append(servedModels, model)
	}
	sort.Strings(servedModels)
	return append(models, servedModels...)
}

func classifyRegistryModel(provider domain.Provider, rule domain.ProviderRule, model string) (billing string, allowed bool, priceException bool, inUSD float64, outUSD float64) {
	meta, hasMeta := provider.ModelMeta[model]
	if hasMeta {
		if meta.InputPrice != nil {
			inUSD = *meta.InputPrice
		}
		if meta.OutputPrice != nil {
			outUSD = *meta.OutputPrice
		}
	}

	if provider.Free || (hasMeta && meta.Free) || strings.HasSuffix(model, ":free") {
		return domain.BillingFree, HasBilling(rule, domain.BillingFree), false, inUSD, outUSD
	}

	if hasMeta && (meta.InputPrice != nil || meta.OutputPrice != nil) {
		billing, allowed, priceException = EvaluatePricePolicy(inUSD, outUSD, rule, model)
		return billing, allowed, priceException, inUSD, outUSD
	}

	return domain.BillingCommercial, HasBilling(rule, domain.BillingCommercial), IsPriceException(rule, model), 0, 0
}
