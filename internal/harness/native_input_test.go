package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/core"
)

// piTelegramNoteExpectedSentence is the exact note sentence the embedded
// Telegram extension must carry exactly once.
const piTelegramNoteExpectedSentence = "The user chats over Telegram and sees only your final replies, never intermediate progress. If you need clarification, approval, or support, ask in one final reply and end your turn; the user's next message continues this same conversation session."

func TestPiNativeConversationInputFollowsOrdinaryChatGrammar(t *testing.T) {
	pi := &Pi{}
	tests := []struct {
		key  string
		want bool
	}{
		{"chat:telegram:TG-7", true},
		{"chat:telegram:TG-7-topic-5", true},
		{"chat:telegram:TG-group-9-topic-2", true},
		{"chat:tui:0f0e0d0c-1111-2222-3333-444455556666", true},
		{"chat:whatsapp:WA-1", true},
		{"chat:cli:local", true},
		{"", false},
		{"orchestrator:semantic-heartbeat", false},
		{"n:TG-7", false},
		{"chatt:telegram:TG-7", false},
		{"chat:", false},
		{"chat:telegram", false},
		{"chat:telegram:", false},
		{"chat::conv", false},
		{"chat:telegram:TG-7:extra", false},
	}
	for _, test := range tests {
		if got := pi.NativeConversationInput(test.key); got != test.want {
			t.Fatalf("Pi.NativeConversationInput(%q) = %v, want %v", test.key, got, test.want)
		}
	}
}

type nativeInputSupervisorHarness struct {
	*supervisorHarness
	mu   sync.Mutex
	key  string
	note bool
}

func (n *nativeInputSupervisorHarness) NativeConversationInput(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.key = key
	return n.note
}

