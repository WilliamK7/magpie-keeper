package planner

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"magpie-keeper/internal/domain"
)

// SyncInput supplies the read-only inputs needed to plan Magpie group synchronization.
type SyncInput struct {
	Config   domain.Config
	Snapshot domain.RegistrySnapshot
	State    domain.State
	Admitted []domain.Candidate
	Now      time.Time
}

// PlanChannelSync computes an idempotent, bottom-up group mutation plan from
// admitted candidates per SPEC §4.2 and §4.3 without side effects.
func PlanChannelSync(in SyncInput) domain.GroupPlan {
	plan := domain.GroupPlan{
		TargetGroups: make(map[string]domain.Group),
	}

	roles := resolveRoles(in.Config.Roles)
	eligible := filterEligibleCandidates(in)

	exposeSet := make(map[exposePair]bool)
	supplierDesired := make(map[string]domain.Group)
	var tierCmds, topCmds []domain.CLICommand

	for _, role := range roles {
		roleSuppliers := buildRoleSupplierSpecs(in.Config, eligible, role)
		var freeSubgroups, paidSubgroups []string
		freeMemberSet := make(map[string]bool)

		for _, spec := range roleSuppliers {
			snapGrp, snapExists := in.Snapshot.Groups[spec.name]
			if snapExists && snapGrp.Hidden {
				plan.TargetGroups[spec.name] = cloneGroup(snapGrp)
				continue
			}

			active := filterHeldCandidates(in.State, spec.name, spec.candidates)
			for _, cand := range active {
				if cand.NeedsExpose && !isModelExposed(in.Snapshot, cand.ProviderID, cand.Model) {
					exposeSet[exposePair{providerID: cand.ProviderID, model: cand.Model}] = true
				}
			}

			members := sortAndDedupeCandidateMembers(in.Config, active)
			subMember := "group/" + spec.name
			if len(members) == 0 {
				if snapExists && len(snapGrp.Models) > 0 {
					plan.TargetGroups[spec.name] = cloneGroup(snapGrp)
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("group %s has no active unheld candidates; preserving existing members to keep group non-empty", spec.name))
					if !allMembersHeld(in.State, spec.name, snapGrp.Models) {
						appendTierSubgroup(spec.billing, subMember, &freeSubgroups, &paidSubgroups, freeMemberSet)
					}
				}
				continue
			}

			desired := makeDesiredGroup(spec.name, members, domain.RoutingOrder)
			plan.TargetGroups[spec.name] = cloneGroup(desired)
			supplierDesired[spec.name] = desired
			appendTierSubgroup(spec.billing, subMember, &freeSubgroups, &paidSubgroups, freeMemberSet)
		}

		tierRouting := tierRoutingForRole(role)
		freeName := role + "-free"
		paidName := role + "-paid"

		tierCmds = append(tierCmds, planFreeTierGroup(in, role, freeName, freeSubgroups, tierRouting, &plan)...)
		tierCmds = append(tierCmds, planPaidTierGroup(in, paidName, paidSubgroups, freeMemberSet, tierRouting, &plan)...)
		topCmds = append(topCmds, planTopRoleGroup(in, role, freeName, paidName, &plan)...)
	}

	// Emit bottom-up commands: 1) expose models, 2) supplier groups, 3) tier groups, 4) top role groups.
	plan.Commands = append(plan.Commands, buildExposeCommands(exposeSet)...)

	supplierNames := make([]string, 0, len(supplierDesired))
	for name := range supplierDesired {
		supplierNames = append(supplierNames, name)
	}
	sort.Strings(supplierNames)
	for _, name := range supplierNames {
		plan.Commands = append(plan.Commands, diffAgainstSnapshot(in.Snapshot, name, supplierDesired[name], "sync supplier group")...)
	}

	plan.Commands = append(plan.Commands, tierCmds...)
	plan.Commands = append(plan.Commands, topCmds...)
	return plan
}

func appendTierSubgroup(billing, subMember string, freeOut, paidOut *[]string, freeSet map[string]bool) {
	if billing == domain.BillingFree {
		*freeOut = append(*freeOut, subMember)
		freeSet[subMember] = true
		return
	}
	*paidOut = append(*paidOut, subMember)
}

