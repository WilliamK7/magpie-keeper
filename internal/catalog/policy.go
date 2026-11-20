package catalog

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"magpie-keeper/internal/domain"
)

// Default commercial price ceilings (USD per 1M tokens) per SPEC §6.1.
const (
	DefaultMaxInputUSDPerMillion  = 2.0
	DefaultMaxOutputUSDPerMillion = 10.0
)

// MatchGlobs reports whether value matches at least one wildcard pattern.
// Patterns support '*' (any zero or more runes, including '/') and '?' (any single rune).
func MatchGlobs(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if MatchGlob(pattern, value) {
			return true
		}
	}
	return false
}

// MatchGlob reports whether value matches a single wildcard pattern supporting '*' and '?'.
func MatchGlob(pattern, value string) bool {
	p := []rune(pattern)
	v := []rune(value)
	pi, vi := 0, 0
	starIdx, matchIdx := -1, 0

	for vi < len(v) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == v[vi]) {
			pi++
			vi++
		} else if pi < len(p) && p[pi] == '*' {
			starIdx = pi
			matchIdx = vi
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			vi = matchIdx
		} else {
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// FilterRule checks whether model passes rule.Include and rule.Exclude globs.
func FilterRule(model string, rule domain.ProviderRule) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	if len(rule.Include) > 0 && !MatchGlobs(rule.Include, model) {
		return false
	}
	if len(rule.Exclude) > 0 && MatchGlobs(rule.Exclude, model) {
		return false
	}
	return true
}

// HasBilling reports whether tier is enabled by rule.Billing (or adapter defaults when omitted).
func HasBilling(rule domain.ProviderRule, tier string) bool {
	if len(rule.Billing) == 0 {
		if rule.Adapter == AdapterOpenRouter || rule.Adapter == AdapterVLLMSR {
			return strings.EqualFold(tier, domain.BillingFree)
		}
		return strings.EqualFold(tier, domain.BillingFree) || strings.EqualFold(tier, domain.BillingCommercial)
	}
	for _, b := range rule.Billing {
		if strings.EqualFold(strings.TrimSpace(b), tier) {
			return true
		}
	}
	return false
}

// IsPreferred reports whether model matches any entry in rule.Preferred.
func IsPreferred(rule domain.ProviderRule, model string) bool {
	return preferredIndex(rule.Preferred, model) >= 0
}

// IsPriceException reports whether model matches any entry in rule.PriceExceptions.
func IsPriceException(rule domain.ProviderRule, model string) bool {
	return MatchGlobs(rule.PriceExceptions, model)
}

// DeriveSupplierSlug computes the supplier group slug for a provider and billing tier.
func DeriveSupplierSlug(providerID string, rule domain.ProviderRule, billing string) string {
	if slug := strings.TrimSpace(rule.SupplierSlug); slug != "" {
		return slug
	}
	base := strings.TrimPrefix(strings.TrimSpace(providerID), "transcribe-")
	if base == "" {
		base = strings.TrimSpace(providerID)
	}
	mixed := HasBilling(rule, domain.BillingFree) && HasBilling(rule, domain.BillingCommercial)
	switch billing {
	case domain.BillingFree:
		if strings.HasSuffix(base, "-free") {
			return base
		}
		return strings.TrimSuffix(base, "-paid") + "-free"
	case domain.BillingCommercial:
		if mixed {
			if strings.HasSuffix(base, "-paid") {
				return base
			}
			return strings.TrimSuffix(base, "-free") + "-paid"
		}
		return base
	default:
		return base
	}
}

// EvaluatePricePolicy classifies a model's billing tier and checks price ceilings/exceptions.
func EvaluatePricePolicy(inUSD, outUSD float64, rule domain.ProviderRule, model string) (billing string, allowed bool, priceException bool) {
	if inUSD == 0 && outUSD == 0 {
		return domain.BillingFree, HasBilling(rule, domain.BillingFree), false
	}
	return evaluateCommercialPricePolicy(inUSD, outUSD, rule, model)
}

func evaluateCommercialPricePolicy(inUSD, outUSD float64, rule domain.ProviderRule, model string) (billing string, allowed bool, priceException bool) {
	pe := IsPriceException(rule, model)
	if !HasBilling(rule, domain.BillingCommercial) {
		return domain.BillingCommercial, false, pe
	}
	if pe {
		return domain.BillingCommercial, true, true
	}
	if inUSD < 0 || outUSD < 0 || math.IsNaN(inUSD) || math.IsNaN(outUSD) || math.IsInf(inUSD, 0) || math.IsInf(outUSD, 0) {
		return domain.BillingCommercial, false, false
	}
	return domain.BillingCommercial, inUSD <= maxInputCeiling(rule) && outUSD <= maxOutputCeiling(rule), false
}

// RankAndCap sorts candidates (preferred order, then free before commercial, then model ID)
// and enforces per-tier rule.MaxCandidates caps when positive.
func RankAndCap(candidates []domain.Candidate, rule domain.ProviderRule) []domain.Candidate {
	if len(candidates) == 0 {
		return nil
	}
	ranked := slices.Clone(candidates)
	slices.SortStableFunc(ranked, func(a, b domain.Candidate) int {
		if c := comparePreferred(rule.Preferred, a, b); c != 0 {
			return c
		}
		if c := cmp.Compare(billingRank(a.Billing), billingRank(b.Billing)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Model, b.Model); c != 0 {
			return c
		}
		return cmp.Compare(a.Member, b.Member)
	})
	if len(rule.MaxCandidates) == 0 {
		return ranked
	}
	counts := make(map[string]int, len(rule.MaxCandidates))
	out := make([]domain.Candidate, 0, len(ranked))
	for _, c := range ranked {
		if limit, ok := rule.MaxCandidates[c.Billing]; ok && limit > 0 && counts[c.Billing] >= limit {
			continue
		}
		counts[c.Billing]++
		out = append(out, c)
	}
	return out
}

func comparePreferred(patterns []string, a, b domain.Candidate) int {
	pa := preferredIndex(patterns, a.Model)
	pb := preferredIndex(patterns, b.Model)
	if pa < 0 {
		pa = math.MaxInt
	}
	if pb < 0 {
		pb = math.MaxInt
	}
	if pa != pb {
		return cmp.Compare(pa, pb)
	}
	if a.Preferred != b.Preferred {
		if a.Preferred {
			return -1
		}
		return 1
	}
	return 0
}

func preferredIndex(patterns []string, model string) int {
	for i, p := range patterns {
		if MatchGlob(p, model) {
			return i
		}
	}
	return -1
}

func billingRank(billing string) int {
	switch billing {
	case domain.BillingFree:
		return 0
	case domain.BillingCommercial:
		return 1
	default:
		return 2
	}
}

func maxInputCeiling(rule domain.ProviderRule) float64 {
	if rule.MaxInputUSDPerMillion > 0 {
		return rule.MaxInputUSDPerMillion
	}
	return DefaultMaxInputUSDPerMillion
}

func maxOutputCeiling(rule domain.ProviderRule) float64 {
	if rule.MaxOutputUSDPerMillion > 0 {
		return rule.MaxOutputUSDPerMillion
	}
	return DefaultMaxOutputUSDPerMillion
}
