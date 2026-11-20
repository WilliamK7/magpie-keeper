package planner

import (
	"slices"
	"strings"
	"testing"
	"time"

	"magpie-keeper/internal/domain"
)

var fixedNow = time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

func TestPlanChannelSyncBottomUpAndIdempotency(t *testing.T) {
	cfg := domain.Config{
		Roles: []string{domain.RoleTranscribe},
		BailianTrial: domain.BailianTrialConfig{
			Enabled: false,
		},
		Providers: map[string]domain.ProviderRule{
			"transcribe-openrouter-free": {Preferred: []string{"m2", "m1"}},
		},
	}
	snap := domain.RegistrySnapshot{
		Providers: map[string]domain.Provider{
			"transcribe-openrouter-free": {ID: "transcribe-openrouter-free", Models: []string{"m1"}},
			"transcribe-bailian":         {ID: "transcribe-bailian"},
		},
		Groups: map[string]domain.Group{},
	}
	admitted := []domain.Candidate{
		{
			ProviderID:   "transcribe-openrouter-free",
			Model:        "m1",
			Member:       "transcribe-openrouter-free/m1",
			Billing:      domain.BillingFree,
			SupplierSlug: "openrouter-free",
		},
		{
			ProviderID:   "transcribe-openrouter-free",
			Model:        "m2",
			Member:       "transcribe-openrouter-free/m2",
			Billing:      domain.BillingFree,
			NeedsExpose:  true,
			SupplierSlug: "openrouter-free",
		},
		{
			ProviderID:   "transcribe-bailian",
			Model:        "qwen-max",
			Member:       "transcribe-bailian/qwen-max",
			Billing:      domain.BillingCommercial,
			NeedsExpose:  true,
			SupplierSlug: "bailian",
		},
	}

	plan := PlanChannelSync(SyncInput{
		Config:   cfg,
		Snapshot: snap,
		Admitted: admitted,
		Now:      fixedNow,
	})

	// Verify strict bottom-up ordering:
	// provider models -> supplier groups -> tier groups -> top role group.
	gotCmds := formatCmds(plan.Commands)
	wantOrder := []string{
		"provider models transcribe-bailian +qwen-max",
		"provider models transcribe-openrouter-free +m2",
		"group add transcribe-bailian models=transcribe-bailian/qwen-max routing=order stays=off",
		"group add transcribe-openrouter-free models=transcribe-openrouter-free/m2,transcribe-openrouter-free/m1 routing=order stays=off",
		"group add transcribe-free models=group/transcribe-openrouter-free routing=weight stays=off",
		"group set transcribe-free weight=group/transcribe-openrouter-free=1",
		"group add transcribe-paid models=group/transcribe-bailian routing=weight stays=off",
		"group set transcribe-paid weight=group/transcribe-bailian=1",
		"group add transcribe models=group/transcribe-free,group/transcribe-paid routing=order stays=off",
	}
	if !slices.Equal(gotCmds, wantOrder) {
		t.Fatalf("command sequence mismatch:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(gotCmds, "\n  "), strings.Join(wantOrder, "\n  "))
	}

	// Simulate execution against Magpie snapshot and verify idempotency.
	nextSnap := applyCommandsForTest(t, snap, plan.Commands)
	assertGroupsMatch(t, nextSnap.Groups, plan.TargetGroups)

	rerun := PlanChannelSync(SyncInput{
		Config:   cfg,
		Snapshot: nextSnap,
		Admitted: admitted,
		Now:      fixedNow,
	})
	if len(rerun.Commands) != 0 {
		t.Fatalf("expected 0 commands on idempotent rerun, got: %v", formatCmds(rerun.Commands))
	}
}

func TestPlanChannelSyncMultiRoleReviewFastAndExcludesVllmSR(t *testing.T) {
	cfg := domain.Config{
		Roles: []string{domain.RoleTranscribe, domain.RoleReview},
		BailianTrial: domain.BailianTrialConfig{
			Enabled: true,
			Paused:  false,
		},
	}
	admitted := []domain.Candidate{
		{
			ProviderID:   "vllm-sr",
			Model:        "vllm-sr/auto",
			Member:       "vllm-sr/vllm-sr/auto",
			Billing:      domain.BillingFree,
			AllowedRoles: []string{domain.RoleTranscribe},
			SupplierSlug: "vllm-sr",
		},
		{
			ProviderID:   "openrouter",
			Model:        "m1:free",
			Member:       "openrouter/m1:free",
			Billing:      domain.BillingFree,
			SupplierSlug: "openrouter-free",
		},
		{
			ProviderID:   "bailian",
			Model:        "qwen-max",
			Member:       "bailian/qwen-max",
			Billing:      domain.BillingCommercial,
			SupplierSlug: "bailian",
		},
	}

	plan := PlanChannelSync(SyncInput{
		Config:   cfg,
		Snapshot: domain.RegistrySnapshot{Groups: map[string]domain.Group{}},
		Admitted: admitted,
		Now:      fixedNow,
	})

	if _, ok := plan.TargetGroups["review-vllm-sr"]; ok {
		t.Fatal("vllm-sr must be excluded from review role")
	}
	if tg := plan.TargetGroups["transcribe-free"]; tg.Routing != domain.RoutingWeight ||
		!slices.Contains(tg.Models, "group/transcribe-vllm-sr") ||
		!slices.Contains(tg.Models, "group/transcribe-trial") {
		t.Fatalf("unexpected transcribe-free group: %+v", tg)
	}
	if tg := plan.TargetGroups["review-free"]; tg.Routing != domain.RoutingFast ||
		slices.Contains(tg.Models, "group/review-vllm-sr") ||
		slices.Contains(tg.Models, "group/transcribe-trial") ||
		!slices.Equal(tg.Models, []string{"group/review-openrouter-free"}) {
		t.Fatalf("unexpected review-free group: %+v", tg)
	}
	if tg := plan.TargetGroups["review-paid"]; tg.Routing != domain.RoutingFast {
		t.Fatalf("expected review-paid routing=fast, got %q", tg.Routing)
	}
	if tg := plan.TargetGroups["review"]; tg.Routing != domain.RoutingOrder ||
		!slices.Equal(tg.Models, []string{"group/review-free", "group/review-paid"}) {
		t.Fatalf("unexpected review top group: %+v", tg)
	}
}

func TestPlanChannelSyncPaidInvariants(t *testing.T) {
	// 1. Mixed free+commercial candidates sharing the same supplier slug must split cleanly,
	//    and trial/free candidates must never enter <role>-paid.
	cfg := domain.Config{Roles: []string{domain.RoleTranscribe}}
	admitted := []domain.Candidate{
		{
			ProviderID:   "vercel",
			Model:        "free-m",
			Member:       "vercel/free-m",
			Billing:      domain.BillingFree,
			SupplierSlug: "vercel",
		},
		{
			ProviderID:   "vercel",
			Model:        "paid-m",
			Member:       "vercel/paid-m",
			Billing:      domain.BillingCommercial,
			SupplierSlug: "vercel",
		},
		{
			ProviderID:   domain.TrialProviderID("qwen-plus"),
			Model:        "qwen-plus",
			Member:       domain.TrialMember("qwen-plus"),
			Billing:      domain.BillingCommercial, // hostile/corrupt tag must still be blocked
			SupplierSlug: "bailian",
		},
	}

	plan := PlanChannelSync(SyncInput{
		Config:   cfg,
		Snapshot: domain.RegistrySnapshot{Groups: map[string]domain.Group{}},
		Admitted: admitted,
		Now:      fixedNow,
	})

	paid := plan.TargetGroups["transcribe-paid"]
	if !slices.Equal(paid.Models, []string{"group/transcribe-vercel-paid"}) {
		t.Fatalf("unexpected transcribe-paid members: %v", paid.Models)
	}
	free := plan.TargetGroups["transcribe-free"]
	if !slices.Equal(free.Models, []string{"group/transcribe-vercel-free"}) {
		t.Fatalf("unexpected transcribe-free members: %v", free.Models)
	}

	// 2. When commercial candidates drop to 0, existing transcribe-paid is preserved with warning.
	existingSnap := domain.RegistrySnapshot{
		Groups: map[string]domain.Group{
			"transcribe-paid": {
				Name:    "transcribe-paid",
				Models:  []string{"group/transcribe-bailian"},
				Routing: domain.RoutingWeight,
				Stays:   "off",
				Weights: map[string]int{"group/transcribe-bailian": 1},
			},
			"transcribe-bailian": {
				Name:    "transcribe-bailian",
				Models:  []string{"bailian/qwen-max"},
				Routing: domain.RoutingOrder,
				Stays:   "off",
			},
		},
	}
	preservePlan := PlanChannelSync(SyncInput{
		Config:   cfg,
		Snapshot: existingSnap,
		Admitted: []domain.Candidate{{
			ProviderID:   "openrouter",
			Model:        "m1:free",
			Member:       "openrouter/m1:free",
			Billing:      domain.BillingFree,
			SupplierSlug: "openrouter-free",
		}},
		Now: fixedNow,
	})
	if len(preservePlan.Warnings) == 0 {
		t.Fatal("expected warning when preserving existing transcribe-paid with 0 commercial candidates")
	}
	if !slices.Equal(preservePlan.TargetGroups["transcribe-paid"].Models, []string{"group/transcribe-bailian"}) {
		t.Fatalf("expected existing transcribe-paid preserved, got: %+v", preservePlan.TargetGroups["transcribe-paid"])
	}
	if !slices.Equal(preservePlan.TargetGroups["transcribe"].Models, []string{"group/transcribe-free", "group/transcribe-paid"}) {
		t.Fatalf("expected transcribe top group to keep preserved paid tier, got: %+v", preservePlan.TargetGroups["transcribe"])
	}
	for _, cmd := range preservePlan.Commands {
		if len(cmd.Args) >= 3 && cmd.Args[0] == "group" && cmd.Args[2] == "transcribe-paid" {
			t.Fatalf("must not mutate preserved transcribe-paid, got cmd: %v", cmd.Args)
		}
	}
}

func TestDetectManualHoldsAndPreservation(t *testing.T) {
	snap := domain.RegistrySnapshot{
		Groups: map[string]domain.Group{
			"transcribe-openrouter": {
				Name:    "transcribe-openrouter",
				Models:  []string{"or/kept"},
				Routing: domain.RoutingOrder,
				Stays:   "off",
			},
			"transcribe-vercel": {
				Name:    "transcribe-vercel",
				Models:  []string{"vercel/v1"},
				Hidden:  true,
				Routing: domain.RoutingOrder,
				Stays:   "off",
			},
			"transcribe-bailian": {
				Name:    "transcribe-bailian",
				Models:  []string{"bailian/b1", "bailian/b2"},
				Routing: domain.RoutingOrder,
				Stays:   "off",
			},
			"off": {
				Name:   "off",
				Models: []string{"bailian/b2"},
			},
		},
	}
	st := domain.State{
		ManagedGroups: map[string][]string{
			"transcribe-openrouter": {"or/kept", "or/removed", "or/already-held"},
			"transcribe-vercel":     {"vercel/v1"},
			"transcribe-bailian":    {"bailian/b1", "bailian/b2"},
		},
		ManualHolds: map[string]domain.ManualHold{
			domain.ManualHoldKey("transcribe-openrouter", "or/already-held"): {
				Group:  "transcribe-openrouter",
				Member: "or/already-held",
				Reason: "removed from group",
			},
		},
	}

	holds := DetectManualHolds(snap, st, []string{
		"transcribe-bailian",
		"transcribe-openrouter",
		"transcribe-vercel",
	}, fixedNow)

	if len(holds) != 3 {
		t.Fatalf("expected 3 new holds, got %d: %+v", len(holds), holds)
	}
	want := []domain.ManualHold{
		{Group: "transcribe-bailian", Member: "bailian/b2", Reason: "placed in group.off", DetectedAt: fixedNow},
		{Group: "transcribe-openrouter", Member: "or/removed", Reason: "removed from group", DetectedAt: fixedNow},
		{Group: "transcribe-vercel", Member: "vercel/v1", Reason: "group hidden", DetectedAt: fixedNow},
	}
	for i := range want {
		if holds[i].Group != want[i].Group || holds[i].Member != want[i].Member ||
			holds[i].Reason != want[i].Reason || !holds[i].DetectedAt.Equal(want[i].DetectedAt) {
			t.Fatalf("hold[%d] = %+v, want %+v", i, holds[i], want[i])
		}
		st.ManualHolds[domain.ManualHoldKey(holds[i].Group, holds[i].Member)] = holds[i]
	}

	// Verify PlanChannelSync respects all holds and skips hidden group mutation.
	plan := PlanChannelSync(SyncInput{
		Config:   domain.Config{Roles: []string{domain.RoleTranscribe}},
		Snapshot: snap,
		State:    st,
		Admitted: []domain.Candidate{
			{ProviderID: "or", Model: "kept", Member: "or/kept", Billing: domain.BillingFree, SupplierSlug: "openrouter"},
			{ProviderID: "or", Model: "removed", Member: "or/removed", Billing: domain.BillingFree, SupplierSlug: "openrouter"},
			{ProviderID: "or", Model: "already-held", Member: "or/already-held", Billing: domain.BillingFree, SupplierSlug: "openrouter"},
			{ProviderID: "vercel", Model: "v1", Member: "vercel/v1", Billing: domain.BillingFree, SupplierSlug: "vercel"},
			{ProviderID: "vercel", Model: "v2", Member: "vercel/v2", Billing: domain.BillingFree, SupplierSlug: "vercel"},
			{ProviderID: "bailian", Model: "b1", Member: "bailian/b1", Billing: domain.BillingCommercial, SupplierSlug: "bailian"},
			{ProviderID: "bailian", Model: "b2", Member: "bailian/b2", Billing: domain.BillingCommercial, SupplierSlug: "bailian"},
		},
		Now: fixedNow,
	})

	if got := plan.TargetGroups["transcribe-openrouter"].Models; !slices.Equal(got, []string{"or/kept"}) {
		t.Fatalf("held members leaked back into transcribe-openrouter: %v", got)
	}
	if got := plan.TargetGroups["transcribe-bailian"].Models; !slices.Equal(got, []string{"bailian/b1"}) {
		t.Fatalf("group.off member leaked back into transcribe-bailian: %v", got)
	}
	for _, cmd := range plan.Commands {
		if len(cmd.Args) >= 3 && cmd.Args[0] == "group" && cmd.Args[2] == "transcribe-vercel" {
			t.Fatalf("hidden group transcribe-vercel must not be mutated, got: %v", cmd.Args)
		}
	}
}

func TestPlanChannelSyncCompleteReplacementAndOrderSafety(t *testing.T) {
	snap := domain.RegistrySnapshot{
		Groups: map[string]domain.Group{
			"transcribe-openrouter": {
				Name:    "transcribe-openrouter",
				Models:  []string{"or/old1", "or/old2"},
				Routing: domain.RoutingOrder,
				Stays:   "off",
			},
			"transcribe-free": {
				Name:    "transcribe-free",
				Models:  []string{"group/transcribe-openrouter"},
				Routing: domain.RoutingWeight,
				Stays:   "off",
				Weights: map[string]int{"group/transcribe-openrouter": 1},
			},
			"transcribe-paid": {
				Name:    "transcribe-paid",
				Models:  []string{"group/transcribe-bailian"},
				Routing: domain.RoutingWeight,
				Stays:   "off",
				Weights: map[string]int{"group/transcribe-bailian": 1},
			},
			"transcribe-bailian": {
				Name:    "transcribe-bailian",
				Models:  []string{"bailian/qwen-max"},
				Routing: domain.RoutingOrder,
				Stays:   "off",
			},
			"transcribe": {
				Name:    "transcribe",
				Models:  []string{"group/transcribe-paid"}, // wrong order / missing free
				Routing: domain.RoutingOrder,
				Stays:   "off",
			},
		},
	}

	admitted := []domain.Candidate{
		{ProviderID: "or", Model: "new1", Member: "or/new1", Billing: domain.BillingFree, SupplierSlug: "openrouter"},
		{ProviderID: "or", Model: "new2", Member: "or/new2", Billing: domain.BillingFree, SupplierSlug: "openrouter"},
		{ProviderID: "bailian", Model: "qwen-max", Member: "bailian/qwen-max", Billing: domain.BillingCommercial, SupplierSlug: "bailian"},
	}

	plan := PlanChannelSync(SyncInput{
		Config:   domain.Config{Roles: []string{domain.RoleTranscribe}},
		Snapshot: snap,
		Admitted: admitted,
		Now:      fixedNow,
	})

	// Check add-before-remove for transcribe-openrouter.
	var orCmds []string
	for _, cmd := range plan.Commands {
		if len(cmd.Args) >= 3 && cmd.Args[0] == "group" && cmd.Args[2] == "transcribe-openrouter" {
			orCmds = append(orCmds, strings.Join(cmd.Args, " "))
		}
	}
	wantOrCmds := []string{
		"group set transcribe-openrouter models+=or/new1,or/new2",
		"group set transcribe-openrouter models-=or/old1,or/old2",
	}
	if !slices.Equal(orCmds, wantOrCmds) {
		t.Fatalf("complete replacement commands mismatch:\ngot:  %v\nwant: %v", orCmds, wantOrCmds)
	}

	// Simulate applying commands and confirm NO group is ever empty at any step,
	// and final transcribe order is [group/transcribe-free, group/transcribe-paid].
	finalSnap := applyCommandsForTest(t, snap, plan.Commands)
	assertGroupsMatch(t, finalSnap.Groups, plan.TargetGroups)
	if !slices.Equal(finalSnap.Groups["transcribe"].Models, []string{"group/transcribe-free", "group/transcribe-paid"}) {
		t.Fatalf("unexpected final transcribe order: %v", finalSnap.Groups["transcribe"].Models)
	}
}

func TestPlanTrialGroupLifecycleAndNonEmptyDrain(t *testing.T) {
	cfg := domain.Config{
		Roles: []string{domain.RoleTranscribe},
		BailianTrial: domain.BailianTrialConfig{
			Enabled:          true,
			TemplateProvider: "transcribe-bailian-trial",
			MaxActiveModels:  3,
		},
	}

	// 1. Initial creation + attach to transcribe-free.
	snap1 := domain.RegistrySnapshot{
		Providers: map[string]domain.Provider{
			"transcribe-bailian-trial": {ID: "transcribe-bailian-trial", Off: true, Models: []string{"qwen-plus"}},
		},
		Groups: map[string]domain.Group{
			"transcribe-free": {
				Name:    "transcribe-free",
				Models:  []string{"group/transcribe-openrouter"},
				Routing: domain.RoutingWeight,
				Stays:   "off",
				Weights: map[string]int{"group/transcribe-openrouter": 1},
			},
		},
	}
	pA := domain.TrialProviderID("mA")
	pB := domain.TrialProviderID("mB")
	pC := domain.TrialProviderID("mC")
	mA := pA + "/mA"
	mB := pB + "/mB"
	mC := pC + "/mC"

	plan1 := PlanTrialGroup(TrialPlanInput{
		Config:         cfg,
		Snapshot:       snap1,
		DesiredMembers: []string{mA, mB},
		ProviderCommands: []domain.CLICommand{
			{Args: []string{"provider", "add", pA}, Reason: "create trial"},
			{Args: []string{"provider", "add", pB}, Reason: "create trial"},
		},
		Now: fixedNow,
	})
	snap2 := applyCommandsForTest(t, snap1, plan1.Commands)
	if !slices.Equal(snap2.Groups["transcribe-trial"].Models, []string{mA, mB}) {
		t.Fatalf("unexpected transcribe-trial after create: %v", snap2.Groups["transcribe-trial"].Models)
	}
	if !slices.Contains(snap2.Groups["transcribe-free"].Models, "group/transcribe-trial") {
		t.Fatalf("expected transcribe-trial attached to transcribe-free: %v", snap2.Groups["transcribe-free"].Models)
	}

	// Idempotent rerun with active members.
	rerun1 := PlanTrialGroup(TrialPlanInput{
		Config:         cfg,
		Snapshot:       snap2,
		DesiredMembers: []string{mA, mB},
		Now:            fixedNow,
	})
	if len(rerun1.Commands) != 0 {
		t.Fatalf("expected 0 commands on steady active trial rerun, got: %v", formatCmds(rerun1.Commands))
	}

	// 2. Complete replacement [mA, mB] -> [mC] must add mC before removing mA, mB.
	planReplace := PlanTrialGroup(TrialPlanInput{
		Config:         cfg,
		Snapshot:       snap2,
		DesiredMembers: []string{mC},
		ProviderCommands: []domain.CLICommand{
			{Args: []string{"provider", "add", pC}},
			{Args: []string{"provider", "off", pA}},
			{Args: []string{"provider", "off", pB}},
		},
		Now: fixedNow,
	})
	snap3 := applyCommandsForTest(t, snap2, planReplace.Commands)
	if !slices.Equal(snap3.Groups["transcribe-trial"].Models, []string{mC}) {
		t.Fatalf("expected [mC] after replacement, got: %v", snap3.Groups["transcribe-trial"].Models)
	}

	// 3. Drain to 0 via ProviderCommands turning pC off: must keep mC disabled (never empty!)
	//    and detach group/transcribe-trial from transcribe-free.
	planDrain := PlanTrialGroup(TrialPlanInput{
		Config:           cfg,
		Snapshot:         snap3,
		DesiredMembers:   nil,
		ProviderCommands: []domain.CLICommand{{Args: []string{"provider", "off", pC}}},
		Now:              fixedNow,
	})
	snap4 := applyCommandsForTest(t, snap3, planDrain.Commands)
	if len(snap4.Groups["transcribe-trial"].Models) != 1 || snap4.Groups["transcribe-trial"].Models[0] != mC {
		t.Fatalf("drained transcribe-trial must keep 1 disabled member, got: %v", snap4.Groups["transcribe-trial"].Models)
	}
	if !snap4.Providers[pC].Off {
		t.Fatalf("kept member provider %s must be disabled", pC)
	}
	if slices.Contains(snap4.Groups["transcribe-free"].Models, "group/transcribe-trial") {
		t.Fatalf("expected group/transcribe-trial detached from transcribe-free, got: %v", snap4.Groups["transcribe-free"].Models)
	}

	// Idempotent rerun after drain when pC is already Off.
	rerunDrained := PlanTrialGroup(TrialPlanInput{
		Config:         cfg,
		Snapshot:       snap4,
		DesiredMembers: nil,
		Now:            fixedNow,
	})
	if len(rerunDrained.Commands) != 0 {
		t.Fatalf("expected 0 commands on steady drained trial rerun, got: %v", formatCmds(rerunDrained.Commands))
	}

	// 4. Drain to 0 when active member is still ON and template provider is OFF:
	//    falls back to template_provider/<model>, adding it BEFORE removing active member.
	snapOnWithOffTmpl := domain.RegistrySnapshot{
		Providers: map[string]domain.Provider{
			"transcribe-bailian-trial": {ID: "transcribe-bailian-trial", Off: true, Models: []string{"qwen-plus"}},
			pA:                         {ID: pA, Off: false},
		},
		Groups: map[string]domain.Group{
			"transcribe-trial": {Name: "transcribe-trial", Models: []string{mA}, Routing: domain.RoutingOrder, Stays: "off"},
			"transcribe-free": {
				Name:    "transcribe-free",
				Models:  []string{"group/transcribe-trial"},
				Routing: domain.RoutingWeight,
				Stays:   "off",
				Weights: map[string]int{"group/transcribe-trial": 1},
			},
		},
	}
	planTmplDrain := PlanTrialGroup(TrialPlanInput{
		Config:         cfg,
		Snapshot:       snapOnWithOffTmpl,
		DesiredMembers: nil,
		Now:            fixedNow,
	})
	snapAfterTmpl := applyCommandsForTest(t, snapOnWithOffTmpl, planTmplDrain.Commands)
	if !slices.Equal(snapAfterTmpl.Groups["transcribe-trial"].Models, []string{"transcribe-bailian-trial/qwen-plus"}) {
		t.Fatalf("expected template fallback placeholder, got: %v", snapAfterTmpl.Groups["transcribe-trial"].Models)
	}
	// transcribe-free only had 1 member so it must NOT be emptied.
	if !slices.Equal(snapAfterTmpl.Groups["transcribe-free"].Models, []string{"group/transcribe-trial"}) {
		t.Fatalf("transcribe-free must not be emptied, got: %v", snapAfterTmpl.Groups["transcribe-free"].Models)
	}
	if len(planTmplDrain.Warnings) == 0 {
		t.Fatal("expected warning when transcribe-free cannot detach sole trial member")
	}

	// 5. Drain when no group exists yet emits 0 group commands.
	planNoop := PlanTrialGroup(TrialPlanInput{
		Config:   cfg,
		Snapshot: domain.RegistrySnapshot{Groups: map[string]domain.Group{}},
		Now:      fixedNow,
	})
	if len(planNoop.Commands) != 0 || len(planNoop.TargetGroups) != 0 {
		t.Fatalf("expected empty plan when transcribe-trial absent and desired empty, got: %+v", planNoop)
	}

	// 6. Drain when all members are ON and no off template exists: keeps 1st member,
	//    runs provider off BEFORE removing the remaining member.
	snapAllOn := domain.RegistrySnapshot{
		Providers: map[string]domain.Provider{
			pA: {ID: pA, Off: false},
			pB: {ID: pB, Off: false},
		},
		Groups: map[string]domain.Group{
			"transcribe-trial": {Name: "transcribe-trial", Models: []string{mA, mB}, Routing: domain.RoutingOrder, Stays: "off"},
		},
	}
	planFallbackOff := PlanTrialGroup(TrialPlanInput{
		Config:   cfg,
		Snapshot: snapAllOn,
		Now:      fixedNow,
	})
	gotFallback := formatCmds(planFallbackOff.Commands)
	wantFallback := []string{
		"provider off " + pA,
		"group set transcribe-trial models-=" + mB,
	}
	if !slices.Equal(gotFallback, wantFallback) {
		t.Fatalf("fallback off drain commands mismatch:\ngot:  %v\nwant: %v", gotFallback, wantFallback)
	}
	snapAfterFallback := applyCommandsForTest(t, snapAllOn, planFallbackOff.Commands)
	if !slices.Equal(snapAfterFallback.Groups["transcribe-trial"].Models, []string{mA}) || !snapAfterFallback.Providers[pA].Off {
		t.Fatalf("unexpected state after fallback off drain: %+v", snapAfterFallback)
	}

	// 7. ManualHold on transcribe-free -> group/transcribe-trial prevents attachment.
	planHeld := PlanTrialGroup(TrialPlanInput{
		Config:   cfg,
		Snapshot: snap1,
		State: domain.State{
			ManualHolds: map[string]domain.ManualHold{
				domain.ManualHoldKey("transcribe-free", "group/transcribe-trial"): {
					Group:  "transcribe-free",
					Member: "group/transcribe-trial",
					Reason: "removed from group",
				},
			},
		},
		DesiredMembers: []string{mA},
		Now:            fixedNow,
	})
	for _, cmd := range planHeld.Commands {
		if len(cmd.Args) >= 3 && cmd.Args[0] == "group" && cmd.Args[2] == "transcribe-free" {
			t.Fatalf("must not attach held group/transcribe-trial to transcribe-free: %v", cmd.Args)
		}
	}
}

func TestPlanGroupDiffOrderReorderingAndWeightIdempotency(t *testing.T) {
	// Order group transitions across complex permutations without ever becoming empty.
	cases := []struct {
		name     string
		existing []string
		desired  []string
	}{
		{name: "swap_two", existing: []string{"B", "A"}, desired: []string{"A", "B"}},
		{name: "prepend_new", existing: []string{"A", "B"}, desired: []string{"C", "A", "B"}},
		{name: "move_first_to_last", existing: []string{"C", "A", "B"}, desired: []string{"A", "B", "C"}},
		{name: "full_disjoint", existing: []string{"A", "B"}, desired: []string{"C", "D"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := domain.RegistrySnapshot{
				Groups: map[string]domain.Group{
					"g": {Name: "g", Models: tc.existing, Routing: domain.RoutingOrder, Stays: "off"},
				},
			}
			desired := makeDesiredGroup("g", tc.desired, domain.RoutingOrder)
			cmds := planGroupDiff(&[]domain.Group{snap.Groups["g"]}[0], desired, "test")
			after := applyCommandsForTest(t, snap, cmds)
			if !slices.Equal(after.Groups["g"].Models, tc.desired) {
				t.Fatalf("got %v, want %v (cmds=%v)", after.Groups["g"].Models, tc.desired, formatCmds(cmds))
			}
		})
	}

	// Weight group with identical set in reverse order and matching weights emits 0 commands.
	existingWeight := domain.Group{
		Name:    "transcribe-free",
		Models:  []string{"group/b", "group/a"},
		Routing: domain.RoutingWeight,
		Stays:   "off",
		Weights: map[string]int{"group/a": 1, "group/b": 1},
	}
	desiredWeight := makeDesiredGroup("transcribe-free", []string{"group/a", "group/b"}, domain.RoutingWeight)
	if cmds := planGroupDiff(&existingWeight, desiredWeight, "test"); len(cmds) != 0 {
		t.Fatalf("expected 0 commands for weight-equivalent group, got: %v", formatCmds(cmds))
	}

	// Weight group missing Weights emits weight= update.
	existingNoWeights := domain.Group{
		Name:    "transcribe-free",
		Models:  []string{"group/a", "group/b"},
		Routing: domain.RoutingWeight,
		Stays:   "off",
	}
	cmds := planGroupDiff(&existingNoWeights, desiredWeight, "test")
	if got := formatCmds(cmds); !slices.Equal(got, []string{"group set transcribe-free weight=group/a=1,group/b=1"}) {
		t.Fatalf("unexpected weight update commands: %v", got)
	}
}

func TestPlanChannelSyncExclusionsAndAvailability(t *testing.T) {
	cfg := domain.Config{
		Roles:             []string{domain.RoleTranscribe},
		ExcludedProviders: []string{"excluded-prov"},
	}
	snap := domain.RegistrySnapshot{
		Providers: map[string]domain.Provider{
			"off-prov": {ID: "off-prov", Off: true},
			"ok-prov":  {ID: "ok-prov", Models: []string{"m-ok"}, ModelsOff: []string{"m-off"}},
		},
		Groups: map[string]domain.Group{},
	}
	plan := PlanChannelSync(SyncInput{
		Config:   cfg,
		Snapshot: snap,
		State: domain.State{
			ManualHolds: map[string]domain.ManualHold{
				domain.ManualHoldKey("transcribe-ok-prov", "ok-prov/m-held"): {
					Group: "transcribe-ok-prov", Member: "ok-prov/m-held", Reason: "removed from group",
				},
			},
		},
		Admitted: []domain.Candidate{
			{ProviderID: "excluded-prov", Model: "m1", Member: "excluded-prov/m1", Billing: domain.BillingFree, NeedsExpose: true},
			{ProviderID: "off-prov", Model: "m1", Member: "off-prov/m1", Billing: domain.BillingFree, NeedsExpose: true},
			{ProviderID: "ok-prov", Model: "m-off", Member: "ok-prov/m-off", Billing: domain.BillingFree, NeedsExpose: true},
			{ProviderID: "ok-prov", Model: "m-held", Member: "ok-prov/m-held", Billing: domain.BillingFree, NeedsExpose: true},
			{ProviderID: "ok-prov", Model: "m-ok", Member: "ok-prov/m-ok", Billing: domain.BillingFree},
		},
		Now: fixedNow,
	})

	if !slices.Equal(plan.TargetGroups["transcribe-ok-prov"].Models, []string{"ok-prov/m-ok"}) {
		t.Fatalf("expected only ok-prov/m-ok, got: %v", plan.TargetGroups["transcribe-ok-prov"].Models)
	}
	for _, cmd := range plan.Commands {
		if cmd.Args[0] == "provider" && cmd.Args[1] == "models" {
			t.Fatalf("excluded/off/held candidates must not trigger provider models, got: %v", cmd.Args)
		}
	}
}

func formatCmds(cmds []domain.CLICommand) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = strings.Join(c.Args, " ")
	}
	return out
}

