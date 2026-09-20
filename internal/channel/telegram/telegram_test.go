package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitalygo/spynel/internal/channel"
	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	markdownfmt "github.com/digitalygo/spynel/internal/markdown"
	"github.com/digitalygo/spynel/internal/media"
)

type fixedTranscriber struct{ text string }

type telegramRoundTripFunc func(*http.Request) (*http.Response, error)

func (f telegramRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (t fixedTranscriber) Transcribe(context.Context, string) (string, error) { return t.text, nil }

type blockingTelegramTranscriber struct {
	entered chan struct{}
	release <-chan struct{}
}

func (t blockingTelegramTranscriber) Transcribe(ctx context.Context, _ string) (string, error) {
	close(t.entered)
	select {
	case <-t.release:
		return "voice words", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestTelegramRealShapeReplyCarriesIDAndCaption(t *testing.T) {
	var update telegramUpdate
	if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":303,"from":{"id":7},"chat":{"id":7,"type":"private"},"date":10,"text":"reply","reply_to_message":{"message_id":91,"from":{"id":8},"chat":{"id":7,"type":"private"},"date":9,"caption":"  caption\nwith   space  "}}}`), &update); err != nil {
		t.Fatal(err)
	}
	var got core.Message
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.handle(context.Background(), func(_ context.Context, message core.Message, _ core.Emit) error {
		got = message
		return nil
	}, update.Message)
	if got.ReplyTo != "91 caption with space" {
		t.Fatalf("Telegram reply_to = %q", got.ReplyTo)
	}
	if got.SourceMessageID != "telegram:7:303" {
		t.Fatalf("Telegram source_message_id = %q", got.SourceMessageID)
	}
	update.Message.ReplyToMessage.Text = ""
	update.Message.ReplyToMessage.Caption = ""
	if got := telegramReplyTo(update.Message); got != "91" {
		t.Fatalf("Telegram ID-only reply_to = %q", got)
	}
	update.Message.ReplyToMessage = nil
	if got := telegramReplyTo(update.Message); got != "" {
		t.Fatalf("Telegram non-reply reply_to = %q", got)
	}
}

func TestSendAttachmentUsesNativeTelegramMediaMethods(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "report.txt")
	if err := os.WriteFile(path, []byte("report body"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, got *http.Request) {
		if err := got.ParseMultipartForm(1024); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		request <- got
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"42"}}, "test")
	bot.baseURL = server.URL
	route, err := ParseConversation("TG-42")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.sendAttachment(context.Background(), route, core.OutboundAttachment{
		Kind: "attachment", Name: "report.txt", Path: path, MediaType: "text/plain", MaxBytes: 1024,
	}, 8); err != nil {
		t.Fatal(err)
	}
	got := <-request
	if got.URL.Path != "/bottest/sendDocument" || got.FormValue("chat_id") != "42" || got.FormValue("reply_parameters") != `{"message_id":8}` {
		t.Fatalf("request path = %q, form = %#v", got.URL.Path, got.MultipartForm.Value)
	}
	if _, hasThread := got.MultipartForm.Value["message_thread_id"]; hasThread {
		t.Fatalf("base conversation sent a thread id: %#v", got.MultipartForm.Value)
	}
	file, header, err := got.FormFile("document")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if header.Filename != "report.txt" {
		t.Fatalf("filename = %q", header.Filename)
	}
}

func TestLongPollingRoutesMessageAndSendsReply(t *testing.T) {
	var polls atomic.Int32
	sent := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/getUpdates":
			if polls.Add(1) == 1 {
				_, _ = writer.Write([]byte(`{"ok":true,"result":[{"update_id":4,"message":{"message_id":8,"from":{"id":7,"username":"trusted"},"chat":{"id":7,"type":"private"},"date":1,"text":"/status"}}]}`))
				return
			}
			<-request.Context().Done()
		case "/bottest/sendChatAction":
			_, _ = writer.Write([]byte(`{"ok":true}`))
		case "/bottest/sendMessage":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			sent <- payload
			_, _ = writer.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 1}, "test")
	bot.baseURL = server.URL
	statuses := make(chan channel.ConnectionStatus, 4)
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses <- status })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(_ context.Context, message core.Message, emit core.Emit) error {
			if message.Channel != "telegram" || message.Conversation != "TG-7" || message.Sender != "@trusted" || message.Text != "/status" {
				t.Errorf("unexpected message %#v", message)
			}
			emit(core.Event{Kind: core.EventFinal, Text: "**ready** with `code`", Done: true})
			return nil
		})
	}()
	select {
	case payload := <-sent:
		if payload["chat_id"] != "7" || payload["text"] != "<b>ready</b> with <code>code</code>" || payload["parse_mode"] != "HTML" {
			t.Fatalf("unexpected Telegram reply %#v", payload)
		}
		cancel()
	case <-ctx.Done():
		t.Fatal("timed out waiting for Telegram reply")
	}
	select {
	case status := <-statuses:
		if status.State != channel.ConnectionConnected || status.Name != "telegram" || status.Identity != "@spynel_test_bot" || status.Link != "https://t.me/spynel_test_bot" {
			t.Fatalf("unexpected Telegram status %#v", status)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Telegram connection status")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Telegram poller did not stop")
	}
}

func TestWorkflowSlashCommandIsRoutedToSharedHandler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	var got core.Message
	bot.processUpdate(context.Background(), func(_ context.Context, message core.Message, emit core.Emit) error {
		got = message
		emit(core.Event{Kind: core.EventFinal, Done: true})
		return nil
	}, telegramUpdate{Message: &telegramMessage{
		MessageID: 9,
		From:      telegramUser{ID: 7, Username: "trusted"},
		Chat:      telegramChat{ID: 7, Type: "private"},
		Date:      10,
		Text:      "/goals review --detail",
	}})

	if got.Channel != "telegram" || got.Conversation != "TG-7" || got.Text != "/goals review --detail" {
		t.Fatalf("routed message = %#v", got)
	}
}

