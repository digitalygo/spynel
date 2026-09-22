package harness

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/core"
)

func TestPiRPCStreamsSettlesAndResumes(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	sessionsPath := filepath.Join(root, "sessions.json")
	config := HarnessConfig{
		Command: command, Cwd: root, Model: "fixture/model-a", Effort: "high",
		Sandbox: "read-only", SessionsFile: sessionsPath,
	}
	pi, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []core.Event
	deltaSeen := make(chan struct{}, 1)
	done := make(chan core.Event, 1)
	threadID, steered, err := pi.Send(ctx, "chat", "test prompt", func(event core.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Kind == core.EventDelta && strings.Contains(event.Text, "world") {
			select {
			case deltaSeen <- struct{}{}:
			default:
			}
		}
		if event.Done {
			done <- event
		}
	})
	if err != nil || steered || threadID != "pi-session" {
		t.Fatalf("Send() = %q, %t, %v", threadID, steered, err)
	}
	select {
	case <-deltaSeen:
	case <-ctx.Done():
		t.Fatal("timed out waiting for Pi delta")
	}
	select {
	case event := <-done:
		t.Fatalf("Pi treated agent_end as terminal before agent_settled: %#v", event)
	case <-time.After(30 * time.Millisecond):
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for Pi agent_settled")
	}
	if final.Kind != core.EventFinal || final.Text != "hello world" || final.FinalText == nil || *final.FinalText != "hello world" || pi.IsActive("chat") {
		t.Fatalf("Pi final = %#v, active %t", final, pi.IsActive("chat"))
	}
	models, err := pi.Models(ctx)
	if err != nil || len(models) != 1 || models[0].ID != "fixture/model-a" || models[0].DefaultEffort != "high" || strings.Join(models[0].Efforts, ",") != "off,low,medium,high" {
		t.Fatalf("Pi models = %#v, %v", models, err)
	}
	if err := pi.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPi(config)
	if err != nil || restarted.ThreadID("chat") != "pi-session" {
		t.Fatalf("persisted Pi session = %q, %v", restarted.ThreadID("chat"), err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	restartedDone := make(chan struct{}, 1)
	if threadID, steered, err := restarted.Send(ctx, "chat", "continued", func(event core.Event) {
		if event.Done {
			restartedDone <- struct{}{}
		}
	}); err != nil || steered || threadID != "pi-session" {
		t.Fatalf("resumed Pi Send() = %q, %t, %v", threadID, steered, err)
	}
	select {
	case <-restartedDone:
	case <-ctx.Done():
		t.Fatal("timed out waiting for resumed Pi turn")
	}
	_ = restarted.Close()

	var rpcInvocations, resumed int
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "invocation" || containsArgument(record.Args, "--version") {
			continue
		}
		rpcInvocations++
		arguments := strings.Join(record.Args, " ")
		if !strings.Contains(arguments, "--mode rpc") || record.Cwd != root || record.Executable != command {
			t.Fatalf("portable Pi invocation = %#v", record)
		}
		if containsArgument(record.Args, "--session") {
			resumed++
		}
	}
	if rpcInvocations != 4 || resumed != 1 {
		t.Fatalf("Pi RPC invocations = %d, resumed = %d", rpcInvocations, resumed)
	}
}

func TestPiModelsUseRPCThinkingLevelsPerModel(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-model-capabilities")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()

	models, err := pi.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("Pi model count = %d, want 3: %#v", len(models), models)
	}
	if got := strings.Join(models[0].Efforts, ","); got != "off,low,medium,high" || models[0].DefaultEffort != "high" {
		t.Fatalf("model-a efforts/default = %q/%q", got, models[0].DefaultEffort)
	}
	if len(models[1].Efforts) != 0 || models[1].DefaultEffort != "" {
		t.Fatalf("non-reasoning model properties = %#v", models[1])
	}
	if got := strings.Join(models[2].Efforts, ","); got != "off,medium,xhigh,max" || models[2].DefaultEffort != "" {
		t.Fatalf("model-max efforts/default = %q/%q", got, models[2].DefaultEffort)
	}
	if !models[0].Default || models[1].Default || models[2].Default {
		t.Fatalf("Pi current/default model mapping = %#v", models)
	}
	if err := ValidateInferenceSelection(models, InferenceSelection{Model: "fixture/model-a", Effort: "xhigh"}); err != nil {
		t.Fatalf("Pi rejected a manual effort absent from discovery: %v", err)
	}
	if err := ValidateInferenceSelection(models, InferenceSelection{Model: "fixture/model-max", Effort: "max"}); err != nil {
		t.Fatalf("Pi rejected an RPC-advertised effort: %v", err)
	}

	var sets, levelQueries, modelProbes int
	for _, record := range readFixtureRecords(t, logPath) {
		switch record.Method {
		case "set_model":
			sets++
		case "get_available_thinking_levels":
			levelQueries++
		}
		if record.Kind == "invocation" {
			for index, arg := range record.Args {
				if arg == "--model" && index+1 < len(record.Args) && strings.HasPrefix(record.Args[index+1], "fixture/model-") {
					modelProbes++
				}
			}
		}
	}
	if sets != 0 || levelQueries != 3 || modelProbes != 3 {
		t.Fatalf("Pi capability discovery = %d persistent set_model calls, %d level queries, %d runtime model probes; want 0, 3, 3", sets, levelQueries, modelProbes)
	}
}

