package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"magpie-keeper/internal/admit"
	"magpie-keeper/internal/catalog"
	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/state"
)

type fakeGateway struct {
	mu          sync.Mutex
	snapshot    domain.RegistrySnapshot
	served      []string
	ops         []string
	backupCount int
	cliCalls    [][]string
}

func (g *fakeGateway) ReadRegistry(context.Context) (domain.RegistrySnapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	snap := domain.RegistrySnapshot{
		MagpieDir: g.snapshot.MagpieDir,
		Port:      g.snapshot.Port,
		Providers: make(map[string]domain.Provider, len(g.snapshot.Providers)),
		Groups:    make(map[string]domain.Group, len(g.snapshot.Groups)),
	}
	for k, p := range g.snapshot.Providers {
		p.Models = slices.Clone(p.Models)
		p.ModelsOff = slices.Clone(p.ModelsOff)
		snap.Providers[k] = p
	}
	for k, grp := range g.snapshot.Groups {
		grp.Models = slices.Clone(grp.Models)
		snap.Groups[k] = grp
	}
	return snap, nil
}

func (g *fakeGateway) BackupRegistry(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.backupCount++
	g.ops = append(g.ops, "backup")
	return filepath.Join(g.snapshot.MagpieDir, "data", "backups", "b1", "providers.json"), nil
}

func (g *fakeGateway) ExecCLI(_ context.Context, args ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ops = append(g.ops, "cli:"+strings.Join(args, " "))
	g.cliCalls = append(g.cliCalls, slices.Clone(args))
	if len(args) < 2 {
		return nil
	}
	switch args[0] {
	case "provider":
		switch args[1] {
		case "add":
			id := args[2]
			p := domain.Provider{ID: id, Headers: map[string]string{}}
			for _, kv := range args[3:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				switch k {
				case "chat":
					p.ChatURL = v
				case "key":
					p.APIKey = v
				case "models":
					p.Models = strings.Split(v, ",")
				case "free":
					p.Free = v == "yes"
				default:
					if strings.HasPrefix(k, "header.") {
						p.Headers[strings.TrimPrefix(k, "header.")] = v
					}
				}
			}
			g.snapshot.Providers[id] = p
		case "on":
			if p, ok := g.snapshot.Providers[args[2]]; ok {
				p.Off = false
				g.snapshot.Providers[args[2]] = p
			}
		case "off":
			if p, ok := g.snapshot.Providers[args[2]]; ok {
				p.Off = true
				g.snapshot.Providers[args[2]] = p
			}
		case "rpm":
			if p, ok := g.snapshot.Providers[args[2]]; ok {
				// leave RPM update visible in snapshot if needed
				_ = p
			}
		case "models":
			id := args[2]
			if p, ok := g.snapshot.Providers[id]; ok {
				for _, m := range args[3:] {
					if strings.HasPrefix(m, "+") {
						model := strings.TrimPrefix(m, "+")
						if !slices.Contains(p.Models, model) {
							p.Models = append(p.Models, model)
						}
						g.served = append(g.served, id+"/"+model)
					}
				}
				g.snapshot.Providers[id] = p
			}
		}
	case "group":
		verb, name := args[1], args[2]
		grp := g.snapshot.Groups[name]
		grp.Name = name
		if verb == "add" || verb == "set" {
			for _, kv := range args[3:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				switch k {
				case "models":
					grp.Models = nonEmptyCSV(v)
				case "models+":
					for _, m := range nonEmptyCSV(v) {
						if !slices.Contains(grp.Models, m) {
							grp.Models = append(grp.Models, m)
						}
					}
				case "models-":
					remove := nonEmptyCSV(v)
					var next []string
					for _, m := range grp.Models {
						if !slices.Contains(remove, m) {
							next = append(next, m)
						}
					}
					grp.Models = next
				case "routing":
					grp.Routing = v
				case "stays":
					grp.Stays = v
				}
			}
			if len(grp.Models) == 0 {
				return errors.New("magpie stub: group cannot be emptied")
			}
			g.snapshot.Groups[name] = grp
		}
	}
	return nil
}

func (g *fakeGateway) ListServedModels(context.Context) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.served), nil
}

func nonEmptyCSV(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

type fakeAdmitter struct {
	mu    sync.Mutex
	calls []string
	fn    func(member string) (domain.AdmissionResult, error)
}

func (a *fakeAdmitter) Evaluate(_ context.Context, member string) (domain.AdmissionResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, member)
	fn := a.fn
	a.mu.Unlock()
	if fn != nil {
		return fn(member)
	}
	return domain.AdmissionResult{
		Member:       member,
		Status:       domain.AdmissionPassed,
		SuiteVersion: admit.SuiteVersion,
	}, nil
}

