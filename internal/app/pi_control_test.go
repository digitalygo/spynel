package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/harness"
	markdownfmt "github.com/digitalygo/spynel/internal/markdown"
	"github.com/digitalygo/spynel/internal/workspace"
)

const (
	piControlTestSession = "11111111-2222-3333-4444-555555555555"
	piControlNewSession  = "22222222-3333-4444-5555-666666666666"
	piControlImportID    = "33333333-4444-5555-6666-777777777777"
)

// piControlHarness is a provider-neutral conversation harness that also
// implements the optional Pi session-control capabilities. It records every
// capability call so channel restrictions can be proven to short-circuit
// before any session identity is read.
type piControlHarness struct {
	*serviceHarness
	mu            sync.Mutex
	info          harness.SessionInfo
	found         bool
	infoErr       error
	rotate        string
	compact       harness.CompactResult
	compactErr    error
	importErr     error
	failSend      bool
	suppressFinal int
	reply         string
	sendErr       error
	steered       bool
	sendThread    string
	nameResult    harness.SessionNameResult
	nameErr       error
	sessionCalls  int
	compactCalls  []string
	importCalls   []string
	nameCalls     []piNameCall
}

// piNameCall records one SetSessionName invocation so the automatic trigger
// and `/pi name` can be asserted independently.
type piNameCall struct {
	key         string
	expected    string
	name        string
	onlyIfEmpty bool
}

func newPiControlHarness() *piControlHarness {
	return &piControlHarness{serviceHarness: newServiceHarness()}
}

func (h *piControlHarness) SessionInfo(string) (harness.SessionInfo, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionCalls++
	return h.info, h.found, h.infoErr
}

func (h *piControlHarness) CompactSession(_ context.Context, key, instructions string) (harness.CompactResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.compactCalls = append(h.compactCalls, key+"\x00"+instructions)
	return h.compact, h.compactErr
}

func (h *piControlHarness) ImportSession(_ context.Context, key, sessionID string) (harness.SessionInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.importCalls = append(h.importCalls, key+"\x00"+sessionID)
	if h.importErr != nil {
		return harness.SessionInfo{}, h.importErr
	}
	h.info = harness.SessionInfo{ID: "imported-" + sessionID[:8], Path: "/tmp/pi-sessions/imported.jsonl", Command: "pi"}
	h.found = true
	return h.info, nil
}

func (h *piControlHarness) SetSessionName(_ context.Context, key, expectedSessionID, name string, onlyIfEmpty bool) (harness.SessionNameResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nameCalls = append(h.nameCalls, piNameCall{key: key, expected: expectedSessionID, name: name, onlyIfEmpty: onlyIfEmpty})
	if h.nameErr != nil {
		return harness.SessionNameResult{}, h.nameErr
	}
	if h.nameResult == (harness.SessionNameResult{}) {
		return harness.SessionNameResult{Name: name, Changed: true}, nil
	}
	return h.nameResult, nil
}

func (h *piControlHarness) Send(_ context.Context, key, prompt string, emit core.Emit) (string, bool, error) {
	h.mu.Lock()
	h.prompts[key] = append(h.prompts[key], prompt)
	rotated := h.rotate
	if rotated != "" {
		h.info = harness.SessionInfo{ID: rotated, Path: "/tmp/pi-sessions/" + rotated + ".jsonl", Command: "pi"}
		h.found = true
		h.rotate = ""
	}
	info, found := h.info, h.found
	fail := h.failSend
	sendErr := h.sendErr
	steered := h.steered
	sendThread := h.sendThread
	reply := h.reply
	suppress := h.suppressFinal > 0
	if suppress {
		h.suppressFinal--
	}
	h.mu.Unlock()
	if sendErr != nil {
		return "", false, sendErr
	}
	if fail {
		emit(core.Event{Kind: core.EventError, Text: "provider failed", Done: true})
		return "", false, nil
	}
	thread := "thread-" + key
	if found {
		thread = info.ID
	}
	if sendThread != "" {
		thread = sendThread
	}
	if suppress {
		// A steered turn releases its predecessor with a nonterminal done
		// status; only the successor's final response belongs to the user.
		emit(core.Event{Kind: core.EventStatus, Text: "Response continued on a newer message", ThreadID: thread, Done: true})
		return thread, false, nil
	}
	if steered {
		text := "steered answer"
		emit(core.Event{Kind: core.EventFinal, Text: text, FinalText: &text, ThreadID: thread, Done: true})
		return thread, true, nil
	}
	text := "answer for " + key
	if reply != "" {
		text = reply
	}
	emit(core.Event{Kind: core.EventFinal, Text: text, FinalText: &text, ThreadID: thread, Done: true})
	return thread, false, nil
}