func TestPiLegacyOmittedModelAndEffortUseCurrentModelCapabilities(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-off-default")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, Effort: "medium", LegacyEffort: true, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	if _, _, err := pi.Send(ctx, "legacy", "legacy defaults", nil); err != nil {
		t.Fatalf("Pi rejected omitted model with legacy medium effort: %v", err)
	}
	if _, _, err := pi.SendWithInference(ctx, "explicit", "manual value", InferenceSelection{Effort: "medium"}, nil); err != nil {
		t.Fatalf("Pi rejected a manual effort: %v", err)
	}
	var mediumInvocations int
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "invocation" && containsArgument(record.Args, "--thinking") && containsArgument(record.Args, "medium") {
			mediumInvocations++
		}
	}
	if mediumInvocations != 2 {
		t.Fatalf("legacy and explicit Pi --thinking medium invocations = %d, want 2", mediumInvocations)
	}
}

func TestPiSettledTurnPrefersSuccessfulAssistantMessages(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		wantKind    string
		wantText    string
		wantFinal   string
		wantError   string
		wantRetried bool
	}{
		{
			name: "failed attempt then successful retry", mode: "pi-retry-success",
			wantKind: core.EventFinal, wantText: "hello world", wantFinal: "hello world", wantRetried: true,
		},
		{
			name: "all assistant messages failed", mode: "pi-all-failed",
			wantKind: core.EventError, wantError: "Error 429 rate-limited upstream",
		},
		{
			name: "success then later failed message", mode: "pi-success-then-failed",
			wantKind: core.EventFinal, wantText: "answer", wantFinal: "answer",
		},
		{
			name: "no assistant message", mode: "pi-no-message",
			wantKind: core.EventFinal,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			command, root, _ := portableHarnessFixture(t, testCase.mode)
			pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := pi.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer pi.Close()

			events := make(chan core.Event, 64)
			if _, steered, err := pi.Send(ctx, "chat", "prompt", func(event core.Event) { events <- event }); err != nil || steered {
				t.Fatalf("Pi send = steered %t, %v", steered, err)
			}
			var final core.Event
			terminals, retried := 0, false
			var deltas strings.Builder
			for terminals == 0 {
				select {
				case event := <-events:
					if event.Kind == core.EventDelta {
						deltas.WriteString(event.Text)
					}
					if event.Kind == core.EventStatus && strings.Contains(event.Text, "retrying") {
						if event.Done {
							t.Fatalf("Pi retry status was terminal: %#v", event)
						}
						retried = true
					}
					if event.Done {
						terminals++
						final = event
					}
				case <-ctx.Done():
					t.Fatal("timed out waiting for the settled Pi turn")
				}
			}
			// A settled turn emits exactly one terminal event; give any delayed
			// duplicate a moment to surface before asserting.
			grace := time.After(60 * time.Millisecond)
		drain:
			for terminals == 1 {
				select {
				case event := <-events:
					if event.Done {
						terminals++
					}
				case <-grace:
					break drain
				}
			}
			if terminals != 1 {
				t.Fatalf("Pi terminal events = %d, want exactly 1", terminals)
			}
			if pi.IsActive("chat") {
				t.Fatal("Pi turn remained active after settle")
			}
			if final.Kind != testCase.wantKind {
				t.Fatalf("Pi terminal = %#v, want kind %v", final, testCase.wantKind)
			}
			switch testCase.wantKind {
			case core.EventFinal:
				if final.Text != testCase.wantText {
					t.Fatalf("Pi final text = %q, want %q", final.Text, testCase.wantText)
				}
				if final.FinalText == nil || *final.FinalText != testCase.wantFinal {
					t.Fatalf("Pi final item = %#v, want %q", final.FinalText, testCase.wantFinal)
				}
			case core.EventError:
				if final.Text != testCase.wantError || final.Execution == nil || final.Execution.State != "error" {
					t.Fatalf("Pi error terminal = %#v, want %q", final, testCase.wantError)
				}
			}
			if retried != testCase.wantRetried {
				t.Fatalf("Pi retry status seen = %t, want %t", retried, testCase.wantRetried)
			}
			if testCase.mode == "pi-retry-success" {
				// The failed attempt streamed live, but its text is rolled back
				// from the settled final.
				if !strings.Contains(deltas.String(), "rate limited partial") {
					t.Fatalf("Pi did not stream the failed attempt: %q", deltas.String())
				}
				if strings.Contains(final.Text, "rate limited partial") {
					t.Fatalf("Pi final retained failed attempt text: %q", final.Text)
				}
			}
			if testCase.mode == "pi-success-then-failed" && strings.Contains(final.Text, "tail partial") {
				t.Fatalf("Pi final retained a later failed message: %q", final.Text)
			}
		})
	}
}

