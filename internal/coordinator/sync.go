package coordinator

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"magpie-keeper/internal/admit"
	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/planner"
	"magpie-keeper/internal/state"
)

func (c *Coordinator) handleListCandidates(ctx context.Context) (CandidatesReport, error) {
	now := c.now()
	st, err := c.store.Load()
	if err != nil {
		return CandidatesReport{}, err
	}
	snap, err := c.gateway.ReadRegistry(ctx)
	if err != nil {
		return CandidatesReport{}, err
	}
	servedList, err := c.gateway.ListServedModels(ctx)
	if err != nil {
		return CandidatesReport{}, err
	}
	servedSet := make(map[string]bool, len(servedList))
	for _, m := range servedList {
		servedSet[m] = true
	}

	candidates, warnings := c.discoverAllCandidates(ctx, snap, servedSet)
	report := CandidatesReport{
		GeneratedAt: now,
		ServedCount: len(servedList),
		Warnings:    warnings,
	}
	storeWithClock := state.NewStoreWithClock(c.store.Path(), c.now)
	for _, cand := range candidates {
		dec := CandidateDecision{Candidate: cand}
		if _, ok := state.GetCachedAdmission(st, cand.EndpointKeyHash, cand.Model, admit.SuiteVersion); ok {
			dec.Decision = "admitted_cached"
		} else if cd, active := storeWithClock.IsInCooldown(st, cand.Member); active {
			dec.Decision = "cooldown"
			dec.Reason = fmt.Sprintf("until %s (%s)", cd.Until.Format(time.RFC3339), cd.Reason)
		} else {
			dec.Decision = "needs_evaluation"
		}
		report.Candidates = append(report.Candidates, dec)
	}
	return report, nil
}

func (c *Coordinator) handleChannelSync(ctx context.Context, opts SyncOptions) (SyncReport, error) {
	now := c.now()
	report := SyncReport{
		ExecutedAt: now,
		Applied:    opts.Apply && !opts.DryRun,
		DryRun:     opts.DryRun || !opts.Apply,
	}

	st, err := c.store.Load()
	if err != nil {
		return report, err
	}

	if opts.IfStale > 0 && !st.LastChannelSyncAt.IsZero() {
		age := now.Sub(st.LastChannelSyncAt)
		if age < opts.IfStale {
			report.Skipped = true
			report.SkipReason = fmt.Sprintf("last channel sync %s ago (< %s)", age.Round(time.Second), opts.IfStale)
			return report, nil
		}
	}

	snap, err := c.gateway.ReadRegistry(ctx)
	if err != nil {
		return report, err
	}
	servedList, err := c.gateway.ListServedModels(ctx)
	if err != nil {
		return report, err
	}
	servedSet := make(map[string]bool, len(servedList))
	for _, m := range servedList {
		servedSet[m] = true
	}

	storeClock := state.NewStoreWithClock(c.store.Path(), c.now)
	storeClock.PruneExpiredCooldowns(&st)
	holds := planner.DetectManualHolds(snap, st, managedGroupNames(c.cfg, st), now)
	for _, h := range holds {
		state.AddManualHold(&st, h)
	}

	candidates, warnings := c.discoverAllCandidates(ctx, snap, servedSet)
	report.Warnings = append(report.Warnings, warnings...)

	admitted, evalCount, evalWarnings, err := c.admitCandidates(ctx, storeClock, &st, candidates, now)
	report.Warnings = append(report.Warnings, evalWarnings...)
	report.Evaluated = evalCount
	if err != nil {
		return report, err
	}
	for _, a := range admitted {
		report.Admitted = append(report.Admitted, a.Member)
	}

	plan := planner.PlanChannelSync(planner.SyncInput{
		Config:   c.cfg,
		Snapshot: snap,
		State:    st,
		Admitted: admitted,
		Now:      now,
	})
	report.Commands = plan.Commands
	report.Warnings = append(report.Warnings, plan.Warnings...)

	targetNames := make([]string, 0, len(plan.TargetGroups))
	for name := range plan.TargetGroups {
		targetNames = append(targetNames, name)
	}
	sort.Strings(targetNames)
	for _, name := range targetNames {
		g := plan.TargetGroups[name]
		report.TargetGroups = append(report.TargetGroups, GroupStatus{
			Name:    name,
			Routing: g.Routing,
			Stays:   g.Stays,
			Hidden:  g.Hidden,
			Members: append([]string(nil), g.Models...),
		})
	}

	if report.Applied {
		backupPath, err := c.executePlan(ctx, plan.Commands, false)
		report.BackupPath = backupPath
		if err != nil {
			_ = c.store.Save(st)
			return report, err
		}
		recordHoldsAndTargetGroups(&st, holds, plan.TargetGroups, "transcribe-trial")
		st.LastChannelSyncAt = now
		if err := c.store.Save(st); err != nil {
			return report, err
		}
	} else if len(holds) > 0 || evalCount > 0 {
		if err := c.store.Save(st); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (c *Coordinator) discoverAllCandidates(ctx context.Context, snap domain.RegistrySnapshot, served map[string]bool) ([]domain.Candidate, []string) {
	providerIDs := make([]string, 0, len(c.cfg.Providers))
	for id := range c.cfg.Providers {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)

	var all []domain.Candidate
	var warnings []string
	for _, id := range providerIDs {
		if slices.Contains(c.cfg.ExcludedProviders, id) {
			continue
		}
		prov, ok := snap.Providers[id]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("configured provider %q not found in Magpie registry", id))
			continue
		}
		rule := c.cfg.Providers[id]
		discovered, err := c.catalogReg.Discover(ctx, prov, rule, served)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("discover %q (%s): %v", id, rule.Adapter, err))
			continue
		}
		all = append(all, discovered...)
	}
	return all, warnings
}

