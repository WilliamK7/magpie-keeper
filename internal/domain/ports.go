package domain

import "context"

// MagpieGateway abstracts read, backup, CLI mutation, and model listing
// operations against a portable Magpie installation.
type MagpieGateway interface {
	ReadRegistry(ctx context.Context) (RegistrySnapshot, error)
	BackupRegistry(ctx context.Context) (backupPath string, err error)
	ExecCLI(ctx context.Context, args ...string) error
	ListServedModels(ctx context.Context) ([]string, error) // GET /v1/models
}

// CatalogAdapter discovers candidate models from a configured provider and
// classifies them by billing tier and price policy.
type CatalogAdapter interface {
	Name() string
	Discover(ctx context.Context, provider Provider, rule ProviderRule, served map[string]bool) ([]Candidate, error)
}

// Admitter runs deterministic synthetic evaluation cases against a member.
type Admitter interface {
	Evaluate(ctx context.Context, member string) (AdmissionResult, error)
}

// QuotaProvider queries cloud free-tier quotas and workspace rate limits.
type QuotaProvider interface {
	QueryFreeQuotas(ctx context.Context) ([]QuotaObservation, error)
	QueryModelLimits(ctx context.Context, model string) (RateLimits, error)
}

// RouteTailer emits real-time alerts parsed incrementally from Magpie routing logs.
type RouteTailer interface {
	Start(ctx context.Context) (<-chan RouteAlert, error)
}
