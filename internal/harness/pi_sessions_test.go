package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		if _, err := pi.CompactSession(ctx, "chat", strings.Repeat("x", piCompactMaxInstructions+1)); err == nil || !strings.Contains(err.Error(), "at most") {
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
	text := piSafeErrorText(errors.New("failed to compact "+path+"\nprovider detail\x00"), path)
	if strings.Contains(text, path) || strings.ContainsAny(text, "\n\x00") {
		t.Fatalf("sanitized error = %q", text)
	}
	if !strings.Contains(text, "<session>") || !strings.Contains(text, "provider detail") {
		t.Fatalf("sanitized error lost context: %q", text)
	}
	if got := piSafeErrorText(errors.New(strings.Repeat("x", piControlErrorMaxRunes+50))); len([]rune(got)) != piControlErrorMaxRunes+3 {
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
