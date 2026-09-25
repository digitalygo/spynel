package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/core"
)

// TestPiRuntimeDirectoryDerivations covers both runtime-directory shapes:
// the canonical SessionsFile-backed derivation and the Cwd fallback used
// when no sessions file is configured.
func TestPiRuntimeDirectoryDerivations(t *testing.T) {
	if got, want := piRuntimeDirectory(HarnessConfig{Cwd: filepath.Join("ws", "root")}), filepath.Join("ws", "root", ".spynel", "runtime"); got != want {
		t.Fatalf("piRuntimeDirectory without SessionsFile = %q, want %q", got, want)
	}
	cfg := HarnessConfig{Cwd: filepath.Join("somewhere", "else"), SessionsFile: filepath.Join("ws", ".spynel", "runtime", "sessions.json")}
	if got, want := piRuntimeDirectory(cfg), filepath.Join("ws", ".spynel", "runtime"); got != want {
		t.Fatalf("piRuntimeDirectory with SessionsFile = %q, want %q", got, want)
	}
}

// stageUnresolvableWorkingDirectory removes the process working directory so
// os.Getwd, and therefore filepath.Abs of a relative path, fails. The
// platform must allow deleting the current directory; otherwise the test
// skips because the negative path cannot be staged there.
func stageUnresolvableWorkingDirectory(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("windows locks the process working directory, so the unresolvable-cwd failure cannot be staged")
	}
	original, err := os.Getwd()
	if err != nil {
		t.Skipf("os.Getwd already fails in this environment: %v", err)
	}
	dir, err := os.MkdirTemp("", "spynel-note-cwd-")
	if err != nil {
		t.Skipf("cannot create a disposable working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		_ = os.Remove(dir)
		t.Skipf("cannot enter the disposable working directory: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		_ = os.Chdir(original)
		_ = os.Remove(dir)
		t.Skipf("cannot remove the current working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	if _, err := os.Getwd(); err == nil {
		t.Skip("os.Getwd still resolves inside the removed directory, so the failure cannot be staged")
	}
}

func TestPiTelegramNoteExtensionPathRequiresResolvableWorkingDirectory(t *testing.T) {
	stageUnresolvableWorkingDirectory(t)
	cfg := HarnessConfig{Cwd: "."}
	if _, err := piTelegramNoteExtensionPath(cfg); err == nil {
		t.Fatal("piTelegramNoteExtensionPath resolved a path although os.Getwd fails")
	}
	if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
		t.Fatal("ensurePiTelegramNoteExtension succeeded although the runtime path is unresolvable")
	}
}

func TestEnsurePiTelegramNoteExtensionRefusesFileOccupyingTrustDirectories(t *testing.T) {
	t.Run("state root", func(t *testing.T) {
		home := t.TempDir()
		cfg := noteExtensionWorkspaceConfig(home)
		stateRoot := filepath.Join(home, ".spynel")
		if err := os.WriteFile(stateRoot, []byte("ordinary file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
			t.Fatal("ensure accepted a plain file occupying the .spynel state root")
		}
		info, err := os.Lstat(stateRoot)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("state root file was replaced or removed: %v %v", info, err)
		}
	})
	t.Run("runtime directory", func(t *testing.T) {
		home := t.TempDir()
		cfg := noteExtensionWorkspaceConfig(home)
		stateRoot := filepath.Join(home, ".spynel")
		if err := os.Mkdir(stateRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		runtimeDir := filepath.Join(stateRoot, "runtime")
		if err := os.WriteFile(runtimeDir, []byte("ordinary file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
			t.Fatal("ensure accepted a plain file occupying the runtime directory")
		}
		info, err := os.Lstat(runtimeDir)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("runtime file was replaced or removed: %v %v", info, err)
		}
		entries, err := os.ReadDir(stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "runtime" {
			t.Fatalf("state root gained entries despite the refused runtime path: %v", entries)
		}
	})
}

// TestPiNativeCommandSettlementStandsDownWhileStreaming covers the settlement
// guard for a get_state query that reports a still-streaming session: the
// no-run settlement stands down and the ordinary agent_settled lifecycle
// stays authoritative for the turn.
func TestPiNativeCommandSettlementStandsDownWhileStreaming(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-native-commands-streaming-state")
	key := "chat:tui:native-streaming"
	var mu sync.Mutex
	var events []core.Event
	done := make(chan core.Event, 4)
	threadID, steered, err := pi.Send(ctx, key, "/extcmd busy", func(event core.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Done {
			done <- event
		}
	})
	if err != nil || steered || threadID != "pi-session" {
		t.Fatalf("streaming-state Send() = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the ordinary settlement after a streaming state")
	}
	mu.Lock()
	defer mu.Unlock()
	if final.Kind != core.EventFinal || final.Text != "hello world" || final.FinalText == nil || *final.FinalText != "hello world" {
		t.Fatalf("streaming-state final = %#v, want the ordinary model turn", final)
	}
	for _, event := range events {
		if event.Kind == core.EventStatus && strings.Contains(event.Text, "native command /extcmd") {
			t.Fatalf("streaming state fabricated the no-run settlement status: %#v", event)
		}
	}
	if queries := readNativeRequests(t, logPath, "get_state"); len(queries) != 2 {
		t.Fatalf("get_state queries = %d, want the startup and settlement pair", len(queries))
	}
	if pi.IsActive(key) {
		t.Fatal("streaming-state turn remained active")
	}
}

// TestPiExtensionCommandRecognizedNegativePaths covers the negative
// recognition answers: an empty command name short-circuits without any RPC,
// and a failed get_commands query reports false so dispatch keeps the
// ordinary event-driven lifecycle.
func TestPiExtensionCommandRecognizedNegativePaths(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-native-commands")
	key := "chat:tui:recognized-negative"
	awaitPiTurn(t, ctx, pi, key, "warm the session")
	pi.mu.Lock()
	process := pi.processes[key]
	pi.mu.Unlock()
	if process == nil {
		t.Fatal("fixture did not keep a live process")
	}
	before := len(readNativeRequests(t, logPath, "get_commands"))
	if process.extensionCommandRecognized(ctx, "") {
		t.Fatal("empty command name was reported as recognized")
	}
	if after := len(readNativeRequests(t, logPath, "get_commands")); after != before {
		t.Fatalf("empty command name issued %d get_commands queries, want 0", after-before)
	}
	// Close the provider pipe so the recognition query itself fails; the
	// answer must stay false and issue no recovery.
	process.close()
	if process.extensionCommandRecognized(ctx, "extcmd") {
		t.Fatal("failed get_commands query was reported as recognized")
	}
}

// TestPiNativeCommandMalformedRecognitionKeepsOrdinaryLifecycle covers a
// get_commands response that is valid on the wire but unusable for command
// recognition: the failed recognition query reports false and the idle slash
// dispatch settles through the ordinary agent_settled lifecycle instead of
// the fabricated no-run settlement.
func TestPiNativeCommandMalformedRecognitionKeepsOrdinaryLifecycle(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-native-commands-malformed")
	key := "chat:tui:native-malformed"
	var mu sync.Mutex
	var events []core.Event
	done := make(chan core.Event, 4)
	threadID, steered, err := pi.Send(ctx, key, "/extcmd malformed", func(event core.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Done {
			done <- event
		}
	})
	if err != nil || steered || threadID != "pi-session" {
		t.Fatalf("malformed-recognition Send() = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the ordinary settlement after a malformed recognition response")
	}
	mu.Lock()
	defer mu.Unlock()
	if final.Kind != core.EventFinal || final.Text != "hello world" || final.FinalText == nil || *final.FinalText != "hello world" {
		t.Fatalf("malformed-recognition final = %#v, want the ordinary model turn", final)
	}
	for _, event := range events {
		if event.Kind == core.EventStatus && strings.Contains(event.Text, "native command /extcmd") {
			t.Fatalf("malformed recognition fabricated the no-run settlement status: %#v", event)
		}
	}
	if queries := readNativeRequests(t, logPath, "get_commands"); len(queries) != 1 {
		t.Fatalf("malformed recognition queries = %d, want 1", len(queries))
	}
}

// guards: a failed get_state query leaves the turn untouched, and a get_state
// that succeeds while the turn is no longer active settles nothing. It then
// proves the conversation still continues safely on the same session.
func TestPiSettleNativeCommandWithoutRunNegativePaths(t *testing.T) {
	pi, _, ctx := newPiNativeInputFixture(t, "pi-native-commands")
	key := "chat:tui:settle-negative"
	awaitPiTurn(t, ctx, pi, key, "warm the session")
	pi.mu.Lock()
	process := pi.processes[key]
	pi.mu.Unlock()
	if process == nil {
		t.Fatal("fixture did not keep a live process")
	}
	if pi.IsActive(key) {
		t.Fatal("fixture turn is still active before the negative settlement checks")
	}

	// A failed get_state query must return without settling anything.
	processed := &piTurn{emit: func(core.Event) {
		t.Error("settlement emitted events despite the failed get_state query")
	}}
	process.close()
	process.settleNativeCommandWithoutRun(ctx, processed, "extcmd")
	if processed.completed {
		t.Fatal("settlement completed the turn despite the failed get_state query")
	}

	// Reopen the conversation after the provider exit: the durable session
	// survives and the next ordinary turn continues the same session.
	closePiProcess(t, pi, key)
	awaitPiTurn(t, ctx, pi, key, "continue after restart")
	if pi.ThreadID(key) != "pi-session" {
		t.Fatalf("session identity after restart = %q, want pi-session", pi.ThreadID(key))
	}
	pi.mu.Lock()
	process = pi.processes[key]
	pi.mu.Unlock()
	if process == nil {
		t.Fatal("restarted conversation kept no live process")
	}
	if _, err := process.call(ctx, map[string]any{"type": "get_state"}, nil); err != nil {
		t.Fatalf("fixture get_state query failed, so the guard branch is not exercised: %v", err)
	}
	// The turn under inspection is not the process's active turn, so a
	// successful idle get_state must settle nothing and emit nothing.
	foreignEvents := make(chan core.Event, 8)
	foreign := &piTurn{emit: func(event core.Event) { foreignEvents <- event }}
	process.settleNativeCommandWithoutRun(ctx, foreign, "extcmd")
	if foreign.completed {
		t.Fatal("settlement completed a turn that the process no longer owns")
	}
	select {
	case event := <-foreignEvents:
		t.Fatalf("foreign-turn settlement emitted %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
	if pi.IsActive(key) {
		t.Fatal("negative settlement activated the idle conversation")
	}
	if pi.ThreadID(key) != "pi-session" {
		t.Fatalf("continuation session identity = %q, want pi-session", pi.ThreadID(key))
	}
}
