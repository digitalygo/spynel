package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/harness"
	"github.com/digitalygo/spynel/internal/history"
	"github.com/digitalygo/spynel/internal/workspace"
)

// retainedContextServiceHarness reports a provider session that already
// retains the conversation, like a live Pi session with matching policy.
type retainedContextServiceHarness struct {
	*serviceHarness
	retained bool
}

func (r *retainedContextServiceHarness) ProvidesConversationContext(string) bool { return r.retained }

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

func TestRetainedProviderReceivesOnlyCurrentMessageAndHistoryPath(t *testing.T) {
	target := &retainedContextServiceHarness{serviceHarness: newServiceHarness(), retained: true}
	service, _ := promptContextTestService(t, target)
	for _, text := range []string{"earlier coordination", "latest question"} {
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: text}, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	prompts := target.prompts["chat:tui:local"]
	if len(prompts) != 2 {
		t.Fatalf("prompts = %#v", prompts)
	}
	prompt := prompts[1]
	if !strings.Contains(prompt, "latest question") {
		t.Fatalf("retained prompt omitted the current message:\n%s", prompt)
	}
	if strings.Contains(prompt, "earlier coordination") {
		t.Fatalf("retained prompt re-injected bounded history:\n%s", prompt)
	}
	if !strings.Contains(prompt, service.History.Path("tui", "local")) {
		t.Fatalf("retained prompt omitted the full history path:\n%s", prompt)
	}
}

func TestFreshProviderAndNonProviderKeepBoundedSeed(t *testing.T) {
	for _, test := range []struct {
		name   string
		target func() (*serviceHarness, harness.Harness)
	}{
		{"fresh-pi", func() (*serviceHarness, harness.Harness) {
			base := newServiceHarness()
			return base, &retainedContextServiceHarness{serviceHarness: base, retained: false}
		}},
		{"non-provider", func() (*serviceHarness, harness.Harness) {
			base := newServiceHarness()
			return base, base
		}},
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
			if !strings.Contains(prompt, "latest question") || !strings.Contains(prompt, "earlier coordination") {
				t.Fatalf("seeded prompt omitted bounded history:\n%s", prompt)
			}
		})
	}
}

func TestCreationDirectivesIntactInBothContextModes(t *testing.T) {
	for _, retained := range []bool{true, false} {
		name := "seed"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			target := &retainedContextServiceHarness{serviceHarness: newServiceHarness(), retained: retained}
			service, _ := promptContextTestService(t, target)
			ctx := context.Background()
			for _, text := range []string{"earlier context", "/task ship it", "/goal keep it healthy"} {
				if err := service.Handle(ctx, core.Message{Channel: "cli", Conversation: "work", Text: text}, func(core.Event) {}); err != nil {
					t.Fatal(err)
				}
			}
			prompts := target.prompts["chat:cli:work"]
			if len(prompts) != 3 {
				t.Fatalf("prompts = %#v", prompts)
			}
			taskPrompt, goalPrompt := prompts[1], prompts[2]
			if !strings.Contains(taskPrompt, "explicitly invoked `/task`") || !strings.Contains(taskPrompt, "ship it") {
				t.Fatalf("task directive missing:\n%s", taskPrompt)
			}
			if !strings.Contains(goalPrompt, "explicitly invoked `/goal`") || !strings.Contains(goalPrompt, "keep it healthy") {
				t.Fatalf("goal directive missing:\n%s", goalPrompt)
			}
			seeded := !retained
			for _, prompt := range []string{taskPrompt, goalPrompt} {
				if strings.Contains(prompt, "earlier context") != seeded {
					t.Fatalf("creation base context mismatched retained=%t:\n%s", retained, prompt)
				}
			}
		})
	}
}

func TestRecoveryPromptUsesNewestReservedEntryAndKeepsDirectives(t *testing.T) {
	target := &retainedContextServiceHarness{serviceHarness: newServiceHarness(), retained: true}
	service, _ := promptContextTestService(t, target)
	service.SetPrimaryInstanceID("primary")
	now := time.Now().UTC()
	for _, entry := range []history.Entry{
		{At: now, AcceptedAt: now, Role: "user", Content: "older stalled question", SourceMessageID: "local:older"},
		{At: now.Add(time.Second), AcceptedAt: now.Add(time.Second), Role: "user", Content: "newest stalled question", SourceMessageID: "local:newest"},
	} {
		if _, err := service.History.Append("cli", "recover", entry); err != nil {
			t.Fatal(err)
		}
	}
	result := service.scanRecovery(context.Background(), "periodic")
	if result.Dispatched != 1 || len(target.prompts["chat:cli:recover"]) != 1 {
		t.Fatalf("recovery result = %#v, prompts = %#v", result, target.prompts)
	}
	base, directive, ok := strings.Cut(target.prompts["chat:cli:recover"][0], "\n\n---\n\n")
	if !ok {
		t.Fatal("recovery prompt omitted the directive separator")
	}
	if !strings.Contains(base, "newest stalled question") || strings.Contains(base, "older stalled question") {
		t.Fatalf("recovery user context = %q", base)
	}
	if strings.Contains(base, "recover stalled conversation") {
		t.Fatal("synthetic recovery control text leaked into user context")
	}
	for _, want := range []string{
		"Spynel is recovering bounded conversation messages",
		`<stalled_message index="1" source_message_id="local:newest">`,
		`<stalled_message index="2" source_message_id="local:older">`,
		"Always produce a visible response",
	} {
		if !strings.Contains(directive, want) {
			t.Fatalf("recovery directive missing %q:\n%s", want, directive)
		}
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

func TestRetainedPromptKeepsCustomTemplatePlaceholderAndLiteralContent(t *testing.T) {
	target := &retainedContextServiceHarness{serviceHarness: newServiceHarness(), retained: true}
	service, cfg := promptContextTestService(t, target)
	if err := os.WriteFile(cfg.StatePath("prompts", "chat.md"), []byte("CUSTOM CHAT\n{{RECENT_HISTORY}}\nPATH {{HISTORY_FILE}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"earlier coordination", "keep {{RECENT_HISTORY}} literal"} {
		if err := service.Handle(context.Background(), core.Message{Channel: "tui", Conversation: "local", Text: text}, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	prompt := target.prompts["chat:tui:local"][1]
	if !strings.Contains(prompt, "CUSTOM CHAT") || !strings.Contains(prompt, "PATH "+service.History.Path("tui", "local")) {
		t.Fatalf("custom template placeholder was not rendered:\n%s", prompt)
	}
	if !strings.Contains(prompt, "keep {{RECENT_HISTORY}} literal") {
		t.Fatalf("placeholder-like user content was rewritten:\n%s", prompt)
	}
	if strings.Contains(prompt, "earlier coordination") {
		t.Fatalf("retained custom prompt re-injected history:\n%s", prompt)
	}
}
