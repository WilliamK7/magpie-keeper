package coordinator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"magpie-keeper/internal/admit"
	"magpie-keeper/internal/catalog"
	"magpie-keeper/internal/config"
	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/state"
)

// Dependencies wires all domain ports and collaborators into Coordinator.
type Dependencies struct {
	Config         domain.Config
	Store          *state.Store
	Gateway        domain.MagpieGateway
	Catalog        *catalog.Registry
	Admitter       domain.Admitter
	Quota          domain.QuotaProvider
	Tailer         domain.RouteTailer
	Logger         *slog.Logger
	Now            func() time.Time
	FallbackAPIKey string
	Hooks          WatchHooks
}

// Coordinator is the Single-Writer Actor owning all mutations to magpie.com and state.json.
type Coordinator struct {
	cfg            domain.Config
	store          *state.Store
	gateway        domain.MagpieGateway
	catalogReg     *catalog.Registry
	admitter       domain.Admitter
	quota          domain.QuotaProvider
	tailer         domain.RouteTailer
	logger         *slog.Logger
	now            func() time.Time
	fallbackAPIKey string
	hooks          WatchHooks

	mu         sync.Mutex // serializes direct CLI entry points if invoked concurrently
	lastQuotas []domain.QuotaObservation
}

// New constructs a Single-Writer Coordinator from Dependencies.
func New(deps Dependencies) (*Coordinator, error) {
	if deps.Store == nil {
		return nil, errors.New("coordinator: state store is required")
	}
	if deps.Gateway == nil {
		return nil, errors.New("coordinator: magpie gateway is required")
	}
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	catReg := deps.Catalog
	if catReg == nil {
		catReg = catalog.NewDefaultRegistry(nil)
	}
	adm := deps.Admitter
	if adm == nil {
		adm = admit.NewEvaluator(deps.Config.MagpieURL, deps.Config.MagpieKey, nil)
	}
	return &Coordinator{
		cfg:            deps.Config,
		store:          deps.Store,
		gateway:        deps.Gateway,
		catalogReg:     catReg,
		admitter:       adm,
		quota:          deps.Quota,
		tailer:         deps.Tailer,
		logger:         logger,
		now:            nowFn,
		fallbackAPIKey: deps.FallbackAPIKey,
		hooks:          deps.Hooks,
	}, nil
}

// Status returns a read-only summary of groups, sync timestamps, budget, and trial channels.
func (c *Coordinator) Status(ctx context.Context) (StatusReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handleStatus(ctx)
}

// ListCandidates returns a read-only preview of discovered candidates and cache/cooldown status.
func (c *Coordinator) ListCandidates(ctx context.Context) (CandidatesReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handleListCandidates(ctx)
}

// Sync runs one ordinary-channel discovery, admission, planning, and optional Magpie apply cycle.
func (c *Coordinator) Sync(ctx context.Context, opts SyncOptions) (SyncReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handleChannelSync(ctx, opts)
}

// SyncTrial runs one Bailian free-tier trial reconciliation cycle.
func (c *Coordinator) SyncTrial(ctx context.Context) (TrialReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handleTrialSync(ctx, true)
}