func planFreeTierGroup(in SyncInput, role, groupName string, freeSubgroups []string, routing string, plan *domain.GroupPlan) []domain.CLICommand {
	snapGrp, exists := in.Snapshot.Groups[groupName]
	if exists && snapGrp.Hidden {
		plan.TargetGroups[groupName] = cloneGroup(snapGrp)
		return nil
	}

	desiredMembers := make([]string, 0, len(freeSubgroups)+1)
	if shouldIncludeTrialInFree(in, role, groupName, len(freeSubgroups)) {
		desiredMembers = append(desiredMembers, "group/transcribe-trial")
	}
	sort.Strings(freeSubgroups)
	for _, m := range freeSubgroups {
		if !isMemberHeld(in.State, groupName, m) {
			desiredMembers = append(desiredMembers, m)
		}
	}
	desiredMembers = dedupeStrings(desiredMembers)

	if len(desiredMembers) == 0 {
		if exists && len(snapGrp.Models) > 0 {
			plan.TargetGroups[groupName] = cloneGroup(snapGrp)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("group %s has no active unheld free subgroups; preserving existing members", groupName))
		}
		return nil
	}

	desired := makeDesiredGroup(groupName, desiredMembers, routing)
	plan.TargetGroups[groupName] = cloneGroup(desired)
	return diffAgainstSnapshot(in.Snapshot, groupName, desired, "sync free tier group")
}

func planPaidTierGroup(in SyncInput, groupName string, paidSubgroups []string, freeMemberSet map[string]bool, routing string, plan *domain.GroupPlan) []domain.CLICommand {
	snapGrp, exists := in.Snapshot.Groups[groupName]
	if exists && snapGrp.Hidden {
		plan.TargetGroups[groupName] = cloneGroup(snapGrp)
		return nil
	}

	sort.Strings(paidSubgroups)
	desiredMembers := make([]string, 0, len(paidSubgroups))
	for _, m := range paidSubgroups {
		if isPaidSafeMember(m, freeMemberSet) && !isMemberHeld(in.State, groupName, m) {
			desiredMembers = append(desiredMembers, m)
		}
	}
	desiredMembers = dedupeStrings(desiredMembers)

	if len(desiredMembers) == 0 {
		if exists && len(snapGrp.Models) > 0 {
			plan.TargetGroups[groupName] = cloneGroup(snapGrp)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("group %s has 0 desired commercial members; preserving existing members to prevent empty paid group", groupName))
		}
		return nil
	}

	desired := makeDesiredGroup(groupName, desiredMembers, routing)
	plan.TargetGroups[groupName] = cloneGroup(desired)
	return diffAgainstSnapshot(in.Snapshot, groupName, desired, "sync paid tier group")
}

func planTopRoleGroup(in SyncInput, role, freeName, paidName string, plan *domain.GroupPlan) []domain.CLICommand {
	snapGrp, exists := in.Snapshot.Groups[role]
	if exists && snapGrp.Hidden {
		plan.TargetGroups[role] = cloneGroup(snapGrp)
		return nil
	}

	var desiredMembers []string
	for _, tierName := range []string{freeName, paidName} {
		member := "group/" + tierName
		if isMemberHeld(in.State, role, member) {
			continue
		}
		if tg, ok := plan.TargetGroups[tierName]; ok && len(tg.Models) > 0 {
			desiredMembers = append(desiredMembers, member)
		} else if snapTier, ok := in.Snapshot.Groups[tierName]; ok && len(snapTier.Models) > 0 {
			desiredMembers = append(desiredMembers, member)
		}
	}

	if len(desiredMembers) == 0 {
		if exists && len(snapGrp.Models) > 0 {
			plan.TargetGroups[role] = cloneGroup(snapGrp)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("group %s has no non-empty tiers; preserving existing members", role))
		}
		return nil
	}

	desired := makeDesiredGroup(role, desiredMembers, domain.RoutingOrder)
	plan.TargetGroups[role] = cloneGroup(desired)
	return diffAgainstSnapshot(in.Snapshot, role, desired, "sync role group")
}

func shouldIncludeTrialInFree(in SyncInput, role, freeGroupName string, otherFreeCount int) bool {
	if role != domain.RoleTranscribe || isMemberHeld(in.State, freeGroupName, "group/transcribe-trial") {
		return false
	}
	_, trialExists := in.Snapshot.Groups["transcribe-trial"]
	if in.Config.BailianTrial.Enabled && in.Config.BailianTrial.Paused {
		snapFree, freeExists := in.Snapshot.Groups[freeGroupName]
		return freeExists && slices.Contains(snapFree.Models, "group/transcribe-trial") && otherFreeCount == 0
	}
	return trialExists || in.Config.BailianTrial.Enabled
}
