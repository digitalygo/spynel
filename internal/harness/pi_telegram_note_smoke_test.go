package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/core"
)

// Smoke markers prove which APPEND_SYSTEM.md resource Pi discovered. The
// model only exists in the isolated agent dir, so a run can never silently
// fall back to the real user Pi installation.
const (
	smokeGlobalAppendMarker  = "SPYNEL-SMOKE-GLOBAL-APPEND-MARKER isolated agent dir append system note."
	smokeProjectAppendMarker = "SPYNEL-SMOKE-PROJECT-APPEND-MARKER trusted project append system note."
)

// TestPiTelegramNoteSmokeWithRealPi runs the real installed Pi binary through
// the Spynel adapter against isolated agent directories and a local fake
// OpenAI-compatible provider. It proves the additive Telegram note extension
// keeps Pi discovering the global and trusted-project APPEND_SYSTEM.md
// resources while adding exactly one spynel_telegram section, with no paid
// provider calls and no real credentials.
func TestPiTelegramNoteSmokeWithRealPi(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("the real pi executable is not installed; skipping the isolated resource-parity smoke test")
	}
	server, capture := newSmokeProvider(t)
	t.Run("global", func(t *testing.T) {
		system, user := runSmokeTurn(t, server.URL, capture, false)
		assertSmokeSystemPrompt(t, system, user, smokeGlobalAppendMarker)
	})
	t.Run("trusted project", func(t *testing.T) {
		// The global file also exists in this layout; Pi discovers exactly one
		// append resource and the trusted project wins, so the project marker
		// proves trusted-project discovery stayed active.
		system, user := runSmokeTurn(t, server.URL, capture, true)
		assertSmokeSystemPrompt(t, system, user, smokeProjectAppendMarker)
	})
}

// assertSmokeSystemPrompt asserts the captured provider request carries the
// discovered append resource and exactly one namespaced Telegram note section.
func assertSmokeSystemPrompt(t *testing.T, system, user []string, wantMarker string) {
	t.Helper()
	if len(system) == 0 {
		t.Fatal("the fake provider received no system prompt")
	}
	combined := strings.Join(system, "\n")
	if !strings.Contains(combined, wantMarker) {
		t.Fatalf("Pi system prompt lost the discovered append resource %q; system prompt %q", wantMarker, combined)
	}
	if sectionCount := strings.Count(combined, "<spynel_telegram>"); sectionCount != 1 || strings.Count(combined, "</spynel_telegram>") != 1 {
		t.Fatalf("Pi system prompt carried the spynel_telegram section %d times: %q", sectionCount, combined)
	}
	start := strings.Index(combined, "<spynel_telegram>") + len("<spynel_telegram>")
	end := strings.Index(combined, "</spynel_telegram>")
	if section := strings.TrimSpace(combined[start:end]); section != piTelegramNoteExpectedSentence {
		t.Fatalf("spynel_telegram section = %q, want exactly the note sentence", section)
	}
	if len(combined) <= len(piTelegramNoteExpectedSentence)+len(wantMarker) {
		t.Fatalf("Pi system prompt looks forced instead of additive: %q", combined)
	}
	if !strings.Contains(strings.Join(user, "\n"), "smoke prompt") {
		t.Fatalf("the accepted user prompt never reached the provider: %q", user)
	}
}

