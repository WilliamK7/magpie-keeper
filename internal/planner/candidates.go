package planner

import (
	"slices"
	"sort"
	"strings"

	"magpie-keeper/internal/domain"
)

type exposePair struct {
	providerID string
	model      string
}

type supplierGroupSpec struct {
	name       string
	billing    string
	candidates []domain.Candidate
}

func resolveRoles(configured []string) []string {
	roles := dedupeStrings(configured)
	if len(roles) == 0 {
		return []string{domain.RoleTranscribe}
	}
	return roles
}

func tierRoutingForRole(role string) string {
	if role == domain.RoleReview {
		return domain.RoutingFast
	}
	return domain.RoutingWeight
}

func filterEligibleCandidates(in SyncInput) []domain.Candidate {
	excluded := toSet(in.Config.ExcludedProviders)
	out := make([]domain.Candidate, 0, len(in.Admitted))
	for _, cand := range in.Admitted {
		if cand.ProviderID == "" || cand.Model == "" {
			continue
		}
		if cand.Member == "" {
			cand.Member = cand.ProviderID + "/" + cand.Model
		}
		if excluded[cand.ProviderID] || isTrialCandidate(in.Config, cand) {
			continue
		}
		if cand.Billing != domain.BillingFree && cand.Billing != domain.BillingCommercial {
			continue
		}
		if p, ok := in.Snapshot.Providers[cand.ProviderID]; ok && !domain.IsModelAvailable(p, cand.Model) {
			continue
		}
		out = append(out, cand)
	}
	return out
}

func isTrialCandidate(cfg domain.Config, cand domain.Candidate) bool {
	tmpl := strings.TrimSpace(cfg.BailianTrial.TemplateProvider)
	if tmpl == "" {
		tmpl = "transcribe-bailian-trial"
	}
	return strings.HasPrefix(cand.ProviderID, domain.TrialProviderPrefix) ||
		strings.HasPrefix(cand.Member, domain.TrialProviderPrefix) ||
		cand.ProviderID == tmpl
}

