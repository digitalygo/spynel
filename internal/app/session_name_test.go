package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/harness"
)

// recordingLabelRouter captures every best-effort conversation label request.
type recordingLabelRouter struct {
	mu    sync.Mutex
	calls []labelRouterCall
	err   error
}

type labelRouterCall struct {
	channelName  string
	conversation string
	label        string
	force        bool
}

func (r *recordingLabelRouter) RenameConversation(_ context.Context, channelName, conversation, label string, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, labelRouterCall{channelName: channelName, conversation: conversation, label: label, force: force})
	return r.err
}

func (r *recordingLabelRouter) snapshot() []labelRouterCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]labelRouterCall(nil), r.calls...)
}

func TestConversationSessionLabelDerivation(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "plain prose", text: "Fix the login bug", want: "Fix the login bug"},
		{name: "whitespace collapse", text: "  Fix \n the\tbug  ", want: "Fix the bug"},
		{name: "attachment only", text: "[Attachment photo.jpg](</tmp/photo.jpg>)", want: ""},
		{name: "caption with attachment", text: "Deploy the fix\n\n[Attachment log.txt](</tmp/log.txt>)", want: "Deploy the fix"},
		{name: "voice disabled", text: "[Voice transcription is disabled; inspect the attached audio manually]", want: ""},
		{name: "voice failed", text: "[Voice transcription failed — inspect the attached audio manually: boom]", want: ""},
		{name: "transcript keeps prose", text: "[Generated voice transcription — may contain errors]\nRemember to water the plants", want: "Remember to water the plants"},
		{name: "slash task", text: "/task Refactor the parser", want: "Refactor the parser"},
		{name: "slash goal bot suffix", text: "/goal@spynel_bot Ship v2", want: "Ship v2"},
		{name: "slash todo", text: "/todo Fix the flaky test", want: "Fix the flaky test"},
		{name: "slash without argument", text: "/task", want: ""},
		{name: "control characters drop", text: "hello\x00world\x1b", want: "helloworld"},
		{name: "emoji preserved", text: "Ship the 🚀 release", want: "Ship the 🚀 release"},
		{name: "exactly 32 runes", text: strings.Repeat("a", 32), want: strings.Repeat("a", 32)},
		{name: "33 runes truncate", text: strings.Repeat("a", 33), want: strings.Repeat("a", 31) + "…"},
		{name: "combining marks stay intact", text: strings.Repeat("a", 29) + "e\u0301e\u0301z", want: strings.Repeat("a", 29) + "e\u0301…"},
		{name: "zwj grapheme stays intact", text: strings.Repeat("x", 26) + "👨‍👩‍👧‍👦", want: strings.Repeat("x", 26) + "…"},
		{name: "family grapheme fits", text: strings.Repeat("x", 25) + "👨‍👩‍👧‍👦", want: strings.Repeat("x", 25) + "👨‍👩‍👧‍👦"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := conversationSessionLabel(test.text); got != test.want {
				t.Fatalf("conversationSessionLabel(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

func TestAutomaticSessionNamingNamesTheFirstMessage(t *testing.T) {
	target := newPiControlHarness()
	service := newPiControlService(t, target)
	router := &recordingLabelRouter{}
	service.ConversationLabels = router
	final := runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-5", Text: "Fix the login bug"})
	if final.Kind != core.EventFinal || !strings.Contains(final.Text, "answer for chat:telegram:TG-7-topic-5") {
		t.Fatalf("turn response changed: %#v", final)
	}
	target.mu.Lock()
	calls := append([]piNameCall(nil), target.nameCalls...)
	target.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("automatic naming calls = %#v", calls)
	}
	if calls[0].key != "chat:telegram:TG-7-topic-5" || calls[0].expected != "thread-chat:telegram:TG-7-topic-5" || calls[0].name != "Fix the login bug" || !calls[0].onlyIfEmpty {
		t.Fatalf("automatic naming call = %#v", calls[0])
	}
	routed := router.snapshot()
	if len(routed) != 1 {
		t.Fatalf("conversation label calls = %#v", routed)
	}
	if routed[0].channelName != "telegram" || routed[0].conversation != "TG-7-topic-5" || routed[0].label != "Fix the login bug" || routed[0].force {
		t.Fatalf("conversation label call = %#v", routed[0])
	}
}

func TestAutomaticSessionNamingNamesEveryNewSessionAfterClear(t *testing.T) {
	target := newPiControlHarness()
	service := newPiControlService(t, target)
	router := &recordingLabelRouter{}
	service.ConversationLabels = router
	runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-5", Text: "Fix the login bug"})
	runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-5", Text: "/clear"})
	final := runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-5", Text: "Fix the signup bug"})
	if final.Kind != core.EventFinal {
		t.Fatalf("post-clear turn = %#v", final)
	}
	target.mu.Lock()
	calls := append([]piNameCall(nil), target.nameCalls...)
	target.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("naming calls across /clear = %#v", calls)
	}
	if calls[1].name != "Fix the signup bug" || !calls[1].onlyIfEmpty {
		t.Fatalf("post-clear naming call = %#v", calls[1])
	}
	// The transport owns the once-only automatic topic rename: the adapter
	// suppresses this repeated automatic request without another provider call.
	routed := router.snapshot()
	if len(routed) != 2 {
		t.Fatalf("label routing across /clear = %#v", routed)
	}
	if routed[1].force || routed[1].conversation != "TG-7-topic-5" || routed[1].label != "Fix the signup bug" {
		t.Fatalf("post-clear label routing = %#v", routed[1])
	}
}

func TestAutomaticSessionNamingAppliesToNonControlSurfaces(t *testing.T) {
	target := newPiControlHarness()
	service := newPiControlService(t, target)
	router := &recordingLabelRouter{}
	service.ConversationLabels = router
	runPiControlMessage(t, service, core.Message{Channel: "cli", Conversation: "local", Text: "Name this session"})
	target.mu.Lock()
	calls := append([]piNameCall(nil), target.nameCalls...)
	target.mu.Unlock()
	if len(calls) != 1 || calls[0].key != "chat:cli:local" || calls[0].name != "Name this session" || !calls[0].onlyIfEmpty {
		t.Fatalf("non-control surface naming = %#v", calls)
	}
	if routed := router.snapshot(); len(routed) != 1 || routed[0].channelName != "cli" {
		t.Fatalf("non-control surface label routing = %#v", routed)
	}
}

func TestAutomaticSessionNamingDerivesCommandArguments(t *testing.T) {
	target := newPiControlHarness()
	service := newPiControlService(t, target)
	runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/task Clean up the docs"})
	target.mu.Lock()
	calls := append([]piNameCall(nil), target.nameCalls...)
	target.mu.Unlock()
	if len(calls) != 1 || calls[0].name != "Clean up the docs" {
		t.Fatalf("command-argument naming = %#v", calls)
	}
}

func TestAutomaticSessionNamingSkipsNonNewOrSteeredSessions(t *testing.T) {
	t.Run("steered", func(t *testing.T) {
		target := newPiControlHarness()
		target.steered = true
		service := newPiControlService(t, target)
		runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
		target.mu.Lock()
		defer target.mu.Unlock()
		if len(target.nameCalls) != 0 {
			t.Fatalf("steered naming calls = %#v", target.nameCalls)
		}
	})
	t.Run("existing session", func(t *testing.T) {
		target := newPiControlHarness()
		target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
		target.found = true
		service := newPiControlService(t, target)
		runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
		target.mu.Lock()
		defer target.mu.Unlock()
		if len(target.nameCalls) != 0 {
			t.Fatalf("existing-session naming calls = %#v", target.nameCalls)
		}
	})
	t.Run("rotated session", func(t *testing.T) {
		target := newPiControlHarness()
		target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
		target.found = true
		target.rotate = piControlNewSession
		service := newPiControlService(t, target)
		runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
		target.mu.Lock()
		defer target.mu.Unlock()
		if len(target.nameCalls) != 0 {
			t.Fatalf("rotated-session naming calls = %#v", target.nameCalls)
		}
	})
	t.Run("rotated session on a non-control surface", func(t *testing.T) {
		target := newPiControlHarness()
		target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
		target.found = true
		target.rotate = piControlNewSession
		service := newPiControlService(t, target)
		router := &recordingLabelRouter{}
		service.ConversationLabels = router
		runPiControlMessage(t, service, core.Message{Channel: "cli", Conversation: "local", Text: "hello"})
		target.mu.Lock()
		calls := append([]piNameCall(nil), target.nameCalls...)
		target.mu.Unlock()
		if len(calls) != 0 {
			t.Fatalf("non-control-surface rotated-session naming calls = %#v", calls)
		}
		if routed := router.snapshot(); len(routed) != 0 {
			t.Fatalf("non-control-surface rotated-session label routing = %#v", routed)
		}
	})
	t.Run("imported session", func(t *testing.T) {
		target := newPiControlHarness()
		service := newPiControlService(t, target)
		runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi import " + piControlImportID})
		runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
		target.mu.Lock()
		defer target.mu.Unlock()
		if len(target.nameCalls) != 0 {
			t.Fatalf("imported-session naming calls = %#v", target.nameCalls)
		}
	})
	t.Run("send failure", func(t *testing.T) {
		target := newPiControlHarness()
		target.sendErr = errors.New("provider exploded")
		service := newPiControlService(t, target)
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: "hello"}, func(core.Event) {}); err == nil {
			t.Fatal("send failure was not reported")
		}
		target.mu.Lock()
		defer target.mu.Unlock()
		if len(target.nameCalls) != 0 {
			t.Fatalf("failed-send naming calls = %#v", target.nameCalls)
		}
	})
	t.Run("empty label", func(t *testing.T) {
		target := newPiControlHarness()
		service := newPiControlService(t, target)
		runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "[Attachment photo.jpg](</tmp/photo.jpg>)"})
		target.mu.Lock()
		defer target.mu.Unlock()
		if len(target.nameCalls) != 0 {
			t.Fatalf("empty-label naming calls = %#v", target.nameCalls)
		}
	})
	t.Run("unsupported harness", func(t *testing.T) {
		service := newPiControlService(t, newServiceHarness())
		runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
	})
}