// runSmokeTurn performs one ordinary Telegram dispatch through the real Pi
// adapter in a fully isolated workspace and returns the captured system and
// user prompt texts.
func runSmokeTurn(t *testing.T, baseURL string, capture *smokeProviderCapture, trustedProjectAppend bool) (system, user []string) {
	t.Helper()
	home := t.TempDir()
	agentDir := filepath.Join(home, "agent")
	project := filepath.Join(home, "project")
	runtimeDir := filepath.Join(home, ".spynel", "runtime")
	for _, directory := range []string{agentDir, filepath.Join(project, ".pi"), runtimeDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeSmokeFile(t, filepath.Join(agentDir, "APPEND_SYSTEM.md"), smokeGlobalAppendMarker+"\n")
	if trustedProjectAppend {
		writeSmokeFile(t, filepath.Join(project, ".pi", "APPEND_SYSTEM.md"), smokeProjectAppendMarker+"\n")
	}

	// Explicitly pre-approve the isolated project inside the isolated agent
	// directory so its trust-gated resources load without any interactive
	// approval, override flag, or real user state.
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := json.Marshal(map[string]any{canonicalProject: true})
	if err != nil {
		t.Fatal(err)
	}
	writeSmokeFile(t, filepath.Join(agentDir, "trust.json"), string(trust)+"\n")

	models := map[string]any{"providers": map[string]any{
		"spynelfake": map[string]any{
			"baseUrl": baseURL + "/v1",
			"api":     "openai-completions",
			// A literal dummy key: the smoke test must never touch real
			// credentials or make a paid provider call.
			"apiKey": "spynel-smoke-dummy-key",
			"models": []any{map[string]any{"id": "fake-model", "name": "Spynel Smoke Fake"}},
		},
	}}
	encodedModels, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	writeSmokeFile(t, filepath.Join(agentDir, "models.json"), string(encodedModels)+"\n")

	// Isolate the child Pi from the real user installation twice: the
	// adapter-level overrides cover the launched process, and the test-level
	// environment keeps the inherited environment itself disposable.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	t.Setenv("PI_OFFLINE", "1")
	t.Setenv("PI_SKIP_VERSION_CHECK", "1")
	isolation := []string{
		"PI_CODING_AGENT_DIR=" + agentDir,
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
		"HOME=" + home,
		"USERPROFILE=" + home,
		"PI_SUBAGENT_CHILD=",
		"GENTLE_PI_AGENTS_CHILD=",
	}
	config := HarnessConfig{
		Command:      "pi",
		Cwd:          project,
		Model:        "spynelfake/fake-model",
		SessionsFile: filepath.Join(runtimeDir, "harness-pi-sessions.json"),
		Env:          isolation,
	}
	adapter, err := NewPi(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("real Pi smoke harness failed to start: %v", err)
	}
	defer func() { _ = adapter.Close() }()

	events := make(chan core.Event, 64)
	if _, _, err := adapter.Send(ctx, "chat:telegram:TG-42", "smoke prompt", func(event core.Event) {
		events <- event
	}); err != nil {
		t.Fatalf("real Pi smoke dispatch failed: %v", err)
	}
	var final core.Event
	awaiting := true
	for awaiting {
		select {
		case event := <-events:
			if event.Done {
				final, awaiting = event, false
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the real Pi smoke turn")
		}
	}
	if final.Kind != core.EventFinal {
		t.Fatalf("real Pi smoke terminal = %#v, want a final response", final)
	}
	if final.Text != "smoke ok" {
		t.Fatalf("real Pi smoke final text = %q, want the fake provider reply", final.Text)
	}
	if err := adapter.Close(); err != nil {
		t.Fatalf("close real Pi smoke harness: %v", err)
	}
	// The extension must have been materialized to the private runtime path
	// that the launch argv referenced, never to a Pi discovery location.
	assertTelegramNoteExtensionFile(t, filepath.Join(runtimeDir, piTelegramNoteExtensionFileName))
	return capture.latest(t)
}

// newSmokeProvider starts the local fake OpenAI-compatible completion server
// and returns it with the capture handle for request inspection.
func newSmokeProvider(t *testing.T) (*httptest.Server, *smokeProviderCapture) {
	t.Helper()
	capture := &smokeProviderCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/chat/completions") {
			http.NotFound(w, request)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 8<<20))
		if err != nil {
			http.Error(w, "unreadable completion body", http.StatusBadRequest)
			return
		}
		var payload struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "undecodable completion body", http.StatusBadRequest)
			return
		}
		capture.record(payload.Messages)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, canFlush := w.(http.Flusher)
		chunk := func(payload string) {
			_, _ = io.WriteString(w, payload)
			if canFlush {
				flusher.Flush()
			}
		}
		chunk("data: {\"id\":\"chatcmpl-spynel-smoke\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"smoke ok\"},\"finish_reason\":null}]}\n\n")
		chunk("data: {\"id\":\"chatcmpl-spynel-smoke\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fake-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
		chunk("data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, capture
}

// smokeProviderCapture accumulates every completion request the fake
// provider receives.
type smokeProviderCapture struct {
	mu       sync.Mutex
	requests []smokeRequest
}

func (c *smokeProviderCapture) record(messages []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, smokeRequest{messages: messages})
}

// latest returns the system and user texts of the most recent request.
func (c *smokeProviderCapture) latest(t *testing.T) (system, user []string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		t.Fatal("the fake provider received no completion request")
	}
	request := c.requests[len(c.requests)-1]
	for _, message := range request.messages {
		text := smokeMessageText(t, message.Content)
		switch message.Role {
		case "system":
			system = append(system, text)
		case "user":
			user = append(user, text)
		}
	}
	return system, user
}

type smokeRequest struct {
	messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
}

func writeSmokeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// smokeMessageText extracts prompt text from an OpenAI-compatible message
// content, which may be a plain string or an array of typed parts.
func smokeMessageText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("decode completion message content %s: %v", raw, err)
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n")
}
