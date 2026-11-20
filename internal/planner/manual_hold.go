package planner

import (
	"slices"
	"sort"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

// DetectManualHolds compares the live Magpie registry snapshot against previously
// recorded managed memberships to detect user interventions per SPEC §4.1:
//  1. A managed group exists in snapshot.Groups with Hidden == true ("group hidden").
//  2. A member recorded in st.ManagedGroups[group] was removed from snapshot.Groups[group].Models ("removed from group").
//  3. A managed member appears in snapshot.Groups["off"].Models ("placed in group.off").
//
// The returned slice is deterministically sorted and skips holds already recorded in st.ManualHolds.
func DetectManualHolds(snapshot domain.RegistrySnapshot, st domain.State, managedGroups []string, now time.Time) []domain.ManualHold {
	groups := resolveInspectionGroups(managedGroups, st.ManagedGroups)
	if len(groups) == 0 {
		return nil
	}

	offMembers := groupMemberSet(snapshot.Groups["off"])
	seen := make(map[string]bool)
	var holds []domain.ManualHold

	addHold := func(group, member, reason string) {
		if group == "" || member == "" {
			return
		}
		key := domain.ManualHoldKey(group, member)
		if seen[key] || isMemberHeld(st, group, member) {
			return
		}
		seen[key] = true
		holds = append(holds, domain.ManualHold{
			Group:      group,
			Member:     member,
			Reason:     reason,
			DetectedAt: now,
		})
	}

	for _, groupName := range groups {
		if groupName == "" || groupName == "off" {
			continue
		}
		snapGrp, groupExists := snapshot.Groups[groupName]
		managedMembers := managedMembersForHoldCheck(st.ManagedGroups[groupName], snapGrp, groupExists)
		if len(managedMembers) == 0 {
			continue
		}

		// Rule 1: managed group was hidden by the user.
		if groupExists && snapGrp.Hidden {
			for _, member := range managedMembers {
				addHold(groupName, member, "group hidden")
			}
			continue
		}

		// Rule 3: managed member was placed in group.off (checked before Rule 2
		// so moving a member into group.off records the more specific reason).
		for _, member := range managedMembers {
			if offMembers[member] {
				addHold(groupName, member, "placed in group.off")
			}
		}

		// Rule 2: previously managed member is no longer in the live group.
		if groupExists && len(st.ManagedGroups[groupName]) > 0 {
			liveMembers := groupMemberSet(snapGrp)
			for _, member := range dedupeStrings(st.ManagedGroups[groupName]) {
				if !liveMembers[member] {
					addHold(groupName, member, "removed from group")
				}
			}
		}
	}

	sort.Slice(holds, func(i, j int) bool {
		if holds[i].Group != holds[j].Group {
			return holds[i].Group < holds[j].Group
		}
		if holds[i].Member != holds[j].Member {
			return holds[i].Member < holds[j].Member
		}
		return holds[i].Reason < holds[j].Reason
	})
	return holds
}

func resolveInspectionGroups(managedGroups []string, recorded map[string][]string) []string {
	var list []string
	if len(managedGroups) > 0 {
		list = dedupeStrings(managedGroups)
	} else {
		list = make([]string, 0, len(recorded))
		for groupName := range recorded {
			if trimmed := strings.TrimSpace(groupName); trimmed != "" {
				list = append(list, trimmed)
			}
		}
		list = dedupeStrings(list)
	}
	sort.Strings(list)
	return list
}

func managedMembersForHoldCheck(recorded []string, snapGrp domain.Group, groupExists bool) []string {
	if len(recorded) > 0 {
		return dedupeStrings(recorded)
	}
	if groupExists {
		return dedupeStrings(snapGrp.Models)
	}
	return nil
}

func groupMemberSet(g domain.Group) map[string]bool {
	set := make(map[string]bool, len(g.Models))
	for _, m := range g.Models {
		if m != "" {
			set[m] = true
		}
	}
	return set
}

func isMemberHeld(st domain.State, groupName, member string) bool {
	if len(st.ManualHolds) == 0 || member == "" {
		return false
	}
	if _, ok := st.ManualHolds[domain.ManualHoldKey(groupName, member)]; ok {
		return true
	}
	if _, ok := st.ManualHolds[domain.ManualHoldKey("off", member)]; ok {
		return true
	}
	for _, h := range st.ManualHolds {
		if h.Member == member && (h.Group == groupName || h.Group == "off") {
			return true
		}
	}
	return false
}

func allMembersHeld(st domain.State, groupName string, members []string) bool {
	if len(members) == 0 {
		return false
	}
	for _, m := range members {
		if !isMemberHeld(st, groupName, m) {
			return false
		}
	}
	return true
}

func dedupeStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return slices.Clip(out)
}