func TestSupervisorForwardsNativeConversationInputCapability(t *testing.T) {
	target := &nativeInputSupervisorHarness{
		supervisorHarness: &supervisorHarness{name: "pi", active: map[string]bool{}, emits: map[string]core.Emit{}},
		note:              true,
	}
	registry := NewRegistry()
	registry.Register("pi", func(HarnessConfig) (Harness, error) { return target, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "pi"})
	if supervisor.NativeConversationInput("chat:telegram:TG-7") {
		t.Fatal("unstarted supervisor reported native conversation input")
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !supervisor.NativeConversationInput("chat:telegram:TG-7") {
		t.Fatal("capability was not forwarded to the active adapter")
	}
	target.mu.Lock()
	key := target.key
	target.mu.Unlock()
	if key != "chat:telegram:TG-7" {
		t.Fatalf("forwarded capability key = %q", key)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	if supervisor.NativeConversationInput("chat:telegram:TG-7") {
		t.Fatal("closed supervisor reported native conversation input")
	}
}

func TestSupervisorNativeConversationInputFailsClosedWithoutCapability(t *testing.T) {
	plain := &supervisorHarness{name: "claude-code", active: map[string]bool{}, emits: map[string]core.Emit{}}
	registry := NewRegistry()
	registry.Register("claude-code", func(HarnessConfig) (Harness, error) { return plain, nil })
	supervisor := NewSupervisor(registry, HarnessConfig{Name: "claude-code"})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if supervisor.NativeConversationInput("chat:telegram:TG-7") {
		t.Fatal("harness without the capability reported native conversation input")
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
		t.Fatal("unavailable harness started unexpectedly")
	}
	if down.NativeConversationInput("chat:telegram:TG-7") {
		t.Fatal("unavailable supervisor reported native conversation input")
	}
	_ = down.Close()
}

// countArgument reports how often one exact argument appears in argv.
func countArgument(arguments []string, wanted string) int {
	count := 0
	for _, argument := range arguments {
		if argument == wanted {
			count++
		}
	}
	return count
}

// assertSingleTelegramNoteExtension verifies exactly one additive
// --extension argument carrying the materialized Telegram note extension
// and that the suppressed --append-system-prompt flag never returned.
func assertSingleTelegramNoteExtension(t *testing.T, args []string) {
	t.Helper()
	if count := countArgument(args, "--append-system-prompt"); count != 0 {
		t.Fatalf("launch argv still carried %d --append-system-prompt flags: %q", count, args)
	}
	if count := countArgument(args, "--extension"); count != 1 {
		t.Fatalf("launch argv carried %d --extension flags, want exactly the one Telegram note extension: %q", count, args)
	}
	for index, argument := range args {
		if argument == "--extension" {
			if index+1 >= len(args) || args[index+1] == "" {
				t.Fatalf("launch argv carried a dangling --extension argument: %q", args)
			}
			assertTelegramNoteExtensionFile(t, args[index+1])
		}
	}
}

// assertTelegramNoteExtensionFile asserts the materialized extension is a
// private regular file holding exactly the embedded source with the section
// name and exactly one note sentence.
func assertTelegramNoteExtensionFile(t *testing.T, path string) {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatalf("launch argv extension path %q is not absolute", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("materialized Telegram note extension %q: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("materialized Telegram note extension %q is not a regular file: %v", path, info.Mode())
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("materialized Telegram note extension %q is not private: %v", path, info.Mode().Perm())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialized Telegram note extension %q: %v", path, err)
	}
	if !bytes.Equal(content, piTelegramNoteExtensionSource) {
		t.Fatalf("materialized Telegram note extension %q does not hold the embedded source", path)
	}
	if !strings.Contains(string(content), piTelegramNoteSectionName) {
		t.Fatalf("materialized Telegram note extension %q lacks the %q section", path, piTelegramNoteSectionName)
	}
	if count := strings.Count(string(content), piTelegramNoteExpectedSentence); count != 1 {
		t.Fatalf("materialized Telegram note extension %q carries the note sentence %d times, want exactly 1", path, count)
	}
}

// launchInvocations returns every fixture invocation record except the
// --version capability check.
func launchInvocations(t *testing.T, logPath string) []fixtureRecord {
	t.Helper()
	var launches []fixtureRecord
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "invocation" || containsArgument(record.Args, "--version") {
			continue
		}
		launches = append(launches, record)
	}
	return launches
}

// newPiNativeInputFixture starts a Pi adapter against one portable fixture
// provider and returns the fixture evidence log path for argv inspection.
func newPiNativeInputFixture(t *testing.T, mode string) (*Pi, string, context.Context) {
	t.Helper()
	command, root, logPath := portableHarnessFixture(t, mode)
	config := HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")}
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
	return pi, logPath, ctx
}

func TestPiTelegramLaunchesCarryOneNoteExtensionAcrossRestarts(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-lifecycle")
	key := "chat:telegram:TG-7-topic-5"
	awaitPiTurn(t, ctx, pi, key, "first")
	sessionID := pi.ThreadID(key)
	if sessionID == "" {
		t.Fatal("fixture did not create a Telegram conversation session")
	}
	pi.mu.Lock()
	storedPath := pi.sessions[key].Path
	pi.mu.Unlock()
	launches := launchInvocations(t, logPath)
	if len(launches) != 1 {
		t.Fatalf("fresh turn produced %d process launches", len(launches))
	}
	if containsArgument(launches[0].Args, "--session") {
		t.Fatalf("fresh launch unexpectedly resumed a session: %q", launches[0].Args)
	}
	assertSingleTelegramNoteExtension(t, launches[0].Args)
	// Simulate a provider process exit: the durable session survives and the
	// next ordinary prompt must resume it under the same note.
	pi.mu.Lock()
	process := pi.processes[key]
	delete(pi.processes, key)
	pi.mu.Unlock()
	if process == nil {
		t.Fatal("fixture did not keep a live Telegram process")
	}
	process.close()
	awaitPiTurn(t, ctx, pi, key, "second")
	if pi.ThreadID(key) != sessionID {
		t.Fatalf("resumed Telegram session = %q, want %q", pi.ThreadID(key), sessionID)
	}
	launches = launchInvocations(t, logPath)
	if len(launches) != 2 {
		t.Fatalf("restart produced %d process launches", len(launches))
	}
	resumed := launches[1]
	index := -1
	for position, argument := range resumed.Args {
		if argument == "--session" && position+1 < len(resumed.Args) {
			index = position + 1
		}
	}
	if index < 0 || !samePiPath(resumed.Args[index], storedPath) {
		t.Fatalf("restart did not resume the stored session path %q: %q", storedPath, resumed.Args)
	}
	assertSingleTelegramNoteExtension(t, resumed.Args)
}

func TestPiNonTelegramLaunchesOmitNoteExtension(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-lifecycle")
	for _, key := range []string{"chat:tui:0f0e0d0c-1111-2222-3333-444455556666", "chat:whatsapp:WA-1"} {
		awaitPiTurn(t, ctx, pi, key, "hello")
		if pi.ThreadID(key) == "" {
			t.Fatalf("fixture did not create a session for %q", key)
		}
	}
	for _, record := range launchInvocations(t, logPath) {
		if count := countArgument(record.Args, "--extension"); count != 0 {
			t.Fatalf("launch argv carried %d --extension flags outside Telegram: %q", count, record.Args)
		}
	}
}

func TestPiTelegramImportCarriesNoteExtension(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
		note bool
	}{
		{name: "telegram", key: "chat:telegram:TG-9", note: true},
		{name: "tui", key: "chat:tui:import-target", note: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pi, logPath, ctx := newPiNativeInputFixture(t, "pi-lifecycle")
			external := isolatePiSessionRoots(t)
			sourceID := "11111111-2222-3333-4444-555555555555"
			sourcePath := filepath.Join(external, "direct-session.jsonl")
			writeExternalPiSessionFile(t, sourcePath, sourceID, pi.config.Cwd)
			info, err := pi.ImportSession(ctx, test.key, sourceID)
			if err != nil {
				t.Fatal(err)
			}
			if info.ID == "" || info.ID == sourceID {
				t.Fatalf("imported session identity = %#v", info)
			}
			if pi.ThreadID(test.key) != info.ID {
				t.Fatalf("imported session continuity lost: live %q vs imported %q", pi.ThreadID(test.key), info.ID)
			}
			forked := 0
			for _, record := range launchInvocations(t, logPath) {
				if !containsArgument(record.Args, "--fork") {
					continue
				}
				forked++
				if !test.note {
					if count := countArgument(record.Args, "--extension"); count != 0 {
						t.Fatalf("fork launch argv carried %d --extension flags outside Telegram: %q", count, record.Args)
					}
					continue
				}
				assertSingleTelegramNoteExtension(t, record.Args)
			}
			if forked != 1 {
				t.Fatalf("import produced %d fork launches", forked)
			}
			if persisted, err := NewPi(pi.config); err != nil || persisted.ThreadID(test.key) != info.ID {
				t.Fatalf("persisted import = %q, %v", persisted.ThreadID(test.key), err)
			}
		})
	}
}