func TestTelegramSendsOnlyLastTerminalResponse(t *testing.T) {
	sent := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			_, _ = writer.Write([]byte(`{"ok":true}`))
		case "/bottest/sendMessage":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			sent <- payload["text"].(string)
			_, _ = writer.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		lastResponse := "last response"
		emit(core.Event{Kind: core.EventDelta, Text: "streamed progress"})
		emit(core.Event{Kind: core.EventStatus, Text: "transport handoff", Done: true})
		emit(core.Event{Kind: core.EventFinal, Text: "intermediate response", Done: true, Continues: true})
		emit(core.Event{Kind: core.EventFinal, Text: "progress update\nlast response", FinalText: &lastResponse, Done: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})

	select {
	case got := <-sent:
		if got != "last response" {
			t.Fatalf("Telegram sent %q, want only the last response", got)
		}
	default:
		t.Fatal("Telegram did not send the last response")
	}
	select {
	case extra := <-sent:
		t.Fatalf("Telegram sent an intermediate response: %q", extra)
	default:
	}
}

func TestTelegramFormatsErrorsAsOrdinaryUnindentedResponses(t *testing.T) {
	sent := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			_, _ = writer.Write([]byte(`{"ok":true}`))
		case "/bottest/sendMessage":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			sent <- payload["text"].(string)
			_, _ = writer.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	partial := "partial"
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventError, Text: "first line\nsecond line", FinalText: &partial, Done: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})
	bot.handle(context.Background(), func(context.Context, core.Message, core.Emit) error {
		return errors.New("handler failed")
	}, &telegramMessage{MessageID: 9, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 2, Text: "hello"})

	for _, want := range []string{"Error first line\nsecond line", "Error handler failed"} {
		select {
		case got := <-sent:
			if got != want {
				t.Fatalf("Telegram error reply = %q, want %q", got, want)
			}
		default:
			t.Fatalf("Telegram did not send error reply %q", want)
		}
	}
}

func TestTelegramConnectionStatusRejectsUnsafeBotUsername(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.me.Username = "unsafe](https://example.com)"
	var got channel.ConnectionStatus
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { got = status })
	bot.reportStatus(channel.ConnectionConnected, "")
	if got.Identity != "" || got.Link != "" {
		t.Fatalf("unsafe Telegram username reached connection status: %#v", got)
	}
}

func TestMissingTokenReportsConnectionError(t *testing.T) {
	bot := New(config.Telegram{}, "")
	var got channel.ConnectionStatus
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { got = status })

	if err := bot.Run(context.Background(), nil); err == nil {
		t.Fatal("Run() succeeded without a token")
	}
	if got.Name != "telegram" || got.State != channel.ConnectionError || got.Detail == "" {
		t.Fatalf("connection status = %#v", got)
	}
}

func TestEmptyWhitelistReportsConnectionErrorAndRejectsUsers(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"  "}}, "test")
	var got channel.ConnectionStatus
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { got = status })
	if err := bot.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "allowed_users") {
		t.Fatalf("Run() accepted an empty whitelist: %v", err)
	}
	if got.State != channel.ConnectionError || !strings.Contains(got.Detail, "allowed_users") {
		t.Fatalf("connection status = %#v", got)
	}
	if bot.allowed(telegramUser{ID: 7, Username: "trusted"}) {
		t.Fatal("empty whitelist accepted a Telegram user")
	}
}

func TestInvalidRuntimeAllowListsHaveZeroPollingOrWebhookSideEffects(t *testing.T) {
	invalid := [][]string{nil, {}, {"  "}, {"@"}, {"..."}, {"bad user"}}
	for _, mode := range []string{"polling", "webhook"} {
		for index, allowed := range invalid {
			t.Run(fmt.Sprintf("%s-%d", mode, index), func(t *testing.T) {
				cfg := config.Telegram{Mode: mode, AllowedUsers: allowed, WebhookListen: "127.0.0.1:0", WebhookURL: "https://public.example", WebhookSecret: "secret"}
				bot := New(cfg, "token")
				providerCalls, listenerBinds := 0, 0
				bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
					providerCalls++
					return nil, errors.New("unexpected provider call")
				})
				bot.listen = func(string, string) (net.Listener, error) {
					listenerBinds++
					return nil, errors.New("unexpected listener bind")
				}
				var statuses []channel.ConnectionStatus
				bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses = append(statuses, status) })
				if err := bot.Run(context.Background(), nil); !errors.Is(err, errTelegramRuntimeAuthorization) {
					t.Fatalf("Run() error = %v", err)
				}
				if providerCalls != 0 || listenerBinds != 0 {
					t.Fatalf("invalid runtime attempted provider=%d listener=%d", providerCalls, listenerBinds)
				}
				if len(statuses) != 1 || statuses[0].State != channel.ConnectionError {
					t.Fatalf("statuses = %#v", statuses)
				}
			})
		}
	}
}

func TestNextInboundUpdateRevalidatesLiveAllowListBeforeSideEffects(t *testing.T) {
	allowed := []string{"7"}
	identityPath := filepath.Join(t.TempDir(), "identities.json")
	bot := NewWithIdentityStore(config.Telegram{AllowedUsers: allowed}, "token", identityPath)
	bot.SetAllowedUsersSource(func() []string { return allowed })
	if err := bot.identity.RecordVerifiedPrivate(7, 7, "trusted"); err != nil {
		t.Fatal(err)
	}
	identityBefore, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	providerCalls, handlerCalls := 0, 0
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected provider call")
	})
	var status channel.ConnectionStatus
	bot.SetStatusReporter(func(next channel.ConnectionStatus) { status = next })
	allowed = []string{"..."}
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
		handlerCalls++
		return nil
	}, telegramUpdate{Message: &telegramMessage{MessageID: 1, From: telegramUser{ID: 7, Username: "trusted"}, Chat: telegramChat{ID: 7, Type: "private"}, Text: "hello"}})
	if providerCalls != 0 || handlerCalls != 0 {
		t.Fatalf("revoked update attempted provider=%d handler=%d", providerCalls, handlerCalls)
	}
	identityAfter, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(identityAfter) != string(identityBefore) {
		t.Fatal("revoked update mutated persisted Telegram identity state")
	}
	if status.State != channel.ConnectionError || !strings.Contains(status.Detail, "allowed_users") {
		t.Fatalf("status = %#v", status)
	}
}

