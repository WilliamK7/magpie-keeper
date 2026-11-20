package domain

import "time"

// Route alert kinds emitted by routing/*.jsonl tailers.
const (
	RouteAlertQuotaExhausted   = "quota_exhausted"
	RouteAlertAccountCredit    = "account_credit"
	RouteAlertPermissionDenied = "permission_denied"
)

// Billing tiers.
const (
	BillingFree       = "free"
	BillingCommercial = "commercial"
)

// Supported roles and routing strategies.
const (
	RoleTranscribe = "transcribe"
	RoleReview     = "review"

	RoutingOrder  = "order"
	RoutingWeight = "weight"
	RoutingFast   = "fast"
)

// Admission status outcomes.
const (
	AdmissionPassed    = "passed"
	AdmissionFailed    = "failed"
	AdmissionTransient = "transient"
)

// TransientCooldown is the fixed 5-minute cooldown for 408/429/5xx/timeout failures.
const TransientCooldown = 5 * time.Minute

// RouteAlert represents an actionable failure observed in Magpie routing logs.
type RouteAlert struct {
	ProviderID string    `json:"provider_id"`
	Model      string    `json:"model"`
	Kind       string    `json:"kind"` // "quota_exhausted" | "account_credit" | "permission_denied"
	Error      string    `json:"error,omitempty"`
	Status     int       `json:"status,omitempty"`
	Timestamp  time.Time `json:"timestamp,omitempty"`
}

// Config mirrors data/channels.json.
type Config struct {
	MagpieDir              string                  `json:"magpie_dir"`
	MagpieURL              string                  `json:"magpie_url"`
	MagpieKey              string                  `json:"magpie_key"`
	Roles                  []string                `json:"roles"`
	ChannelSyncInterval    string                  `json:"channel_sync_interval"`
	ExcludedProviders      []string                `json:"excluded_providers"`
	MaxEvaluationsPerCycle int                     `json:"max_evaluations_per_cycle"`
	MaxEvaluationsPerDay   int                     `json:"max_evaluations_per_day"`
	CooldownSeconds        int                     `json:"cooldown_seconds"`
	BailianTrial           BailianTrialConfig      `json:"bailian_trial"`
	Providers              map[string]ProviderRule `json:"providers"`
}

// BailianTrialConfig controls the Bailian free-tier trial sub-channel pool.
type BailianTrialConfig struct {
	Enabled            bool     `json:"enabled"`
	Paused             bool     `json:"paused"`
	WorkspaceID        string   `json:"workspace_id"`
	TemplateProvider   string   `json:"template_provider"`
	CommercialProvider string   `json:"commercial_provider"`
	MaxActiveModels    int      `json:"max_active_models"`
	RefreshInterval    string   `json:"refresh_interval"`
	ExcludedModels     []string `json:"excluded_models"`
	BridgeScript       string   `json:"bridge_script,omitempty"`
	BailianConfigDir   string   `json:"bailian_config_dir,omitempty"`
	LimitsBaseURL      string   `json:"limits_base_url,omitempty"`
	NodeBin            string   `json:"node_bin,omitempty"`
}

// ProviderRule defines discovery, price ceiling, and selection policy for a provider.
type ProviderRule struct {
	Adapter                string         `json:"adapter"`
	Include                []string       `json:"include,omitempty"`
	Exclude                []string       `json:"exclude,omitempty"`
	Preferred              []string       `json:"preferred,omitempty"`
	PriceExceptions        []string       `json:"price_exceptions,omitempty"`
	MaxInputUSDPerMillion  float64        `json:"max_input_usd_per_million,omitempty"`
	MaxOutputUSDPerMillion float64        `json:"max_output_usd_per_million,omitempty"`
	Billing                []string       `json:"billing,omitempty"`
	MaxCandidates          map[string]int `json:"max_candidates,omitempty"`
	SupplierSlug           string         `json:"supplier_slug,omitempty"`
	Roles                  []string       `json:"roles,omitempty"`
	EndpointURL            string         `json:"endpoint_url,omitempty"`
}