func TestAutomaticSessionNamingFailuresAreNonFatal(t *testing.T) {
	t.Run("unsupported capability is silent", func(t *testing.T) {
		target := newPiControlHarness()
		target.nameErr = harness.ErrSessionControlsUnsupported
		service := newPiControlService(t, target)
		router := &recordingLabelRouter{}
		service.ConversationLabels = router
		final := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "hello"})
		if final.Kind != core.EventFinal || !strings.Contains(final.Text, "answer for chat:tui:local") {
			t.Fatalf("unsupported naming changed the turn: %#v", final)
		}
		if routed := router.snapshot(); len(routed) != 0 {
			t.Fatalf("unsupported naming routed a label: %#v", routed)
		}
		for _, entry := range service.Runtime.Logs() {
			if entry.Event == "session_name_failed" {
				t.Fatalf("unsupported capability logged a failure: %#v", entry)
			}
		}
	})

	t.Run("provider failure logs without content", func(t *testing.T) {
		target := newPiControlHarness()
		target.nameErr = errors.New("provider exploded")
		service := newPiControlService(t, target)
		router := &recordingLabelRouter{}
		service.ConversationLabels = router
		final := runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-5", Text: "Secret label text"})
		if final.Kind != core.EventFinal || !strings.Contains(final.Text, "answer for chat:telegram:TG-7-topic-5") {
			t.Fatalf("failed naming changed the turn: %#v", final)
		}
		if routed := router.snapshot(); len(routed) != 0 {
			t.Fatalf("failed naming routed a label: %#v", routed)
		}
		logged := 0
		for _, entry := range service.Runtime.Logs() {
			if entry.Event != "session_name_failed" {
				continue
			}
			logged++
			if entry.Component != "harness" {
				t.Fatalf("naming failure component = %q", entry.Component)
			}
			for _, secret := range []string{"Secret label text", "TG-7-topic-5", "chat:telegram:TG-7-topic-5", "provider exploded"} {
				if strings.Contains(entry.Text, secret) {
					t.Fatalf("naming failure log leaked %q: %q", secret, entry.Text)
				}
			}
		}
		if logged != 1 {
			t.Fatalf("naming failure log entries = %d", logged)
		}
		entries, _, err := service.History.RecentEntries("telegram", "TG-7-topic-5", 50, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Role == "assistant" && entry.Content != "answer for chat:telegram:TG-7-topic-5" {
				t.Fatalf("naming failure altered the assistant reply: %#v", entry)
			}
		}
	})

	t.Run("empty effective name skips the label route", func(t *testing.T) {
		target := newPiControlHarness()
		target.nameResult = harness.SessionNameResult{Name: "", Changed: true}
		service := newPiControlService(t, target)
		router := &recordingLabelRouter{}
		service.ConversationLabels = router
		runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-5", Text: "hello"})
		if routed := router.snapshot(); len(routed) != 0 {
			t.Fatalf("empty effective name routed a label: %#v", routed)
		}
	})
}