func TestLiveAllowListReplacementRejectsPreviouslyAllowedTelegramSender(t *testing.T) {
	allowed := []string{"7"}
	bot := New(config.Telegram{AllowedUsers: allowed}, "token")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	allowed = []string{"8"}
	handlerCalls := 0
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
		handlerCalls++
		return nil
	}, telegramUpdate{Message: &telegramMessage{MessageID: 1, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, Text: "hello"}})
	if handlerCalls != 0 {
		t.Fatal("replaced live Telegram allow-list retained the old sender")
	}
}

func TestGroupWelcomeDoesNotRequireAddressingTheBot(t *testing.T) {
	sent := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/bottest/sendMessage" {
			http.NotFound(writer, request)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(request.Body).Decode(&payload)
		sent <- payload["text"].(string)
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{GroupMode: "mention", WelcomeEnabled: true, WelcomeMessage: "Hello, {name}!", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	called := false
	bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
		called = true
		return nil
	}, telegramUpdate{Message: &telegramMessage{
		MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: -42, Type: "group"},
		NewChatMembers: []telegramUser{{ID: 9, FirstName: "Ada"}},
	}})
	select {
	case welcome := <-sent:
		if welcome != "Hello, Ada!" {
			t.Fatalf("welcome = %q", welcome)
		}
	case <-time.After(time.Second):
		t.Fatal("group welcome was blocked by mention-only message policy")
	}
	if called {
		t.Fatal("membership update was dispatched to the harness")
	}
}

func TestTelegramVoiceIsStoredAndTranscribed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getFile":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"file_path":"voice/file.ogg"}}`))
		case "/file/bottest/voice/file.ogg":
			_, _ = writer.Write([]byte("voice bytes"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	store := &media.Store{Directory: filepath.Join(t.TempDir(), "attachments"), MaxBytes: 1024}
	bot.SetMedia(store, fixedTranscriber{text: "hello from audio"})
	text, err := bot.messageText(context.Background(), &telegramMessage{Voice: &telegramMedia{FileID: "voice-id", FileUniqueID: "unique"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "[Attachment voice-unique.ogg]") || !strings.Contains(text, "[Generated voice transcription") || !strings.Contains(text, "hello from audio") {
		t.Fatalf("message text = %q", text)
	}
	data, err := os.ReadFile(filepath.Join(store.Directory, "voice-unique.ogg"))
	if err != nil || string(data) != "voice bytes" {
		t.Fatalf("stored voice = %q, %v", string(data), err)
	}
}

func TestTelegramTypingRefreshesFromArrivalThroughVoiceTranscriptionAndAgentTurn(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			actions.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/getFile":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"file_path":"voice/file.ogg"}}`))
		case "/file/bottest/voice/file.ogg":
			_, _ = writer.Write([]byte("voice bytes"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	transcriptionEntered := make(chan struct{})
	transcriptionRelease := make(chan struct{})
	agentEntered := make(chan struct{})
	agentRelease := make(chan struct{})
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	bot.SetMedia(&media.Store{Directory: filepath.Join(t.TempDir(), "attachments"), MaxBytes: 1024}, blockingTelegramTranscriber{
		entered: transcriptionEntered, release: transcriptionRelease,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		bot.handle(ctx, func(_ context.Context, _ core.Message, emit core.Emit) error {
			emit(core.Event{Kind: core.EventActivity, Active: true})
			close(agentEntered)
			<-agentRelease
			emit(core.Event{Kind: core.EventActivity})
			emit(core.Event{Kind: core.EventFinal, Done: true})
			return nil
		}, &telegramMessage{
			MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1,
			Voice: &telegramMedia{FileID: "voice-id", FileUniqueID: "unique"},
		})
	}()
	waitClosed(t, transcriptionEntered, "Telegram transcription")
	waitAtomicCount(t, &actions, 2, "Telegram typing during transcription")
	close(transcriptionRelease)
	waitClosed(t, agentEntered, "Telegram agent turn")
	beforeAgentRefresh := actions.Load()
	waitAtomicCount(t, &actions, beforeAgentRefresh+1, "Telegram typing during agent turn")
	close(agentRelease)
	waitClosed(t, done, "Telegram message completion")
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after the final event: %d -> %d", stoppedAt, got)
	}
}

func TestTelegramProactiveConversationEventUsesTypingLifecycle(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bottest/sendChatAction" {
			actions.Add(1)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bot.DeliverEvent(ctx, "TG-7", "activity", core.Event{Kind: core.EventActivity, Active: true}); err != nil {
		t.Fatal(err)
	}
	waitAtomicCount(t, &actions, 2, "proactive Telegram typing refresh")
	if err := bot.DeliverEvent(ctx, "TG-7", "activity", core.Event{Kind: core.EventActivity}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("proactive Telegram typing continued after stop: %d -> %d", stoppedAt, got)
	}
}

func TestTelegramFrameworkOnlyResponseDoesNotStartTyping(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bottest/sendChatAction" {
			actions.Add(1)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventFinal, Text: "local result", Done: true, Local: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "/status"})

	time.Sleep(20 * time.Millisecond)
	if got := actions.Load(); got != 0 {
		t.Fatalf("framework-only response emitted %d typing actions", got)
	}
}

func TestTelegramHandlerReturnWithoutTerminalStopsArrivalActivity(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bottest/sendChatAction" {
			actions.Add(1)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	bot.handle(context.Background(), func(context.Context, core.Message, core.Emit) error {
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})

	time.Sleep(20 * time.Millisecond)
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after handler return: %d -> %d", stoppedAt, got)
	}
}

func TestTelegramHandlerFailureStopsActivityBeforeErrorDelivery(t *testing.T) {
	activityEntered := make(chan struct{})
	var activityContext context.Context
	stoppedBeforeDelivery := false
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.client = &http.Client{Transport: telegramRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			activityContext = request.Context()
			close(activityEntered)
			<-request.Context().Done()
			return nil, request.Context().Err()
		case "/bottest/sendMessage":
			stoppedBeforeDelivery = activityContext != nil && activityContext.Err() != nil
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
				Request:    request,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected Telegram API path %q", request.URL.Path)
		}
	})}

	bot.handle(context.Background(), func(context.Context, core.Message, core.Emit) error {
		<-activityEntered
		return errors.New("provider admission failed")
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})

	if !stoppedBeforeDelivery {
		t.Fatal("Telegram handler failure was delivered before its activity context stopped")
	}
}

