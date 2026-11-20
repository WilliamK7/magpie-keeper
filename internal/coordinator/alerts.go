package coordinator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
	"magpie-keeper/internal/state"
)

func (c *Coordinator) handleStatus(ctx context.Context) (StatusReport, error) {
	st, err := c.store.Load()
	if err != nil {
		return StatusReport{}, err
	}
	snap, err := c.gateway.ReadRegistry(ctx)
	if err != nil {
		return StatusReport{}, err
	}

	report := StatusReport{
		GeneratedAt:       c.now(),
		MagpieDir:         snap.MagpieDir,
		MagpiePort:        snap.Port,
		Roles:             append([]string(nil), c.cfg.Roles...),
		LastChannelSyncAt: st.LastChannelSyncAt,
		DailyBudget:       st.DailyBudget,
		MaxEvalPerDay:     c.cfg.MaxEvaluationsPerDay,
		Trial: TrialStatus{
			Enabled:                   c.cfg.BailianTrial.Enabled,
			Paused:                    c.cfg.BailianTrial.Paused,
			WorkspaceID:               c.cfg.BailianTrial.WorkspaceID,
			LastQuotaSyncAt:           st.TrialPool.LastQuotaSyncAt,
			NextQuotaRetryAt:          st.TrialPool.NextQuotaRetryAt,
			ConsecutiveErrors:         st.TrialPool.ConsecutiveErrors,
			LastError:                 st.TrialPool.LastError,
			AccountCreditBlockedUntil: st.TrialPool.AccountCreditBlockedUntil,
		},
	}

	groupNames := make([]string, 0, len(snap.Groups))
	for name := range snap.Groups {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	for _, name := range groupNames {
		g := snap.Groups[name]
		report.Groups = append(report.Groups, GroupStatus{
			Name:    name,
			Routing: g.Routing,
			Stays:   g.Stays,
			Hidden:  g.Hidden,
			Members: append([]string(nil), g.Models...),
		})
	}

	holdKeys := make([]string, 0, len(st.ManualHolds))
	for k := range st.ManualHolds {
		holdKeys = append(holdKeys, k)
	}
	sort.Strings(holdKeys)
	for _, k := range holdKeys {
		report.ManualHolds = append(report.ManualHolds, st.ManualHolds[k])
	}

	for _, rec := range st.TrialPool.Channels {
		report.Trial.ActiveChannels = append(report.Trial.ActiveChannels, rec)
	}
	sort.Slice(report.Trial.ActiveChannels, func(i, j int) bool {
		return report.Trial.ActiveChannels[i].Model < report.Trial.ActiveChannels[j].Model
	})
	return report, nil
}

func (c *Coordinator) handleRouteAlert(ctx context.Context, alert domain.RouteAlert) error {
	now := c.now()
	st, err := c.store.Load()
	if err != nil {
		return err
	}
	snap, err := c.gateway.ReadRegistry(ctx)
	if err != nil {
		return err
	}
	storeClock := state.NewStoreWithClock(c.store.Path(), c.now)

	cooldownDur := time.Duration(c.cfg.CooldownSeconds) * time.Second
	if cooldownDur <= 0 {
		cooldownDur = time.Hour
	}

	switch alert.Kind {
	case domain.RouteAlertAccountCredit:
		st.TrialPool.AccountCreditBlockedUntil = now.Add(cooldownDur)
		for k, rec := range st.TrialPool.Channels {
			rec.Active = false
			st.TrialPool.Channels[k] = rec
		}
		if err := c.store.Save(st); err != nil {
			return err
		}
		_, err := c.handleTrialSync(ctx, true)
		return err

	case domain.RouteAlertQuotaExhausted, domain.RouteAlertPermissionDenied:
		model := alert.Model
		if model == "" {
			if rec, ok := st.TrialPool.Channels[alert.ProviderID]; ok {
				model = rec.Model
			} else if p, ok := snap.Providers[alert.ProviderID]; ok && len(p.Models) > 0 {
				model = p.Models[0]
			}
		}
		blockDur := cooldownDur
		if alert.Kind == domain.RouteAlertQuotaExhausted && blockDur < 24*time.Hour {
			blockDur = 24 * time.Hour
		}
		for _, k := range []string{alert.ProviderID, model, alert.ProviderID + "/" + model} {
			if strings.TrimSpace(k) != "" && k != "/" {
				storeClock.SetCooldown(&st, k, blockDur, alert.Kind, false)
			}
		}
		if rec, ok := st.TrialPool.Channels[alert.ProviderID]; ok {
			rec.Active = false
			st.TrialPool.Channels[alert.ProviderID] = rec
		}
		if p, ok := snap.Providers[alert.ProviderID]; ok && !p.Off {
			if _, err := c.gateway.BackupRegistry(ctx); err != nil {
				return fmt.Errorf("backup before immediate off (%s): %w", alert.ProviderID, err)
			}
			if err := c.gateway.ExecCLI(ctx, "provider", "off", alert.ProviderID); err != nil {
				return fmt.Errorf("immediate provider off (%s): %w", alert.ProviderID, err)
			}
		}
		if err := c.store.Save(st); err != nil {
			return err
		}
		_, err := c.handleTrialSync(ctx, true)
		return err

	default:
		return nil
	}
}