func newPiControlService(t *testing.T, target harness.Harness) *Service {
	t.Helper()
	root := t.TempDir()
	if err := workspace.Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	service := New(cfg, target)
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func runPiControlMessage(t *testing.T, service *Service, message core.Message) core.Event {
	t.Helper()
	var reply core.Event
	seen := false
	if err := service.Handle(context.Background(), message, func(event core.Event) {
		if event.Kind == core.EventFinal || event.Kind == core.EventError {
			reply = event
			seen = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatalf("%s did not emit a terminal response", message.Text)
	}
	return reply
}

func TestPiControlCatalogAndExactUsage(t *testing.T) {
	catalog := map[string]bool{}
	for _, command := range SlashCommands() {
		catalog[command.Usage] = true
	}
	for _, usage := range []string{
		"/pi session | /pi name <name> | /pi compact [instructions] | /pi import <full-session-id>",
		"/pi session",
		"/pi name <name>",
		"/pi compact [instructions]",
		"/pi import <full-session-id>",
	} {
		if !catalog[usage] {
			t.Errorf("slash command catalog is missing %q", usage)
		}
	}
	target := newPiControlHarness()
	service := newPiControlService(t, target)
	help := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/help commands"})
	for _, want := range []string{"`/pi session`", "`/pi name <name>`", "`/pi compact [instructions]`", "`/pi import <full-session-id>`"} {
		if !strings.Contains(help.Text, want) {
			t.Errorf("command help is missing %q:\n%s", want, help.Text)
		}
	}
	for _, text := range []string{"/pi", "/pi unknown", "/pi session extra", "/pi name", "/pi import", "/pi import one two"} {
		reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: text})
		if reply.Text != piCommandUsage {
			t.Errorf("%s reply = %q, want %q", text, reply.Text, piCommandUsage)
		}
	}
	if target.sessionCalls != 0 || len(target.compactCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("usage replies touched session capabilities: %d, %v, %v", target.sessionCalls, target.compactCalls, target.importCalls)
	}
}

func TestPiControlsRefuseRestrictedChannelsWithoutCapabilityCalls(t *testing.T) {
	target := newPiControlHarness()
	target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
	target.found = true
	service := newPiControlService(t, target)
	restricted := []core.Message{
		{Channel: "cli", Conversation: "local"},
		{Channel: "whatsapp", Conversation: "WA-15551234567"},
		{Channel: "telegram", Conversation: "TG-group-99"},
		{Channel: "telegram", Conversation: "TG-group-99-topic-5"},
		{Channel: "telegram", Conversation: "TG-not-canonical"},
		{Channel: "signal", Conversation: "unknown"},
	}
	for _, message := range restricted {
		for _, text := range []string{"/pi session", "/pi name renamed", "/pi compact", "/pi import " + piControlTestSession} {
			message.Text = text
			reply := runPiControlMessage(t, service, message)
			if reply.Text != piControlsUnavailable {
				t.Fatalf("%s %s reply = %q", message.Channel, message.Conversation, reply.Text)
			}
			if strings.Contains(reply.Text, piControlTestSession) || strings.Contains(reply.Text, "/tmp/") {
				t.Fatalf("restricted reply leaked a session value: %q", reply.Text)
			}
		}
	}
	if target.sessionCalls != 0 || len(target.compactCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("restricted channels touched session capabilities: %d, %v, %v", target.sessionCalls, target.compactCalls, target.importCalls)
	}
}

func TestPiSessionShowsTheFullIDAndAShellSafeForkCommand(t *testing.T) {
	target := newPiControlHarness()
	target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/spynel sessions/pi's file.jsonl", Command: "/opt/pi bin/pi"}
	target.found = true
	service := newPiControlService(t, target)
	want := `'/opt/pi bin/pi' --fork '/tmp/spynel sessions/pi'\''s file.jsonl'`
	for _, message := range []core.Message{
		{Channel: "tui", Conversation: "local"},
		{Channel: "telegram", Conversation: "TG-7"},
		{Channel: "telegram", Conversation: "TG-7-topic-3"},
	} {
		message.Text = "/pi session"
		reply := runPiControlMessage(t, service, message)
		if !strings.Contains(reply.Text, piControlTestSession) {
			t.Fatalf("%s reply is missing the full session ID: %q", message.Channel, reply.Text)
		}
		if !strings.Contains(reply.Text, want) {
			t.Fatalf("%s reply is missing the quoted fork command %q: %q", message.Channel, want, reply.Text)
		}
	}
}

func TestPiSessionExplainsNoSessionAndUnsupportedHarness(t *testing.T) {
	empty := newPiControlHarness()
	service := newPiControlService(t, empty)
	reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi session"})
	if !strings.Contains(reply.Text, "first ordinary prompt creates one") {
		t.Fatalf("no-session reply = %q", reply.Text)
	}
	basic := newPiControlService(t, newServiceHarness())
	for _, text := range []string{"/pi session", "/pi compact", "/pi import " + piControlTestSession} {
		unsupported := runPiControlMessage(t, basic, core.Message{Channel: "tui", Conversation: "local", Text: text})
		if unsupported.Text != piControlsUnsupported {
			t.Fatalf("%s unsupported reply = %q", text, unsupported.Text)
		}
	}
}

func TestPiCompactReportsBoundedCountsAndBoundsInstructions(t *testing.T) {
	target := newPiControlHarness()
	target.compact = harness.CompactResult{TokensBefore: 150000, TokensAfter: 32000, TokensAfterKnown: true}
	service := newPiControlService(t, target)
	reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi compact focus on regressions"})
	if !strings.Contains(reply.Text, "150000") || !strings.Contains(reply.Text, "32000") {
		t.Fatalf("compact reply = %q", reply.Text)
	}
	if len(target.compactCalls) != 1 || target.compactCalls[0] != "chat:tui:local\x00focus on regressions" {
		t.Fatalf("compact calls = %#v", target.compactCalls)
	}
	target.mu.Lock()
	target.compact = harness.CompactResult{TokensBefore: 150000}
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi compact"})
	if reply.Text != "Pi compaction complete: 150000 tokens before." {
		t.Fatalf("compact reply without estimate = %q", reply.Text)
	}
	oversized := "/pi compact " + strings.Repeat("x", harness.SessionCompactMaxInstructions+1)
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: oversized})
	if !strings.Contains(reply.Text, "4096") {
		t.Fatalf("oversized compact reply = %q", reply.Text)
	}
	if len(target.compactCalls) != 2 {
		t.Fatalf("oversized instructions reached the compactor: %#v", target.compactCalls)
	}
	target.mu.Lock()
	target.compactErr = errors.New("provider capacity exhausted")
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi compact"})
	if !strings.Contains(reply.Text, "Cannot compact the Pi session: provider capacity exhausted") {
		t.Fatalf("failed compact reply = %q", reply.Text)
	}
	target.mu.Lock()
	target.compactErr = errors.New("no Pi session exists yet; the first ordinary prompt creates one")
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi compact"})
	if !strings.Contains(reply.Text, "first ordinary prompt creates one") {
		t.Fatalf("no-session compact reply = %q", reply.Text)
	}
	target.mu.Lock()
	target.compactErr = errors.New("cannot compact a Pi session while its turn is active")
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi compact"})
	if !strings.Contains(reply.Text, "while its turn is active") {
		t.Fatalf("active compact reply = %q", reply.Text)
	}
	target.mu.Lock()
	target.compactErr = harness.ErrSessionControlsUnsupported
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi compact"})
	if reply.Text != piControlsUnsupported {
		t.Fatalf("unsupported compact reply = %q", reply.Text)
	}
}