// readNativeRequests returns every fixture request record with the given RPC
// method, decoded into its raw parameter form.
func readNativeRequests(t *testing.T, logPath, method string) []fixtureRecord {
	t.Helper()
	var requests []fixtureRecord
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "request" && record.Method == method {
			requests = append(requests, record)
		}
	}
	return requests
}

// nativePromptRequest decodes one logged prompt request into its message text
// and streaming behavior.
func nativePromptRequest(t *testing.T, record fixtureRecord) (text, streamingBehavior string) {
	t.Helper()
	var params struct {
		Message           string `json:"message"`
		StreamingBehavior string `json:"streamingBehavior"`
	}
	if err := json.Unmarshal(record.Params, &params); err != nil {
		t.Fatalf("decode fixture prompt request: %v", err)
	}
	return params.Message, params.StreamingBehavior
}

// closePiProcess simulates a provider process exit while the durable session
// map survives, so the next dispatch must resume the retained session.
func closePiProcess(t *testing.T, pi *Pi, key string) string {
	t.Helper()
	pi.mu.Lock()
	process := pi.processes[key]
	stored := pi.sessions[key].Path
	delete(pi.processes, key)
	pi.mu.Unlock()
	if process == nil || stored == "" {
		t.Fatal("fixture did not keep a live process with a stored session")
	}
	process.close()
	return stored
}