func TestTelegramPanicUnwindStopsAgentActivity(t *testing.T) {
	var actions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/bottest/sendChatAction" {
			actions.Add(1)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Telegram handler panic was not propagated")
			}
		}()
		bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
			emit(core.Event{Kind: core.EventActivity, Active: true})
			waitAtomicCount(t, &actions, 1, "Telegram typing before panic")
			panic("provider panic")
		}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})
	}()

	time.Sleep(20 * time.Millisecond)
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after panic unwind: %d -> %d", stoppedAt, got)
	}
}

func TestTelegramTypingStopsBeforeFinalDelivery(t *testing.T) {
	var actions atomic.Int32
	deliveryEntered := make(chan struct{})
	deliveryRelease := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/sendChatAction":
			actions.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/sendMessage":
			close(deliveryEntered)
			<-deliveryRelease
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
			emit(core.Event{Kind: core.EventActivity, Active: true})
			waitAtomicCount(t, &actions, 1, "Telegram typing before final delivery")
			emit(core.Event{Kind: core.EventFinal, Text: "intermediate", Done: true, Continues: true})
			emit(core.Event{Kind: core.EventActivity})
			emit(core.Event{Kind: core.EventFinal, Text: "complete", Done: true})
			return nil
		}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 42, Type: "private"}, Date: 1, Text: "hello"})
	}()
	waitClosed(t, deliveryEntered, "Telegram final delivery")
	stoppedAt := actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		close(deliveryRelease)
		t.Fatalf("Telegram typing continued during final delivery: %d -> %d", stoppedAt, got)
	}
	close(deliveryRelease)
	waitClosed(t, done, "Telegram final delivery completion")
	stoppedAt = actions.Load()
	time.Sleep(30 * time.Millisecond)
	if got := actions.Load(); got != stoppedAt {
		t.Fatalf("Telegram typing continued after delivery: %d -> %d", stoppedAt, got)
	}
}