// Watch runs the Single-Writer event loop: lazy ordinary-channel refresh,
// immediate + periodic Bailian trial sync, and incremental routing log monitoring.
func (c *Coordinator) Watch(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	syncInterval, err := config.ParseDuration(c.cfg.ChannelSyncInterval, 6*time.Hour)
	if err != nil {
		return fmt.Errorf("parse channel_sync_interval: %w", err)
	}
	trialInterval, err := config.ParseDuration(c.cfg.BailianTrial.RefreshInterval, 60*time.Second)
	if err != nil {
		return fmt.Errorf("parse bailian_trial.refresh_interval: %w", err)
	}

	// 1. Start routing/*.jsonl tailer before initial sync so EOF offsets are
	// captured immediately and any alerts emitted during startup sync are queued.
	var alertCh <-chan domain.RouteAlert
	if c.tailer != nil && ctx.Err() == nil {
		ch, err := c.tailer.Start(ctx)
		if err != nil {
			return fmt.Errorf("start route tailer: %w", err)
		}
		alertCh = ch
	}

	// 2. Lazy channel refresh on startup.
	if ctx.Err() == nil {
		rep, err := c.handleChannelSync(ctx, SyncOptions{
			Apply:   true,
			IfStale: syncInterval,
		})
		if err != nil {
			c.logger.Warn("initial lazy channel sync failed", "error", err)
		} else if rep.Skipped {
			c.logger.Info("lazy channel sync skipped", "reason", rep.SkipReason)
		} else {
			c.logger.Info("initial channel sync completed", "evaluated", rep.Evaluated, "commands", len(rep.Commands))
		}
		if c.hooks.OnChannelSync != nil {
			c.hooks.OnChannelSync(rep, err)
		}
	}

	// 3. Immediate Bailian trial sync on startup (if enabled).
	if ctx.Err() == nil && c.cfg.BailianTrial.Enabled {
		rep, err := c.handleTrialSync(ctx, false)
		if err != nil {
			c.logger.Warn("initial trial sync failed", "error", err)
		} else {
			c.logger.Info("initial trial sync completed", "skipped", rep.Skipped, "desired", len(rep.DesiredMembers))
		}
		if c.hooks.OnTrialSync != nil {
			c.hooks.OnTrialSync(rep, err)
		}
	}

	channelTicker := time.NewTicker(syncInterval)
	defer channelTicker.Stop()

	var trialTicker *time.Ticker
	var trialTickCh <-chan time.Time
	if c.cfg.BailianTrial.Enabled {
		trialTicker = time.NewTicker(trialInterval)
		defer trialTicker.Stop()
		trialTickCh = trialTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("watch loop shutting down", "reason", ctx.Err())
			return nil

		case alert, ok := <-alertCh:
			if !ok {
				alertCh = nil
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			err := c.handleRouteAlert(ctx, alert)
			if err != nil {
				c.logger.Warn("route alert handling failed", "kind", alert.Kind, "provider", alert.ProviderID, "error", err)
			}
			if c.hooks.OnRouteAlert != nil {
				c.hooks.OnRouteAlert(alert, err)
			}

		case <-trialTickCh:
			if ctx.Err() != nil {
				return nil
			}
			rep, err := c.handleTrialSync(ctx, false)
			if err != nil {
				c.logger.Warn("periodic trial sync failed", "error", err)
			}
			if c.hooks.OnTrialSync != nil {
				c.hooks.OnTrialSync(rep, err)
			}

		case <-channelTicker.C:
			if ctx.Err() != nil {
				return nil
			}
			rep, err := c.handleChannelSync(ctx, SyncOptions{
				Apply:   true,
				IfStale: syncInterval,
			})
			if err != nil {
				c.logger.Warn("periodic channel sync failed", "error", err)
			}
			if c.hooks.OnChannelSync != nil {
				c.hooks.OnChannelSync(rep, err)
			}
		}
	}
}

func (c *Coordinator) executePlan(ctx context.Context, cmds []domain.CLICommand, alreadyBackedUp bool) (string, error) {
	if len(cmds) == 0 {
		return "", nil
	}
	var backupPath string
	if !alreadyBackedUp {
		var err error
		backupPath, err = c.gateway.BackupRegistry(ctx)
		if err != nil {
			return "", fmt.Errorf("backup providers.json before write: %w", err)
		}
	}
	for _, cmd := range cmds {
		if err := c.gateway.ExecCLI(ctx, cmd.Args...); err != nil {
			return backupPath, fmt.Errorf("execute magpie CLI (%s): %w", cmd.Reason, err)
		}
	}
	return backupPath, nil
}

func managedGroupNames(cfg domain.Config, st domain.State) []string {
	seen := make(map[string]bool)
	var names []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, role := range cfg.Roles {
		add(role)
		add(role + "-free")
		add(role + "-paid")
	}
	add("transcribe-trial")
	for name := range st.ManagedGroups {
		add(name)
	}
	sort.Strings(names)
	return names
}

func recordHoldsAndTargetGroups(st *domain.State, holds []domain.ManualHold, targets map[string]domain.Group, skipGroup string) {
	for _, h := range holds {
		state.AddManualHold(st, h)
	}
	if st.ManagedGroups == nil {
		st.ManagedGroups = make(map[string][]string)
	}
	for name, g := range targets {
		if name == skipGroup || len(g.Models) == 0 {
			continue
		}
		st.ManagedGroups[name] = append([]string(nil), g.Models...)
	}
}