func TestPiNameCommandRenamesTheSessionAndPrivateTopic(t *testing.T) {
	target := newPiControlHarness()
	target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
	target.found = true
	service := newPiControlService(t, target)
	router := &recordingLabelRouter{}
	service.ConversationLabels = router
	reply := runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-3", Text: "/pi name Release Candidate"})
	target.mu.Lock()
	calls := append([]piNameCall(nil), target.nameCalls...)
	target.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("name calls = %#v", calls)
	}
	if calls[0].key != "chat:telegram:TG-7-topic-3" || calls[0].expected != piControlTestSession || calls[0].name != "Release Candidate" || calls[0].onlyIfEmpty {
		t.Fatalf("name call = %#v", calls[0])
	}
	routed := router.snapshot()
	if len(routed) != 1 || !routed[0].force || routed[0].label != "Release Candidate" || routed[0].conversation != "TG-7-topic-3" {
		t.Fatalf("topic rename call = %#v", routed)
	}
	if !strings.Contains(reply.Text, "Pi session renamed to `Release Candidate`") || !strings.Contains(reply.Text, "topic was renamed") {
		t.Fatalf("name reply = %q", reply.Text)
	}
}

func TestPiNameCommandUsesTheEffectiveProviderName(t *testing.T) {
	target := newPiControlHarness()
	target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
	target.found = true
	target.nameResult = harness.SessionNameResult{Name: "Provider Normalized", Changed: true}
	service := newPiControlService(t, target)
	router := &recordingLabelRouter{}
	service.ConversationLabels = router
	reply := runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-3", Text: "/pi name   raw value  "})
	if !strings.Contains(reply.Text, "Provider Normalized") || strings.Contains(reply.Text, "raw value") {
		t.Fatalf("effective-name reply = %q", reply.Text)
	}
	if routed := router.snapshot(); len(routed) != 1 || routed[0].label != "Provider Normalized" {
		t.Fatalf("effective-name topic rename = %#v", routed)
	}
}