func TestPiImportReportsSuccessAndRefusesAnExistingSession(t *testing.T) {
	target := newPiControlHarness()
	target.importErr = errors.New("this conversation already has a harness session; use /clear before importing another")
	service := newPiControlService(t, target)
	reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi import " + piControlImportID})
	if !strings.Contains(reply.Text, "already has a harness session") || !strings.Contains(reply.Text, "/clear") {
		t.Fatalf("import refusal reply = %q", reply.Text)
	}
	if len(target.importCalls) != 1 || target.importCalls[0] != "chat:tui:local\x00"+piControlImportID {
		t.Fatalf("import calls = %#v", target.importCalls)
	}
	target.mu.Lock()
	target.importErr = nil
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7", Text: "/pi import " + piControlImportID})
	if !strings.Contains(reply.Text, "Imported Pi session `imported-33333333`") || !strings.Contains(reply.Text, "not modified") {
		t.Fatalf("import success reply = %q", reply.Text)
	}
}

func TestPiNewSessionNoticeLeadsTheFirstFinalOnAllowedSurfaces(t *testing.T) {
	target := newPiControlHarness()
	target.rotate = piControlNewSession
	service := newPiControlService(t, target)
	message := core.Message{Channel: "tui", Conversation: "local", Text: "hello"}
	final := runPiControlMessage(t, service, message)
	notice := "Pi session `" + piControlNewSession + "`."
	if !strings.HasPrefix(final.Text, notice) {
		t.Fatalf("first final = %q, want leading %q", final.Text, notice)
	}
	if final.FinalText == nil || !strings.HasPrefix(*final.FinalText, notice) {
		t.Fatalf("first final FinalText = %v", final.FinalText)
	}
	if !strings.Contains(final.Text, "answer for chat:tui:local") {
		t.Fatalf("first final lost the response: %q", final.Text)
	}
	second := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "again"})
	if strings.Contains(second.Text, "Pi session `") {
		t.Fatalf("session notice repeated: %q", second.Text)
	}
	entries, _, err := service.History.RecentEntries("tui", "local", 50, 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Content, piControlNewSession) {
			t.Fatalf("session notice entered durable history: %#v", entry)
		}
		if entry.FinalText != nil && strings.Contains(*entry.FinalText, piControlNewSession) {
			t.Fatalf("session notice entered durable final text: %#v", entry)
		}
	}
}

