package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"magpie-keeper/internal/admit"
	"magpie-keeper/internal/bailian"
	"magpie-keeper/internal/catalog"
	"magpie-keeper/internal/config"
	"magpie-keeper/internal/coordinator"
	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/magpie"
	"magpie-keeper/internal/state"
)

type appOptions struct {
	configPath string
	statePath  string
	backupDir  string
	jsonOutput bool
	logger     *slog.Logger
	stdin      io.Reader
	stdout     io.Writer
}

func main() {
	opts := &appOptions{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		stdin:  os.Stdin,
		stdout: os.Stdout,
	}
	root := newRootCommand(opts)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand(opts *appOptions) *cobra.Command {
	root := &cobra.Command{
		Use:           "magpie-keeper",
		Short:         "Event-driven Magpie gateway & Bailian free-trial pool keeper",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	if opts.configPath == "" {
		opts.configPath = defaultAppPath("data", "channels.json")
	}
	if opts.statePath == "" {
		opts.statePath = defaultAppPath("data", "state.json")
	}
	if opts.backupDir == "" {
		opts.backupDir = defaultAppPath("data", "backups")
	}
	pf := root.PersistentFlags()
	pf.StringVar(&opts.configPath, "config", opts.configPath, "path to channels.json")
	pf.StringVar(&opts.statePath, "state", opts.statePath, "path to state.json")
	pf.StringVar(&opts.backupDir, "backups", opts.backupDir, "directory for providers.json backups")
	pf.BoolVar(&opts.jsonOutput, "json", opts.jsonOutput, "output structured JSON to stdout")

	root.AddCommand(
		newWatchCommand(opts),
		newStatusCommand(opts),
		newListCandidatesCommand(opts),
		newSyncCommand(opts),
		newSyncTrialCommand(opts),
		newAddProviderCommand(opts),
		newRemoveProviderCommand(opts),
		newPauseTrialCommand(opts),
		newResumeTrialCommand(opts),
	)
	return root
}

func defaultAppPath(parts ...string) string {
	rel := filepath.Join(parts...)
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(append([]string{filepath.Dir(exe)}, parts...)...)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		// Prefer executable dir for default data/ when cwd is another program's dir (e.g. --companion).
		if parts[0] == "data" {
			if _, cfgErr := os.Stat(filepath.Join(filepath.Dir(exe), "data", "channels.json")); cfgErr == nil {
				return candidate
			}
		}
	}
	return rel
}

func (o *appOptions) buildCoordinator(watchMode bool) (*coordinator.Coordinator, domain.Config, error) {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return nil, domain.Config{}, err
	}
	if strings.TrimSpace(cfg.BailianTrial.BridgeScript) == "" {
		cfg.BailianTrial.BridgeScript = defaultAppPath("scripts", "bailian-quota-bridge.mjs")
	}

	var fallbackAPIKey string
	if creds, err := bailian.LoadBailianCredentials(cfg.BailianTrial.BailianConfigDir); err == nil {
		fallbackAPIKey = creds.APIKey
	}

	store := state.NewStore(o.statePath)
	gw := magpie.NewGateway(cfg.MagpieDir, cfg.MagpieURL, cfg.MagpieKey, o.backupDir, nil)
	catReg := catalog.NewDefaultRegistry(nil)
	adm := admit.NewEvaluator(cfg.MagpieURL, cfg.MagpieKey, nil)
	quota := bailian.NewProvider(cfg.BailianTrial, fallbackAPIKey)

	var tailer domain.RouteTailer
	if watchMode && cfg.BailianTrial.Enabled {
		tailer = magpie.NewTailer(cfg.MagpieDir, magpie.WithStartAtEOF(true))
	}

	coord, err := coordinator.New(coordinator.Dependencies{
		Config:         cfg,
		Store:          store,
		Gateway:        gw,
		Catalog:        catReg,
		Admitter:       adm,
		Quota:          quota,
		Tailer:         tailer,
		Logger:         o.logger,
		FallbackAPIKey: fallbackAPIKey,
	})
	if err != nil {
		return nil, domain.Config{}, err
	}
	return coord, cfg, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func watchStdinEOF(r io.Reader, cancel context.CancelFunc, logger *slog.Logger) {
	if r == nil {
		return
	}
	go func() {
		_, err := io.Copy(io.Discard, r)
		if logger != nil {
			logger.Info("companion stdin closed; initiating graceful shutdown", "error", err)
		}
		cancel()
	}()
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
