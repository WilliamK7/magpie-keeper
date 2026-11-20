package planner

import (
	"slices"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

// TrialPlanInput supplies the read-only state and provider commands needed to
// synchronize group/transcribe-trial and attach/detach it in group/transcribe-free.
type TrialPlanInput struct {
	Config           domain.Config
	Snapshot         domain.RegistrySnapshot
	State            domain.State
	DesiredMembers   []string
	ProviderCommands []domain.CLICommand
	Now              time.Time
}

// PlanTrialGroup plans Bailian trial provider commands, group/transcribe-trial
// transitions, and group/transcribe-free membership while guaranteeing that
// neither group/transcribe-trial nor group/transcribe-free is ever emptied.
func PlanTrialGroup(in TrialPlanInput) domain.GroupPlan {
	plan := domain.GroupPlan{
		TargetGroups: make(map[string]domain.Group),
	}

	for _, cmd := range in.ProviderCommands {
		plan.Commands = append(plan.Commands, domain.CLICommand{
			Args:   slices.Clone(cmd.Args),
			Reason: cmd.Reason,
		})
	}

	activeDesired := filterDesiredTrialMembers(in)
	planTrialMembers(in, activeDesired, &plan)
	planTrialFreeAttachment(in, len(activeDesired) > 0, &plan)
	return plan
}

func filterDesiredTrialMembers(in TrialPlanInput) []string {
	deduped := dedupeStrings(in.DesiredMembers)
	out := make([]string, 0, len(deduped))
	for _, m := range deduped {
		if isMemberHeld(in.State, "transcribe-trial", m) {
			continue
		}
		out = append(out, m)
	}
	if max := in.Config.BailianTrial.MaxActiveModels; max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

func planTrialMembers(in TrialPlanInput, activeDesired []string, plan *domain.GroupPlan) {
	trialSnap, trialExists := in.Snapshot.Groups["transcribe-trial"]
	if trialExists && trialSnap.Hidden {
		plan.TargetGroups["transcribe-trial"] = cloneGroup(trialSnap)
		plan.Warnings = append(plan.Warnings, "group transcribe-trial is hidden; skipping mutation")
		return
	}

	if len(activeDesired) > 0 {
		desired := makeDesiredGroup("transcribe-trial", activeDesired, domain.RoutingOrder)
		plan.TargetGroups["transcribe-trial"] = cloneGroup(desired)
		plan.Commands = append(plan.Commands, diffAgainstSnapshot(in.Snapshot, "transcribe-trial", desired, "sync trial group")...)
		return
	}

	// DesiredMembers is empty: never create an empty group, and never empty an existing one.
	if !trialExists || len(trialSnap.Models) == 0 {
		return
	}

	keptMember, extraOff := selectDisabledTrialMember(in, trialSnap)
	if keptMember == "" {
		plan.TargetGroups["transcribe-trial"] = cloneGroup(trialSnap)
		plan.Warnings = append(plan.Warnings, "group transcribe-trial has no selectable disabled fallback; preserving existing members")
		return
	}
	if extraOff != nil {
		plan.Commands = append(plan.Commands, *extraOff)
	}

	desired := makeDesiredGroup("transcribe-trial", []string{keptMember}, domain.RoutingOrder)
	plan.TargetGroups["transcribe-trial"] = cloneGroup(desired)
	plan.Commands = append(plan.Commands, planGroupDiff(&trialSnap, desired, "drain trial group keeping one disabled member")...)
}

func selectDisabledTrialMember(in TrialPlanInput, trialSnap domain.Group) (string, *domain.CLICommand) {
	// 1. Prefer an existing member whose provider is already off or being turned off.
	for _, holdAware := range []bool{true, false} {
		for _, m := range trialSnap.Models {
			if holdAware && isMemberHeld(in.State, "transcribe-trial", m) {
				continue
			}
			pid := memberProviderID(m)
			if pid != "" && isProviderDisabledOrTurningOff(in.Snapshot, in.ProviderCommands, pid) {
				return m, nil
			}
		}
	}

	// 2. Fallback to disabled template provider placeholder if template provider is off.
	tmplID := strings.TrimSpace(in.Config.BailianTrial.TemplateProvider)
	if tmplID == "" {
		tmplID = "transcribe-bailian-trial"
	}
	if tmplProv, ok := in.Snapshot.Providers[tmplID]; ok && tmplProv.Off && !hasProviderCmd(in.ProviderCommands, "on", tmplID) {
		placeholder := "placeholder"
		if len(tmplProv.Models) > 0 && tmplProv.Models[0] != "" {
			placeholder = tmplProv.Models[0]
		} else if len(tmplProv.ModelsOff) > 0 && tmplProv.ModelsOff[0] != "" {
			placeholder = tmplProv.ModelsOff[0]
		}
		member := tmplID + "/" + placeholder
		if !isMemberHeld(in.State, "transcribe-trial", member) {
			return member, nil
		}
	}

	// 3. Otherwise keep the first existing member and turn its provider off before draining.
	if len(trialSnap.Models) > 0 {
		kept := trialSnap.Models[0]
		pid := memberProviderID(kept)
		if pid != "" && !isProviderDisabledOrTurningOff(in.Snapshot, in.ProviderCommands, pid) {
			cmd := domain.CLICommand{
				Args:   []string{"provider", "off", pid},
				Reason: "disable kept trial placeholder before drain",
			}
			return kept, &cmd
		}
		return kept, nil
	}
	return "", nil
}

func planTrialFreeAttachment(in TrialPlanInput, trialActive bool, plan *domain.GroupPlan) {
	if !isTranscribeRoleConfigured(in.Config.Roles) {
		return
	}
	freeSnap, freeExists := in.Snapshot.Groups["transcribe-free"]
	if !freeExists || freeSnap.Hidden {
		if freeExists {
			plan.TargetGroups["transcribe-free"] = cloneGroup(freeSnap)
		}
		return
	}

	const trialRef = "group/transcribe-trial"
	if isMemberHeld(in.State, "transcribe-free", trialRef) {
		plan.TargetGroups["transcribe-free"] = cloneGroup(freeSnap)
		return
	}

	hasTrial := slices.Contains(freeSnap.Models, trialRef)
	if trialActive {
		if hasTrial {
			plan.TargetGroups["transcribe-free"] = cloneGroup(freeSnap)
			return
		}
		updated := append(slices.Clone(freeSnap.Models), trialRef)
		target := cloneGroup(freeSnap)
		target.Models = updated
		args := []string{"group", "set", "transcribe-free", "models+=" + trialRef}
		if freeSnap.Routing == domain.RoutingWeight {
			target.Weights = mergedWeights(updated, freeSnap.Weights)
			args = append(args, "weight="+formatWeights(updated, target.Weights))
		}
		plan.TargetGroups["transcribe-free"] = target
		plan.Commands = append(plan.Commands, domain.CLICommand{
			Args:   args,
			Reason: "attach active trial group to transcribe-free",
		})
		return
	}

	if !hasTrial {
		plan.TargetGroups["transcribe-free"] = cloneGroup(freeSnap)
		return
	}
	if len(freeSnap.Models) < 2 {
		plan.TargetGroups["transcribe-free"] = cloneGroup(freeSnap)
		plan.Warnings = append(plan.Warnings, "cannot detach group/transcribe-trial from transcribe-free because it is the only remaining member")
		return
	}

	remaining := make([]string, 0, len(freeSnap.Models)-1)
	for _, m := range freeSnap.Models {
		if m != trialRef {
			remaining = append(remaining, m)
		}
	}
	target := cloneGroup(freeSnap)
	target.Models = remaining
	args := []string{"group", "set", "transcribe-free", "models-=" + trialRef}
	if freeSnap.Routing == domain.RoutingWeight {
		target.Weights = mergedWeights(remaining, freeSnap.Weights)
		args = append(args, "weight="+formatWeights(remaining, target.Weights))
	}
	plan.TargetGroups["transcribe-free"] = target
	plan.Commands = append(plan.Commands, domain.CLICommand{
		Args:   args,
		Reason: "detach drained trial group from transcribe-free",
	})
}

func isTranscribeRoleConfigured(roles []string) bool {
	if len(roles) == 0 {
		return true
	}
	return slices.Contains(roles, domain.RoleTranscribe)
}

func memberProviderID(member string) string {
	if member == "" || strings.HasPrefix(member, "group/") {
		return ""
	}
	pid, _, ok := strings.Cut(member, "/")
	if !ok {
		return ""
	}
	return pid
}

func isProviderDisabledOrTurningOff(snap domain.RegistrySnapshot, cmds []domain.CLICommand, providerID string) bool {
	if hasProviderCmd(cmds, "off", providerID) {
		return true
	}
	if hasProviderCmd(cmds, "on", providerID) {
		return false
	}
	p, ok := snap.Providers[providerID]
	return ok && p.Off
}

func hasProviderCmd(cmds []domain.CLICommand, verb, providerID string) bool {
	for _, cmd := range cmds {
		if len(cmd.Args) >= 3 && cmd.Args[0] == "provider" && cmd.Args[1] == verb && cmd.Args[2] == providerID {
			return true
		}
	}
	return false
}

func mergedWeights(models []string, existing map[string]int) map[string]int {
	out := make(map[string]int, len(models))
	for _, m := range models {
		if w := existing[m]; w > 0 {
			out[m] = w
		} else {
			out[m] = 1
		}
	}
	return out
}