func TestPiNewSessionNoticeStaysOnPrivateTelegramAndTUIOnly(t *testing.T) {
	allowed := []core.Message{
		{Channel: "tui", Conversation: "local"},
		{Channel: "telegram", Conversation: "TG-7"},
		{Channel: "telegram", Conversation: "TG-7-topic-3"},
	}
	for _, message := range allowed {
		target := newPiControlHarness()
		target.rotate = piControlNewSession
		service := newPiControlService(t, target)
		message.Text = "hello"
		final := runPiControlMessage(t, service, message)
		if !strings.HasPrefix(final.Text, "Pi session `"+piControlNewSession+"`.") {
			t.Fatalf("%s/%s final = %q", message.Channel, message.Conversation, final.Text)
		}
	}
	blocked := []core.Message{
		{Channel: "telegram", Conversation: "TG-group-99"},
		{Channel: "telegram", Conversation: "TG-group-99-topic-5"},
		{Channel: "cli", Conversation: "local"},
		{Channel: "whatsapp", Conversation: "WA-15551234567"},
	}
	for _, message := range blocked {
		target := newPiControlHarness()
		target.rotate = piControlNewSession
		service := newPiControlService(t, target)
		message.Text = "hello"
		final := runPiControlMessage(t, service, message)
		if strings.Contains(final.Text, "Pi session `") || strings.Contains(final.Text, piControlNewSession) {
			t.Fatalf("%s/%s leaked the session notice: %q", message.Channel, message.Conversation, final.Text)
		}
		// Automatic naming inspects the pre-dispatch session on every surface,
		// but a blocked surface must still never disclose its identity.
	}
}

func TestPiNewSessionNoticeSkipsErrorsAndKnownSessions(t *testing.T) {
	target := newPiControlHarness()
	target.rotate = piControlNewSession
	target.failSend = true
	service := newPiControlService(t, target)
	message := core.Message{Channel: "tui", Conversation: "local", Text: "hello"}
	final := runPiControlMessage(t, service, message)
	if final.Kind != core.EventError || strings.Contains(final.Text, "Pi session `") {
		t.Fatalf("error final = %#v", final)
	}

	resumed := newPiControlHarness()
	resumed.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/resumed.jsonl", Command: "pi"}
	resumed.found = true
	service = newPiControlService(t, resumed)
	final = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
	if strings.Contains(final.Text, "Pi session `") {
		t.Fatalf("resumed session was re-announced: %q", final.Text)
	}

	imported := newPiControlHarness()
	service = newPiControlService(t, imported)
	runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi import " + piControlImportID})
	final = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
	if strings.Contains(final.Text, "Pi session `") {
		t.Fatalf("imported session was re-announced: %q", final.Text)
	}
}

func TestPiNewSessionNoticeSurvivesAnEarlySteerRelease(t *testing.T) {
	target := newPiControlHarness()
	target.rotate = piControlNewSession
	target.suppressFinal = 1
	service := newPiControlService(t, target)
	var early core.Event
	if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "first"}, func(event core.Event) {
		if event.Kind == core.EventFinal || event.Kind == core.EventError {
			early = event
		}
	}); err != nil {
		t.Fatal(err)
	}
	if early.Kind != "" {
		t.Fatalf("steered-away dispatch produced a terminal response: %#v", early)
	}
	second := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "second"})
	notice := "Pi session `" + piControlNewSession + "`."
	if !strings.HasPrefix(second.Text, notice) {
		t.Fatalf("successor final = %q, want leading %q", second.Text, notice)
	}
	third := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "third"})
	if strings.Contains(third.Text, "Pi session `") {
		t.Fatalf("session notice repeated after a steer release: %q", third.Text)
	}
}