// RegistrySnapshot represents the merged state of portable Magpie configuration files.
type RegistrySnapshot struct {
	MagpieDir string              `json:"magpie_dir"`
	Port      int                 `json:"port"`
	Providers map[string]Provider `json:"providers"`
	Groups    map[string]Group    `json:"groups"`
}

// Provider captures a standard or plugin provider from Magpie's registry.
type Provider struct {
	ID        string               `json:"id"`
	Kind      string               `json:"kind,omitempty"`
	ChatURL   string               `json:"chat,omitempty"`
	BaseURL   string               `json:"base,omitempty"`
	APIKey    string               `json:"key,omitempty"`
	Models    []string             `json:"models,omitempty"`
	ModelsOff []string             `json:"modelsOff,omitempty"`
	ModelMeta map[string]ModelMeta `json:"_models,omitempty"`
	Headers   map[string]string    `json:"headers,omitempty"`
	Off       bool                 `json:"off,omitempty"`
	NoModels  bool                 `json:"noModels,omitempty"`
	SignedIn  *bool                `json:"signedIn,omitempty"`
	Free      bool                 `json:"free,omitempty"`
	List      string               `json:"list,omitempty"`
	RPM       int                  `json:"rpm,omitempty"`
	Plugin    bool                 `json:"plugin,omitempty"`
}

// ModelMeta holds per-model metadata stored under _models in Magpie.
type ModelMeta struct {
	Decides          bool     `json:"decides,omitempty"`
	Type             string   `json:"type,omitempty"`
	OutputModalities []string `json:"outputModalities,omitempty"`
	Free             bool     `json:"free,omitempty"`
	InputPrice       *float64 `json:"input_price,omitempty"`
	OutputPrice      *float64 `json:"output_price,omitempty"`
}

// ModelCache represents data/cache/models/<providerID>.json.
type ModelCache struct {
	Base   string             `json:"base"`
	Models []CachedModelEntry `json:"models"`
}

// CachedModelEntry is a single model record inside a Magpie model cache file.
type CachedModelEntry struct {
	ID               string   `json:"id"`
	Type             string   `json:"type,omitempty"`
	OutputModalities []string `json:"outputModalities,omitempty"`
	Decides          bool     `json:"decides,omitempty"`
	Free             bool     `json:"free,omitempty"`
}

// Group represents a Magpie routing group.
type Group struct {
	Name    string         `json:"name"`
	Models  []string       `json:"models,omitempty"`
	Routing string         `json:"routing,omitempty"`
	Stays   string         `json:"stays,omitempty"`
	Off     bool           `json:"off,omitempty"`
	Hidden  bool           `json:"hidden,omitempty"`
	Weights map[string]int `json:"weights,omitempty"`
}

// Candidate is a discovered model eligible for admission or group assignment.
type Candidate struct {
	ProviderID          string   `json:"provider_id"`
	Model               string   `json:"model"`
	Member              string   `json:"member"`
	Billing             string   `json:"billing"` // "free" | "commercial"
	NeedsExpose         bool     `json:"needs_expose,omitempty"`
	Preferred           bool     `json:"preferred,omitempty"`
	PriceException      bool     `json:"price_exception,omitempty"`
	InputUSDPerMillion  float64  `json:"input_usd_per_million,omitempty"`
	OutputUSDPerMillion float64  `json:"output_usd_per_million,omitempty"`
	AllowedRoles        []string `json:"allowed_roles,omitempty"`
	SupplierSlug        string   `json:"supplier_slug,omitempty"`
	EndpointKeyHash     string   `json:"endpoint_key_hash,omitempty"`
}

// AdmissionCaseResult records the outcome of one deterministic synthetic case.
type AdmissionCaseResult struct {
	CaseID          string `json:"case_id"`
	Passed          bool   `json:"passed"`
	Reason          string `json:"reason,omitempty"`
	FinishReason    string `json:"finish_reason,omitempty"`
	ReasoningTokens int    `json:"reasoning_tokens,omitempty"`
	DurationMS      int64  `json:"duration_ms,omitempty"`
}

