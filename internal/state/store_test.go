package state

import (
	"path/filepath"
	"testing"
	"time"

	"magpie-keeper/internal/domain"
)

func TestLoadInitAndAtomicSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data dir", "state.json")
	store := NewStore(path)

	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load on missing file failed: %v", err)
	}
	if st.Version != 1 || st.Admissions == nil || st.Cooldowns == nil || st.ManagedGroups == nil || st.ManualHolds == nil || st.TrialPool.Channels == nil {
		t.Fatalf("expected initialized state maps, got %+v", st)
	}

	AddManualHold(&st, domain.ManualHold{
		Group:      "transcribe-free",
		Member:     "openrouter/foo:free",
		Reason:     "removed from group",
		DetectedAt: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	})
	st.ManagedGroups["transcribe-free"] = []string{"group/transcribe-openrouter-free"}
	if err := store.Save(st); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if !IsManualHold(reloaded, "transcribe-free", "openrouter/foo:free") {
		t.Fatal("manual hold did not persist")
	}
	if len(reloaded.ManagedGroups["transcribe-free"]) != 1 {
		t.Fatalf("managed groups did not persist: %+v", reloaded.ManagedGroups)
	}
}

func TestReserveDailyBudgetPreDeductAndRollover(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC) // 23:00 Asia/Shanghai
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStoreWithClock(path, func() time.Time { return now })

	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	ok, err := store.ReserveDailyBudget(&st, 2)
	if err != nil || !ok {
		t.Fatalf("first reservation failed: ok=%v err=%v", ok, err)
	}
	// Verify pre-deduction was persisted immediately on disk.
	diskSt, err := store.Load()
	if err != nil {
		t.Fatalf("disk Load failed: %v", err)
	}
	if diskSt.DailyBudget.Date != "2026-11-20" || diskSt.DailyBudget.Used != 1 {
		t.Fatalf("unexpected persisted daily budget: %+v", diskSt.DailyBudget)
	}

	ok, err = store.ReserveDailyBudget(&st, 2)
	if err != nil || !ok {
		t.Fatalf("second reservation failed: ok=%v err=%v", ok, err)
	}
	ok, err = store.ReserveDailyBudget(&st, 2)
	if err != nil || ok {
		t.Fatalf("expected cap at 2, got ok=%v err=%v", ok, err)
	}
	if store.RemainingDailyBudget(st, 2) != 0 {
		t.Fatalf("expected 0 remaining")
	}

	// Advance 2 hours -> 2026-11-21 01:00 Asia/Shanghai.
	now = now.Add(2 * time.Hour)
	if store.RemainingDailyBudget(st, 2) != 2 {
		t.Fatalf("expected rollover remaining 2, got %d", store.RemainingDailyBudget(st, 2))
	}
	ok, err = store.ReserveDailyBudget(&st, 2)
	if err != nil || !ok {
		t.Fatalf("post-rollover reservation failed: ok=%v err=%v", ok, err)
	}
	if st.DailyBudget.Date != "2026-11-21" || st.DailyBudget.Used != 1 {
		t.Fatalf("unexpected rollover budget: %+v", st.DailyBudget)
	}
}

func TestCooldownAndAdmissionCache(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	store := NewStoreWithClock(filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	st, _ := store.Load()

	store.SetCooldown(&st, "p/m", 5*time.Minute, "429", true)
	if _, active := store.IsInCooldown(st, "p/m"); !active {
		t.Fatal("expected cooldown active")
	}
	now = now.Add(6 * time.Minute)
	if _, active := store.IsInCooldown(st, "p/m"); active {
		t.Fatal("expected cooldown expired")
	}
	store.PruneExpiredCooldowns(&st)
	if len(st.Cooldowns) != 0 {
		t.Fatalf("expected pruned cooldowns, got %+v", st.Cooldowns)
	}

	res := domain.AdmissionResult{
		Member:          "p/m",
		Status:          domain.AdmissionPassed,
		SuiteVersion:    "v1",
		EndpointKeyHash: "hash1",
		EvaluatedAt:     now,
	}
	RecordAdmissionByModel(&st, "m", res)
	if _, ok := GetCachedAdmission(st, "hash1", "m", "v1"); !ok {
		t.Fatal("expected cache hit")
	}
	if _, ok := GetCachedAdmission(st, "hash2", "m", "v1"); ok {
		t.Fatal("expected cache miss on changed endpoint hash")
	}
	if _, ok := GetCachedAdmission(st, "hash1", "m", "v2"); ok {
		t.Fatal("expected cache miss on changed suite version")
	}
}