func TestPiIdleNativeExtensionCommandSettlesWithoutModelRun(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-native-commands")
	key := "chat:tui:native"
	var mu sync.Mutex
	var events []core.Event
	done := make(chan core.Event, 4)
	threadID, steered, err := pi.Send(ctx, key, "/extcmd one two", func(event core.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Done {
			done <- event
		}
	})
	if err != nil || steered || threadID != "pi-session" {
		t.Fatalf("native no-run Send() = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the no-run native settlement")
	}
	mu.Lock()
	terminals := 0
	commandStatus := false
	for _, event := range events {
		if event.Done {
			terminals++
		}
		if event.Kind == core.EventStatus && strings.Contains(event.Text, "native command /extcmd") {
			commandStatus = !event.Done && event.Kind == core.EventStatus
		}
	}
	eventCount := len(events)
	mu.Unlock()
	if terminals != 1 {
		t.Fatalf("no-run native terminal events = %d, want exactly 1", terminals)
	}
	if final.Kind != core.EventFinal || final.Text != "" || final.FinalText == nil || *final.FinalText != "" {
		t.Fatalf("no-run native settlement = %#v, want the honest empty final", final)
	}
	if !commandStatus {
		t.Fatalf("no-run native events = %#v, want one plain command status", events[:eventCount])
	}
	if pi.IsActive(key) {
		t.Fatal("no-run native turn remained active")
	}
	for _, record := range readNativeRequests(t, logPath, "extension_ui_response") {
		t.Fatalf("fire-and-forget notify unexpectedly received a response: %#v", record)
	}

	// A provider process exit keeps the durable session: the next native
	// dispatch resumes the retained session and settles the same way.
	stored := closePiProcess(t, pi, key)
	restarted := make(chan core.Event, 4)
	if _, steered, err := pi.Send(ctx, key, "/extcmd again", func(event core.Event) {
		if event.Done {
			restarted <- event
		}
	}); err != nil || steered {
		t.Fatalf("retained-session native Send() = steered %t, %v", steered, err)
	}
	select {
	case final = <-restarted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the retained-session native settlement")
	}
	if final.Kind != core.EventFinal || final.Text != "" || pi.ThreadID(key) != "pi-session" {
		t.Fatalf("retained-session native final = %#v, thread %q", final, pi.ThreadID(key))
	}

	// A final clarification ends the turn; the next ordinary message continues
	// the same Pi session without rotating or relaunching a process.
	continued := make(chan core.Event, 4)
	if _, steered, err := pi.Send(ctx, key, "now act on that", func(event core.Event) {
		if event.Done {
			continued <- event
		}
	}); err != nil || steered {
		t.Fatalf("continuing Send() = steered %t, %v", steered, err)
	}
	select {
	case final = <-continued:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the continuing turn")
	}
	if final.Kind != core.EventFinal || final.Text != "hello world" || pi.ThreadID(key) != "pi-session" {
		t.Fatalf("continuing final = %#v, thread %q", final, pi.ThreadID(key))
	}

	launches := launchInvocations(t, logPath)
	if len(launches) != 2 {
		t.Fatalf("native dispatches produced %d launches, want the fresh and resumed pair", len(launches))
	}
	resumed := -1
	for index, argument := range launches[1].Args {
		if argument == "--session" && index+1 < len(launches[1].Args) {
			resumed = index + 1
		}
	}
	if resumed < 0 || !samePiPath(launches[1].Args[resumed], stored) {
		t.Fatalf("retained native dispatch did not resume the stored session %q: %q", stored, launches[1].Args)
	}
	commands := readNativeRequests(t, logPath, "get_commands")
	if len(commands) != 2 {
		t.Fatalf("native recognition queries = %d, want one per idle slash dispatch", len(commands))
	}
	if steers := readNativeRequests(t, logPath, "steer"); len(steers) != 0 {
		t.Fatalf("idle native dispatch used the steer RPC: %#v", steers)
	}
	prompts := readNativeRequests(t, logPath, "prompt")
	if len(prompts) != 3 {
		t.Fatalf("prompt requests = %d, want 3", len(prompts))
	}
	if text, behavior := nativePromptRequest(t, prompts[0]); text != "/extcmd one two" || behavior != "" {
		t.Fatalf("first native prompt = %q behavior %q", text, behavior)
	}
}