func waitAtomicCount(t *testing.T, value *atomic.Int32, minimum int32, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value.Load() >= minimum {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (got %d, want at least %d)", description, value.Load(), minimum)
}

func waitClosed(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestWebhookModeVerifiesSecretAndRoutesUpdate(t *testing.T) {
	var webhookURL string
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/setWebhook":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			webhookURL, _ = payload["url"].(string)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/deleteWebhook":
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer api.Close()
	bot := New(config.Telegram{Mode: "webhook", WebhookURL: "https://public.example", WebhookListen: "127.0.0.1:0", WebhookSecret: "secret", GroupMode: "mention", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = api.URL
	statuses := make(chan channel.ConnectionStatus, 4)
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses <- status })
	messages := make(chan core.Message, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx, func(_ context.Context, message core.Message, _ core.Emit) error {
			messages <- message
			return nil
		})
	}()
	var detail string
	select {
	case status := <-statuses:
		if status.State != channel.ConnectionConnected {
			t.Fatalf("webhook status = %#v", status)
		}
		detail = status.Detail
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook listener")
	}
	parsed, err := url.Parse(webhookURL)
	if err != nil || parsed.Path == "" {
		t.Fatalf("registered webhook = %q, %v", webhookURL, err)
	}
	marker := " via "
	index := strings.LastIndex(detail, marker)
	if index < 0 {
		t.Fatalf("webhook status detail = %q", detail)
	}
	if strings.Contains(detail, parsed.Path) || strings.Contains(detail, webhookURL) {
		t.Fatalf("webhook status exposed its private public URL: %q", detail)
	}
	localURL := "http://" + detail[index+len(marker):] + parsed.Path
	post := func(secret string) int {
		request, _ := http.NewRequest(http.MethodPost, localURL, strings.NewReader(`{"update_id":1,"message":{"message_id":2,"from":{"id":7,"username":"trusted"},"chat":{"id":42,"type":"private"},"date":1,"text":"hello"}}`))
		request.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := post("wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong-secret status = %d", status)
	}
	if status := post("secret"); status != http.StatusOK {
		t.Fatalf("valid webhook status = %d", status)
	}
	select {
	case message := <-messages:
		if message.Text != "hello" || message.Conversation != "TG-7" {
			t.Fatalf("webhook message = %#v", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook update was not routed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook bot did not stop")
	}
}

func TestWebhookAuthorizationLossStopsListenerAndDeletesWebhook(t *testing.T) {
	allowed := []string{"7"}
	var webhookURL string
	var deletes atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bottest/getMe":
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":1,"username":"spynel_test_bot"}}`))
		case "/bottest/setWebhook":
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			webhookURL, _ = payload["url"].(string)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case "/bottest/deleteWebhook":
			deletes.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer api.Close()
	bot := New(config.Telegram{Mode: "webhook", WebhookURL: "https://public.example", WebhookListen: "127.0.0.1:0", WebhookSecret: "secret", AllowedUsers: allowed}, "test")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	bot.baseURL = api.URL
	statuses := make(chan channel.ConnectionStatus, 4)
	bot.SetStatusReporter(func(status channel.ConnectionStatus) { statuses <- status })
	done := make(chan error, 1)
	go func() {
		done <- bot.Run(context.Background(), func(context.Context, core.Message, core.Emit) error { return nil })
	}()
	var localURL string
	select {
	case status := <-statuses:
		if status.State != channel.ConnectionConnected {
			t.Fatalf("webhook status = %#v", status)
		}
		parsed, err := url.Parse(webhookURL)
		if err != nil || parsed.Path == "" {
			t.Fatalf("registered webhook = %q, %v", webhookURL, err)
		}
		const marker = " via "
		index := strings.LastIndex(status.Detail, marker)
		if index < 0 {
			t.Fatalf("webhook status detail = %q", status.Detail)
		}
		localURL = "http://" + status.Detail[index+len(marker):] + parsed.Path
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook listener")
	}
	allowed = nil
	request, _ := http.NewRequest(http.MethodPost, localURL, strings.NewReader(`{"update_id":1,"message":{"message_id":2,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"blocked"}}`))
	request.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("revoked webhook status = %d", response.StatusCode)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errTelegramRuntimeAuthorization) {
			t.Fatalf("webhook Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoked webhook listener did not stop")
	}
	if deletes.Load() != 1 {
		t.Fatalf("revoked webhook cleanup calls = %d, want 1", deletes.Load())
	}
	if _, err := http.DefaultClient.Get(localURL); err == nil {
		t.Fatal("revoked webhook listener still accepts connections")
	}
}

type telegramPayloadRecorder struct {
	mu       sync.Mutex
	messages []map[string]any
	actions  []map[string]any
}

func (r *telegramPayloadRecorder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	var payload map[string]any
	switch {
	case strings.HasSuffix(request.URL.Path, "/sendMessage"), strings.HasSuffix(request.URL.Path, "/sendChatAction"):
		_ = json.NewDecoder(request.Body).Decode(&payload)
	}
	r.mu.Lock()
	switch {
	case strings.HasSuffix(request.URL.Path, "/sendMessage"):
		r.messages = append(r.messages, payload)
	case strings.HasSuffix(request.URL.Path, "/sendChatAction"):
		r.actions = append(r.actions, payload)
	}
	r.mu.Unlock()
	_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
}

func (r *telegramPayloadRecorder) snapshot() (messages, actions []map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.messages...), append([]map[string]any(nil), r.actions...)
}

func waitRecordedTelegramActions(t *testing.T, recorder *telegramPayloadRecorder, minimum int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, actions := recorder.snapshot(); len(actions) >= minimum {
			return actions
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d recorded Telegram actions", minimum)
	return nil
}

func TestInboundMessageRoutesCanonicalTelegramConversations(t *testing.T) {
	bot := New(config.Telegram{GroupMode: "all", AllowedUsers: []string{"7"}}, "test")
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("inbound conversation routing contacted Telegram")
	})
	private := func(threadID int64, text string) *telegramMessage {
		return &telegramMessage{MessageID: 1, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, MessageThreadID: threadID, Date: 1, Text: text}
	}
	group := func(threadID int64, text string) *telegramMessage {
		return &telegramMessage{MessageID: 2, From: telegramUser{ID: 7}, Chat: telegramChat{ID: -100, Type: "supergroup"}, MessageThreadID: threadID, Date: 2, Text: text}
	}
	tests := []struct {
		name    string
		message *telegramMessage
		want    string
	}{
		{name: "private absent thread", message: private(0, "/status"), want: "TG-7"},
		{name: "private general topic", message: private(1, "/status"), want: "TG-7"},
		{name: "private topic", message: private(5, "/status"), want: "TG-7-topic-5"},
		{name: "group absent thread", message: group(0, "/status"), want: "TG-group--100"},
		{name: "group general topic", message: group(1, "/status"), want: "TG-group--100"},
		{name: "group first topic", message: group(2, "/status"), want: "TG-group--100-topic-2"},
		{name: "group second topic", message: group(3, "/status"), want: "TG-group--100-topic-3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got core.Message
			bot.processUpdate(context.Background(), func(_ context.Context, message core.Message, _ core.Emit) error {
				got = message
				return nil
			}, telegramUpdate{Message: test.message})
			if got.Conversation != test.want {
				t.Fatalf("conversation = %q, want %q", got.Conversation, test.want)
			}
		})
	}
}

