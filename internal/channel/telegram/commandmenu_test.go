package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
)

// commandMenuRequest is one captured Bot API call.
type commandMenuRequest struct {
	path string
	body string
}

// commandMenuRecorder captures Bot API request paths and bodies so tests can
// assert exact startup ordering and payload shape.
type commandMenuRecorder struct {
	mu       sync.Mutex
	requests []commandMenuRequest
}

func (r *commandMenuRecorder) record(request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		body = nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, commandMenuRequest{path: request.URL.Path, body: string(body)})
}

func (r *commandMenuRecorder) snapshot() []commandMenuRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]commandMenuRequest(nil), r.requests...)
}

func (r *commandMenuRecorder) paths() []string {
	requests := r.snapshot()
	paths := make([]string, 0, len(requests))
	for _, request := range requests {
		paths = append(paths, request.path)
	}
	return paths
}

// waitRecorded blocks until at least minimum requests are captured, then
// returns them in arrival order. It fails the test on timeout.
func (r *commandMenuRecorder) waitRecorded(t *testing.T, minimum int) []commandMenuRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests := r.snapshot()
		if len(requests) >= minimum {
			return requests
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d Bot API requests, recorded %v", minimum, r.paths())
	return nil
}

func TestCommandMenuExtractsCommandRootFromValue(t *testing.T) {
	menu := buildTelegramCommandMenu([]core.SlashCommand{
		{Value: "/job info <n>", Description: "Show one job"},
		{Value: "/config get <key>", Description: "Read one setting"},
	})
	want := []telegramBotCommand{
		{Command: "job", Description: "Show one job"},
		{Command: "config", Description: "Read one setting"},
	}
	if !reflect.DeepEqual(menu, want) {
		t.Fatalf("buildTelegramCommandMenu() = %#v, want %#v", menu, want)
	}
}

func TestCommandMenuNameValidation(t *testing.T) {
	thirtyTwo := strings.Repeat("n", 32)
	thirtyThree := strings.Repeat("n", 33)
	tests := []struct {
		name  string
		value string
		want  string // accepted root, empty means the entry is dropped
	}{
		{name: "lowercase root is accepted", value: "/status", want: "status"},
		{name: "digits and underscores are accepted", value: "/job_2 info <n>", want: "job_2"},
		{name: "one-character root is accepted", value: "/a", want: "a"},
		{name: "thirty-two-character root is accepted", value: "/" + thirtyTwo, want: thirtyTwo},
		{name: "uppercase root is dropped", value: "/Job info"},
		{name: "hyphenated root is dropped", value: "/my-job"},
		{name: "whitespace ends the root so a space never enters a name", value: "/two words", want: "two"},
		// U+200B zero width space is not Unicode whitespace, so it reaches
		// name validation and proves a space character cannot join a root.
		{name: "space character inside a root is dropped", value: "/two\u200bwords"},
		{name: "whitespace-only value is dropped", value: "   "},
		{name: "missing leading slash yields an empty name", value: "job info <n>"},
		{name: "lone slash yields an empty name", value: "/"},
		{name: "double slash keeps one slash and is dropped", value: "//job"},
		{name: "thirty-three-character root is dropped", value: "/" + thirtyThree},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			menu := buildTelegramCommandMenu([]core.SlashCommand{{Value: test.value, Description: "Show details"}})
			if test.want == "" {
				if len(menu) != 0 {
					t.Fatalf("buildTelegramCommandMenu(%q) = %#v, want no command", test.value, menu)
				}
				return
			}
			if len(menu) != 1 || menu[0].Command != test.want {
				t.Fatalf("buildTelegramCommandMenu(%q) = %#v, want root %q", test.value, menu, test.want)
			}
		})
	}
}

func TestCommandMenuDescriptionBounds(t *testing.T) {
	// The accented letter is one Unicode code point in two bytes, so the
	// bounds are proven in code points rather than bytes.
	const mark = "é"
	tests := []struct {
		name        string
		description string
		want        bool
	}{
		{name: "one code point is accepted", description: mark, want: true},
		{name: "two hundred fifty-six code points are accepted", description: strings.Repeat(mark, 256), want: true},
		{name: "two hundred fifty-seven code points are dropped", description: strings.Repeat(mark, 257)},
		{name: "trimming happens before counting", description: "  " + strings.Repeat(mark, 256) + "  ", want: true},
		{name: "empty description is dropped", description: ""},
		{name: "whitespace-only description is dropped", description: "   "},
		{name: "invalid UTF-8 description is dropped", description: string([]byte{0xff, 0xfe, 0x21})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			menu := buildTelegramCommandMenu([]core.SlashCommand{{Value: "/show", Description: test.description}})
			if !test.want {
				if len(menu) != 0 {
					t.Fatalf("description produced %#v, want no command", menu)
				}
				return
			}
			want := strings.TrimSpace(test.description)
			if len(menu) != 1 || menu[0].Description != want {
				t.Fatalf("description produced %#v, want trimmed description %q", menu, want)
			}
		})
	}
}