// applyCommandsForTest simulates Magpie CLI execution and fails immediately if
// any intermediate step leaves an existing group empty.
func applyCommandsForTest(t *testing.T, snap domain.RegistrySnapshot, cmds []domain.CLICommand) domain.RegistrySnapshot {
	t.Helper()
	out := domain.RegistrySnapshot{
		MagpieDir: snap.MagpieDir,
		Port:      snap.Port,
		Providers: make(map[string]domain.Provider, len(snap.Providers)),
		Groups:    make(map[string]domain.Group, len(snap.Groups)),
	}
	for k, p := range snap.Providers {
		cp := p
		cp.Models = slices.Clone(p.Models)
		out.Providers[k] = cp
	}
	for k, g := range snap.Groups {
		out.Groups[k] = cloneGroup(g)
	}

	for idx, cmd := range cmds {
		if len(cmd.Args) < 3 {
			t.Fatalf("cmd[%d] too short: %v", idx, cmd.Args)
		}
		switch cmd.Args[0] {
		case "provider":
			pid := cmd.Args[2]
			p := out.Providers[pid]
			p.ID = pid
			switch cmd.Args[1] {
			case "add":
				p.Off = false
			case "on":
				p.Off = false
			case "off":
				p.Off = true
			case "models":
				for _, arg := range cmd.Args[3:] {
					if m, ok := strings.CutPrefix(arg, "+"); ok && !slices.Contains(p.Models, m) {
						p.Models = append(p.Models, m)
					}
				}
			}
			out.Providers[pid] = p
		case "group":
			name := cmd.Args[2]
			switch cmd.Args[1] {
			case "add":
				if _, exists := out.Groups[name]; exists {
					t.Fatalf("cmd[%d] group add on existing group %q", idx, name)
				}
				g := domain.Group{Name: name}
				for _, kv := range cmd.Args[3:] {
					k, v, _ := strings.Cut(kv, "=")
					switch k {
					case "models":
						if v != "" {
							g.Models = strings.Split(v, ",")
						}
					case "routing":
						g.Routing = v
					case "stays":
						g.Stays = v
					}
				}
				if len(g.Models) == 0 {
					t.Fatalf("cmd[%d] created empty group %q", idx, name)
				}
				out.Groups[name] = g
			case "set":
				g, exists := out.Groups[name]
				if !exists {
					t.Fatalf("cmd[%d] group set on missing group %q", idx, name)
				}
				for _, kv := range cmd.Args[3:] {
					switch {
					case strings.HasPrefix(kv, "models+="):
						for _, m := range strings.Split(strings.TrimPrefix(kv, "models+="), ",") {
							if m != "" && !slices.Contains(g.Models, m) {
								g.Models = append(g.Models, m)
							}
						}
					case strings.HasPrefix(kv, "models-="):
						rem := toSet(strings.Split(strings.TrimPrefix(kv, "models-="), ","))
						kept := make([]string, 0, len(g.Models))
						for _, m := range g.Models {
							if !rem[m] {
								kept = append(kept, m)
							}
						}
						g.Models = kept
					case strings.HasPrefix(kv, "routing="):
						g.Routing = strings.TrimPrefix(kv, "routing=")
					case strings.HasPrefix(kv, "stays="):
						g.Stays = strings.TrimPrefix(kv, "stays=")
					case strings.HasPrefix(kv, "weight="):
						wmap := make(map[string]int)
						for _, pair := range strings.Split(strings.TrimPrefix(kv, "weight="), ",") {
							if m, _, ok := strings.Cut(pair, "="); ok && m != "" {
								wmap[m] = 1
							}
						}
						g.Weights = wmap
					}
				}
				if len(g.Models) == 0 {
					t.Fatalf("cmd[%d] (%v) emptied group %q!", idx, cmd.Args, name)
				}
				out.Groups[name] = g
			}
		}
	}
	return out
}

