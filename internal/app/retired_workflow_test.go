package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/channel"
	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/history"
	"github.com/digitalygo/spynel/internal/workspace"
)

// sentinelFile writes one legacy document and returns its path plus the exact
// bytes a retired scan must never rewrite or remove.
func sentinelFile(t *testing.T, path, content string) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return []byte(content)
}

func assertSentinel(t *testing.T, path string, want []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("legacy path %s changed: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy path %s permissions changed: %v", path, info.Mode().Perm())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("legacy path %s content changed:\n%s", path, got)
	}
}

func TestLegacyWorkflowWorkspaceIsNeverScannedOrMutated(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(root, ".spynel", "tasks", "todo", "retained-task.md")
	goalPath := filepath.Join(root, ".spynel", "goals", "proposed", "retained-goal.md")
	instructionPath := filepath.Join(root, ".spynel", "instructions", "agent-chat.md")
	notificationPath := filepath.Join(root, ".spynel", "runtime", "notification-agents", "agent.md")
	task := sentinelFile(t, taskPath, "---\nid: retained-task\nstatus: todo\n---\n# Retained task\n")
	goal := sentinelFile(t, goalPath, "---\nid: retained-goal\nstatus: proposed\n---\n# Retained goal\n")
	instruction := sentinelFile(t, instructionPath, "RETIRED PERSISTENT INSTRUCTION\n")
	notification := sentinelFile(t, notificationPath, "RETIRED NOTIFICATION AGENT STATE\n")

	target := newServiceHarness()
	service := New(cfg, target)
	service.SetPrimaryInstanceID("legacy-sentinel")
	// A reconnect transition used to request an immediate recovery scan.
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	if err := service.Handle(context.Background(), core.Message{Channel: "cli", Conversation: "local", Text: "keep working"}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}

	// A short maintenance window must not run any workflow scan or heartbeat.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.RunPrimaryMaintenance(ctx) }()
	time.Sleep(25 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunPrimaryMaintenance returned %v", err)
	}
	if len(target.prompts) != 1 {
		t.Fatalf("unexpected harness dispatch from a legacy scan: %#v", target.prompts)
	}

	assertSentinel(t, taskPath, task)
	assertSentinel(t, goalPath, goal)
	assertSentinel(t, instructionPath, instruction)
	assertSentinel(t, notificationPath, notification)
}

func TestCleanupRetainsLegacyWorkflowDocumentsAndNotificationState(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(root, ".spynel", "tasks", "todo", "old-task.md")
	notificationDir := filepath.Join(root, ".spynel", "runtime", "notification-agent-locks")
	task := sentinelFile(t, taskPath, "---\nid: old-task\nstatus: done\n---\n# Old terminal task\n")
	if err := os.MkdirAll(notificationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	for _, path := range []string{taskPath, notificationDir} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	service := New(cfg, newServiceHarness())
	if _, err := service.runCleanup(7, "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	assertSentinel(t, taskPath, task)
	if _, err := os.Stat(notificationDir); err != nil {
		t.Fatalf("retired notification state was removed by cleanup: %v", err)
	}
}

func TestNoSpontaneousRecoveryDispatchAfterOwnerStartup(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowedUsers = []string{"7"}
	target := newServiceHarness()
	service := New(cfg, target)
	now := time.Now().UTC()
	if _, err := service.History.Append("telegram", "TG-7", history.Entry{At: now, AcceptedAt: now, Role: "user", Content: "unanswered message", SourceMessageID: "local:unanswered"}); err != nil {
		t.Fatal(err)
	}
	service.SetPrimaryInstanceID("owner")
	service.SetConnectionStatus(channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnected})
	time.Sleep(50 * time.Millisecond)
	if len(target.prompts) != 0 {
		t.Fatalf("owner startup dispatched a recovery turn: %#v", target.prompts)
	}
}

func TestRunPrimaryMaintenanceDeliversOutboxThenStopsCleanly(t *testing.T) {
	service := newJobInfoService(t)
	delivered := make(chan string, 4)
	service.outbox = Outbox{
		Directory: service.Config.StatePath("runtime", "outbox"),
		Deliver: func(_ context.Context, origin Origin, _ string, text string) error {
			delivered <- origin.Channel + "/" + origin.Conversation + "\x00" + text
			return nil
		},
	}
	if _, err := service.outbox.Enqueue("manual-retry", "manual", "tui/local", "retry me"); err != nil {
		t.Fatal(err)
	}
	service.SetPrimaryInstanceID("primary")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.RunPrimaryMaintenance(ctx) }()
	select {
	case got := <-delivered:
		if got != "tui/local\x00retry me" {
			t.Fatalf("delivered = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending explicit notification was not retried")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPrimaryMaintenance returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunPrimaryMaintenance did not return after cancellation")
	}
}

func TestRunPrimaryMaintenanceLeavesOutboxAloneWithoutOwnership(t *testing.T) {
	service := newJobInfoService(t)
	delivered := make(chan struct{}, 1)
	service.outbox = Outbox{
		Directory: service.Config.StatePath("runtime", "outbox"),
		Deliver: func(context.Context, Origin, string, string) error {
			delivered <- struct{}{}
			return errors.New("must not be delivered without ownership")
		},
	}
	if _, err := service.outbox.Enqueue("manual-retry", "manual", "tui/local", "retry me"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.RunPrimaryMaintenance(ctx) }()
	time.Sleep(25 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunPrimaryMaintenance returned %v", err)
	}
	select {
	case <-delivered:
		t.Fatal("outbox retry ran without primary ownership")
	default:
	}
}

func TestDirectPiClarificationFinalKeepsSameSession(t *testing.T) {
	target := &nativePiControlHarness{piControlHarness: newPiControlHarness()}
	target.sendThread = "session-0001"
	service := newPiControlService(t, target)
	if final := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "start the refactor"}); final.Kind != core.EventFinal {
		t.Fatalf("first turn = %#v", final)
	}
	target.mu.Lock()
	target.reply = "Which module should I start with?"
	target.mu.Unlock()
	if final := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "the parser"}); final.Text != "Which module should I start with?" {
		t.Fatalf("clarification final = %#v", final)
	}
	target.mu.Lock()
	target.reply = "Starting with the parser."
	target.mu.Unlock()
	if final := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "go ahead"}); final.Text != "Starting with the parser." {
		t.Fatalf("continuation final = %#v", final)
	}
	target.mu.Lock()
	prompts := append([]string(nil), target.prompts["chat:tui:local"]...)
	target.mu.Unlock()
	if len(prompts) != 3 || prompts[0] != "start the refactor" || prompts[1] != "the parser" || prompts[2] != "go ahead" {
		t.Fatalf("same-session raw prompts = %#v", prompts)
	}
}