func buildRoleSupplierSpecs(cfg domain.Config, eligible []domain.Candidate, role string) []supplierGroupSpec {
	var freeCands, paidCands []domain.Candidate
	freeSlugs := make(map[string]bool)
	paidSlugs := make(map[string]bool)

	for _, cand := range eligible {
		if !candidateAllowedForRole(cfg, cand, role) {
			continue
		}
		slug := resolveSupplierSlug(cfg, cand)
		if cand.Billing == domain.BillingFree {
			freeCands = append(freeCands, cand)
			freeSlugs[slug] = true
		} else {
			paidCands = append(paidCands, cand)
			paidSlugs[slug] = true
		}
	}

	grouped := make(map[string]*supplierGroupSpec)
	freeNames := groupCandidatesByTier(cfg, role, domain.BillingFree, freeCands, paidSlugs, nil, grouped)
	groupCandidatesByTier(cfg, role, domain.BillingCommercial, paidCands, freeSlugs, freeNames, grouped)

	out := make([]supplierGroupSpec, 0, len(grouped))
	for _, spec := range grouped {
		out = append(out, *spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func groupCandidatesByTier(
	cfg domain.Config,
	role, billing string,
	cands []domain.Candidate,
	otherSlugs map[string]bool,
	forbiddenNames map[string]bool,
	grouped map[string]*supplierGroupSpec,
) map[string]bool {
	tierSuffix := "-free"
	if billing == domain.BillingCommercial {
		tierSuffix = "-paid"
	}
	usedNames := make(map[string]bool)
	for _, cand := range cands {
		base := resolveSupplierSlug(cfg, cand)
		if otherSlugs[base] && !strings.HasSuffix(base, tierSuffix) {
			base += tierSuffix
		}
		if isReservedGroupSlug(role, base) {
			base += "-supplier"
		}
		name := role + "-" + base
		for forbiddenNames[name] {
			if !strings.HasSuffix(name, tierSuffix) {
				name += tierSuffix
			} else {
				name += "-commercial"
			}
		}
		usedNames[name] = true
		spec := grouped[name]
		if spec == nil {
			spec = &supplierGroupSpec{name: name, billing: billing}
			grouped[name] = spec
		}
		spec.candidates = append(spec.candidates, cand)
	}
	return usedNames
}

func candidateAllowedForRole(cfg domain.Config, cand domain.Candidate, role string) bool {
	if len(cand.AllowedRoles) > 0 {
		return slices.Contains(cand.AllowedRoles, role)
	}
	if rule, ok := cfg.Providers[cand.ProviderID]; ok && len(rule.Roles) > 0 {
		return slices.Contains(rule.Roles, role)
	}
	return true
}

func resolveSupplierSlug(cfg domain.Config, cand domain.Candidate) string {
	raw := strings.TrimSpace(cand.SupplierSlug)
	if raw == "" {
		if rule, ok := cfg.Providers[cand.ProviderID]; ok {
			raw = strings.TrimSpace(rule.SupplierSlug)
		}
	}
	if raw == "" {
		raw = cand.ProviderID
		for _, prefix := range []string{"transcribe-", "review-"} {
			if trimmed := strings.TrimPrefix(raw, prefix); trimmed != raw && trimmed != "" {
				raw = trimmed
				break
			}
		}
	}
	replacer := strings.NewReplacer(" ", "-", "/", "-", ",", "-", "=", "-")
	cleaned := strings.Trim(replacer.Replace(raw), "-")
	if cleaned == "" {
		return "provider"
	}
	return cleaned
}

func isReservedGroupSlug(role, slug string) bool {
	return slug == "" || slug == "free" || slug == "paid" || (role == domain.RoleTranscribe && slug == "trial")
}

func filterHeldCandidates(st domain.State, groupName string, cands []domain.Candidate) []domain.Candidate {
	out := make([]domain.Candidate, 0, len(cands))
	for _, cand := range cands {
		if !isMemberHeld(st, groupName, cand.Member) {
			out = append(out, cand)
		}
	}
	return out
}

func sortAndDedupeCandidateMembers(cfg domain.Config, cands []domain.Candidate) []string {
	if len(cands) == 0 {
		return nil
	}
	sorted := slices.Clone(cands)
	sort.Slice(sorted, func(i, j int) bool {
		ri := preferredRank(cfg, sorted[i])
		rj := preferredRank(cfg, sorted[j])
		if ri != rj {
			return ri < rj
		}
		return sorted[i].Member < sorted[j].Member
	})
	members := make([]string, 0, len(sorted))
	for _, cand := range sorted {
		members = append(members, cand.Member)
	}
	return dedupeStrings(members)
}

func preferredRank(cfg domain.Config, cand domain.Candidate) int {
	rule, ok := cfg.Providers[cand.ProviderID]
	if ok {
		for idx, pref := range rule.Preferred {
			if pref == cand.Model || pref == cand.Member {
				return idx
			}
		}
	}
	base := len(rule.Preferred)
	if cand.Preferred {
		return base
	}
	return base + 1
}

func isModelExposed(snap domain.RegistrySnapshot, providerID, model string) bool {
	p, ok := snap.Providers[providerID]
	return ok && slices.Contains(p.Models, model)
}

func buildExposeCommands(pairs map[exposePair]bool) []domain.CLICommand {
	if len(pairs) == 0 {
		return nil
	}
	list := make([]exposePair, 0, len(pairs))
	for p := range pairs {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].providerID != list[j].providerID {
			return list[i].providerID < list[j].providerID
		}
		return list[i].model < list[j].model
	})
	cmds := make([]domain.CLICommand, 0, len(list))
	for _, p := range list {
		cmds = append(cmds, domain.CLICommand{
			Args:   []string{"provider", "models", p.providerID, "+" + p.model},
			Reason: "expose admitted model",
		})
	}
	return cmds
}

func isPaidSafeMember(member string, freeMemberSet map[string]bool) bool {
	if member == "" || member == "group/transcribe-trial" || freeMemberSet[member] {
		return false
	}
	return !strings.HasPrefix(member, domain.TrialProviderPrefix) &&
		!strings.HasPrefix(member, "group/"+domain.TrialProviderPrefix) &&
		!strings.HasPrefix(member, "group/transcribe-trial")
}
