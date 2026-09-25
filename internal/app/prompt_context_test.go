package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/harness"
	"github.com/digitalygo/spynel/internal/workspace"
)

// retainedContextServiceHarness reports a provider session that already
// retains the conversation, like a live provider session with matching policy.
type retainedContextServiceHarness struct {
	*serviceHarness
	retained bool
}

func (r *retainedContextServiceHarness) ProvidesConversationContext(string) bool { return r.retained }

// nativeConversationServiceHarness accepts raw conversation input like the Pi
// adapter. Every session key is chat input; retained reports whether the
// provider session already holds the conversation.
type nativeConversationServiceHarness struct {
	*serviceHarness
	retained bool
	keys     []string
}

func (n *nativeConversationServiceHarness) ProvidesConversationContext(string) bool {
	return n.retained
}

func (n *nativeConversationServiceHarness) NativeConversationInput(key string) bool {
	n.mu.Lock()
	n.keys = append(n.keys, key)
	n.mu.Unlock()
	return true
}

func promptContextTestService(t *testing.T, target harness.Harness) (*Service, config.Config) {
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
	return service, cfg
}

// Fresh, retained, and imported Pi sessions share one dispatch path, so the
// native capability is exercised through both conversation-context answers.
func TestNativeHarnessReceivesRawCurrentMessageForFreshAndRetainedSessions(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "fresh"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			target := &nativeConversationServiceHarness{serviceHarness: newServiceHarness(), retained: retained}
			service, cfg := promptContextTestService(t, target)
			// Fresh initialization no longer creates the retired workflow
			// directories, so materialize them to keep these legacy files as
			// present-but-inert fixtures for the regression assertion below.
			if err := os.MkdirAll(cfg.StatePath("prompts"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.StatePath("prompts", "chat.md"), []byte("CUSTOM DISPATCHER {{RECENT_HISTORY}} {{HISTORY_FILE}}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(cfg.StatePath("instructions"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.StatePath("instructions", "agent-chat.md"), []byte("PERSISTENT RULE"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"earlier coordination", "latest question"} {
				if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: text}, func(core.Event) {}); err != nil {
					t.Fatal(err)
				}
			}
			prompts := target.prompts["chat:tui:local"]
			if len(prompts) != 2 || prompts[0] != "earlier coordination" || prompts[1] != "latest question" {
				t.Fatalf("native prompts = %#v", prompts)
			}
			for _, prompt := range prompts {
				for _, unwanted := range []string{"CUSTOM DISPATCHER", "PERSISTENT RULE", service.History.Path("tui", "local")} {
					if strings.Contains(prompt, unwanted) {
						t.Fatalf("native prompt injected %q:\n%s", unwanted, prompt)
					}
				}
			}
			if len(target.keys) != 2 || target.keys[0] != "chat:tui:local" {
				t.Fatalf("native capability queries = %#v", target.keys)
			}
		})
	}
}

func TestNonNativeHarnessKeepsBoundedHistoryContext(t *testing.T) {
	for _, test := range []struct {
		name   string
		target func() (*serviceHarness, harness.Harness)
		seeded bool
	}{
		{"non-provider", func() (*serviceHarness, harness.Harness) {
			base := newServiceHarness()
			return base, base
		}, true},
		{"retained-provider", func() (*serviceHarness, harness.Harness) {
			base := newServiceHarness()
			return base, &retainedContextServiceHarness{serviceHarness: base, retained: true}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder, target := test.target()
			service, _ := promptContextTestService(t, target)
			for _, text := range []string{"earlier coordination", "latest question"} {
				if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: text}, func(core.Event) {}); err != nil {
					t.Fatal(err)
				}
			}
			prompt := recorder.prompts["chat:tui:local"][1]
			if !strings.Contains(prompt, "latest question") {
				t.Fatalf("%s prompt omitted the current message:\n%s", test.name, prompt)
			}
			if strings.Contains(prompt, "earlier coordination") != test.seeded {
				t.Fatalf("%s prompt context seeded=%t:\n%s", test.name, test.seeded, prompt)
			}
			for _, unwanted := range []string{"Triage every message", "explicitly invoked", "Configured task review mode"} {
				if strings.Contains(prompt, unwanted) {
					t.Fatalf("%s prompt injected %q:\n%s", test.name, unwanted, prompt)
				}
			}
		})
	}
}

func TestRemoteAttachmentGuidanceAppliesOnlyToNonNativeHarnesses(t *testing.T) {
	fallbackHarness := newServiceHarness()
	fallback, _ := promptContextTestService(t, fallbackHarness)
	if err := fallback.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "send the report"}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if prompt := fallbackHarness.prompts["chat:telegram:TG-7"][0]; !strings.Contains(prompt, "[Send photo]") || !strings.Contains(prompt, "send the report") {
		t.Fatalf("non-native remote prompt lost outbound attachment guidance:\n%s", prompt)
	}
	native := &nativeConversationServiceHarness{serviceHarness: newServiceHarness()}
	nativeService, _ := promptContextTestService(t, native)
	if err := nativeService.Handle(context.Background(), core.Message{Channel: "telegram", Conversation: "TG-7", Text: "send the report"}, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if prompt := native.prompts["chat:telegram:TG-7"][0]; prompt != "send the report" {
		t.Fatalf("native remote prompt was not raw: %q", prompt)
	}
}

func TestZeroHistoryLimitsStillDeliverCurrentMessage(t *testing.T) {
	target := newServiceHarness()
	service, _ := promptContextTestService(t, target)
	if _, err := service.Settings.Update(func(next *config.Config) error {
		next.Workspace.HistoryMaxMessages = 0
		next.Workspace.HistoryCharLimit = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"older question", "current question"} {
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: text}, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	prompt := target.prompts["chat:tui:local"][1]
	if !strings.Contains(prompt, "current question") {
		t.Fatalf("zero limits dropped the current message:\n%s", prompt)
	}
	if strings.Contains(prompt, "older question") {
		t.Fatalf("zero limits delivered prior history:\n%s", prompt)
	}
}
