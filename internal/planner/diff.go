package planner

import (
	"slices"
	"strconv"
	"strings"

	"magpie-keeper/internal/domain"
)

type modelStep struct {
	add     bool
	members []string
}

func makeDesiredGroup(name string, models []string, routing string) domain.Group {
	cleanModels := dedupeStrings(models)
	if routing == "" {
		routing = domain.RoutingOrder
	}
	g := domain.Group{
		Name:    name,
		Models:  cleanModels,
		Routing: routing,
		Stays:   "off",
	}
	if routing == domain.RoutingWeight && len(cleanModels) > 0 {
		weights := make(map[string]int, len(cleanModels))
		for _, m := range cleanModels {
			weights[m] = 1
		}
		g.Weights = weights
	}
	return g
}

func cloneGroup(g domain.Group) domain.Group {
	out := g
	if g.Models != nil {
		out.Models = slices.Clone(g.Models)
	}
	if g.Weights != nil {
		out.Weights = make(map[string]int, len(g.Weights))
		for k, v := range g.Weights {
			out.Weights[k] = v
		}
	}
	return out
}

func diffAgainstSnapshot(snap domain.RegistrySnapshot, groupName string, desired domain.Group, reason string) []domain.CLICommand {
	if snapGrp, exists := snap.Groups[groupName]; exists {
		return planGroupDiff(&snapGrp, desired, reason)
	}
	return planGroupDiff(nil, desired, reason)
}

// planGroupDiff computes idempotent, non-empty-safe magpie.com CLI commands to
// transition existing (nil if absent) to desired.
func planGroupDiff(existing *domain.Group, desired domain.Group, reason string) []domain.CLICommand {
	if len(desired.Models) == 0 {
		return nil
	}
	desired = makeDesiredGroup(desired.Name, desired.Models, desired.Routing)

	if existing == nil {
		cmds := []domain.CLICommand{{
			Args: []string{
				"group", "add", desired.Name,
				"models=" + strings.Join(desired.Models, ","),
				"routing=" + desired.Routing,
				"stays=off",
			},
			Reason: reason,
		}}
		if desired.Routing == domain.RoutingWeight {
			cmds = append(cmds, domain.CLICommand{
				Args:   []string{"group", "set", desired.Name, "weight=" + formatWeights(desired.Models, desired.Weights)},
				Reason: reason + " weights",
			})
		}
		return cmds
	}
	if existing.Hidden {
		return nil
	}

	existingModels := dedupeStrings(existing.Models)
	steps := computeModelSteps(existingModels, desired.Models, desired.Routing)
	needRouting := existing.Routing != desired.Routing
	needStays := existing.Stays != "off"
	needWeights := desired.Routing == domain.RoutingWeight && !weightsEqual(existing.Weights, desired.Weights)

	if len(steps) == 0 && !needRouting && !needStays && !needWeights {
		return nil
	}

	if len(steps) == 0 {
		args := []string{"group", "set", desired.Name}
		if needRouting {
			args = append(args, "routing="+desired.Routing)
		}
		if needStays {
			args = append(args, "stays=off")
		}
		if needWeights {
			args = append(args, "weight="+formatWeights(desired.Models, desired.Weights))
		}
		return []domain.CLICommand{{Args: args, Reason: reason}}
	}

	cmds := make([]domain.CLICommand, 0, len(steps))
	for i, step := range steps {
		args := []string{"group", "set", desired.Name}
		if step.add {
			args = append(args, "models+="+strings.Join(step.members, ","))
		} else {
			args = append(args, "models-="+strings.Join(step.members, ","))
		}
		if i == 0 {
			if needRouting {
				args = append(args, "routing="+desired.Routing)
			}
			if needStays {
				args = append(args, "stays=off")
			}
		}
		if i == len(steps)-1 && needWeights {
			args = append(args, "weight="+formatWeights(desired.Models, desired.Weights))
		}
		cmds = append(cmds, domain.CLICommand{Args: args, Reason: reason})
	}
	return cmds
}

// computeModelSteps produces non-empty-safe add/remove steps. For routing=order,
// exact member ordering is preserved; for weight/fast, set equality is sufficient.
func computeModelSteps(existing, desired []string, routing string) []modelStep {
	if len(existing) == 0 {
		return []modelStep{{add: true, members: slices.Clone(desired)}}
	}
	if routing != domain.RoutingOrder {
		return unorderedModelSteps(existing, desired)
	}
	if slices.Equal(existing, desired) {
		return nil
	}

	existingSet := toSet(existing)
	desiredSet := toSet(desired)
	afterRemove := make([]string, 0, len(existing))
	for _, m := range existing {
		if desiredSet[m] {
			afterRemove = append(afterRemove, m)
		}
	}

	// Keep the longest prefix of desired already present in order so we never empty the group.
	matched := longestPrefixSubsequence(desired, afterRemove)
	if matched == 0 {
		prefixLen := 0
		for prefixLen < len(desired) && !existingSet[desired[prefixLen]] {
			prefixLen++
		}
		steps := []modelStep{
			{add: true, members: slices.Clone(desired[:prefixLen])},
			{add: false, members: slices.Clone(existing)},
		}
		if prefixLen < len(desired) {
			steps = append(steps, modelStep{add: true, members: slices.Clone(desired[prefixLen:])})
		}
		return steps
	}

	keptSet := toSet(desired[:matched])
	var toRemove []string
	for _, m := range existing {
		if !keptSet[m] {
			toRemove = append(toRemove, m)
		}
	}
	toAdd := slices.Clone(desired[matched:])
	return orderSafeSteps(toAdd, toRemove)
}

func unorderedModelSteps(existing, desired []string) []modelStep {
	existingSet := toSet(existing)
	desiredSet := toSet(desired)
	var added, removed []string
	for _, m := range desired {
		if !existingSet[m] {
			added = append(added, m)
		}
	}
	for _, m := range existing {
		if !desiredSet[m] {
			removed = append(removed, m)
		}
	}
	var steps []modelStep
	if len(added) > 0 {
		steps = append(steps, modelStep{add: true, members: added})
	}
	if len(removed) > 0 {
		steps = append(steps, modelStep{add: false, members: removed})
	}
	return steps
}

func orderSafeSteps(toAdd, toRemove []string) []modelStep {
	var steps []modelStep
	if slicesOverlap(toAdd, toRemove) {
		if len(toRemove) > 0 {
			steps = append(steps, modelStep{add: false, members: toRemove})
		}
		if len(toAdd) > 0 {
			steps = append(steps, modelStep{add: true, members: toAdd})
		}
		return steps
	}
	if len(toAdd) > 0 {
		steps = append(steps, modelStep{add: true, members: toAdd})
	}
	if len(toRemove) > 0 {
		steps = append(steps, modelStep{add: false, members: toRemove})
	}
	return steps
}

func longestPrefixSubsequence(desired, existing []string) int {
	di := 0
	for _, item := range existing {
		if di < len(desired) && item == desired[di] {
			di++
		}
	}
	return di
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}

func slicesOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := toSet(a)
	for _, item := range b {
		if set[item] {
			return true
		}
	}
	return false
}

func weightsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func formatWeights(models []string, weights map[string]int) string {
	parts := make([]string, 0, len(models))
	for _, m := range models {
		w := weights[m]
		if w <= 0 {
			w = 1
		}
		parts = append(parts, m+"="+strconv.Itoa(w))
	}
	return strings.Join(parts, ",")
}