type ownedCleanupFixture struct {
	removedHistory     string
	liveHistory        string
	newestHistory      string
	finishedJobArchive string
	liveJobArchive     string
	taskPath           string
	goalPath           string
	taskBytes          []byte
	goalBytes          []byte
}

// seedOwnedCleanupFixture lays out one eligible and one protected item for
// every cleanup category so automatic retention is exercised end to end.
func seedOwnedCleanupFixture(t *testing.T, service *Service, now time.Time) ownedCleanupFixture {
	t.Helper()
	old := now.Add(-40 * 24 * time.Hour)
	fixture := ownedCleanupFixture{
		removedHistory: service.History.Path("telegram", "expired-conversation"),
		liveHistory:    service.History.Path("tui", "live-idle"),
		newestHistory:  service.History.Path("tui", "newest"),
	}
	if _, err := service.History.Append("telegram", "expired-conversation", history.Entry{At: old, Role: "assistant", Content: "expired"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.History.Append("tui", "live-idle", history.Entry{At: old, Role: "assistant", Content: "live but idle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.History.Append("tui", "newest", history.Entry{At: now, Role: "assistant", Content: "newest"}); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("client", "live-idle", now); err != nil {
		t.Fatal(err)
	}

	service.Runtime.Now = func() time.Time { return old }
	finishedHandle := service.Runtime.BeginJob("finished", "cli", "private", "finished")
	finishedJob, ok := service.Runtime.Job(finishedHandle)
	if !ok {
		t.Fatal("finished job registration failed")
	}
	service.Runtime.EndJob(finishedHandle)
	liveHandle := service.Runtime.BeginJob("live", "cli", "private", "live")
	liveJob, ok := service.Runtime.Job(liveHandle)
	if !ok {
		t.Fatal("live job registration failed")
	}
	jobsDirectory := service.Config.StatePath("jobs")
	fixture.finishedJobArchive = filepath.Join(jobsDirectory, finishedJob.StableID+".md")
	fixture.liveJobArchive = filepath.Join(jobsDirectory, liveJob.StableID+".md")
	for _, path := range []string{fixture.finishedJobArchive, fixture.liveJobArchive} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("job archive %s missing: %v", path, err)
		}
	}

	fixture.taskPath = service.Config.StatePath("tasks", "todo", "expired-task.md")
	fixture.goalPath = service.Config.StatePath("goals", "proposed", "expired-goal.md")
	fixture.taskBytes = sentinelFile(t, fixture.taskPath, "---\nid: expired-task\nstatus: done\n---\n# Retired task\n")
	fixture.goalBytes = sentinelFile(t, fixture.goalPath, "---\nid: expired-goal\nstatus: proposed\n---\n# Retired goal\n")
	for _, path := range []string{fixture.taskPath, fixture.goalPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func cleanupLogEntry(logs []LogEntry, event string) (LogEntry, bool) {
	for _, entry := range logs {
		if entry.Component == "cleanup" && entry.Event == event {
			return entry, true
		}
	}
	return LogEntry{}, false
}

// expireOwnerTransitionFence simulates an owner that has been serving longer
// than the one-lease re-establishment window, so live leases are the only
// protection cleanup observes.
func expireOwnerTransitionFence(service *Service) {
	service.instanceMu.Lock()
	service.cleanupNotBefore = time.Time{}
	service.instanceMu.Unlock()
}

func TestRunOwnedAutomaticCleanupRemovesEligibleDataAndProtectsLiveState(t *testing.T) {
	service := newJobInfoService(t)
	now := time.Now().UTC()
	fixture := seedOwnedCleanupFixture(t, service, now)
	service.SetPrimaryInstanceID("owner")
	expireOwnerTransitionFence(service)

	service.runOwnedAutomaticCleanup(context.Background())

	if _, err := os.Stat(fixture.removedHistory); !os.IsNotExist(err) {
		t.Fatalf("eligible conversation was not removed: %v", err)
	}
	for _, path := range []string{fixture.liveHistory, fixture.newestHistory, fixture.liveJobArchive} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected state %s was removed: %v", path, err)
		}
	}
	if _, err := os.Stat(fixture.finishedJobArchive); !os.IsNotExist(err) {
		t.Fatalf("eligible job archive was not removed: %v", err)
	}
	assertSentinel(t, fixture.taskPath, fixture.taskBytes)
	assertSentinel(t, fixture.goalPath, fixture.goalBytes)
	entry, found := cleanupLogEntry(service.Runtime.Logs(), "automatic_completed")
	if !found || entry.Level != "info" || !strings.Contains(entry.Text, "Cleanup complete:") {
		t.Fatalf("automatic completion log = %#v, %t", entry, found)
	}
}

func TestRunOwnedAutomaticCleanupDoesNothingWithoutPrimaryOwnership(t *testing.T) {
	service := newJobInfoService(t)
	now := time.Now().UTC()
	fixture := seedOwnedCleanupFixture(t, service, now)

	service.runOwnedAutomaticCleanup(context.Background())

	for _, path := range []string{fixture.removedHistory, fixture.finishedJobArchive, fixture.liveHistory, fixture.liveJobArchive} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("ownership-free cleanup changed %s: %v", path, err)
		}
	}
	assertSentinel(t, fixture.taskPath, fixture.taskBytes)
	assertSentinel(t, fixture.goalPath, fixture.goalBytes)
	if entry, found := cleanupLogEntry(service.Runtime.Logs(), "automatic_completed"); found {
		t.Fatalf("ownership-free cleanup logged completion: %#v", entry)
	}
}