func TestPiReconcileMessageMergesAuthoritativeText(t *testing.T) {
	cases := []struct {
		name          string
		streamed      string
		authoritative string
		wantText      string
		wantDelta     string
	}{
		{
			name:     "authoritative extends the streamed prefix",
			streamed: "partial ", authoritative: "partial response",
			wantText: "partial response", wantDelta: "response",
		},
		{
			name:     "authoritative was already streamed as a suffix",
			streamed: "provider diagnostic prose then final answer", authoritative: "final answer",
			wantText: "provider diagnostic prose then final answer",
		},
		{
			name:     "unrelated authoritative text merges on a new line",
			streamed: "streamed draft", authoritative: "authoritative answer",
			wantText: "streamed draft\nauthoritative answer", wantDelta: "\nauthoritative answer",
		},
		{
			name:          "authoritative arrives without streamed text",
			authoritative: "authoritative only",
			wantText:      "authoritative only", wantDelta: "authoritative only",
		},
		{
			name:     "empty authoritative keeps the streamed text",
			streamed: "streamed only",
			wantText: "streamed only",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			turn := &piTurn{}
			if testCase.streamed != "" {
				turn.appendText("session", testCase.streamed)
			}
			var deltas []string
			turn.emit = func(event core.Event) {
				if event.Kind != core.EventDelta {
					t.Fatalf("reconcile emitted a %v event: %#v", event.Kind, event)
				}
				deltas = append(deltas, event.Text)
			}
			// A stale error from an earlier failed attempt must clear on success.
			turn.errorText = "stale error"
			turn.finishMessage("session", testCase.authoritative)
			wantDeltas := 0
			if testCase.wantDelta != "" {
				wantDeltas = 1
			}
			if len(deltas) != wantDeltas {
				t.Fatalf("reconcile emitted deltas %q, want %d", deltas, wantDeltas)
			}
			if wantDeltas == 1 && deltas[0] != testCase.wantDelta {
				t.Fatalf("reconcile delta = %q, want %q", deltas[0], testCase.wantDelta)
			}
			if got := turn.text.String(); got != testCase.wantText {
				t.Fatalf("reconciled Pi turn text = %q, want %q", got, testCase.wantText)
			}
			if got := turn.currentMessage.String(); got != testCase.wantText {
				t.Fatalf("reconciled Pi current message = %q, want %q", got, testCase.wantText)
			}
			wantLast := testCase.authoritative
			if wantLast == "" {
				wantLast = testCase.wantText
			}
			if turn.lastMessage != wantLast {
				t.Fatalf("Pi final assistant item = %q, want %q", turn.lastMessage, wantLast)
			}
			if len(turn.successfulMessages) != 1 || turn.successfulMessages[0] != testCase.wantText {
				t.Fatalf("Pi successful messages = %q, want [%q]", turn.successfulMessages, testCase.wantText)
			}
			if turn.errorText != "" {
				t.Fatalf("Pi error after a successful message = %q", turn.errorText)
			}
			if turn.assistantOpen {
				t.Fatal("Pi assistant message remained open after finishing")
			}
		})
	}
}