type fakeQuota struct {
	mu     sync.Mutex
	quotas []domain.QuotaObservation
	limits map[string]domain.RateLimits
	err    error
}

func (q *fakeQuota) QueryFreeQuotas(context.Context) ([]domain.QuotaObservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return nil, q.err
	}
	return slices.Clone(q.quotas), nil
}

func (q *fakeQuota) QueryModelLimits(_ context.Context, model string) (domain.RateLimits, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if lim, ok := q.limits[model]; ok {
		return lim, nil
	}
	return domain.RateLimits{Model: model, WorkspaceRPM: 120, WorkspaceTPM: 36864 * 100}, nil
}

type fakeTailer struct {
	ch chan domain.RouteAlert
}

func (t *fakeTailer) Start(ctx context.Context) (<-chan domain.RouteAlert, error) {
	go func() {
		<-ctx.Done()
		close(t.ch)
	}()
	return t.ch, nil
}

func baseConfig() domain.Config {
	return domain.Config{
		MagpieDir:              `D:\Program Files\magpie`,
		MagpieURL:              "http://127.0.0.1:3425/v1",
		MagpieKey:              "k",
		Roles:                  []string{domain.RoleTranscribe},
		ChannelSyncInterval:    "6h",
		MaxEvaluationsPerCycle: 4,
		MaxEvaluationsPerDay:   10,
		CooldownSeconds:        3600,
		BailianTrial: domain.BailianTrialConfig{
			Enabled:            true,
			WorkspaceID:        "ws-1",
			TemplateProvider:   "transcribe-bailian-trial",
			CommercialProvider: "transcribe-bailian",
			MaxActiveModels:    3,
			RefreshInterval:    "60s",
		},
		Providers: map[string]domain.ProviderRule{
			"transcribe-bailian": {
				Adapter: "registry",
				Billing: []string{domain.BillingCommercial},
			},
		},
	}
}

