package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/core"
)

// isolatePiSessionRoots redirects every ordinary Pi session location to
// disposable directories and returns the effective external session root.
func isolatePiSessionRoots(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "agent"))
	external := t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", external)
	return external
}

func writeExternalPiSessionFile(t *testing.T, path, id, cwd string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	header, err := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": "2026-01-01T00:00:00.000Z", "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	content := append(header, '\n')
	content = append(content, []byte("{\"type\":\"message\",\"id\":\"a1b2c3d4\",\"parentId\":null,\"timestamp\":\"2026-01-01T00:00:01.000Z\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func piSessionTestConfig(command, root string) HarnessConfig {
	return HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")}
}

func piSessionTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPiSessionInfoIsProcessFreeAndReportsTheConfiguredCommand(t *testing.T) {
	root := t.TempDir()
	sessionsPath := filepath.Join(root, "sessions.json")
	store := `{"chat":{"id":"11111111-1111-1111-1111-111111111111","path":"/tmp/pi/one.jsonl","policy":"ordinary"}}`
	if err := os.WriteFile(sessionsPath, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	// The configured executable does not exist, so any process start would
	// fail the test rather than silently succeed.
	pi, err := NewPi(HarnessConfig{Command: filepath.Join(root, "missing-pi-binary"), Cwd: root, SessionsFile: sessionsPath})
	if err != nil {
		t.Fatal(err)
	}
	info, ok, err := pi.SessionInfo("chat")
	if err != nil || !ok {
		t.Fatalf("SessionInfo = %#v, %t, %v", info, ok, err)
	}
	if info.ID != "11111111-1111-1111-1111-111111111111" || info.Path != "/tmp/pi/one.jsonl" || info.Command != filepath.Join(root, "missing-pi-binary") {
		t.Fatalf("SessionInfo = %#v", info)
	}
	if _, ok, err := pi.SessionInfo("missing"); ok || err != nil {
		t.Fatalf("missing SessionInfo = ok %t, err %v", ok, err)
	}
}

func TestPiCompactMapsTokensAndSendsOptionalInstructions(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	done := make(chan struct{}, 1)
	if _, _, err := pi.Send(ctx, "chat", "seed", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	before, _, _ := pi.SessionInfo("chat")
	result, err := pi.CompactSession(ctx, "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.TokensBefore != 150000 || !result.TokensAfterKnown || result.TokensAfter != 32000 {
		t.Fatalf("compact result = %#v", result)
	}
	if _, err := pi.CompactSession(ctx, "chat", "focus on regressions"); err != nil {
		t.Fatal(err)
	}
	after, _, _ := pi.SessionInfo("chat")
	if after.ID != before.ID || after.Path != before.Path {
		t.Fatalf("compaction replaced the session: before %#v, after %#v", before, after)
	}
	compactions, instructed := 0, 0
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "request" || record.Method != "compact" {
			continue
		}
		compactions++
		var params struct {
			CustomInstructions string `json:"customInstructions"`
		}
		if err := json.Unmarshal(record.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.CustomInstructions != "" {
			instructed++
			if params.CustomInstructions != "focus on regressions" {
				t.Fatalf("customInstructions = %q", params.CustomInstructions)
			}
		}
	}
	if compactions != 2 || instructed != 1 {
		t.Fatalf("compact requests = %d, instructed = %d", compactions, instructed)
	}
}

func TestPiCompactWithoutEstimateReportsOnlyKnownCounts(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-compact-without-estimate")
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	done := make(chan struct{}, 1)
	if _, _, err := pi.Send(ctx, "chat", "seed", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	result, err := pi.CompactSession(ctx, "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.TokensBefore != 150000 || result.TokensAfterKnown || result.TokensAfter != 0 {
		t.Fatalf("compact result = %#v", result)
	}
}

func TestPiCompactRejections(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
		pi, err := NewPi(piSessionTestConfig(command, root))
		if err != nil {
			t.Fatal(err)
		}
		ctx := piSessionTestContext(t)
		if err := pi.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer pi.Close()
		if _, err := pi.CompactSession(ctx, "chat", ""); err == nil || !strings.Contains(err.Error(), "first ordinary prompt") {
			t.Fatalf("no-session compact error = %v", err)
		}
	})

	t.Run("active turn", func(t *testing.T) {
		command, root, _ := portableHarnessFixture(t, "pi-steer")
		pi, err := NewPi(piSessionTestConfig(command, root))
		if err != nil {
			t.Fatal(err)
		}
		ctx := piSessionTestContext(t)
		if err := pi.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer pi.Close()
		delta := make(chan struct{}, 1)
		if _, _, err := pi.Send(ctx, "chat", "hold", func(event core.Event) {
			if event.Kind == core.EventDelta {
				select {
				case delta <- struct{}{}:
				default:
				}
			}
		}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-delta:
		case <-ctx.Done():
			t.Fatal("timed out waiting for the active Pi turn")
		}
		if _, err := pi.CompactSession(ctx, "chat", ""); err == nil || !strings.Contains(err.Error(), "active") {
			t.Fatalf("active compact error = %v", err)
		}
	})

	t.Run("oversized instructions", func(t *testing.T) {
		command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
		pi, err := NewPi(piSessionTestConfig(command, root))
		if err != nil {
			t.Fatal(err)
		}
		ctx := piSessionTestContext(t)
		if err := pi.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer pi.Close()
		done := make(chan struct{}, 1)
		if _, _, err := pi.Send(ctx, "chat", "seed", func(event core.Event) {
			if event.Done {
				done <- struct{}{}
			}
		}); err != nil {
			t.Fatal(err)
		}
		<-done
		if _, err := pi.CompactSession(ctx, "chat", strings.Repeat("x", SessionCompactMaxInstructions+1)); err == nil || !strings.Contains(err.Error(), "at most") {
			t.Fatalf("oversized compact error = %v", err)
		}
		for _, record := range readFixtureRecords(t, logPath) {
			if record.Kind == "request" && record.Method == "compact" {
				t.Fatal("oversized instructions reached the provider")
			}
		}
	})

	t.Run("missing stored file", func(t *testing.T) {
		command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
		sessionsPath := filepath.Join(root, "sessions.json")
		store, err := json.Marshal(map[string]piSession{"chat": {ID: "11111111-1111-1111-1111-111111111111", Path: filepath.Join(root, "missing.jsonl"), Policy: "ordinary"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionsPath, store, 0o600); err != nil {
			t.Fatal(err)
		}
		pi, err := NewPi(piSessionTestConfig(command, root))
		if err != nil {
			t.Fatal(err)
		}
		ctx := piSessionTestContext(t)
		if err := pi.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer pi.Close()
		if _, err := pi.CompactSession(ctx, "chat", ""); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("missing-file compact error = %v", err)
		}
	})
}

func TestPiCompactLazilyResumesThePersistedSession(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	config := piSessionTestConfig(command, root)
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	if _, _, err := pi.Send(ctx, "chat", "seed", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := pi.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	result, err := restarted.CompactSession(ctx, "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.TokensBefore != 150000 {
		t.Fatalf("resumed compact result = %#v", result)
	}
	resumed := false
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" && containsArgument(record.Args, "--session") {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("compaction did not resume the persisted session file")
	}
}

func TestPiImportForksExternalSessionAndResumesAfterRestart(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	external := isolatePiSessionRoots(t)
	sourceID := "11111111-2222-3333-4444-555555555555"
	sourcePath := filepath.Join(external, "direct-session.jsonl")
	writeExternalPiSessionFile(t, sourcePath, sourceID, root)
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	config := piSessionTestConfig(command, root)
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := pi.ImportSession(ctx, "chat", sourceID)
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(root, "pi-sessions")
	if info.ID == "" || info.ID == sourceID {
		t.Fatalf("imported session identity = %#v", info)
	}
	if !piPathWithin(sessionDir, info.Path) || samePiPath(info.Path, sourcePath) {
		t.Fatalf("imported session path = %q, want inside %q and distinct from the source", info.Path, sessionDir)
	}
	after, err := os.Stat(sourcePath)
	if err != nil || !piSourceUnchanged(before, after) {
		t.Fatalf("source session changed during import: %v", err)
	}
	if persisted, err := NewPi(config); err != nil || persisted.ThreadID("chat") != info.ID {
		t.Fatalf("persisted import = %q, %v", persisted.ThreadID("chat"), err)
	}
	forked := false
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "invocation" || containsArgument(record.Args, "--version") {
			continue
		}
		if containsArgument(record.Args, "--fork") && containsArgument(record.Args, filepath.Clean(sourcePath)) && containsArgument(record.Args, sessionDir) {
			forked = true
		}
	}
	if !forked {
		t.Fatal("import did not fork the validated source path with the Spynel session directory")
	}
	if err := pi.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.ThreadID("chat") != info.ID {
		t.Fatalf("restarted session = %q, want %q", restarted.ThreadID("chat"), info.ID)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	done := make(chan struct{}, 1)
	if _, _, err := restarted.Send(ctx, "chat", "continue", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	resumed := false
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" && containsArgument(record.Args, "--session") && containsArgument(record.Args, info.Path) {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("restart did not resume the imported fork")
	}
}

func TestPiImportRejectsUnsafeSources(t *testing.T) {
	const sourceID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	tests := []struct {
		name    string
		prepare func(t *testing.T, external, cwd string) string
		want    string
	}{
		{
			name:    "partial id",
			prepare: func(*testing.T, string, string) string { return "aaaaaaaa-bbbb" },
			want:    "full canonical session UUID",
		},
		{
			name:    "unknown id",
			prepare: func(*testing.T, string, string) string { return sourceID },
			want:    "no direct Pi session",
		},
		{
			name: "malformed header",
			prepare: func(t *testing.T, external, _ string) string {
				if err := os.WriteFile(filepath.Join(external, sourceID+".jsonl"), []byte("not a header\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return sourceID
			},
			want: "no direct Pi session",
		},
		{
			name: "ambiguous id",
			prepare: func(t *testing.T, external, cwd string) string {
				writeExternalPiSessionFile(t, filepath.Join(external, "one.jsonl"), sourceID, cwd)
				writeExternalPiSessionFile(t, filepath.Join(external, "two.jsonl"), sourceID, cwd)
				return sourceID
			},
			want: "multiple direct Pi sessions",
		},
		{
			name: "oversized header",
			prepare: func(t *testing.T, external, cwd string) string {
				line := `{"type":"session","version":3,"id":"` + sourceID + `","cwd":"` + cwd + `","padding":"` + strings.Repeat("x", piSessionHeaderMaxBytes+1) + `"}` + "\n"
				if err := os.WriteFile(filepath.Join(external, sourceID+".jsonl"), []byte(line), 0o600); err != nil {
					t.Fatal(err)
				}
				return sourceID
			},
			want: "no direct Pi session",
		},
		{
			name: "wrong cwd",
			prepare: func(t *testing.T, external, _ string) string {
				writeExternalPiSessionFile(t, filepath.Join(external, sourceID+".jsonl"), sourceID, filepath.Join(t.TempDir(), "other project"))
				return sourceID
			},
			want: "different workspace",
		},
		{
			name: "nonregular file",
			prepare: func(t *testing.T, external, _ string) string {
				if err := os.MkdirAll(filepath.Join(external, sourceID+".jsonl"), 0o700); err != nil {
					t.Fatal(err)
				}
				return sourceID
			},
			want: "no direct Pi session",
		},
		{
			name: "symlinked file",
			prepare: func(t *testing.T, external, cwd string) string {
				target := filepath.Join(t.TempDir(), "target.jsonl")
				writeExternalPiSessionFile(t, target, sourceID, cwd)
				if err := os.Symlink(target, filepath.Join(external, sourceID+".jsonl")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return sourceID
			},
			want: "no direct Pi session",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
			external := isolatePiSessionRoots(t)
			sessionID := test.prepare(t, external, root)
			pi, err := NewPi(piSessionTestConfig(command, root))
			if err != nil {
				t.Fatal(err)
			}
			ctx := piSessionTestContext(t)
			if err := pi.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer pi.Close()
			if _, err := pi.ImportSession(ctx, "chat", sessionID); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ImportSession error = %v, want %q", err, test.want)
			}
			pi.mu.Lock()
			_, hasSession := pi.sessions["chat"]
			process := pi.processes["chat"]
			pi.mu.Unlock()
			if hasSession || process != nil {
				t.Fatalf("rejected import left session=%t process=%t", hasSession, process != nil)
			}
		})
	}
}

func TestPiImportChangingSourceFailsClosedAndCleansUpTheFork(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-import-changing")
	external := isolatePiSessionRoots(t)
	sourceID := "12345678-1234-1234-1234-123456789abc"
	sourcePath := filepath.Join(external, "source.jsonl")
	writeExternalPiSessionFile(t, sourcePath, sourceID, root)
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	if _, err := pi.ImportSession(ctx, "chat", sourceID); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changing-source import error = %v", err)
	}
	pi.mu.Lock()
	_, hasSession := pi.sessions["chat"]
	process := pi.processes["chat"]
	pi.mu.Unlock()
	if hasSession || process != nil {
		t.Fatalf("changing import left session=%t process=%t", hasSession, process != nil)
	}
	entries, err := os.ReadDir(filepath.Join(root, "pi-sessions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("changing import left orphan session files: %v", entries)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("source session was removed: %v", err)
	}
}

func TestPiSafeErrorTextRedactsKnownSessionPathsAndControls(t *testing.T) {
	path := "/home/person/.pi/agent/sessions/--secret--/private.jsonl"
	text := SafeControlErrorText(errors.New("failed to compact "+path+"\nprovider detail\x00"), filepath.Dir(path), path)
	if strings.Contains(text, path) || strings.ContainsAny(text, "\n\x00") {
		t.Fatalf("sanitized error = %q", text)
	}
	if !strings.Contains(text, "<session>") || !strings.Contains(text, "provider detail") {
		t.Fatalf("sanitized error lost context: %q", text)
	}
	if got := SafeControlErrorText(errors.New(strings.Repeat("x", ControlErrorMaxRunes+50))); len([]rune(got)) != ControlErrorMaxRunes+3 {
		t.Fatalf("bounded error length = %d", len([]rune(got)))
	}
}

func TestPiImportRefusesAnExistingConversationSession(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
	sessionsPath := filepath.Join(root, "sessions.json")
	store, err := json.Marshal(map[string]piSession{"chat": {ID: "11111111-1111-1111-1111-111111111111", Path: filepath.Join(root, "existing.jsonl"), Policy: "ordinary"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionsPath, store, 0o600); err != nil {
		t.Fatal(err)
	}
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	if _, err := pi.ImportSession(ctx, "chat", "11111111-2222-3333-4444-555555555555"); err == nil || !strings.Contains(err.Error(), "/clear") {
		t.Fatalf("existing-session import error = %v", err)
	}
}

func piFixtureInvocationCount(t *testing.T, logPath string) int {
	t.Helper()
	count := 0
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" && !containsArgument(record.Args, "--version") {
			count++
		}
	}
	return count
}

func TestPiCompactRejectsPolicyMismatchWithoutChangingStoredSession(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	config := piSessionTestConfig(command, root)
	config.Model = "fixture/model-a"
	config.Effort = "high"
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	if _, _, err := pi.Send(ctx, "chat", "seed", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	pi.mu.Lock()
	stored := pi.sessions["chat"]
	pi.mu.Unlock()
	if stored.Policy == "" {
		t.Fatal("seeded session carries no policy")
	}
	if err := pi.Close(); err != nil {
		t.Fatal(err)
	}
	invocationsBefore := piFixtureInvocationCount(t, logPath)

	changed := config
	changed.Model = "fixture/model-max"
	restarted, err := NewPi(changed)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.CompactSession(ctx, "chat", ""); err == nil || !strings.Contains(err.Error(), "different harness configuration") || !strings.Contains(err.Error(), "ordinary prompt") {
		t.Fatalf("policy-mismatch compact error = %v", err)
	}
	restarted.mu.Lock()
	unchanged := restarted.sessions["chat"]
	process := restarted.processes["chat"]
	restarted.mu.Unlock()
	if unchanged != stored || process != nil {
		t.Fatalf("policy-mismatch compact changed the stored session: %#v, process %t", unchanged, process != nil)
	}
	if after := piFixtureInvocationCount(t, logPath); after != invocationsBefore {
		t.Fatalf("policy-mismatch compact attempted a resume: invocations %d -> %d", invocationsBefore, after)
	}
	data, err := os.ReadFile(config.SessionsFile)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]piSession
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["chat"] != stored {
		t.Fatalf("persisted session changed on disk: %#v", persisted["chat"])
	}
}

func TestPiCompactReusesLiveIdleProcess(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	done := make(chan struct{}, 1)
	if _, _, err := pi.Send(ctx, "chat", "seed", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	pi.mu.Lock()
	before := pi.processes["chat"]
	pi.mu.Unlock()
	if before == nil {
		t.Fatal("completed turn released its live Pi process")
	}
	result, err := pi.CompactSession(ctx, "chat", "")
	if err != nil || result.TokensBefore != 150000 {
		t.Fatalf("idle compact = %#v, %v", result, err)
	}
	pi.mu.Lock()
	after := pi.processes["chat"]
	pi.mu.Unlock()
	if after != before {
		t.Fatal("compact replaced the live idle Pi process")
	}
	resumed, invocations := 0, 0
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "invocation" || containsArgument(record.Args, "--version") {
			continue
		}
		invocations++
		if containsArgument(record.Args, "--session") {
			resumed++
		}
	}
	if invocations != 1 || resumed != 0 {
		t.Fatalf("idle compact launched a new process: %d invocations, %d resumes", invocations, resumed)
	}
}

func TestPiImportIsSerializedAgainstAnActiveTurn(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-steer")
	external := isolatePiSessionRoots(t)
	sourceID := "77777777-8888-9999-aaaa-bbbbbbbbbbbb"
	writeExternalPiSessionFile(t, filepath.Join(external, "direct.jsonl"), sourceID, root)
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	delta := make(chan struct{}, 1)
	if _, _, err := pi.Send(ctx, "chat", "hold", func(event core.Event) {
		if event.Kind == core.EventDelta {
			select {
			case delta <- struct{}{}:
			default:
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-delta:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the active Pi turn")
	}
	pi.mu.Lock()
	before := pi.sessions["chat"]
	pi.mu.Unlock()
	if _, err := pi.ImportSession(ctx, "chat", sourceID); err == nil {
		t.Fatal("import succeeded while a turn was active")
	} else if !strings.Contains(err.Error(), "/clear") && !strings.Contains(err.Error(), "active") {
		t.Fatalf("active import error = %v", err)
	}
	// The refused import must not disturb the live turn; steering it still
	// finishes with the provider's own result and the original mapping.
	second := make(chan core.Event, 16)
	if _, steered, err := pi.Send(ctx, "chat", "second", func(event core.Event) { second <- event }); err != nil || !steered {
		t.Fatalf("steered Pi send = steered %t, %v", steered, err)
	}
	var final core.Event
	for !final.Done {
		select {
		case event := <-second:
			if event.Done {
				final = event
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the steered Pi turn")
		}
	}
	if final.Kind != core.EventFinal || final.Text != "first second" {
		t.Fatalf("steered Pi final = %#v", final)
	}
	pi.mu.Lock()
	after := pi.sessions["chat"]
	pi.mu.Unlock()
	if after != before {
		t.Fatalf("refused import changed the stored session: %#v -> %#v", before, after)
	}
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" && containsArgument(record.Args, "--fork") {
			t.Fatal("refused import still launched a fork process")
		}
	}
}

func TestPiImportRollsBackWhenPersistenceFails(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
	external := isolatePiSessionRoots(t)
	sourceID := "66666666-5555-4444-3333-222222222222"
	sourcePath := filepath.Join(external, "source.jsonl")
	writeExternalPiSessionFile(t, sourcePath, sourceID, root)
	config := piSessionTestConfig(command, root)
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	// A directory at the session-map path lets the fork succeed and then makes
	// the atomic replace fail, exercising rollback after a real fork.
	blocker := filepath.Join(root, "sessions-dir")
	if err := os.MkdirAll(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	pi.mu.Lock()
	pi.config.SessionsFile = blocker
	pi.mu.Unlock()
	_, importErr := pi.ImportSession(ctx, "chat", sourceID)
	if importErr == nil || !strings.Contains(importErr.Error(), "persist the imported Pi session") {
		t.Fatalf("persistence-failure import error = %v", importErr)
	}
	if strings.Contains(importErr.Error(), root) || strings.ContainsAny(importErr.Error(), "\n\x00") {
		t.Fatalf("persistence-failure import leaked a path or control character: %q", importErr.Error())
	}
	pi.mu.Lock()
	_, hasSession := pi.sessions["chat"]
	process := pi.processes["chat"]
	pi.mu.Unlock()
	if hasSession || process != nil {
		t.Fatalf("failed persistence left session=%t process=%t", hasSession, process != nil)
	}
	entries, err := os.ReadDir(filepath.Join(root, "pi-sessions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed persistence left fork files: %v", entries)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("source session was removed: %v", err)
	}
}

func TestPiImportPreservesPreExistingSiblingSession(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-import-preexisting")
	external := isolatePiSessionRoots(t)
	sourceID := "abcdefab-1234-5678-9abc-def012345678"
	sourcePath := filepath.Join(external, "source.jsonl")
	writeExternalPiSessionFile(t, sourcePath, sourceID, root)
	sessionDir := filepath.Join(root, "pi-sessions")
	sibling := filepath.Join(sessionDir, "pi-fixture-preexisting.jsonl")
	writeExternalPiSessionFile(t, sibling, "99999999-9999-9999-9999-999999999999", root)
	before, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatal(err)
	}
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	if _, err := pi.ImportSession(ctx, "chat", sourceID); err == nil || !strings.Contains(err.Error(), "already existed") {
		t.Fatalf("hostile fork error = %v", err)
	}
	after, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatalf("pre-existing sibling session was deleted: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("pre-existing sibling session was modified: %q -> %q", before, after)
	}
	pi.mu.Lock()
	_, hasSession := pi.sessions["chat"]
	pi.mu.Unlock()
	if hasSession {
		t.Fatal("hostile fork persisted a session mapping")
	}
}

func TestPiImportLeavesUnreportedForkOrphan(t *testing.T) {
	// Pi always creates the fork file before reporting it through get_state. A
	// failure before that report leaves the adapter with no path to remove, and
	// guessing one would risk deleting another conversation's session. This
	// test documents that unavoidable inert orphan and proves the mapping stays
	// clean.
	command, root, _ := portableHarnessFixture(t, "pi-import-nopath")
	external := isolatePiSessionRoots(t)
	sourceID := "12312312-4567-89ab-cdef-0123456789ab"
	writeExternalPiSessionFile(t, filepath.Join(external, "source.jsonl"), sourceID, root)
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	if _, err := pi.ImportSession(ctx, "chat", sourceID); err == nil || !strings.Contains(err.Error(), "incompatible get_state") {
		t.Fatalf("unreported-fork import error = %v", err)
	}
	orphan := filepath.Join(root, "pi-sessions", "pi-imported-session.jsonl")
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("fixture did not leave the documented orphan: %v", err)
	}
	pi.mu.Lock()
	_, hasSession := pi.sessions["chat"]
	process := pi.processes["chat"]
	pi.mu.Unlock()
	if hasSession || process != nil {
		t.Fatalf("unreported-fork import left session=%t process=%t", hasSession, process != nil)
	}
}

func containsPiPath(values []string, wanted string) bool {
	for _, value := range values {
		if samePiPath(value, wanted) {
			return true
		}
	}
	return false
}

func TestPiSessionLookupEnforcesDirectoryEntryBudget(t *testing.T) {
	external := isolatePiSessionRoots(t)
	cwd := t.TempDir()
	for index := 0; index <= piSessionScanMaxDirectoryEntries; index++ {
		if err := os.WriteFile(filepath.Join(external, fmt.Sprintf("entry-%05d.txt", index)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := resolveExternalPiSession("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", cwd, nil)
	if !errors.Is(err, errPiSessionScanLimit) {
		t.Fatalf("directory entry budget error = %v", err)
	}
}

func TestPiSessionLookupEnforcesGlobalFileBudget(t *testing.T) {
	external := isolatePiSessionRoots(t)
	cwd := t.TempDir()
	for directory := 0; directory < 3; directory++ {
		dir := filepath.Join(external, fmt.Sprintf("batch-%d", directory))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 1400; index++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%05d.txt", index)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if 3*1400 <= piSessionScanMaxFiles {
		t.Fatalf("test setup no longer exceeds the global file budget")
	}
	_, err := resolveExternalPiSession("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", cwd, nil)
	if !errors.Is(err, errPiSessionScanLimit) {
		t.Fatalf("global file budget error = %v", err)
	}
}

func TestPiSessionLookupHonorsDepthBudget(t *testing.T) {
	const sessionID = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	nested := func(t *testing.T, depth int) string {
		t.Helper()
		external := isolatePiSessionRoots(t)
		dir := external
		for index := 1; index <= depth; index++ {
			dir = filepath.Join(dir, fmt.Sprintf("d%d", index))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	t.Run("within budget", func(t *testing.T) {
		cwd := t.TempDir()
		dir := nested(t, piSessionScanMaxDepth-1)
		writeExternalPiSessionFile(t, filepath.Join(dir, "session.jsonl"), sessionID, cwd)
		source, err := resolveExternalPiSession(sessionID, cwd, nil)
		if err != nil || source.ID != sessionID {
			t.Fatalf("within-depth lookup = %#v, %v", source, err)
		}
	})
	t.Run("beyond budget", func(t *testing.T) {
		cwd := t.TempDir()
		dir := nested(t, piSessionScanMaxDepth)
		writeExternalPiSessionFile(t, filepath.Join(dir, "session.jsonl"), sessionID, cwd)
		if _, err := resolveExternalPiSession(sessionID, cwd, nil); err == nil || !strings.Contains(err.Error(), "no direct Pi session") {
			t.Fatalf("beyond-depth lookup error = %v", err)
		}
	})
}

func TestPiSessionLookupFailsClosedOnUnreadableRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not follow POSIX semantics on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	external := isolatePiSessionRoots(t)
	cwd := t.TempDir()
	if err := os.Chmod(external, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(external, 0o700) })
	_, err := resolveExternalPiSession("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", cwd, nil)
	if err == nil || !strings.Contains(err.Error(), "could not read a candidate session directory") {
		t.Fatalf("unreadable-root lookup error = %v", err)
	}
}

func TestPiEffectiveEnvLookupAppliesOverridesLast(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "inherited")
	lookup := piEffectiveEnvLookup([]string{"PI_CODING_AGENT_DIR=first", "PI_CODING_AGENT_DIR=second"})
	if got := lookup("PI_CODING_AGENT_DIR"); got != "second" {
		t.Fatalf("override value = %q, want second", got)
	}
	if got := lookup("PI_CODING_AGENT_SESSION_DIR"); got != "" {
		t.Fatalf("unset value = %q, want empty", got)
	}
}

func TestPiCandidateSessionRootsResolveGlobalAndProjectSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "agent"))
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	cwd := t.TempDir()
	lookup := piEffectiveEnvLookup(nil)
	globalSettings := filepath.Join(home, "agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(globalSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalSettings, []byte(`{"sessionDir":"~/global-sessions"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := piCandidateSessionRoots(cwd, lookup)
	if !containsPiPath(roots, filepath.Join(home, "global-sessions")) {
		t.Fatalf("global tilde sessionDir missing from roots: %v", roots)
	}

	projectSettings := filepath.Join(cwd, ".pi", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projectSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectSettings, []byte(`{"sessionDir":"project-sessions"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	roots = piCandidateSessionRoots(cwd, lookup)
	if !containsPiPath(roots, filepath.Join(cwd, "project-sessions")) {
		t.Fatalf("project relative sessionDir missing from roots: %v", roots)
	}
	if containsPiPath(roots, filepath.Join(home, "global-sessions")) {
		t.Fatalf("project settings did not override global settings: %v", roots)
	}
}

func TestPiCandidateSessionRootsResolveRelativeAgentDirFromCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	cwd := t.TempDir()
	lookup := piEffectiveEnvLookup([]string{"PI_CODING_AGENT_DIR=relative-agent"})
	settings := filepath.Join(cwd, "relative-agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"sessionDir":"agent-relative"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := piCandidateSessionRoots(cwd, lookup)
	if !containsPiPath(roots, filepath.Join(cwd, "agent-relative")) {
		t.Fatalf("relative agent settings sessionDir missing from roots: %v", roots)
	}
	if !containsPiPath(roots, filepath.Join(cwd, "relative-agent", "sessions")) {
		t.Fatalf("relative agent default sessions dir missing from roots: %v", roots)
	}
}

func TestPiImportHonorsHarnessConfigEnvironment(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
	inherited := t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", inherited)
	external := t.TempDir()
	sourceID := "cccccccc-dddd-eeee-ffff-000000000000"
	writeExternalPiSessionFile(t, filepath.Join(external, "source.jsonl"), sourceID, root)
	config := piSessionTestConfig(command, root)
	config.Env = []string{"PI_CODING_AGENT_SESSION_DIR=" + inherited, "PI_CODING_AGENT_SESSION_DIR=" + external}
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	if _, err := pi.ImportSession(ctx, "chat", sourceID); err != nil {
		t.Fatalf("config-env import error = %v", err)
	}
}

func TestPiImportMkdirFailureRedactsSessionDirectory(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-lifecycle")
	external := isolatePiSessionRoots(t)
	sourceID := "dddddddd-eeee-ffff-0000-111111111111"
	writeExternalPiSessionFile(t, filepath.Join(external, "source.jsonl"), sourceID, root)
	// A regular file at the session-directory path makes os.MkdirAll fail with
	// the exact workspace path in its error.
	blocker := filepath.Join(root, "pi-sessions")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	pi, err := NewPi(piSessionTestConfig(command, root))
	if err != nil {
		t.Fatal(err)
	}
	ctx := piSessionTestContext(t)
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	_, importErr := pi.ImportSession(ctx, "chat", sourceID)
	if importErr == nil || !strings.Contains(importErr.Error(), "prepare the Spynel Pi session directory") {
		t.Fatalf("mkdir-failure import error = %v", importErr)
	}
	if strings.Contains(importErr.Error(), root) || strings.ContainsAny(importErr.Error(), "\n\x00") {
		t.Fatalf("mkdir failure leaked a workspace path or control character: %q", importErr.Error())
	}
}

func TestPiSafeControlErrorPreservesSentinelIdentity(t *testing.T) {
	err := piSafeControlError(ErrSessionControlsUnsupported, "/secret/session/path")
	if !errors.Is(err, ErrSessionControlsUnsupported) {
		t.Fatalf("sanitized error lost the unsupported sentinel: %v", err)
	}
	if strings.Contains(err.Error(), "/secret/session/path") {
		t.Fatalf("sanitized error kept a sensitive path: %q", err.Error())
	}
}
