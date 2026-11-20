package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"magpie-keeper/internal/domain"
)

// Store manages atomic persistence and budget accounting for data/state.json.
type Store struct {
	path string
	now  func() time.Time
}

// NewStore creates a Store backed by path using wall-clock time.
func NewStore(path string) *Store {
	return NewStoreWithClock(path, time.Now)
}

// NewStoreWithClock creates a Store with an injectable clock for tests.
func NewStoreWithClock(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, now: now}
}

// Path returns the backing state.json file path.
func (s *Store) Path() string {
	return s.path
}

// Load reads state.json or returns a freshly initialized State when absent.
func (s *Store) Load() (domain.State, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			st := domain.State{Version: 1}
			normalizeState(&st)
			return st, nil
		}
		return domain.State{}, fmt.Errorf("read state %q: %w", s.path, err)
	}
	var st domain.State
	if err := json.Unmarshal(raw, &st); err != nil {
		return domain.State{}, fmt.Errorf("parse state %q: %w", s.path, err)
	}
	normalizeState(&st)
	return st, nil
}

// Save atomically writes st to s.path using a same-directory temp file and rename.
func (s *Store) Save(st domain.State) error {
	normalizeState(&st)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename %q -> %q: %w", tmpName, s.path, err)
	}
	cleanup = false
	return nil
}

// ReserveDailyBudget checks and pre-deducts one evaluation slot for the current
// Beijing calendar day, immediately persisting state.json before returning true.
func (s *Store) ReserveDailyBudget(st *domain.State, maxPerDay int) (bool, error) {
	if st == nil {
		return false, fmt.Errorf("state is nil")
	}
	normalizeState(st)
	today := domain.BeijingDate(s.now())
	dateChanged := st.DailyBudget.Date != today
	if dateChanged {
		st.DailyBudget = domain.DailyBudget{Date: today, Used: 0}
	}
	if maxPerDay <= 0 || st.DailyBudget.Used >= maxPerDay {
		if dateChanged {
			if err := s.Save(*st); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	st.DailyBudget.Used++
	if err := s.Save(*st); err != nil {
		st.DailyBudget.Used--
		return false, err
	}
	return true, nil
}

// RemainingDailyBudget returns how many evaluations remain for today's Beijing date.
func (s *Store) RemainingDailyBudget(st domain.State, maxPerDay int) int {
	if maxPerDay <= 0 {
		return 0
	}
	today := domain.BeijingDate(s.now())
	if st.DailyBudget.Date != today {
		return maxPerDay
	}
	rem := maxPerDay - st.DailyBudget.Used
	if rem < 0 {
		return 0
	}
	return rem
}

// IsInCooldown reports whether key currently has an active cooldown entry.
func (s *Store) IsInCooldown(st domain.State, key string) (domain.CooldownEntry, bool) {
	if len(st.Cooldowns) == 0 {
		return domain.CooldownEntry{}, false
	}
	entry, ok := st.Cooldowns[key]
	if !ok {
		return domain.CooldownEntry{}, false
	}
	if !s.now().Before(entry.Until) {
		return domain.CooldownEntry{}, false
	}
	return entry, true
}

// SetCooldown records or refreshes a cooldown for key.
func (s *Store) SetCooldown(st *domain.State, key string, dur time.Duration, reason string, transient bool) {
	normalizeState(st)
	st.Cooldowns[key] = domain.CooldownEntry{
		Member:    key,
		Until:     s.now().Add(dur),
		Reason:    reason,
		Transient: transient,
	}
}

// ClearCooldown removes a cooldown entry if present.
func (s *Store) ClearCooldown(st *domain.State, key string) {
	if st.Cooldowns != nil {
		delete(st.Cooldowns, key)
	}
}

// PruneExpiredCooldowns removes all cooldown entries whose deadline has passed.
func (s *Store) PruneExpiredCooldowns(st *domain.State) {
	if st.Cooldowns == nil {
		return
	}
	now := s.now()
	for k, entry := range st.Cooldowns {
		if !now.Before(entry.Until) {
			delete(st.Cooldowns, k)
		}
	}
}

// GetCachedAdmission returns a cached passing admission if endpointKeyHash and
// suiteVersion still match.
func GetCachedAdmission(st domain.State, endpointKeyHash, model, suiteVersion string) (domain.AdmissionResult, bool) {
	if len(st.Admissions) == 0 {
		return domain.AdmissionResult{}, false
	}
	key := domain.AdmissionCacheKey(endpointKeyHash, model, suiteVersion)
	res, ok := st.Admissions[key]
	if !ok {
		return domain.AdmissionResult{}, false
	}
	if res.Status != domain.AdmissionPassed || res.EndpointKeyHash != endpointKeyHash || res.SuiteVersion != suiteVersion {
		return domain.AdmissionResult{}, false
	}
	return res, true
}

// RecordAdmission caches passed results or clears stale cache entries on hard failures.
func RecordAdmission(st *domain.State, res domain.AdmissionResult) {
	normalizeState(st)
	key := domain.AdmissionCacheKey(res.EndpointKeyHash, res.Member, res.SuiteVersion)
	if res.Status == domain.AdmissionPassed {
		st.Admissions[key] = res
		return
	}
	if res.Status == domain.AdmissionFailed {
		delete(st.Admissions, key)
	}
}

// RecordAdmissionByModel caches passed results using model name in the cache key per SPEC §6.2.
func RecordAdmissionByModel(st *domain.State, model string, res domain.AdmissionResult) {
	normalizeState(st)
	key := domain.AdmissionCacheKey(res.EndpointKeyHash, model, res.SuiteVersion)
	if res.Status == domain.AdmissionPassed {
		st.Admissions[key] = res
		return
	}
	if res.Status == domain.AdmissionFailed {
		delete(st.Admissions, key)
	}
}

// IsManualHold reports whether group/member is under a manual hold.
func IsManualHold(st domain.State, group, member string) bool {
	if len(st.ManualHolds) == 0 {
		return false
	}
	_, ok := st.ManualHolds[domain.ManualHoldKey(group, member)]
	return ok
}

// AddManualHold stores a manual hold record on st.
func AddManualHold(st *domain.State, hold domain.ManualHold) {
	normalizeState(st)
	st.ManualHolds[domain.ManualHoldKey(hold.Group, hold.Member)] = hold
}

func normalizeState(st *domain.State) {
	if st.Version == 0 {
		st.Version = 1
	}
	if st.Admissions == nil {
		st.Admissions = map[string]domain.AdmissionResult{}
	}
	if st.Cooldowns == nil {
		st.Cooldowns = map[string]domain.CooldownEntry{}
	}
	if st.ManagedGroups == nil {
		st.ManagedGroups = map[string][]string{}
	}
	if st.ManualHolds == nil {
		st.ManualHolds = map[string]domain.ManualHold{}
	}
	if st.TrialPool.Channels == nil {
		st.TrialPool.Channels = map[string]domain.TrialChannelRecord{}
	}
}