func TestPiNameCommandReportsPartialTopicFailure(t *testing.T) {
	target := newPiControlHarness()
	target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
	target.found = true
	service := newPiControlService(t, target)
	router := &recordingLabelRouter{err: errors.New("Telegram editForumTopic: chat not found")}
	service.ConversationLabels = router
	reply := runPiControlMessage(t, service, core.Message{Channel: "telegram", Conversation: "TG-7-topic-3", Text: "/pi name Release"})
	if !strings.Contains(reply.Text, "Pi session renamed to `Release`") || !strings.Contains(reply.Text, "could not be renamed") || !strings.Contains(reply.Text, "chat not found") {
		t.Fatalf("partial-success reply = %q", reply.Text)
	}
	target.mu.Lock()
	calls := len(target.nameCalls)
	target.mu.Unlock()
	if calls != 1 {
		t.Fatalf("partial success rolled the session name back: %d calls", calls)
	}
}

func TestPiNameCommandSkipsNonTopicSurfaces(t *testing.T) {
	for _, message := range []core.Message{
		{Channel: "tui", Conversation: "local"},
		{Channel: "telegram", Conversation: "TG-7"},
	} {
		target := newPiControlHarness()
		target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
		target.found = true
		service := newPiControlService(t, target)
		router := &recordingLabelRouter{}
		service.ConversationLabels = router
		message.Text = "/pi name Session title"
		reply := runPiControlMessage(t, service, message)
		if !strings.Contains(reply.Text, "Pi session renamed to `Session title`") {
			t.Fatalf("%s/%s reply = %q", message.Channel, message.Conversation, reply.Text)
		}
		if strings.Contains(reply.Text, "topic") {
			t.Fatalf("%s/%s renamed a topic: %q", message.Channel, message.Conversation, reply.Text)
		}
		if routed := router.snapshot(); len(routed) != 0 {
			t.Fatalf("%s/%s routed a topic rename: %#v", message.Channel, message.Conversation, routed)
		}
	}
}

func TestPiNameCommandValidationAndNoSession(t *testing.T) {
	target := newPiControlHarness()
	target.info = harness.SessionInfo{ID: piControlTestSession, Path: "/tmp/pi-sessions/session.jsonl", Command: "pi"}
	target.found = true
	service := newPiControlService(t, target)
	oversized := "/pi name " + strings.Repeat("x", piSessionNameMaxRunes+1)
	for name, text := range map[string]string{
		"oversized": oversized,
		"newline":   "/pi name first\nsecond",
		"control":   "/pi name first\x00second",
	} {
		reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: text})
		if strings.Contains(reply.Text, "Pi session renamed") {
			t.Fatalf("%s name was accepted: %q", name, reply.Text)
		}
	}
	target.mu.Lock()
	if len(target.nameCalls) != 0 {
		t.Fatalf("invalid names reached the namer: %#v", target.nameCalls)
	}
	target.found = false
	target.mu.Unlock()
	reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi name Session title"})
	if !strings.Contains(reply.Text, "first ordinary prompt creates one") {
		t.Fatalf("missing-session reply = %q", reply.Text)
	}
	target.mu.Lock()
	if len(target.nameCalls) != 0 {
		t.Fatalf("missing-session naming calls = %#v", target.nameCalls)
	}
	target.mu.Unlock()
}

func TestPiNameCommandRequiresTheCapability(t *testing.T) {
	service := newPiControlService(t, newServiceHarness())
	reply := runPiControlMessage(t, service, core.Message{Channel: "tui", Conversation: "local", Text: "/pi name Session title"})
	if reply.Text != piControlsUnsupported {
		t.Fatalf("unsupported name reply = %q", reply.Text)
	}
}