func TestRunOwnedAutomaticCleanupReportsOwnerTransitionFence(t *testing.T) {
	service := newJobInfoService(t)
	now := time.Now().UTC()
	fixture := seedOwnedCleanupFixture(t, service, now)
	service.SetPrimaryInstanceID("owner")

	service.runOwnedAutomaticCleanup(context.Background())

	if _, err := os.Stat(fixture.removedHistory); err != nil {
		t.Fatalf("fenced cleanup removed history: %v", err)
	}
	entry, found := cleanupLogEntry(service.Runtime.Logs(), "automatic_failed")
	if !found || entry.Level != "error" || !strings.Contains(entry.Text, "re-establishing") {
		t.Fatalf("fenced cleanup log = %#v, %t", entry, found)
	}
}

func TestRunOwnedAutomaticCleanupSkipsNonPositiveRetention(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace.CleanupRetentionDays = 0
	service := New(cfg, newServiceHarness())
	fixture := seedOwnedCleanupFixture(t, service, time.Now().UTC())
	service.SetPrimaryInstanceID("owner")
	expireOwnerTransitionFence(service)

	service.runOwnedAutomaticCleanup(context.Background())

	if _, err := os.Stat(fixture.removedHistory); err != nil {
		t.Fatalf("zero-retention cleanup removed history: %v", err)
	}
	if _, err := os.Stat(fixture.finishedJobArchive); err != nil {
		t.Fatalf("zero-retention cleanup removed job archive: %v", err)
	}
	if entry, found := cleanupLogEntry(service.Runtime.Logs(), "automatic_completed"); found {
		t.Fatalf("zero-retention cleanup logged completion: %#v", entry)
	}
}

func TestRetryPendingNotificationsLogsDeliveryFailure(t *testing.T) {
	service := newJobInfoService(t)
	service.outbox = Outbox{
		Directory: service.Config.StatePath("runtime", "outbox"),
		Deliver: func(context.Context, Origin, string, string) error {
			return errors.New("upstream delivery offline")
		},
	}
	if _, err := service.outbox.Enqueue("manual-retry", "manual", "tui/local", "retry me"); err != nil {
		t.Fatal(err)
	}
	service.SetPrimaryInstanceID("primary")

	service.retryPendingNotifications(context.Background())

	found := false
	for _, entry := range service.Runtime.Logs() {
		if entry.Component == "notify" && entry.Event == "notification_retry" && strings.Contains(entry.Text, "upstream delivery offline") {
			found = true
		}
	}
	if !found {
		t.Fatalf("retry failure was not logged: %#v", service.Runtime.Logs())
	}
}
