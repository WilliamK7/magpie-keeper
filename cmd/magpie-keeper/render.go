package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"magpie-keeper/internal/coordinator"
)

func renderStatusText(w io.Writer, rep coordinator.StatusReport) {
	fmt.Fprintf(w, "Magpie Dir: %s (port %d)\n", rep.MagpieDir, rep.MagpiePort)
	fmt.Fprintf(w, "Roles: %s | Last Channel Sync: %s | Daily Budget: %d/%d (%s)\n",
		strings.Join(rep.Roles, ","), formatTimeOrDash(rep.LastChannelSyncAt),
		rep.DailyBudget.Used, rep.MaxEvalPerDay, rep.DailyBudget.Date)
	fmt.Fprintf(w, "Bailian Trial: enabled=%v paused=%v workspace=%s last_sync=%s errors=%d\n",
		rep.Trial.Enabled, rep.Trial.Paused, rep.Trial.WorkspaceID,
		formatTimeOrDash(rep.Trial.LastQuotaSyncAt), rep.Trial.ConsecutiveErrors)
	if len(rep.Trial.ActiveChannels) > 0 {
		fmt.Fprintln(w, "Trial Channels:")
		for _, ch := range rep.Trial.ActiveChannels {
			fmt.Fprintf(w, "  - %s active=%v rpm=%d expires=%s remaining=%.0f\n",
				ch.Member, ch.Active, ch.RPM, ch.Expires, ch.Remaining)
		}
	}
	fmt.Fprintln(w, "Groups:")
	for _, g := range rep.Groups {
		fmt.Fprintf(w, "  - %s [routing=%s stays=%s hidden=%v]: %s\n",
			g.Name, g.Routing, g.Stays, g.Hidden, strings.Join(g.Members, ", "))
	}
	if len(rep.ManualHolds) > 0 {
		fmt.Fprintln(w, "Manual Holds:")
		for _, h := range rep.ManualHolds {
			fmt.Fprintf(w, "  - %s -> %s (%s)\n", h.Group, h.Member, h.Reason)
		}
	}
}

func renderCandidatesText(w io.Writer, rep coordinator.CandidatesReport) {
	fmt.Fprintf(w, "Served Models: %d | Candidates: %d\n", rep.ServedCount, len(rep.Candidates))
	for _, c := range rep.Candidates {
		fmt.Fprintf(w, "  - %-40s billing=%-10s decision=%-16s expose=%v slug=%s %s\n",
			c.Candidate.Member, c.Candidate.Billing, c.Decision, c.Candidate.NeedsExpose, c.Candidate.SupplierSlug, c.Reason)
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(w, "WARN: %s\n", warn)
	}
}

func renderSyncText(w io.Writer, rep coordinator.SyncReport) {
	if rep.Skipped {
		fmt.Fprintf(w, "Channel sync skipped: %s\n", rep.SkipReason)
		return
	}
	mode := "dry-run"
	if rep.Applied {
		mode = "applied"
	}
	fmt.Fprintf(w, "Channel sync (%s): evaluated=%d admitted=%d commands=%d\n",
		mode, rep.Evaluated, len(rep.Admitted), len(rep.Commands))
	if rep.BackupPath != "" {
		fmt.Fprintf(w, "Backup: %s\n", rep.BackupPath)
	}
	for _, cmd := range rep.Commands {
		fmt.Fprintf(w, "  > magpie.com %s\n", strings.Join(cmd.Args, " "))
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(w, "WARN: %s\n", warn)
	}
}

func renderTrialText(w io.Writer, rep coordinator.TrialReport) {
	if rep.Skipped {
		fmt.Fprintf(w, "Trial sync skipped: %s\n", rep.SkipReason)
		return
	}
	fmt.Fprintf(w, "Trial sync: evaluated=%d desired=%d shared_rpm=%d commands=%d\n",
		rep.Evaluated, len(rep.DesiredMembers), rep.SharedRPM, len(rep.Commands))
	if rep.BackupPath != "" {
		fmt.Fprintf(w, "Backup: %s\n", rep.BackupPath)
	}
	for _, m := range rep.DesiredMembers {
		fmt.Fprintf(w, "  * %s\n", m)
	}
	for _, cmd := range rep.Commands {
		fmt.Fprintf(w, "  > magpie.com %s\n", strings.Join(cmd.Args, " "))
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(w, "WARN: %s\n", warn)
	}
}

func formatTimeOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