func assertGroupsMatch(t *testing.T, got, want map[string]domain.Group) {
	t.Helper()
	for name, wg := range want {
		gg, ok := got[name]
		if !ok {
			t.Fatalf("missing target group %q in simulated snapshot", name)
		}
		if gg.Routing != wg.Routing || gg.Stays != wg.Stays {
			t.Fatalf("group %q routing/stays mismatch: got (%s,%s), want (%s,%s)",
				name, gg.Routing, gg.Stays, wg.Routing, wg.Stays)
		}
		if wg.Routing == domain.RoutingOrder {
			if !slices.Equal(gg.Models, wg.Models) {
				t.Fatalf("order group %q models mismatch: got %v, want %v", name, gg.Models, wg.Models)
			}
		} else {
			gotSorted := slices.Clone(gg.Models)
			wantSorted := slices.Clone(wg.Models)
			slices.Sort(gotSorted)
			slices.Sort(wantSorted)
			if !slices.Equal(gotSorted, wantSorted) {
				t.Fatalf("group %q models mismatch: got %v, want %v", name, gg.Models, wg.Models)
			}
		}
		if wg.Routing == domain.RoutingWeight && !weightsEqual(gg.Weights, wg.Weights) {
			t.Fatalf("group %q weights mismatch: got %v, want %v", name, gg.Weights, wg.Weights)
		}
	}
}