func TestTelegramTopicRichReplyChunksCarryThreadOnEveryMessage(t *testing.T) {
	recorder := &telegramPayloadRecorder{}
	server := httptest.NewServer(recorder)
	defer server.Close()
	bot := New(config.Telegram{GroupMode: "all", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL

	var rich strings.Builder
	for index := 0; index < 120; index++ {
		fmt.Fprintf(&rich, "## Section %d\n\n- **bold %d** and `code %d` for [link](https://example.com/%d)\n\n%s\n\n", index, index, index, index, strings.Repeat("word ", 60))
	}
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		// The arrival typing signal runs asynchronously; hold the turn open until
		// its provider call is observed so the assertion below is deterministic.
		waitRecordedTelegramActions(t, recorder, 1)
		emit(core.Event{Kind: core.EventFinal, Text: rich.String(), Done: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: -100, Type: "supergroup"}, MessageThreadID: 2, Date: 1, Text: "hello"})

	messages, actions := recorder.snapshot()
	if len(messages) < 2 || len(messages) > markdownfmt.TelegramMaxChunks {
		t.Fatalf("chunk count = %d, want 2..%d", len(messages), markdownfmt.TelegramMaxChunks)
	}
	visibleTotal := 0
	for index, payload := range messages {
		if payload["chat_id"] != "-100" || payload["message_thread_id"] != float64(2) {
			t.Fatalf("chunk %d destination = %#v", index, payload)
		}
		if payload["parse_mode"] != "HTML" || payload["disable_web_page_preview"] != true {
			t.Fatalf("chunk %d formatting = %#v", index, payload)
		}
		text, _ := payload["text"].(string)
		visible := utf8.RuneCountInString(markdownfmt.TelegramChunkPlainText(text))
		if visible <= 0 || visible > markdownfmt.TelegramMaxVisiblePerMessage {
			t.Fatalf("chunk %d visible length = %d", index, visible)
		}
		visibleTotal += visible
		if index == 0 {
			if params, ok := payload["reply_parameters"].(map[string]any); !ok || params["message_id"] != float64(8) {
				t.Fatalf("first chunk reply parameters = %#v", payload["reply_parameters"])
			}
			continue
		}
		if _, ok := payload["reply_parameters"]; ok {
			t.Fatalf("chunk %d repeated reply parameters", index)
		}
	}
	if visibleTotal > markdownfmt.TelegramMaxVisiblePerReply {
		t.Fatalf("visible reply length = %d, want at most %d", visibleTotal, markdownfmt.TelegramMaxVisiblePerReply)
	}
	last, _ := messages[len(messages)-1]["text"].(string)
	if !strings.Contains(markdownfmt.TelegramChunkPlainText(last), "response truncated") {
		t.Fatalf("oversized topic reply was not truncated: %q", markdownfmt.TelegramChunkPlainText(last))
	}
	if len(actions) == 0 {
		t.Fatal("topic turn emitted no typing action")
	}
	for _, payload := range actions {
		if payload["chat_id"] != "-100" || payload["message_thread_id"] != float64(2) {
			t.Fatalf("typing destination = %#v", payload)
		}
	}
}

func TestTelegramBaseDeliveryOmitsMessageThreadID(t *testing.T) {
	recorder := &telegramPayloadRecorder{}
	server := httptest.NewServer(recorder)
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		emit(core.Event{Kind: core.EventFinal, Text: "plain response", Done: true})
		return nil
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, Date: 1, Text: "hello"})

	messages, actions := recorder.snapshot()
	if len(messages) != 1 || messages[0]["text"] != "plain response" {
		t.Fatalf("messages = %#v", messages)
	}
	for _, payload := range append(messages, actions...) {
		if _, ok := payload["message_thread_id"]; ok {
			t.Fatalf("base conversation sent a thread id: %#v", payload)
		}
	}
}

