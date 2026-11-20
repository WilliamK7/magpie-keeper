package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"magpie-keeper/internal/config"
	"magpie-keeper/internal/coordinator"
	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/magpie"
)

func newWatchCommand(opts *appOptions) *cobra.Command {
	var companion bool
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run lazy channel refresh, Bailian trial sync, and routing log monitor",
		RunE: func(cmd *cobra.Command, args []string) error {
			coord, _, err := opts.buildCoordinator(true)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			if companion {
				watchStdinEOF(opts.stdin, cancel, opts.logger)
			}
			return coord.Watch(ctx)
		},
	}
	cmd.Flags().BoolVar(&companion, "companion", false, "exit gracefully when stdin reaches EOF")
	return cmd
}

func newStatusCommand(opts *appOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Read-only view of Magpie groups, sync history, and Bailian trial state",
		RunE: func(cmd *cobra.Command, args []string) error {
			coord, _, err := opts.buildCoordinator(false)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := coord.Status(ctx)
			if err != nil {
				return err
			}
			if opts.jsonOutput {
				return printJSON(opts.stdout, rep)
			}
			renderStatusText(opts.stdout, rep)
			return nil
		},
	}
}

func newListCandidatesCommand(opts *appOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list-candidates",
		Short: "Read-only preview of discovered channel candidates and admission status",
		RunE: func(cmd *cobra.Command, args []string) error {
			coord, _, err := opts.buildCoordinator(false)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := coord.ListCandidates(ctx)
			if err != nil {
				return err
			}
			if opts.jsonOutput {
				return printJSON(opts.stdout, rep)
			}
			renderCandidatesText(opts.stdout, rep)
			return nil
		},
	}
}

func newSyncCommand(opts *appOptions) *cobra.Command {
	var apply bool
	var dryRun bool
	var ifStale string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Discover and admit ordinary channel candidates, planning or applying group updates",
		RunE: func(cmd *cobra.Command, args []string) error {
			var staleDur time.Duration
			if strings.TrimSpace(ifStale) != "" {
				d, err := config.ParseDuration(ifStale, 0)
				if err != nil {
					return fmt.Errorf("--if-stale: %w", err)
				}
				staleDur = d
			}
			coord, _, err := opts.buildCoordinator(false)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := coord.Sync(ctx, coordinator.SyncOptions{
				Apply:   apply && !dryRun,
				DryRun:  dryRun || !apply,
				IfStale: staleDur,
			})
			if err != nil {
				return err
			}
			if opts.jsonOutput {
				return printJSON(opts.stdout, rep)
			}
			renderSyncText(opts.stdout, rep)
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "back up providers.json and apply planned changes via magpie.com")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview diff plan without mutating Magpie (default when --apply is omitted)")
	cmd.Flags().StringVar(&ifStale, "if-stale", "", "skip sync when last_channel_sync_at is newer than this duration (e.g. 6h)")
	return cmd
}

func newSyncTrialCommand(opts *appOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "sync-trial",
		Short: "Refresh Bailian free-tier trial quotas and update group/transcribe-trial",
		RunE: func(cmd *cobra.Command, args []string) error {
			coord, _, err := opts.buildCoordinator(false)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := coord.SyncTrial(ctx)
			if err != nil {
				return err
			}
			if opts.jsonOutput {
				return printJSON(opts.stdout, rep)
			}
			renderTrialText(opts.stdout, rep)
			return nil
		},
	}
}

func newAddProviderCommand(opts *appOptions) *cobra.Command {
	rule := domain.ProviderRule{Adapter: "registry"}
	var maxFree, maxCommercial int
	cmd := &cobra.Command{
		Use:   "add-provider <id>",
		Short: "Add or update a provider discovery rule in data/channels.json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			if maxFree > 0 || maxCommercial > 0 {
				rule.MaxCandidates = map[string]int{}
				if maxFree > 0 {
					rule.MaxCandidates[domain.BillingFree] = maxFree
				}
				if maxCommercial > 0 {
					rule.MaxCandidates[domain.BillingCommercial] = maxCommercial
				}
			}
			if cfg.Providers == nil {
				cfg.Providers = map[string]domain.ProviderRule{}
			}
			cfg.Providers[id] = rule
			if err := config.Save(opts.configPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(opts.stdout, "Provider %q saved to %s (adapter=%s)\n", id, opts.configPath, rule.Adapter)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&rule.Adapter, "adapter", "registry", "catalog adapter (registry|openrouter|vercel|vllm-sr)")
	f.StringSliceVar(&rule.Billing, "billing", nil, "allowed billing tiers (free,commercial)")
	f.StringSliceVar(&rule.Include, "include", nil, "include glob patterns")
	f.StringSliceVar(&rule.Exclude, "exclude", nil, "exclude glob patterns")
	f.StringSliceVar(&rule.Preferred, "preferred", nil, "preferred model IDs")
	f.StringSliceVar(&rule.PriceExceptions, "price-exception", nil, "commercial price exception model IDs")
	f.Float64Var(&rule.MaxInputUSDPerMillion, "max-input", 0, "max input USD per 1M tokens")
	f.Float64Var(&rule.MaxOutputUSDPerMillion, "max-output", 0, "max output USD per 1M tokens")
	f.IntVar(&maxFree, "max-free", 0, "max free candidates")
	f.IntVar(&maxCommercial, "max-commercial", 0, "max commercial candidates")
	f.StringSliceVar(&rule.Roles, "roles", nil, "allowed roles (transcribe,review)")
	f.StringVar(&rule.SupplierSlug, "supplier-slug", "", "custom supplier group slug")
	f.StringVar(&rule.EndpointURL, "endpoint-url", "", "custom catalog endpoint URL override")
	return cmd
}

func newRemoveProviderCommand(opts *appOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-provider <id>",
		Short: "Remove a provider rule from data/channels.json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			if _, ok := cfg.Providers[id]; !ok {
				return fmt.Errorf("provider %q is not configured in %s", id, opts.configPath)
			}
			delete(cfg.Providers, id)
			if err := config.Save(opts.configPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(opts.stdout, "Provider %q removed from %s\n", id, opts.configPath)
			return nil
		},
	}
}

func newPauseTrialCommand(opts *appOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "pause-trial",
		Short: "Pause Bailian free-trial pool in data/channels.json and drain active channels if Magpie is available",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			cfg.BailianTrial.Paused = true
			if err := config.Save(opts.configPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(opts.stdout, "Bailian trial paused in %s\n", opts.configPath)
			if _, vErr := magpie.ValidatePortableDir(cfg.MagpieDir, cfg.MagpieURL); vErr == nil {
				coord, _, cErr := opts.buildCoordinator(false)
				if cErr == nil {
					ctx, cancel := signalContext()
					defer cancel()
					if _, sErr := coord.SyncTrial(ctx); sErr != nil {
						opts.logger.Warn("pause-trial drain skipped or failed", "error", sErr)
					}
				}
			}
			return nil
		},
	}
}

func newResumeTrialCommand(opts *appOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "resume-trial",
		Short: "Resume Bailian free-trial pool in data/channels.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(opts.configPath)
			if err != nil {
				return err
			}
			cfg.BailianTrial.Paused = false
			if err := config.Save(opts.configPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(opts.stdout, "Bailian trial resumed in %s\n", opts.configPath)
			return nil
		},
	}
}
