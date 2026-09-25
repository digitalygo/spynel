package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/history"
)

func TestWatchNotificationsRetriesPartialNotificationLine(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Ensure("tui", "partial"); err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := store.RecentEntriesSnapshot("tui", "partial", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchNotifications(ctx, path, offset)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"role":"notification_pending","sender":"Spy","content":"partial notice","event_id":"n-partial"}` + "\n")
	if _, err := file.Write(line[:len(line)/2]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := file.Write(line[len(line)/2:]); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.ID != "n-partial" || event.Text != "partial notice" {
			t.Fatalf("partial notification event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("partial notification line was skipped")
	}
}

func TestWatchNotificationsSurfacesExplicitNotificationIdentity(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Ensure("tui", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("tui", "local", history.Entry{Role: "assistant", Content: "already displayed"}); err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := store.RecentEntriesSnapshot("tui", "local", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchNotifications(ctx, path, offset)
	if _, err := store.Append("tui", "local", history.Entry{Role: "notification_pending", Sender: "Spy", Content: "queued notice", EventID: "n1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.ID != "n1" || event.Text != "queued notice" {
			t.Fatalf("notification event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live TUI watcher did not surface the explicit notification")
	}
}

func TestWatchNotificationsSurfacesIdleAssistantNotification(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Ensure("tui", "local"); err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := store.RecentEntriesSnapshot("tui", "local", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchNotifications(ctx, path, offset)
	if _, err := store.Append("tui", "local", history.Entry{Role: "assistant", Sender: "Spy", Content: "idle notice", EventID: "n2"}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.ID != "n2" || event.Text != "idle notice" {
			t.Fatalf("idle notification event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live TUI watcher did not surface the idle notification")
	}
}

// Entries without a durable event identity cover ordinary streamed replies and
// retired runtime-authored recovery terminals; neither is a notification and
// neither may be consumed by the live watcher.
func TestWatchNotificationsIgnoresEntriesWithoutNotificationIdentity(t *testing.T) {
	store := history.New(t.TempDir())
	if _, err := store.Ensure("tui", "local"); err != nil {
		t.Fatal(err)
	}
	_, path, offset, err := store.RecentEntriesSnapshot("tui", "local", 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchNotifications(ctx, path, offset)
	for _, entry := range []history.Entry{
		{Role: "assistant", Content: "ordinary streamed reply"},
		{Role: "user", Content: "ordinary question"},
		{Role: "assistant", Sender: "Spy", Content: "retired recovered answer", Terminal: true},
		{Role: "error", Sender: "Spy", Content: "retired recovery failure", Terminal: true},
	} {
		if _, err := store.Append("tui", "local", entry); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case event := <-events:
		t.Fatalf("watcher consumed a non-notification entry: %#v", event)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestRestartHistorySnapshotAndLiveNotificationHaveOneStableOrder(t *testing.T) {
	store := history.New(t.TempDir())
	for _, entry := range []history.Entry{
		{Role: "user", Content: "ordinary question"},
		{Role: "user", Content: "/restart"},
		{Role: "assistant", Content: "Restarting Spynel..."},
		{Role: "user", Content: "/restart"},
		{Role: "assistant", Content: "Restarting Spynel..."},
	} {
		if _, err := store.Append("tui", "restart", entry); err != nil {
			t.Fatal(err)
		}
	}
	initial, path, offset, err := store.RecentEntriesSnapshot("tui", "restart", 20, 4000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := watchNotifications(ctx, path, offset)
	if _, err := store.Append("tui", "restart", history.Entry{Role: "notification_pending", Sender: "Spy", Content: "queued notice", EventID: "n3"}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if len(initial) != 5 || initial[0].Content != "ordinary question" || initial[1].Content != "/restart" || initial[2].Content != "Restarting Spynel..." || initial[3].Content != "/restart" || initial[4].Content != "Restarting Spynel..." || event.ID != "n3" || event.Text != "queued notice" {
			t.Fatalf("restart/notification live sequence = initial %#v event %#v", initial, event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live notification did not cross the startup snapshot boundary")
	}
	reopened, _, _, err := store.RecentEntriesSnapshot("tui", "restart", 20, 4000)
	if err != nil || len(reopened) != 6 || reopened[5].Content != "queued notice" {
		t.Fatalf("reopened sequence = %#v, %v", reopened, err)
	}
}
