package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/core"
)

func newPiContextFixture(t *testing.T, mode string) (*Pi, HarnessConfig, context.Context) {
	t.Helper()
	command, root, _ := portableHarnessFixture(t, mode)
	config := HarnessConfig{Command: command, Cwd: root, Model: "fixture/model-a", Effort: "high", Sandbox: "read-only", SessionsFile: filepath.Join(root, "sessions.json")}
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pi.Close() })
	return pi, config, ctx
}

func awaitPiEvent(t *testing.T, ctx context.Context, events <-chan core.Event, match func(core.Event) bool) core.Event {
	t.Helper()
	for {
		select {
		case event := <-events:
			if match(event) {
				return event
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for Pi event")
			return core.Event{}
		}
	}
}

func awaitPiTurn(t *testing.T, ctx context.Context, pi *Pi, key, prompt string) {
	t.Helper()
	events := make(chan core.Event, 32)
	if _, _, err := pi.Send(ctx, key, prompt, func(event core.Event) { events <- event }); err != nil {
		t.Fatal(err)
	}
	awaitPiEvent(t, ctx, events, func(event core.Event) bool {
		return event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError)
	})
}

func TestPiConversationContextTracksSessionPolicyAndFileState(t *testing.T) {
	pi, _, ctx := newPiContextFixture(t, "pi-lifecycle")
	awaitPiTurn(t, ctx, pi, "chat", "first")
	if !pi.ProvidesConversationContext("chat") {
		t.Fatal("idle policy-matching live process did not report retained context")
	}
	pi.mu.Lock()
	process := pi.processes["chat"]
	session := pi.sessions["chat"]
	cwd := pi.config.Cwd
	pi.mu.Unlock()
	if process == nil || session.ID == "" || session.Path == "" {
		t.Fatalf("fixture session state = %+v / %+v", process, session)
	}
	process.mu.Lock()
	captured := process.session.Policy
	process.mu.Unlock()
	if captured != piSessionPolicy(pi.config) {
		t.Fatalf("capability policy %q does not match the dispatch policy %q", captured, piSessionPolicy(pi.config))
	}
	// A live idle process retains the conversation even if its persisted
	// session file was removed.
	if err := os.Remove(session.Path); err != nil {
		t.Fatal(err)
	}
	if !pi.ProvidesConversationContext("chat") {
		t.Fatal("live idle process lost retained context when its session file was removed")
	}
	// Every policy field the dispatch path compares must invalidate the
	// capability while the process idles under a stale policy.
	for _, change := range []struct {
		name  string
		apply func()
	}{
		{"model", func() { pi.mu.Lock(); pi.config.Model = "fixture/model-b"; pi.mu.Unlock() }},
		{"effort", func() { pi.mu.Lock(); pi.config.Effort = "low"; pi.mu.Unlock() }},
		{"sandbox", func() { pi.mu.Lock(); pi.config.Sandbox = "danger-full-access"; pi.mu.Unlock() }},
	} {
		change.apply()
		if pi.ProvidesConversationContext("chat") {
			t.Fatalf("idle process reported retained context after a %s policy change", change.name)
		}
		pi.mu.Lock()
		pi.config.Model, pi.config.Effort, pi.config.Sandbox = "fixture/model-a", "high", "read-only"
		pi.mu.Unlock()
		if !pi.ProvidesConversationContext("chat") {
			t.Fatalf("idle process did not report retained context after restoring the %s policy", change.name)
		}
	}
	// Simulate the provider exiting: the remembered session file now decides.
	process.close()
	pi.mu.Lock()
	delete(pi.processes, "chat")
	pi.mu.Unlock()
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("missing session file reported retained context")
	}
	writeFixturePiSession(session.Path, session.ID, cwd)
	if !pi.ProvidesConversationContext("chat") {
		t.Fatal("policy-matching persisted session file did not report retained context")
	}
	pi.mu.Lock()
	pi.config.Model = "fixture/model-b"
	pi.mu.Unlock()
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("policy-mismatched persisted session file reported retained context")
	}
	pi.mu.Lock()
	pi.config.Model = "fixture/model-a"
	pi.mu.Unlock()
	if err := os.Remove(session.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(session.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("non-regular session path reported retained context")
	}
}

