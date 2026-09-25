package app

import (
	"context"
	"time"
)

const (
	automaticCleanupCadence  = 8 * time.Hour
	notificationRetryCadence = 30 * time.Second
)

// RunPrimaryMaintenance runs the elected owner's classic background services
// until ctx is cancelled. It retries pending explicit notification deliveries
// and runs the existing periodic history and job-archive cleanup while this
// process owns the workspace primary lease. It never dispatches a harness
// prompt, scans durable work, or runs a semantic heartbeat, and returns nil on
// normal stop.
func (s *Service) RunPrimaryMaintenance(ctx context.Context) error {
	s.retryPendingNotifications(ctx)
	notificationTicker := time.NewTicker(notificationRetryCadence)
	defer notificationTicker.Stop()
	cleanupTicker := time.NewTicker(automaticCleanupCadence)
	defer cleanupTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-notificationTicker.C:
			s.retryPendingNotifications(ctx)
		case <-cleanupTicker.C:
			s.runOwnedAutomaticCleanup(ctx)
		}
	}
}

func (s *Service) retryPendingNotifications(ctx context.Context) {
	if s.primaryInstanceID() == "" {
		return
	}
	if err := s.outbox.Process(ctx); err != nil {
		s.Runtime.LogEvent("error", "notify", "notification_retry", "Notification retained for retry: "+err.Error())
	}
}

func (s *Service) runOwnedAutomaticCleanup(ctx context.Context) {
	if s.primaryInstanceID() == "" {
		return
	}
	days := s.Settings.Snapshot().Workspace.CleanupRetentionDays
	if days <= 0 {
		return
	}
	result, err := s.runAutomaticCleanup(ctx, days)
	if err != nil {
		s.Runtime.LogEvent("error", "cleanup", "automatic_failed", "Automatic cleanup skipped or failed: "+err.Error())
		return
	}
	s.Runtime.LogEvent("info", "cleanup", "automatic_completed", result)
}
