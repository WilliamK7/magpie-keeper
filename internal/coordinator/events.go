package coordinator

import (
	"time"

	"magpie-keeper/internal/domain"
)

// SyncOptions controls a manual or lazy ordinary-channel sync run.
type SyncOptions struct {
	Apply   bool
	DryRun  bool
	IfStale time.Duration
}

// GroupStatus summarizes a Magpie routing group for `status`.
type GroupStatus struct {
	Name    string   `json:"name"`
	Routing string   `json:"routing,omitempty"`
	Stays   string   `json:"stays,omitempty"`
	Hidden  bool     `json:"hidden,omitempty"`
	Members []string `json:"members"`
}

// TrialStatus summarizes the Bailian trial pool state for `status`.
type TrialStatus struct {
	Enabled                   bool                        `json:"enabled"`
	Paused                    bool                        `json:"paused"`
	WorkspaceID               string                      `json:"workspace_id,omitempty"`
	LastQuotaSyncAt           time.Time                   `json:"last_quota_sync_at,omitempty"`
	NextQuotaRetryAt          time.Time                   `json:"next_quota_retry_at,omitempty"`
	ConsecutiveErrors         int                         `json:"consecutive_errors,omitempty"`
	LastError                 string                      `json:"last_error,omitempty"`
	AccountCreditBlockedUntil time.Time                   `json:"account_credit_blocked_until,omitempty"`
	ActiveChannels            []domain.TrialChannelRecord `json:"active_channels"`
}

// StatusReport is the read-only snapshot returned by Coordinator.Status.
type StatusReport struct {
	GeneratedAt       time.Time           `json:"generated_at"`
	MagpieDir         string              `json:"magpie_dir"`
	MagpiePort        int                 `json:"magpie_port"`
	Roles             []string            `json:"roles"`
	LastChannelSyncAt time.Time           `json:"last_channel_sync_at,omitempty"`
	DailyBudget       domain.DailyBudget  `json:"daily_budget"`
	MaxEvalPerDay     int                 `json:"max_evaluations_per_day"`
	Groups            []GroupStatus       `json:"groups"`
	ManualHolds       []domain.ManualHold `json:"manual_holds"`
	Trial             TrialStatus         `json:"trial"`
}

// CandidateDecision describes one discovered candidate and its current admission status.
type CandidateDecision struct {
	Candidate domain.Candidate `json:"candidate"`
	Decision  string           `json:"decision"` // "admitted_cached" | "cooldown" | "needs_evaluation"
	Reason    string           `json:"reason,omitempty"`
}

// CandidatesReport is the read-only preview returned by Coordinator.ListCandidates.
type CandidatesReport struct {
	GeneratedAt time.Time           `json:"generated_at"`
	ServedCount int                 `json:"served_count"`
	Candidates  []CandidateDecision `json:"candidates"`
	Warnings    []string            `json:"warnings,omitempty"`
}

// SyncReport summarizes one ordinary-channel sync execution.
type SyncReport struct {
	ExecutedAt   time.Time           `json:"executed_at"`
	Applied      bool                `json:"applied"`
	DryRun       bool                `json:"dry_run"`
	Skipped      bool                `json:"skipped"`
	SkipReason   string              `json:"skip_reason,omitempty"`
	Evaluated    int                 `json:"evaluated"`
	Admitted     []string            `json:"admitted"`
	BackupPath   string              `json:"backup_path,omitempty"`
	Commands     []domain.CLICommand `json:"commands"`
	TargetGroups []GroupStatus       `json:"target_groups,omitempty"`
	Warnings     []string            `json:"warnings,omitempty"`
}

// TrialReport summarizes one Bailian trial pool reconciliation execution.
type TrialReport struct {
	ExecutedAt     time.Time           `json:"executed_at"`
	Skipped        bool                `json:"skipped"`
	SkipReason     string              `json:"skip_reason,omitempty"`
	Evaluated      int                 `json:"evaluated"`
	DesiredMembers []string            `json:"desired_members"`
	SharedRPM      int                 `json:"shared_rpm,omitempty"`
	BackupPath     string              `json:"backup_path,omitempty"`
	Commands       []domain.CLICommand `json:"commands"`
	Warnings       []string            `json:"warnings,omitempty"`
}

// WatchHooks provides optional lifecycle callbacks for diagnostics and tests.
type WatchHooks struct {
	OnChannelSync func(SyncReport, error)
	OnTrialSync   func(TrialReport, error)
	OnRouteAlert  func(domain.RouteAlert, error)
}