// AdmissionResult captures the overall synthetic evaluation result for a member.
type AdmissionResult struct {
	Member          string                `json:"member"`
	Status          string                `json:"status"` // "passed" | "failed" | "transient"
	SuiteVersion    string                `json:"suite_version"`
	EndpointKeyHash string                `json:"endpoint_key_hash"`
	Reason          string                `json:"reason,omitempty"`
	DurationMS      int64                 `json:"duration_ms,omitempty"`
	ReasoningTokens int                   `json:"reasoning_tokens,omitempty"`
	Cases           []AdmissionCaseResult `json:"cases,omitempty"`
	EvaluatedAt     time.Time             `json:"evaluated_at"`
}

// QuotaObservation represents one free-tier quota record returned by Bailian.
type QuotaObservation struct {
	Model            string   `json:"model"`
	Type             string   `json:"type"`
	Remaining        float64  `json:"remaining"`
	Total            float64  `json:"total"`
	UsagePercent     *float64 `json:"usage_percent,omitempty"`
	RemainingPercent *float64 `json:"remaining_percent"`
	Expires          string   `json:"expires"`
	AutoStop         any      `json:"auto_stop,omitempty"`
}

// RateLimits captures model and workspace RPM/TPM quotas.
type RateLimits struct {
	Model        string `json:"model"`
	AccountRPM   int    `json:"account_rpm"`
	AccountTPM   int    `json:"account_tpm"`
	WorkspaceRPM int    `json:"workspace_rpm"`
	WorkspaceTPM int    `json:"workspace_tpm"`
}

// TrialChannelRecord tracks one derived Bailian trial sub-channel.
type TrialChannelRecord struct {
	ProviderID   string    `json:"provider_id"`
	Model        string    `json:"model"`
	Member       string    `json:"member"`
	Active       bool      `json:"active"`
	RPM          int       `json:"rpm"`
	Expires      string    `json:"expires,omitempty"`
	Remaining    float64   `json:"remaining,omitempty"`
	LastSyncedAt time.Time `json:"last_synced_at,omitempty"`
}

// TrialPoolState persists Bailian trial pool metadata and backoff status.
type TrialPoolState struct {
	Channels                  map[string]TrialChannelRecord `json:"channels"`
	LastQuotaSyncAt           time.Time                     `json:"last_quota_sync_at,omitempty"`
	NextQuotaRetryAt          time.Time                     `json:"next_quota_retry_at,omitempty"`
	ConsecutiveErrors         int                           `json:"consecutive_errors,omitempty"`
	LastError                 string                        `json:"last_error,omitempty"`
	AccountCreditBlockedUntil time.Time                     `json:"account_credit_blocked_until,omitempty"`
}

// CLICommand represents a single magpie.com CLI invocation planned by planner/bailian.
type CLICommand struct {
	Args   []string `json:"args"`
	Reason string   `json:"reason,omitempty"`
}

// GroupPlan is the pure-function diff plan for updating Magpie groups.
type GroupPlan struct {
	Commands     []CLICommand     `json:"commands"`
	TargetGroups map[string]Group `json:"target_groups"`
	Warnings     []string         `json:"warnings,omitempty"`
}

// ManualHold records a user-initiated removal, disablement, or group hiding.
type ManualHold struct {
	Group      string    `json:"group"`
	Member     string    `json:"member"`
	Reason     string    `json:"reason"`
	DetectedAt time.Time `json:"detected_at"`
}

// DailyBudget tracks synthetic evaluation spending for a Beijing calendar day.
type DailyBudget struct {
	Date string `json:"date"`
	Used int    `json:"used"`
}

// CooldownEntry records when a member or provider may next be evaluated or activated.
type CooldownEntry struct {
	Member    string    `json:"member"`
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason,omitempty"`
	Transient bool      `json:"transient,omitempty"`
}

// State is the single persisted state structure stored in data/state.json.
type State struct {
	Version           int                        `json:"version"`
	LastChannelSyncAt time.Time                  `json:"last_channel_sync_at,omitempty"`
	DailyBudget       DailyBudget                `json:"daily_budget"`
	Admissions        map[string]AdmissionResult `json:"admissions"`
	Cooldowns         map[string]CooldownEntry   `json:"cooldowns"`
	ManagedGroups     map[string][]string        `json:"managed_groups"`
	ManualHolds       map[string]ManualHold      `json:"manual_holds"`
	TrialPool         TrialPoolState             `json:"trial_pool"`
}