func (c *Coordinator) admitCandidates(ctx context.Context, storeClock *state.Store, st *domain.State, candidates []domain.Candidate, now time.Time) ([]domain.Candidate, int, []string, error) {
	cooldownDur := time.Duration(c.cfg.CooldownSeconds) * time.Second
	if cooldownDur <= 0 {
		cooldownDur = time.Hour
	}

	var admitted []domain.Candidate
	var warnings []string
	evalCount := 0

	for _, cand := range candidates {
		if _, ok := state.GetCachedAdmission(*st, cand.EndpointKeyHash, cand.Model, admit.SuiteVersion); ok {
			admitted = append(admitted, cand)
			continue
		}
		if _, active := storeClock.IsInCooldown(*st, cand.Member); active {
			continue
		}
		if evalCount >= c.cfg.MaxEvaluationsPerCycle {
			warnings = append(warnings, fmt.Sprintf("evaluation cycle limit (%d) reached; deferring %s", c.cfg.MaxEvaluationsPerCycle, cand.Member))
			continue
		}
		reserved, err := storeClock.ReserveDailyBudget(st, c.cfg.MaxEvaluationsPerDay)
		if err != nil {
			return admitted, evalCount, warnings, fmt.Errorf("reserve daily budget: %w", err)
		}
		if !reserved {
			warnings = append(warnings, fmt.Sprintf("daily evaluation budget (%d) exhausted; deferring %s", c.cfg.MaxEvaluationsPerDay, cand.Member))
			continue
		}

		evalCount++
		res, err := c.admitter.Evaluate(ctx, cand.Member)
		if err != nil {
			storeClock.SetCooldown(st, cand.Member, domain.TransientCooldown, err.Error(), true)
			_ = c.store.Save(*st)
			warnings = append(warnings, fmt.Sprintf("evaluate %s error: %v", cand.Member, err))
			continue
		}
		if res.EvaluatedAt.IsZero() {
			res.EvaluatedAt = now
		}
		res.EndpointKeyHash = cand.EndpointKeyHash

		switch res.Status {
		case domain.AdmissionPassed:
			state.RecordAdmissionByModel(st, cand.Model, res)
			storeClock.ClearCooldown(st, cand.Member)
			admitted = append(admitted, cand)
		case domain.AdmissionTransient:
			storeClock.SetCooldown(st, cand.Member, domain.TransientCooldown, res.Reason, true)
		default:
			state.RecordAdmissionByModel(st, cand.Model, res)
			storeClock.SetCooldown(st, cand.Member, cooldownDur, res.Reason, false)
		}
		if err := c.store.Save(*st); err != nil {
			return admitted, evalCount, warnings, fmt.Errorf("save state after evaluation: %w", err)
		}
	}
	return admitted, evalCount, warnings, nil
}