func TestPiControlRepliesSanitizeAdapterErrors(t *testing.T) {
	target := newPiControlHarness()
	service := newPiControlService(t, target)
	sessionDir := filepath.Join(service.Config.Root, ".spynel", "runtime", "pi-sessions")
	sessionPath := filepath.Join(sessionDir, "private.jsonl")
	target.info = harness.SessionInfo{ID: piControlTestSession, Path: sessionPath, Command: "pi"}
	target.found = true
	target.compactErr = errors.New("mkdir " + sessionDir + ": not a directory\nprovider detail\x00")
	reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi compact"})
	if strings.Contains(reply.Text, service.Config.Root) || strings.Contains(reply.Text, sessionDir) || strings.Contains(reply.Text, sessionPath) {
		t.Fatalf("compact reply leaked a workspace or session path: %q", reply.Text)
	}
	if strings.ContainsAny(reply.Text, "\n\x00") {
		t.Fatalf("compact reply kept a control character: %q", reply.Text)
	}
	if !strings.Contains(reply.Text, "<session>") || !strings.Contains(reply.Text, "provider detail") {
		t.Fatalf("compact reply lost bounded context: %q", reply.Text)
	}

	target.mu.Lock()
	target.compactErr = nil
	target.importErr = errors.New("fork Pi session: open " + sessionPath + ": permission denied\x01")
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi import " + piControlImportID})
	if strings.Contains(reply.Text, service.Config.Root) || strings.Contains(reply.Text, sessionPath) {
		t.Fatalf("import reply leaked a workspace or session path: %q", reply.Text)
	}
	if strings.ContainsAny(reply.Text, "\x01") || !strings.Contains(reply.Text, "<session>") {
		t.Fatalf("import reply was not sanitized: %q", reply.Text)
	}

	target.mu.Lock()
	target.importErr = nil
	target.infoErr = errors.New("inspect " + service.Config.Root + "/.spynel: " + strings.Repeat("x", harness.ControlErrorMaxRunes+100))
	target.mu.Unlock()
	reply = runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi session"})
	if strings.Contains(reply.Text, service.Config.Root) {
		t.Fatalf("session reply leaked the workspace path: %q", reply.Text)
	}
	if runes := []rune(reply.Text); len(runes) > len("Cannot inspect the Pi session: ")+harness.ControlErrorMaxRunes+3 {
		t.Fatalf("session reply was not bounded: %d runes", len(runes))
	}
}

func TestPiNewSessionNoticeLeadsTelegramRichChunkingWithoutHistory(t *testing.T) {
	target := newPiControlHarness()
	target.rotate = piControlNewSession
	target.reply = strings.Repeat("Telegram rich response line\n\n", 400)
	service := newPiControlService(t, target)
	final := runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7", Text: "hello"})
	notice := "Pi session `" + piControlNewSession + "`."
	if final.Kind != core.EventFinal || !strings.HasPrefix(final.Text, notice) {
		t.Fatalf("telegram final = %#v, want leading %q", final, notice)
	}
	if final.FinalText == nil || !strings.HasPrefix(*final.FinalText, notice) {
		t.Fatalf("telegram FinalText = %v, want leading %q", final.FinalText, notice)
	}
	chunks := markdownfmt.TelegramChunks(*final.FinalText)
	if len(chunks) < 2 {
		t.Fatalf("reply did not reach rich chunking: %d chunks", len(chunks))
	}
	first := markdownfmt.TelegramChunkPlainText(chunks[0])
	if !strings.HasPrefix(first, "Pi session "+piControlNewSession+".") {
		t.Fatalf("first rich chunk lost the leading session notice: %q", first)
	}
	entries, _, err := service.History.RecentEntries("telegram", "TG-7", 50, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Content, piControlNewSession) {
			t.Fatalf("session notice entered durable history: %#v", entry)
		}
		if entry.FinalText != nil && strings.Contains(*entry.FinalText, piControlNewSession) {
			t.Fatalf("session notice entered durable final text: %#v", entry)
		}
	}
}