func TestPiFailedMessageReconcilesLiveTextWithoutRecordingSuccess(t *testing.T) {
	turn := &piTurn{}
	var emitted []core.Event
	turn.emit = func(event core.Event) { emitted = append(emitted, event) }
	turn.appendText("session", "rate limited partial")
	turn.failMessage("session", "rate limited upstream detail", "Error 429")
	wantLive := "rate limited partial\nrate limited upstream detail"
	if got := turn.text.String(); got != wantLive {
		t.Fatalf("failed Pi message live text = %q, want %q", got, wantLive)
	}
	if got := turn.currentMessage.String(); got != wantLive {
		t.Fatalf("failed Pi message current text = %q, want %q", got, wantLive)
	}
	if len(emitted) == 0 || emitted[len(emitted)-1].Kind != core.EventDelta || emitted[len(emitted)-1].Text != "\nrate limited upstream detail" {
		t.Fatalf("failed Pi message was not reconciled as a live delta: %#v", emitted)
	}
	if len(turn.successfulMessages) != 0 {
		t.Fatalf("failed Pi message recorded successful items: %q", turn.successfulMessages)
	}
	if turn.errorText != "Error 429" {
		t.Fatalf("Pi error text = %q, want %q", turn.errorText, "Error 429")
	}

	// Pi retries inside the same settled run: the retry starts a fresh item,
	// clears the pending error, and only the retry belongs to the final result.
	turn.startAssistant("session")
	turn.appendText("session", "final answer")
	turn.finishMessage("session", "final answer")
	if turn.errorText != "" {
		t.Fatalf("Pi error after a successful retry = %q", turn.errorText)
	}
	if len(turn.successfulMessages) != 1 || turn.successfulMessages[0] != "final answer" {
		t.Fatalf("Pi successful messages after a retry = %q, want [final answer]", turn.successfulMessages)
	}

	var settled []core.Event
	turn.emit = func(event core.Event) { settled = append(settled, event) }
	process := &piProcess{active: turn}
	process.finishTurn(turn)
	if len(settled) != 1 || settled[0].Kind != core.EventFinal {
		t.Fatalf("settled Pi turn emitted %#v, want one final event", settled)
	}
	if settled[0].Text != "final answer" {
		t.Fatalf("settled Pi final text = %q, want %q", settled[0].Text, "final answer")
	}
	if settled[0].FinalText == nil || *settled[0].FinalText != "final answer" {
		t.Fatalf("settled Pi final item = %#v, want final answer", settled[0].FinalText)
	}
	if process.active != nil {
		t.Fatal("settled Pi turn remained active")
	}
}