func TestTelegramTopicErrorsAndProactiveDeliveryCarryThread(t *testing.T) {
	recorder := &telegramPayloadRecorder{}
	server := httptest.NewServer(recorder)
	defer server.Close()
	bot := New(config.Telegram{GroupMode: "all", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.handle(context.Background(), func(context.Context, core.Message, core.Emit) error {
		return errors.New("handler failed")
	}, &telegramMessage{MessageID: 8, From: telegramUser{ID: 7}, Chat: telegramChat{ID: -100, Type: "supergroup"}, MessageThreadID: 2, Date: 1, Text: "hello"})
	if err := bot.DeliverEvent(context.Background(), "TG-7-topic-5", "event", core.Event{Kind: core.EventFinal, Text: "proactive", Done: true}); err != nil {
		t.Fatal(err)
	}
	if err := bot.Deliver(context.Background(), "TG-group--100-topic-2", "event", "direct"); err != nil {
		t.Fatal(err)
	}

	messages, _ := recorder.snapshot()
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0]["text"] != "Error handler failed" || messages[0]["message_thread_id"] != float64(2) {
		t.Fatalf("topic error message = %#v", messages[0])
	}
	if messages[1]["text"] != "proactive" || messages[1]["chat_id"] != "7" || messages[1]["message_thread_id"] != float64(5) {
		t.Fatalf("private topic proactive message = %#v", messages[1])
	}
	if messages[2]["text"] != "direct" || messages[2]["chat_id"] != "-100" || messages[2]["message_thread_id"] != float64(2) {
		t.Fatalf("group topic direct message = %#v", messages[2])
	}
}

func TestTelegramTopicAttachmentsCarryMessageThreadID(t *testing.T) {
	root := t.TempDir()
	documentPath := filepath.Join(root, "report.txt")
	photoPath := filepath.Join(root, "photo.png")
	if err := os.WriteFile(documentPath, []byte("report body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(photoPath, []byte("photo body"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		requests <- request
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()
	route, err := ParseConversation("TG-group--100-topic-9")
	if err != nil {
		t.Fatal(err)
	}
	bot := New(config.Telegram{GroupMode: "all", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	for _, attachment := range []core.OutboundAttachment{
		{Kind: "attachment", Name: "report.txt", Path: documentPath, MediaType: "text/plain", MaxBytes: 1024},
		{Kind: "photo", Name: "photo.png", Path: photoPath, MediaType: "image/png", MaxBytes: 1024},
	} {
		if err := bot.sendAttachment(context.Background(), route, attachment, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := <-requests; got.URL.Path != "/bottest/sendDocument" || got.FormValue("chat_id") != "-100" || got.FormValue("message_thread_id") != "9" {
		t.Fatalf("document request = %q %#v", got.URL.Path, got.MultipartForm.Value)
	}
	if got := <-requests; got.URL.Path != "/bottest/sendPhoto" || got.FormValue("chat_id") != "-100" || got.FormValue("message_thread_id") != "9" {
		t.Fatalf("photo request = %q %#v", got.URL.Path, got.MultipartForm.Value)
	}
}

func waitThreadCount(t *testing.T, mu *sync.Mutex, counts map[int64]int, threadID int64, minimum int, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := counts[threadID]
		mu.Unlock()
		if got >= minimum {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestTelegramTopicActivityDoesNotCrossCancel(t *testing.T) {
	var mu sync.Mutex
	counts := map[int64]int{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/sendChatAction") {
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			if thread, ok := payload["message_thread_id"].(float64); ok {
				mu.Lock()
				counts[int64(thread)]++
				mu.Unlock()
			}
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{GroupMode: "all", AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.activity = newTelegramActivity(bot, 10*time.Millisecond)
	topic := func(threadID int64) *telegramMessage {
		return &telegramMessage{MessageID: threadID, From: telegramUser{ID: 7}, Chat: telegramChat{ID: -100, Type: "supergroup"}, MessageThreadID: threadID, Date: 1, Text: "hello"}
	}
	entered := map[int64]chan struct{}{2: make(chan struct{}), 3: make(chan struct{})}
	releases := map[int64]chan struct{}{2: make(chan struct{}), 3: make(chan struct{})}
	done := map[int64]chan struct{}{2: make(chan struct{}), 3: make(chan struct{})}
	for _, threadID := range []int64{2, 3} {
		go func() {
			defer close(done[threadID])
			bot.handle(context.Background(), func(context.Context, core.Message, core.Emit) error {
				close(entered[threadID])
				<-releases[threadID]
				return nil
			}, topic(threadID))
		}()
	}
	waitClosed(t, entered[2], "topic 2 handler")
	waitClosed(t, entered[3], "topic 3 handler")
	waitThreadCount(t, &mu, counts, 2, 2, "topic 2 typing")
	waitThreadCount(t, &mu, counts, 3, 2, "topic 3 typing")
	close(releases[3])
	waitClosed(t, done[3], "topic 3 completion")
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	stoppedTopic3, beforeTopic2 := counts[3], counts[2]
	mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	afterTopic3, afterTopic2 := counts[3], counts[2]
	mu.Unlock()
	if afterTopic3 != stoppedTopic3 {
		t.Fatalf("topic 3 typing continued after its turn stopped: %d -> %d", stoppedTopic3, afterTopic3)
	}
	if afterTopic2 <= beforeTopic2 {
		t.Fatalf("topic 2 typing stopped with topic 3: %d -> %d", beforeTopic2, afterTopic2)
	}
	close(releases[2])
	waitClosed(t, done[2], "topic 2 completion")
}

func TestTelegramOutboundRoutesFailClosedBeforeProviderCalls(t *testing.T) {
	providerCalls := 0
	bot := New(config.Telegram{GroupMode: "off", AllowedUsers: []string{"7"}}, "test")
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected Telegram provider call")
	})
	for _, conversation := range []string{
		"", "TG-", "tg-7", "TG-7x", "TG-0", "TG-7-topic-0", "TG-7-topic-1", "TG-7-topic-",
		"TG-group-100", "TG-group--100", "TG-group--100-topic-2", "TG-8", "TG-8-topic-4",
	} {
		if err := bot.Deliver(context.Background(), conversation, "event", "text"); err == nil {
			t.Fatalf("Deliver(%q) succeeded for a failing route", conversation)
		}
		if err := bot.DeliverEvent(context.Background(), conversation, "event", core.Event{Kind: core.EventFinal, Text: "text", Done: true}); err == nil {
			t.Fatalf("DeliverEvent(%q) succeeded for a failing route", conversation)
		}
	}
	if providerCalls != 0 {
		t.Fatalf("failing routes contacted Telegram: %d calls", providerCalls)
	}
	bot.RevokeRuntimeAuthorization()
	if err := bot.Deliver(context.Background(), "TG-7", "event", "text"); err == nil {
		t.Fatal("revoked runtime delivered text")
	}
	if err := bot.DeliverEvent(context.Background(), "TG-7", "event", core.Event{Kind: core.EventActivity, Active: true}); err == nil {
		t.Fatal("revoked runtime delivered activity")
	}
	if providerCalls != 0 {
		t.Fatalf("revoked runtime contacted Telegram: %d calls", providerCalls)
	}
}

func TestTelegramHTMLParseFailureFallsBackToPlainTextOnce(t *testing.T) {
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var payload map[string]any
		_ = json.NewDecoder(request.Body).Decode(&payload)
		payloads = append(payloads, payload)
		if len(payloads) == 1 {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Unexpected end tag at byte offset 12"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":5}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	route, err := ParseConversation("TG-7-topic-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.send(context.Background(), route, "**bold** and `code`", 0); err != nil {
		t.Fatalf("HTML parse fallback: %v", err)
	}
	if len(payloads) != 2 {
		t.Fatalf("sendMessage attempts = %d, want 2", len(payloads))
	}
	htmlText, _ := payloads[0]["text"].(string)
	if payloads[0]["parse_mode"] != "HTML" {
		t.Fatalf("first attempt = %#v", payloads[0])
	}
	if _, ok := payloads[1]["parse_mode"]; ok {
		t.Fatalf("plain fallback retained parse_mode: %#v", payloads[1])
	}
	if payloads[1]["text"] != markdownfmt.TelegramChunkPlainText(htmlText) {
		t.Fatalf("plain fallback text = %#v", payloads[1]["text"])
	}
	for index, payload := range payloads {
		if payload["message_thread_id"] != float64(5) {
			t.Fatalf("attempt %d thread = %#v", index, payload["message_thread_id"])
		}
	}
}

func TestTelegramOtherSendFailuresDoNotDowngrade(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "SECRET-TOKEN")
	bot.baseURL = server.URL
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	err = bot.send(context.Background(), route, "**bold**", 0)
	if err == nil {
		t.Fatal("non-parse send failure was downgraded")
	}
	var apiErr *telegramAPIError
	if !errors.As(err, &apiErr) || apiErr.parse || apiErr.code != http.StatusBadRequest {
		t.Fatalf("send error = %#v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("send attempts = %d, want 1", attempts.Load())
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("send error leaked the bot token: %v", err)
	}
}

func TestTelegramRateLimitRetriesOnceWithBoundedWait(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	if _, err := bot.call(context.Background(), "sendMessage", map[string]any{"chat_id": "7", "text": "hello"}); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("provider attempts = %d, want 2", attempts.Load())
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("rate-limit waits = %#v", waits)
	}
}

func TestTelegramRateLimitAboveCapFailsWithoutWaiting(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		attempts.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 90","parameters":{"retry_after":90}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	waited := false
	bot.retryWait = func(context.Context, time.Duration) error {
		waited = true
		return nil
	}
	if _, err := bot.call(context.Background(), "sendMessage", map[string]any{"chat_id": "7", "text": "hello"}); err == nil {
		t.Fatal("rate limit beyond the cap did not fail")
	}
	if attempts.Load() != 1 {
		t.Fatalf("provider attempts = %d, want 1", attempts.Load())
	}
	if waited {
		t.Fatal("rate limit beyond the cap waited")
	}
}

func TestTelegramRateLimitNeverLoopsMoreThanOnce(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		attempts.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 2","parameters":{"retry_after":2}}`))
	}))
	defer server.Close()
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	bot.retryWait = func(context.Context, time.Duration) error { return nil }
	if _, err := bot.call(context.Background(), "sendMessage", map[string]any{"chat_id": "7", "text": "hello"}); err == nil {
		t.Fatal("persistent rate limit did not fail")
	}
	if attempts.Load() != 2 {
		t.Fatalf("provider attempts = %d, want 2", attempts.Load())
	}
}

func TestTelegramRateLimitRetryRechecksRuntimeAuthorization(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		attempts.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`))
	}))
	defer server.Close()
	allowed := []string{"7"}
	bot := New(config.Telegram{AllowedUsers: allowed}, "test")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	bot.baseURL = server.URL
	bot.retryWait = func(context.Context, time.Duration) error {
		allowed = nil
		return nil
	}
	if _, err := bot.call(context.Background(), "sendMessage", map[string]any{"chat_id": "7", "text": "hello"}); !errors.Is(err, errTelegramRuntimeAuthorization) {
		t.Fatalf("retry error = %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("revoked retry contacted Telegram %d times, want 1", attempts.Load())
	}
}

func TestTelegramRateLimitRetryReauthorizesPrivateTopicRecipient(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		attempts.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`))
	}))
	defer server.Close()
	allowed := []string{"7"}
	bot := New(config.Telegram{AllowedUsers: allowed}, "test")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	bot.baseURL = server.URL
	bot.retryWait = func(context.Context, time.Duration) error {
		// The list stays valid while the private topic recipient is replaced.
		allowed = []string{"8"}
		return nil
	}
	route, err := ParseConversation("TG-7-topic-5")
	if err != nil {
		t.Fatal(err)
	}
	err = bot.send(context.Background(), route, "hello", 0)
	if err == nil || !strings.Contains(err.Error(), "allowed_users") {
		t.Fatalf("private topic retry error = %v", err)
	}
	if errors.Is(err, errTelegramRuntimeAuthorization) {
		t.Fatalf("recipient revocation was misreported as global authorization loss: %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("revoked private topic contacted Telegram %d times, want 1", attempts.Load())
	}
}

func TestTelegramRateLimitRetryReauthorizesGroupRoute(t *testing.T) {
	tests := []struct {
		name      string
		groupMode string
		wantErr   string
		wantCalls int32
	}{
		{name: "still allowed", groupMode: "all", wantCalls: 2},
		{name: "disabled during wait", groupMode: "off", wantErr: "group delivery is disabled", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if attempts.Add(1) == 1 {
					writer.WriteHeader(http.StatusTooManyRequests)
					_, _ = writer.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`))
					return
				}
				_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			}))
			defer server.Close()
			bot := New(config.Telegram{GroupMode: "all", AllowedUsers: []string{"7"}}, "test")
			bot.baseURL = server.URL
			bot.retryWait = func(context.Context, time.Duration) error {
				bot.config.GroupMode = test.groupMode
				return nil
			}
			route, err := ParseConversation("TG-group--100-topic-9")
			if err != nil {
				t.Fatal(err)
			}
			err = bot.send(context.Background(), route, "hello", 0)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("group retry error = %v, want %q", err, test.wantErr)
				}
			} else if err != nil {
				t.Fatalf("group retry failed: %v", err)
			}
			if got := attempts.Load(); got != test.wantCalls {
				t.Fatalf("provider calls = %d, want %d", got, test.wantCalls)
			}
		})
	}
}

func TestTelegramProviderBoundaryFailsClosedForInvalidRoute(t *testing.T) {
	providerCalls := 0
	bot := New(config.Telegram{GroupMode: "all", AllowedUsers: []string{"7"}}, "test")
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected Telegram provider call")
	})
	for _, route := range []Route{{}, {chatID: 7, group: true}, {chatID: 0}, {chatID: 7, threadID: -1}} {
		if err := bot.send(context.Background(), route, "text", 0); err == nil {
			t.Fatalf("send(%+v) succeeded for an invalid route", route)
		}
		if err := bot.action(context.Background(), route, "typing"); err == nil {
			t.Fatalf("action(%+v) succeeded for an invalid route", route)
		}
	}
	if providerCalls != 0 {
		t.Fatalf("invalid routes contacted Telegram: %d calls", providerCalls)
	}
}

func TestTelegramAttachmentBoundaryReauthorizesRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte("report body"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed := []string{"7"}
	providerCalls := 0
	bot := New(config.Telegram{AllowedUsers: allowed}, "test")
	bot.SetAllowedUsersSource(func() []string { return allowed })
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("unexpected Telegram provider call")
	})
	route, err := ParseConversation("TG-7-topic-5")
	if err != nil {
		t.Fatal(err)
	}
	// The list stays globally valid while the attachment recipient is revoked.
	allowed = []string{"8"}
	err = bot.sendAttachment(context.Background(), route, core.OutboundAttachment{
		Kind: "attachment", Name: "report.txt", Path: path, MediaType: "text/plain", MaxBytes: 1024,
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "allowed_users") {
		t.Fatalf("revoked attachment route error = %v", err)
	}
	if providerCalls != 0 {
		t.Fatalf("revoked attachment route contacted Telegram: %d calls", providerCalls)
	}
}