func TestPiConversationContextRetainsActiveTurnAcrossPolicyChange(t *testing.T) {
	pi, _, ctx := newPiContextFixture(t, "pi-interrupt")
	events := make(chan core.Event, 32)
	if _, _, err := pi.Send(ctx, "chat", "work", func(event core.Event) { events <- event }); err != nil {
		t.Fatal(err)
	}
	awaitPiEvent(t, ctx, events, func(event core.Event) bool { return event.Kind == core.EventDelta })
	pi.mu.Lock()
	pi.config.Model = "fixture/model-b"
	pi.mu.Unlock()
	if !pi.ProvidesConversationContext("chat") {
		t.Fatal("active turn lost retained context after a policy change")
	}
	if _, err := pi.Interrupt(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	awaitPiEvent(t, ctx, events, func(event core.Event) bool {
		return event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError)
	})
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("idle policy-mismatched process reported retained context")
	}
}

func TestPiConversationContextFailsClosedBeforeStartAndAfterClose(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
	config := HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")}
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("not-started Pi reported retained context")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("started Pi without a conversation session reported retained context")
	}
	if err := pi.Close(); err != nil {
		t.Fatal(err)
	}
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("closed Pi reported retained context")
	}
}

func TestPiConversationContextPolicyMatchesDispatchReplacement(t *testing.T) {
	pi, _, ctx := newPiContextFixture(t, "pi-lifecycle")
	awaitPiTurn(t, ctx, pi, "chat", "first")
	pi.mu.Lock()
	before := pi.processes["chat"]
	pi.mu.Unlock()
	if before == nil {
		t.Fatal("fixture did not keep a live process")
	}
	// A matching policy keeps the live process, exactly as the capability says.
	process, err := pi.ensureProcess(ctx, "chat", "fixture/model-a", "high")
	if err != nil {
		t.Fatal(err)
	}
	if process != before {
		t.Fatal("matching policy replaced a live process")
	}
	if !pi.ProvidesConversationContext("chat") {
		t.Fatal("matching live process did not report retained context")
	}
	// A stale policy replaces the idle process, exactly as the capability says.
	pi.mu.Lock()
	pi.config.Model = "fixture/model-b"
	pi.mu.Unlock()
	if pi.ProvidesConversationContext("chat") {
		t.Fatal("stale policy reported retained context")
	}
	process, err = pi.ensureProcess(ctx, "chat", "fixture/model-b", "high")
	if err != nil {
		t.Fatal(err)
	}
	if process == before {
		t.Fatal("policy change reused the stale process")
	}
}

type retainedContextSupervisorHarness struct {
	*supervisorHarness
	mu       sync.Mutex
	key      string
	retained bool
}

func (r *retainedContextSupervisorHarness) ProvidesConversationContext(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.key = key
	return r.retained
}

func TestSupervisorForwardsConversationContextCapability(t *testing.T) {
	target := &retainedContextSupervisorHarness{
		supervisorHarness: &supervisorHarness{name: "pi", active: map[string]bool{}, emits: map[string]core.Emit{}},
		retained:          true,
	}
	registry := NewRegistry()
	registry.Register("pi", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "pi"})
	if supervisor.ProvidesConversationContext("chat") {
		t.Fatal("unstarted supervisor reported retained context")
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !supervisor.ProvidesConversationContext("chat") {
		t.Fatal("capability was not forwarded to the active adapter")
	}
	target.mu.Lock()
	key := target.key
	target.mu.Unlock()
	if key != "chat" {
		t.Fatalf("forwarded capability key = %q", key)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	if supervisor.ProvidesConversationContext("chat") {
		t.Fatal("closed supervisor reported retained context")
	}
}

func TestSupervisorConversationContextFailsClosedWithoutCapability(t *testing.T) {
	plain := &supervisorHarness{name: "claude-code", active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return plain, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "claude-code"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if supervisor.ProvidesConversationContext("chat") {
		t.Fatal("harness without the capability reported retained context")
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}

	unavailable := NewRegistry()
	unavailable.Register("pi", func(HarnessConfig) (Harness, error) {
		return &supervisorHarness{name: "pi", startErr: errors.New("missing executable"), active: map[string]bool{}, emits: map[string]core.Emit{}}, nil
	})
	down := NewSupervisor(unavailable, HarnessConfig{Name: "pi"})
	if err := down.Start(context.Background()); err == nil {
		t.Fatal("unavailable harness unexpectedly started")
	}
	if down.ProvidesConversationContext("chat") {
		t.Fatal("unavailable harness reported retained context")
	}
	if err := down.Close(); err != nil {
		t.Fatal(err)
	}
}