func TestCommandMenuDeduplicatesRootsKeepingFirstValid(t *testing.T) {
	menu := buildTelegramCommandMenu([]core.SlashCommand{
		{Value: "/job", Description: "First valid"},
		{Value: "/job second", Description: "Second"},
		{Value: "/job info <n>", Description: "Third"},
		{Value: "/extra", Description: ""},
		{Value: "/extra detail", Description: "Valid after invalid"},
	})
	want := []telegramBotCommand{
		{Command: "job", Description: "First valid"},
		{Command: "extra", Description: "Valid after invalid"},
	}
	if !reflect.DeepEqual(menu, want) {
		t.Fatalf("buildTelegramCommandMenu() = %#v, want %#v", menu, want)
	}
}

func TestCommandMenuCapsAtOneHundredValidCommands(t *testing.T) {
	commands := make([]core.SlashCommand, 0, 126)
	for index := 0; index < 120; index++ {
		if index%10 == 0 {
			commands = append(commands, core.SlashCommand{Value: "/Bad-Root", Description: "dropped"})
		}
		commands = append(commands, core.SlashCommand{Value: fmt.Sprintf("/cmd_%03d info", index), Description: fmt.Sprintf("Command %d", index)})
	}
	menu := buildTelegramCommandMenu(commands)
	if len(menu) != 100 {
		t.Fatalf("menu size = %d, want exactly 100", len(menu))
	}
	for index, entry := range menu {
		want := fmt.Sprintf("cmd_%03d", index)
		if entry.Command != want || entry.Description != fmt.Sprintf("Command %d", index) {
			t.Fatalf("menu[%d] = %#v, want %s in source order", index, entry, want)
		}
	}
}

func TestRegisterCommandsSendsExactSetMyCommandsPayload(t *testing.T) {
	recorder := &commandMenuRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.SetCommands([]core.SlashCommand{
		{Value: "/job info <n>", Description: "Show one job"},
		{Value: "/config get <key>", Description: "Read one setting"},
	})
	if err := bot.registerCommands(context.Background()); err != nil {
		t.Fatalf("registerCommands() error = %v", err)
	}
	requests := recorder.waitRecorded(t, 1)
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want exactly one setMyCommands", len(requests))
	}
	if requests[0].path != "/bottest/setMyCommands" {
		t.Fatalf("provider method path = %q", requests[0].path)
	}
	const want = `{"commands":[{"command":"job","description":"Show one job"},{"command":"config","description":"Read one setting"}],"scope":{"type":"all_private_chats"}}`
	if requests[0].body != want {
		t.Fatalf("setMyCommands payload = %s, want %s", requests[0].body, want)
	}
}

func TestCommandMenuPollingStartupOrder(t *testing.T) {
	recorder := &commandMenuRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/setMyCommands", "/bottest/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/getUpdates":
			<-request.Context().Done()
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 1}, "test")
	bot.baseURL = server.URL
	bot.SetCommands([]core.SlashCommand{{Value: "/status", Description: "Show status"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(context.Context, core.Message, core.Emit) error { return nil })
	}()
	recorder.waitRecorded(t, 4)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	assertCommandMenuPathPrefix(t, recorder.paths(), []string{"/bottest/getMe", "/bottest/setMyCommands", "/bottest/deleteWebhook"})
}