func TestPiNativeSlashDuringActiveTurnUsesSteeredPrompt(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-native-commands")
	key := "chat:tui:native-active"
	first := make(chan core.Event, 16)
	if _, steered, err := pi.Send(ctx, key, "first", func(event core.Event) { first <- event }); err != nil || steered {
		t.Fatalf("first Pi send = steered %t, %v", steered, err)
	}
	for {
		select {
		case event := <-first:
			if event.Kind == core.EventDelta && event.Text == "first" {
				goto active
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the active Pi turn")
		}
	}
active:
	second := make(chan core.Event, 16)
	threadID, steered, err := pi.Send(ctx, key, "/extcmd steer now", func(event core.Event) { second <- event })
	if err != nil || !steered || threadID != "pi-session" {
		t.Fatalf("native steered send = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	for !final.Done {
		select {
		case event := <-second:
			if event.Done {
				final = event
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the natively steered result")
		}
	}
	if final.Kind != core.EventFinal || final.Text != "first steered" {
		t.Fatalf("natively steered final = %#v", final)
	}
	released := false
	for len(first) > 0 {
		event := <-first
		released = released || event.Kind == core.EventStatus && event.Done
	}
	if !released {
		t.Fatal("previous Pi emitter was not released after native steering")
	}
	if pi.IsActive(key) {
		t.Fatal("natively steered turn remained active")
	}
	var steeredPrompts, steerCalls int
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "request" {
			continue
		}
		if record.Method == "steer" {
			steerCalls++
		}
		if record.Method == "prompt" {
			text, behavior := nativePromptRequest(t, record)
			if behavior == "steer" && strings.HasPrefix(text, "/") {
				steeredPrompts++
			}
		}
	}
	if steeredPrompts != 1 || steerCalls != 0 {
		t.Fatalf("active native slash = %d steered prompts and %d steer calls, want 1 and 0", steeredPrompts, steerCalls)
	}
}

func TestPiNativeSkillSlashRunsOrdinaryModelTurn(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-native-commands")
	key := "chat:tui:native-skill"
	done := make(chan core.Event, 4)
	threadID, steered, err := pi.Send(ctx, key, "/skill:review the code", func(event core.Event) {
		if event.Done {
			done <- event
		}
	})
	if err != nil || steered || threadID != "pi-session" {
		t.Fatalf("skill slash Send() = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the skill slash turn")
	}
	if final.Kind != core.EventFinal || final.Text != "reviewing code" {
		t.Fatalf("skill slash final = %#v", final)
	}
	if pi.IsActive(key) {
		t.Fatal("skill slash turn remained active")
	}
	// The final clarification ends the turn, and the next message continues
	// the same Pi session in the same live process.
	continued := make(chan core.Event, 4)
	if _, steered, err := pi.Send(ctx, key, "apply the review", func(event core.Event) {
		if event.Done {
			continued <- event
		}
	}); err != nil || steered {
		t.Fatalf("continuing Send() = steered %t, %v", steered, err)
	}
	select {
	case final = <-continued:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the continuing turn")
	}
	if final.Kind != core.EventFinal || final.Text != "hello world" || pi.ThreadID(key) != "pi-session" {
		t.Fatalf("continuing final = %#v, thread %q", final, pi.ThreadID(key))
	}
	if launches := launchInvocations(t, logPath); len(launches) != 1 {
		t.Fatalf("clarification continuation produced %d launches, want 1", len(launches))
	}
	prompts := readNativeRequests(t, logPath, "prompt")
	if len(prompts) != 2 {
		t.Fatalf("prompt requests = %d, want 2", len(prompts))
	}
	if text, behavior := nativePromptRequest(t, prompts[0]); text != "/skill:review the code" || behavior != "" {
		t.Fatalf("skill prompt = %q behavior %q", text, behavior)
	}
}

func TestPiNativeCommandRejectionFailsDispatch(t *testing.T) {
	pi, logPath, ctx := newPiNativeInputFixture(t, "pi-native-commands")
	key := "chat:tui:native-reject"
	var events []core.Event
	_, steered, err := pi.Send(ctx, key, "/extcmd fail", func(event core.Event) { events = append(events, event) })
	if err == nil || !strings.Contains(err.Error(), "Fixture rejected the native command") {
		t.Fatalf("rejected native Send() = %v", err)
	}
	if steered {
		t.Fatal("rejected idle dispatch reported steering")
	}
	if pi.IsActive(key) {
		t.Fatal("rejected native dispatch left an active turn")
	}
	for _, event := range events {
		if event.Done {
			t.Fatalf("rejected dispatch emitted a terminal event: %#v", event)
		}
	}
	for _, record := range readNativeRequests(t, logPath, "steer") {
		t.Fatalf("rejected native dispatch used the steer RPC: %#v", record)
	}
}

func TestPiNativeCommandCancellationClearsTheTurn(t *testing.T) {
	pi, _, ctx := newPiNativeInputFixture(t, "pi-native-commands")
	key := "chat:tui:native-cancel"
	sendCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	_, _, err := pi.Send(sendCtx, key, "/cancel me", func(event core.Event) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled native Send() error = %v, want context.Canceled", err)
	}
	if pi.IsActive(key) {
		t.Fatal("cancelled native dispatch left an active turn")
	}
	// The fixture processes requests serially, so wait out the cancelled
	// dispatch's delayed run before reopening the conversation; its late
	// events bind to no turn and stay ignored.
	time.Sleep(400 * time.Millisecond)
	// The live process survives the cancelled dispatch and serves the next
	// ordinary turn on the same session.
	recovered := make(chan core.Event, 4)
	if _, steered, err := pi.Send(ctx, key, "try again", func(event core.Event) {
		if event.Done {
			recovered <- event
		}
	}); err != nil || steered {
		t.Fatalf("recovery Send() = steered %t, %v", steered, err)
	}
	select {
	case final := <-recovered:
		if final.Kind != core.EventFinal || final.Text != "hello world" {
			t.Fatalf("recovery final = %#v", final)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the recovery turn")
	}
}
