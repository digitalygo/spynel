package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/history"
	"github.com/digitalygo/spynel/internal/workspace"
)

func TestCleanupCommandValidatesDaysAndDoesNotOverlap(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	run := func(command string) string {
		var response core.Event
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: command}, func(event core.Event) { response = event }); err != nil {
			t.Fatal(err)
		}
		return response.Text
	}
	for _, command := range []string{"/cleanup 0", "/cleanup -1", "/cleanup 1.5", "/cleanup seven", "/cleanup 2 extra"} {
		if got := run(command); !strings.Contains(got, "Usage: /cleanup [days]") {
			t.Fatalf("%s response = %q", command, got)
		}
	}
	lock, acquired, err := tryCleanupLock(cfg.StatePath("runtime", "cleanup.lock"))
	if err != nil || !acquired {
		t.Fatalf("hold cleanup lock: acquired=%v err=%v", acquired, err)
	}
	defer releaseCleanupLock(lock)
	if got := run("/cleanup"); !strings.Contains(got, "already running") {
		t.Fatalf("overlap response = %q", got)
	}
}

func TestAutomaticCleanupProtectsEveryLeasedIdleTUIConversation(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	for _, conversation := range []string{"idle-primary", "idle-secondary", "stale-client", "unprotected"} {
		if _, err := service.History.Append("tui", conversation, history.Entry{At: old, Role: "assistant", Content: conversation}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.History.Append("tui", "newest-saved", history.Entry{At: now, Role: "assistant", Content: "newest"}); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("primary-client", "idle-primary", now); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("secondary-client", "idle-secondary", now); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("stale-client", "stale-client", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	result, err := service.runCleanup(7, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedConversations != 2 || result.Protected != 3 {
		t.Fatalf("cleanup result = %#v", result)
	}
	for _, conversation := range []string{"idle-primary", "idle-secondary"} {
		if _, err := os.Stat(service.History.Path("tui", conversation)); err != nil {
			t.Fatalf("live idle conversation %s was removed: %v", conversation, err)
		}
	}
	for _, conversation := range []string{"stale-client", "unprotected"} {
		if _, err := os.Stat(service.History.Path("tui", conversation)); !os.IsNotExist(err) {
			t.Fatalf("expired/unprotected conversation %s remains: %v", conversation, err)
		}
	}
}

func TestLiveTUIConversationSwitchRetainsTransitionLeaseAndUnregisters(t *testing.T) {
	service := &Service{liveTUI: map[string]map[string]time.Time{}}
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if err := service.RegisterLiveTUI("client", "before", now); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterLiveTUI("client", "after", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	protected := service.liveTUIConversations(now.Add(2 * time.Second))
	if !protected["before"] || !protected["after"] {
		t.Fatalf("transition leases = %#v", protected)
	}
	service.UnregisterLiveTUI("client")
	if protected := service.liveTUIConversations(now.Add(2 * time.Second)); len(protected) != 0 {
		t.Fatalf("leases remain after unregister: %#v", protected)
	}
}

func TestPrimaryRestartFencesCleanupUntilLiveTUIsCanRenew(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	if _, err := service.History.Append("tui", "still-open-after-restart", history.Entry{
		At: old, Role: "assistant", Content: "old but live",
	}); err != nil {
		t.Fatal(err)
	}

	service.SetPrimaryInstanceID("replacement-owner")
	service.instanceMu.RLock()
	fenceEnd := service.cleanupNotBefore
	service.instanceMu.RUnlock()
	if _, err := service.runCleanup(7, "", "", fenceEnd.Add(-time.Nanosecond)); !errors.Is(err, errCleanupLeaseReestablishing) {
		t.Fatalf("cleanup during owner restart fence = %v", err)
	}
	if _, err := os.Stat(service.History.Path("tui", "still-open-after-restart")); err != nil {
		t.Fatalf("restart-window cleanup removed live history: %v", err)
	}

	// A live client renews on its ordinary ten-second cadence. At fence release,
	// the rebuilt owner-side lease protects the conversation normally.
	if err := service.RegisterLiveTUI("idle-client", "still-open-after-restart", fenceEnd.Add(-50*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := service.runCleanup(7, "", "", fenceEnd)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedConversations != 0 || result.Protected != 1 {
		t.Fatalf("cleanup after renewal = %#v", result)
	}
	if _, err := os.Stat(service.History.Path("tui", "still-open-after-restart")); err != nil {
		t.Fatalf("renewed live history was removed: %v", err)
	}
}

func TestCleanupSerializesLiveTUIAdmissionThroughHistoryDeletion(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(config.PathForRoot(root))
	service := New(cfg, newServiceHarness())
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if _, err := service.History.Append("tui", "admission-race", history.Entry{
		At: now.Add(-8 * 24 * time.Hour), Role: "assistant", Content: "old",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.History.Append("tui", "newest-saved", history.Entry{
		At: now, Role: "assistant", Content: "newest",
	}); err != nil {
		t.Fatal(err)
	}

	protected := make(chan struct{})
	allowRemoval := make(chan struct{})
	removed := make(chan struct{})
	allowUnlock := make(chan struct{})
	service.cleanupHistoryStep = func(step string) {
		switch step {
		case "protected":
			close(protected)
			<-allowRemoval
		case "removed":
			close(removed)
			<-allowUnlock
		}
	}
	cleanupDone := make(chan error, 1)
	go func() {
		_, err := service.runCleanup(7, "", "", now)
		cleanupDone <- err
	}()
	<-protected

	registrationDone := make(chan error, 1)
	go func() {
		registrationDone <- service.RegisterLiveTUI("new-client", "admission-race", now)
	}()
	select {
	case err := <-registrationDone:
		t.Fatalf("registration crossed cleanup protection snapshot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowRemoval)
	<-removed
	if _, err := os.Stat(service.History.Path("tui", "admission-race")); !os.IsNotExist(err) {
		t.Fatalf("old history was not removed inside serialized boundary: %v", err)
	}
	select {
	case err := <-registrationDone:
		t.Fatalf("registration completed before cleanup released deletion boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowUnlock)
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if err := <-registrationDone; err != nil {
		t.Fatal(err)
	}
}

func TestResumeRegistersBranchBeforeConcurrentCleanupCanDeleteIt(t *testing.T) {
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, newServiceHarness())
	now := time.Now().UTC()
	if _, err := service.History.Append("telegram", "old-source", history.Entry{
		At: now.Add(-8 * 24 * time.Hour), Role: "assistant", Content: "old answer",
	}); err != nil {
		t.Fatal(err)
	}

	branched := make(chan struct{})
	allowRegistration := make(chan struct{})
	service.resumeAdmissionStep = func(step string) {
		if step == "branched" {
			close(branched)
			<-allowRegistration
		}
	}
	action := "resume:" + encodeConversation("telegram", "old-source")
	resumeDone := make(chan struct {
		screen *core.Screen
		err    error
	}, 1)
	go func() {
		screen, err := service.ScreenActionForInstance(context.Background(), "resume-client", "resume", action, nil)
		resumeDone <- struct {
			screen *core.Screen
			err    error
		}{screen: screen, err: err}
	}()
	<-branched

	cleanupDone := make(chan error, 1)
	go func() {
		_, err := service.runCleanup(7, "tui", "invoking-conversation", now)
		cleanupDone <- err
	}()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup crossed resume creation-to-registration boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(allowRegistration)
	resumed := <-resumeDone
	if resumed.err != nil || resumed.screen == nil || resumed.screen.ID != "chat" {
		t.Fatalf("resume result = %#v, %v", resumed.screen, resumed.err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(service.History.Path("tui", resumed.screen.Conversation)); err != nil {
		t.Fatalf("admitted resumed branch was removed: %v", err)
	}
	if _, err := os.Stat(service.History.Path("telegram", "old-source")); !os.IsNotExist(err) {
		t.Fatalf("unprotected old source was not removed: %v", err)
	}
}