func TestPiSettledTurnWithoutSuccessfulMessagesReportsTerminalError(t *testing.T) {
	turn := &piTurn{}
	turn.appendText("session", "doomed partial")
	turn.failMessage("session", "doomed authoritative detail", "Error 429 rate-limited upstream")
	if len(turn.successfulMessages) != 0 {
		t.Fatalf("failed Pi turn recorded successful items: %q", turn.successfulMessages)
	}

	var settled []core.Event
	turn.emit = func(event core.Event) { settled = append(settled, event) }
	process := &piProcess{active: turn}
	process.finishTurn(turn)
	if len(settled) != 1 || settled[0].Kind != core.EventError {
		t.Fatalf("settled failed Pi turn emitted %#v, want one error event", settled)
	}
	if settled[0].Text != "Error 429 rate-limited upstream" {
		t.Fatalf("settled Pi error text = %q", settled[0].Text)
	}
	if settled[0].Execution == nil || settled[0].Execution.State != "error" {
		t.Fatalf("settled Pi error execution = %#v", settled[0].Execution)
	}
	if process.active != nil {
		t.Fatal("settled failed Pi turn remained active")
	}
}

func TestPiSteersActiveTurnAndReleasesPreviousEmitter(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-steer")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	first := make(chan core.Event, 16)
	if _, steered, err := pi.Send(ctx, "chat", "first", func(event core.Event) { first <- event }); err != nil || steered {
		t.Fatalf("first Pi send = steered %t, %v", steered, err)
	}
	for {
		select {
		case event := <-first:
			if event.Kind == core.EventDelta && event.Text == "first" {
				goto firstSeen
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for first Pi delta")
		}
	}

firstSeen:
	second := make(chan core.Event, 16)
	if threadID, steered, err := pi.Send(ctx, "chat", "second", func(event core.Event) { second <- event }); err != nil || !steered || threadID != "pi-session" {
		t.Fatalf("steered Pi send = %q, %t, %v", threadID, steered, err)
	}
	var final core.Event
	for !final.Done {
		select {
		case event := <-second:
			if event.Done {
				final = event
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for steered Pi result")
		}
	}
	if final.Kind != core.EventFinal || final.Text != "first second" {
		t.Fatalf("steered Pi final = %#v", final)
	}
	released := false
	for len(first) > 0 {
		event := <-first
		released = released || event.Kind == core.EventStatus && event.Done
	}
	if !released {
		t.Fatal("previous Pi emitter was not released")
	}
}

func TestPiInterruptUsesRPCAbort(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-interrupt")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	events := make(chan core.Event, 16)
	if _, _, err := pi.Send(ctx, "chat", "work", func(event core.Event) { events <- event }); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			if event.Kind == core.EventDelta {
				goto active
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for active Pi turn")
		}
	}

active:
	if stopped, err := pi.Interrupt(ctx, "chat"); err != nil || !stopped {
		t.Fatalf("Pi interrupt = %t, %v", stopped, err)
	}
	for {
		select {
		case event := <-events:
			if event.Done {
				if event.Kind != core.EventFinal {
					t.Fatalf("Pi interrupt terminal = %#v", event)
				}
				goto done
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for interrupted Pi turn")
		}
	}

done:
	foundAbort := false
	for _, record := range readFixtureRecords(t, logPath) {
		foundAbort = foundAbort || record.Method == "abort"
	}
	if !foundAbort {
		t.Fatal("Pi fixture did not receive abort")
	}
}

func TestPiCompactionEventsRemainStatusOnly(t *testing.T) {
	command, root, _ := portableHarnessFixture(t, "pi-compaction-events")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	var mu sync.Mutex
	var events []core.Event
	done := make(chan core.Event, 1)
	if _, _, err := pi.Send(ctx, "chat", "compact during turn", func(event core.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Done {
			done <- event
		}
	}); err != nil {
		t.Fatal(err)
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the compacting Pi turn")
	}
	if final.Kind != core.EventFinal || final.Text != "hello world" {
		t.Fatalf("compaction-event final = %#v", final)
	}
	mu.Lock()
	defer mu.Unlock()
	statuses := 0
	for _, event := range events {
		switch {
		case strings.Contains(event.Text, "Pi is compacting"):
			if event.Done || event.Kind != core.EventStatus {
				t.Fatalf("compaction_start was terminal: %#v", event)
			}
			statuses++
		case strings.Contains(event.Text, "finished compacting"):
			if event.Done || event.Kind != core.EventStatus {
				t.Fatalf("compaction_end was terminal: %#v", event)
			}
			statuses++
		}
	}
	if statuses != 2 {
		t.Fatalf("compaction status events = %d, want 2: %#v", statuses, events)
	}
}

func TestPiLoadsOrdinaryUserResourcesWithoutSuppressionFlags(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-lifecycle")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, Sandbox: "read-only", SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	done := make(chan struct{}, 1)
	if _, _, err := pi.Send(ctx, "chat", "resource check", func(event core.Event) {
		if event.Done {
			done <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for Pi turn")
	}
	if _, err := pi.Models(ctx); err != nil {
		t.Fatal(err)
	}

	forbidden := []string{"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--approve", "-a", "--no-approve", "-na"}
	ordinary, ephemeral := 0, 0
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind != "invocation" || containsArgument(record.Args, "--version") {
			continue
		}
		if len(record.Args) < 2 || record.Args[0] != "--mode" || record.Args[1] != "rpc" {
			t.Fatalf("Pi base arguments = %#v, want --mode rpc first", record.Args)
		}
		for _, flag := range forbidden {
			if containsArgument(record.Args, flag) {
				t.Fatalf("Pi invocation suppressed ordinary resources or approvals with %q: %#v", flag, record)
			}
		}
		if !containsArgument(record.Args, "read,grep,find,ls") {
			t.Fatalf("read-only Pi invocation omitted the strict tool allowlist: %#v", record)
		}
		if containsArgument(record.Args, "--no-session") {
			ephemeral++
		} else {
			ordinary++
		}
	}
	if ordinary != 1 || ephemeral != 2 {
		t.Fatalf("Pi ordinary/ephemeral invocations = %d/%d, want 1/2", ordinary, ephemeral)
	}
}

func TestPiExtensionUIDialogsCancelWithoutReceivingFireAndForgetResponses(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-extension-ui")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pi.Close()
	events := make(chan core.Event, 32)
	done := make(chan core.Event, 1)
	if _, _, err := pi.Send(ctx, "chat", "work", func(event core.Event) {
		events <- event
		if event.Done {
			done <- event
		}
	}); err != nil {
		t.Fatal(err)
	}
	var final core.Event
	select {
	case final = <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for Pi turn blocked on an extension dialog")
	}
	if final.Kind != core.EventFinal || final.Text != "hello world" || pi.IsActive("chat") {
		t.Fatalf("Pi extension-UI final = %#v, active %t", final, pi.IsActive("chat"))
	}
	cancelledEvent := false
drain:
	for {
		select {
		case event := <-events:
			cancelledEvent = cancelledEvent || event.Kind == core.EventStatus && strings.Contains(event.Text, "cancelled")
		default:
			break drain
		}
	}
	if !cancelledEvent {
		t.Fatal("Pi did not surface a status event for the cancelled extension dialog")
	}

	var responses []fixtureRecord
	for _, record := range readFixtureRecords(t, logPath) {
		if record.Kind == "request" && record.Method == "extension_ui_response" {
			responses = append(responses, record)
		}
	}
	if len(responses) != 1 {
		t.Fatalf("Pi extension UI responses = %d, want only the confirm cancellation: %#v", len(responses), responses)
	}
	var response struct {
		ID        string `json:"id"`
		Cancelled bool   `json:"cancelled"`
	}
	if err := json.Unmarshal(responses[0].Params, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "ui-1" || !response.Cancelled {
		t.Fatalf("Pi extension UI response = %#v", response)
	}
}