func TestCommandMenuWebhookStartupOrder(t *testing.T) {
	recorder := &commandMenuRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/setMyCommands", "/bottest/setWebhook", "/bottest/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{Mode: "webhook", WebhookURL: "https://public.example", WebhookListen: "127.0.0.1:0", WebhookSecret: "secret", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.SetCommands([]core.SlashCommand{{Value: "/status", Description: "Show status"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(context.Context, core.Message, core.Emit) error { return nil })
	}()
	recorder.waitRecorded(t, 3)
	cancel()
	<-done
	assertCommandMenuPathPrefix(t, recorder.paths(), []string{"/bottest/getMe", "/bottest/setMyCommands", "/bottest/setWebhook"})
}

func TestCommandMenuRegistrationFailureStillStartsPolling(t *testing.T) {
	recorder := &commandMenuRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/botmenu-token-42/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/botmenu-token-42/setMyCommands":
			_, _ = writer.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: menu provider-marker rejected"}`))
		case "/botmenu-token-42/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/botmenu-token-42/getUpdates":
			<-request.Context().Done()
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 1}, "menu-token-42")
	bot.baseURL = server.URL
	var logs strings.Builder
	bot.SetLogWriter(&logs)
	bot.SetCommands([]core.SlashCommand{{Value: "/status", Description: "Show status"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(context.Context, core.Message, core.Emit) error { return nil })
	}()
	recorder.waitRecorded(t, 4)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	assertCommandMenuPathPrefix(t, recorder.paths(), []string{
		"/botmenu-token-42/getMe",
		"/botmenu-token-42/setMyCommands",
		"/botmenu-token-42/deleteWebhook",
		"/botmenu-token-42/getUpdates",
	})
	written := logs.String()
	if written == "" {
		t.Fatal("setMyCommands failure was not logged")
	}
	if strings.Contains(written, "provider-marker") || strings.Contains(written, "menu-token-42") || strings.Contains(written, "Show status") {
		t.Fatalf("registration failure log leaked provider or command content: %q", written)
	}
}

func TestCommandMenuRegistrationFailsClosedAfterAuthorizationLoss(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 1}, "test")
	recorder := &commandMenuRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			bot.RevokeRuntimeAuthorization()
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		default:
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	defer server.Close()
	bot.baseURL = server.URL
	bot.SetCommands([]core.SlashCommand{{Value: "/status", Description: "Show status"}})
	err := bot.Run(context.Background(), func(context.Context, core.Message, core.Emit) error { return nil })
	if !errors.Is(err, errTelegramRuntimeAuthorization) {
		t.Fatalf("Run() error = %v, want errTelegramRuntimeAuthorization", err)
	}
	paths := recorder.paths()
	if !reflect.DeepEqual(paths, []string{"/bottest/getMe"}) {
		t.Fatalf("provider calls = %v, want getMe only with no setMyCommands request", paths)
	}
}

func TestCommandMenuUnsetOrEmptySendsNoRegistrationRequest(t *testing.T) {
	providerCalls := 0
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected provider call")
	})
	if err := bot.registerCommands(context.Background()); err != nil {
		t.Fatalf("registerCommands() with unset menu error = %v", err)
	}
	bot.SetCommands([]core.SlashCommand{
		{Value: "missing slash", Description: "valid description"},
		{Value: "/Bad-Root", Description: "valid description"},
		{Value: "/empty", Description: "   "},
	})
	if len(bot.commandMenu) != 0 {
		t.Fatalf("invalid commands produced menu %#v", bot.commandMenu)
	}
	if err := bot.registerCommands(context.Background()); err != nil {
		t.Fatalf("registerCommands() with empty menu error = %v", err)
	}
	if providerCalls != 0 {
		t.Fatalf("empty menu contacted Telegram: %d calls", providerCalls)
	}
}

func TestCommandMenuUnsetStartsPollingWithoutRegistration(t *testing.T) {
	recorder := &commandMenuRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/getUpdates":
			<-request.Context().Done()
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 1}, "test")
	bot.baseURL = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(context.Context, core.Message, core.Emit) error { return nil })
	}()
	recorder.waitRecorded(t, 3)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	paths := recorder.paths()
	assertCommandMenuPathPrefix(t, paths, []string{"/bottest/getMe", "/bottest/deleteWebhook", "/bottest/getUpdates"})
	for _, path := range paths {
		if path == "/bottest/setMyCommands" {
			t.Fatalf("unset menu registered commands: %v", paths)
		}
	}
}

// assertCommandMenuPathPrefix checks that the recorded Bot API calls start
// with the expected method paths in the expected order.
func assertCommandMenuPathPrefix(t *testing.T, paths, want []string) {
	t.Helper()
	if len(paths) < len(want) || !reflect.DeepEqual(paths[:len(want)], want) {
		t.Fatalf("Bot API call order = %v, want prefix %v", paths, want)
	}
}