func TestSyncLazySkipPreBudgetAndBackupFirst(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	store := state.NewStoreWithClock(filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })

	st, _ := store.Load()
	st.LastChannelSyncAt = now.Add(-1 * time.Hour)
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	gw := &fakeGateway{
		snapshot: domain.RegistrySnapshot{
			MagpieDir: `D:\Program Files\magpie`,
			Port:      3425,
			Providers: map[string]domain.Provider{
				"transcribe-bailian": {
					ID:      "transcribe-bailian",
					ChatURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
					APIKey:  "sk-comm",
					Models:  []string{"qwen3.8-max"},
				},
			},
			Groups: map[string]domain.Group{},
		},
		served: []string{"transcribe-bailian/qwen3.8-max"},
	}

	var usedAtEval int
	adm := &fakeAdmitter{fn: func(member string) (domain.AdmissionResult, error) {
		diskSt, err := store.Load()
		if err != nil {
			t.Fatalf("load state inside Evaluate failed: %v", err)
		}
		usedAtEval = diskSt.DailyBudget.Used
		return domain.AdmissionResult{
			Member:       member,
			Status:       domain.AdmissionPassed,
			SuiteVersion: admit.SuiteVersion,
		}, nil
	}}

	coord, err := New(Dependencies{
		Config:   baseConfig(),
		Store:    store,
		Gateway:  gw,
		Catalog:  catalog.NewDefaultRegistry(nil),
		Admitter: adm,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Fresh state -> --if-stale 6h skips evaluation and Magpie calls.
	rep, err := coord.Sync(context.Background(), SyncOptions{Apply: true, IfStale: 6 * time.Hour})
	if err != nil || !rep.Skipped {
		t.Fatalf("expected lazy skip, got rep=%+v err=%v", rep, err)
	}
	if len(adm.calls) != 0 || gw.backupCount != 0 {
		t.Fatalf("expected zero evals and backups on skip, calls=%v backups=%d", adm.calls, gw.backupCount)
	}

	// 2. Advance 7h -> runs eval, verifies budget pre-deducted BEFORE Evaluate, backup BEFORE CLI.
	now = now.Add(7 * time.Hour)
	rep, err = coord.Sync(context.Background(), SyncOptions{Apply: true, IfStale: 6 * time.Hour})
	if err != nil || rep.Skipped {
		t.Fatalf("expected stale sync to run, got rep=%+v err=%v", rep, err)
	}
	if usedAtEval != 1 {
		t.Fatalf("expected DailyBudget.Used == 1 on disk before Evaluate returned, got %d", usedAtEval)
	}
	if len(gw.ops) < 2 || gw.ops[0] != "backup" {
		t.Fatalf("expected backup before CLI writes, got ops=%v", gw.ops)
	}
}

func TestWatchRouteAlertsAndShutdown(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	store := state.NewStoreWithClock(filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	st, _ := store.Load()
	st.LastChannelSyncAt = now.Add(-10 * time.Minute)
	_ = store.Save(st)

	pct := 100.0
	gw := &fakeGateway{
		snapshot: domain.RegistrySnapshot{
			MagpieDir: `D:\Program Files\magpie`,
			Port:      3425,
			Providers: map[string]domain.Provider{
				"transcribe-bailian-trial": {
					ID:      "transcribe-bailian-trial",
					ChatURL: "https://ws-1.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
					APIKey:  "sk-trial",
					Off:     true,
				},
				"transcribe-bailian": {
					ID:      "transcribe-bailian",
					ChatURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
					APIKey:  "sk-comm",
					Models:  []string{"qwen3.8-max"},
				},
			},
			Groups: map[string]domain.Group{
				"transcribe-free": {
					Name:    "transcribe-free",
					Models:  []string{"group/transcribe-openrouter-free"},
					Routing: domain.RoutingWeight,
				},
			},
		},
	}

	quota := &fakeQuota{
		quotas: []domain.QuotaObservation{
			{Model: "m1", Type: "Text", Remaining: 100, Total: 100, RemainingPercent: &pct, Expires: "2026-12-01"},
			{Model: "m2", Type: "Text", Remaining: 100, Total: 100, RemainingPercent: &pct, Expires: "2026-12-02"},
			{Model: "m3", Type: "Text", Remaining: 100, Total: 100, RemainingPercent: &pct, Expires: "2026-12-03"},
			{Model: "m4", Type: "Text", Remaining: 100, Total: 100, RemainingPercent: &pct, Expires: "2026-12-04"},
		},
	}
	tailer := &fakeTailer{ch: make(chan domain.RouteAlert, 8)}

	channelSkipped := make(chan bool, 1)
	trialDone := make(chan TrialReport, 4)
	alertDone := make(chan domain.RouteAlert, 4)

	cfg := baseConfig()
	cfg.BailianTrial.RefreshInterval = "10s"

	coord, err := New(Dependencies{
		Config:   cfg,
		Store:    store,
		Gateway:  gw,
		Catalog:  catalog.NewDefaultRegistry(nil),
		Admitter: &fakeAdmitter{},
		Quota:    quota,
		Tailer:   tailer,
		Now:      func() time.Time { return now },
		Hooks: WatchHooks{
			OnChannelSync: func(r SyncReport, _ error) {
				select {
				case channelSkipped <- r.Skipped:
				default:
				}
			},
			OnTrialSync: func(r TrialReport, _ error) {
				trialDone <- r
			},
			OnRouteAlert: func(a domain.RouteAlert, _ error) {
				alertDone <- a
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- coord.Watch(ctx)
	}()

	if skipped := <-channelSkipped; !skipped {
		t.Fatal("expected initial channel sync to be skipped by lazy refresh")
	}
	initTrial := <-trialDone
	if len(initTrial.DesiredMembers) != 3 {
		t.Fatalf("expected 3 initial active trial members, got %v", initTrial.DesiredMembers)
	}

	// Simulate quota_exhausted for m1 -> must immediately disable m1 and promote m4!
	id1 := domain.TrialProviderID("m1")
	tailer.ch <- domain.RouteAlert{ProviderID: id1, Model: "m1", Kind: domain.RouteAlertQuotaExhausted}
	<-alertDone

	snapAfterQuota, _ := gw.ReadRegistry(context.Background())
	if !snapAfterQuota.Providers[id1].Off {
		t.Fatalf("expected %s to be turned off after quota_exhausted", id1)
	}
	trialMembers := snapAfterQuota.Groups["transcribe-trial"].Models
	if slices.Contains(trialMembers, domain.TrialMember("m1")) {
		t.Fatalf("expected m1 removed from transcribe-trial, got %v", trialMembers)
	}
	if !slices.Contains(trialMembers, domain.TrialMember("m4")) {
		t.Fatalf("expected m4 promoted into transcribe-trial, got %v", trialMembers)
	}

	// Simulate account_credit -> all trial sub-channels off, group stays non-empty!
	tailer.ch <- domain.RouteAlert{ProviderID: domain.TrialProviderID("m2"), Model: "m2", Kind: domain.RouteAlertAccountCredit}
	<-alertDone

	snapAfterCredit, _ := gw.ReadRegistry(context.Background())
	for _, m := range []string{"m1", "m2", "m3", "m4"} {
		if p, ok := snapAfterCredit.Providers[domain.TrialProviderID(m)]; ok && !p.Off {
			t.Fatalf("expected %s off after account_credit", m)
		}
	}
	if len(snapAfterCredit.Groups["transcribe-trial"].Models) == 0 {
		t.Fatal("group/transcribe-trial must never be emptied")
	}

	cancel()
	if err := <-watchErr; err != nil {
		t.Fatalf("Watch exited with error: %v", err)
	}
}
