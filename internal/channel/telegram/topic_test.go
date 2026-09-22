package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/channel"
	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
)

// topicTitleRecorder captures every editForumTopic payload and replies with a
// configurable response per call so provider outcomes stay deterministic.
type topicTitleRecorder struct {
	mu       sync.Mutex
	requests []map[string]any
	statuses []int
	bodies   []string
}

func (r *topicTitleRecorder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var payload map[string]any
	_ = json.NewDecoder(request.Body).Decode(&payload)
	r.mu.Lock()
	index := len(r.requests)
	r.requests = append(r.requests, payload)
	status, body := http.StatusOK, `{"ok":true,"result":{}}`
	if index < len(r.statuses) && r.statuses[index] != 0 {
		status = r.statuses[index]
	}
	if index < len(r.bodies) && r.bodies[index] != "" {
		body = r.bodies[index]
	}
	r.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(body))
}

func (r *topicTitleRecorder) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.requests...)
}

func newTopicTitleBot(t *testing.T, recorder http.Handler) *Bot {
	t.Helper()
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	return bot
}

func TestTelegramTopicTitleTrackingIsTriStateBoundedAndResetSafe(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.client.Transport = telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("title tracking contacted Telegram")
	})
	handled := 0
	update := func(message *telegramMessage) {
		t.Helper()
		bot.processUpdate(context.Background(), func(context.Context, core.Message, core.Emit) error {
			handled++
			return nil
		}, telegramUpdate{Message: message})
	}
	serviceMessage := func(thread int64, created *telegramForumTopicCreated, edited *telegramForumTopicEdited) *telegramMessage {
		return &telegramMessage{
			MessageID: 1, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, MessageThreadID: thread,
			Date: 1, ForumTopicCreated: created, ForumTopicEdited: edited,
		}
	}
	implicit := topicTitleKey{chatID: 7, threadID: 5}
	if got := bot.topicTitleState(implicit); got != topicTitleUnknown {
		t.Fatalf("initial topic state = %v, want unknown", got)
	}
	update(serviceMessage(5, &telegramForumTopicCreated{Name: "New chat", IsNameImplicit: true}, nil))
	if got := bot.topicTitleState(implicit); got != topicTitleImplicit {
		t.Fatalf("implicit created state = %v", got)
	}
	explicit := topicTitleKey{chatID: 7, threadID: 6}
	update(serviceMessage(6, &telegramForumTopicCreated{Name: "Release planning"}, nil))
	if got := bot.topicTitleState(explicit); got != topicTitleExplicit {
		t.Fatalf("explicit created state = %v", got)
	}
	update(serviceMessage(5, nil, &telegramForumTopicEdited{}))
	if got := bot.topicTitleState(implicit); got != topicTitleImplicit {
		t.Fatalf("icon-only edit changed state to %v", got)
	}
	update(serviceMessage(5, nil, &telegramForumTopicEdited{Name: "Renamed"}))
	if got := bot.topicTitleState(implicit); got != topicTitleExplicit {
		t.Fatalf("named edit state = %v", got)
	}
	unauthorized := topicTitleKey{chatID: 7, threadID: 9}
	message := serviceMessage(9, &telegramForumTopicCreated{Name: "New chat", IsNameImplicit: true}, nil)
	message.From = telegramUser{ID: 99}
	update(message)
	if got := bot.topicTitleState(unauthorized); got != topicTitleUnknown {
		t.Fatalf("unauthorized sender recorded state %v", got)
	}
	if handled != 0 {
		t.Fatalf("service messages reached the handler %d times", handled)
	}

	// Restart-equivalent state is unknown, which fails closed for only-if-implicit.
	restarted := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	if got := restarted.topicTitleState(implicit); got != topicTitleUnknown {
		t.Fatalf("fresh process state = %v, want unknown", got)
	}

	// Bounded eviction keeps the map at its cap without refusing new records.
	filled := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	for index := 0; index < maxTrackedTopicTitles; index++ {
		filled.setTopicTitle(topicTitleKey{chatID: 7, threadID: int64(1000 + index)}, topicTitleExplicit)
	}
	filled.setTopicTitle(topicTitleKey{chatID: 7, threadID: 9000}, topicTitleImplicit)
	filled.topicTitleMu.Lock()
	size := len(filled.topicTitles)
	state := filled.topicTitles[topicTitleKey{chatID: 7, threadID: 9000}]
	filled.topicTitleMu.Unlock()
	if size > maxTrackedTopicTitles || state != topicTitleImplicit {
		t.Fatalf("bounded eviction: size=%d state=%v", size, state)
	}
}

func TestTelegramRenameConversationPayLoadAndImplicitState(t *testing.T) {
	recorder := &topicTitleRecorder{}
	bot := newTopicTitleBot(t, recorder)
	bot.setTopicTitle(topicTitleKey{chatID: 7, threadID: 5}, topicTitleImplicit)
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release candidate", true); err != nil {
		t.Fatal(err)
	}
	requests := recorder.snapshot()
	if len(requests) != 1 {
		t.Fatalf("editForumTopic requests = %d", len(requests))
	}
	want := map[string]any{"chat_id": "7", "message_thread_id": float64(5), "name": "Release candidate"}
	if !reflect.DeepEqual(requests[0], want) {
		t.Fatalf("editForumTopic payload = %#v, want %#v", requests[0], want)
	}
	if got := bot.topicTitleState(topicTitleKey{chatID: 7, threadID: 5}); got != topicTitleExplicit {
		t.Fatalf("successful rename state = %v", got)
	}
	// A repeated automatic rename is now a silent no-op because the state is
	// explicit, while a forced rename still reaches the provider.
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release candidate", true); err != nil {
		t.Fatal(err)
	}
	if len(recorder.snapshot()) != 1 {
		t.Fatal("only-if-implicit rename repeated after success")
	}
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Forced", false); err != nil {
		t.Fatal(err)
	}
	if len(recorder.snapshot()) != 2 {
		t.Fatal("forced rename did not reach the provider")
	}
}

func TestTelegramRenameConversationOnlyIfImplicitSkipsUnknownAndExplicit(t *testing.T) {
	recorder := &topicTitleRecorder{}
	bot := newTopicTitleBot(t, recorder)
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", true); err != nil {
		t.Fatalf("unknown topic error = %v", err)
	}
	bot.setTopicTitle(topicTitleKey{chatID: 7, threadID: 5}, topicTitleExplicit)
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", true); err != nil {
		t.Fatalf("explicit topic error = %v", err)
	}
	if requests := recorder.snapshot(); len(requests) != 0 {
		t.Fatalf("only-if-implicit renamed an unknown or explicit topic: %#v", requests)
	}
}

func TestTelegramRenameConversationRejectsInapplicableRoutesAndLabels(t *testing.T) {
	recorder := &topicTitleRecorder{}
	bot := newTopicTitleBot(t, recorder)
	for _, conversation := range []string{"TG-7", "TG-group--100", "TG-group--100-topic-2", "TG-not-canonical", "WA-15551234567"} {
		err := bot.RenameConversation(context.Background(), conversation, "Release", false)
		if err == nil {
			t.Fatalf("%s was accepted as a private Telegram topic", conversation)
		}
		if conversation != "TG-not-canonical" && conversation != "WA-15551234567" && !errors.Is(err, channel.ErrConversationLabelUnsupported) {
			t.Fatalf("%s rejection did not wrap the unsupported sentinel: %v", conversation, err)
		}
	}
	bot.setTopicTitle(topicTitleKey{chatID: 7, threadID: 5}, topicTitleImplicit)
	for name, label := range map[string]string{
		"empty":        "",
		"whitespace":   "   ",
		"multi-line":   "first\nsecond",
		"control":      "first\x00second",
		"too long":     strings.Repeat("a", maxTopicLabelRunes+1),
		"invalid utf8": string([]byte{0xff, 0xfe}),
	} {
		if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", label, true); err == nil {
			t.Fatalf("%s label was accepted", name)
		}
	}
	if requests := recorder.snapshot(); len(requests) != 0 {
		t.Fatalf("rejected routes or labels reached the provider: %#v", requests)
	}
}

func TestTelegramRenameConversationTreatsTopicNotModifiedAsSuccess(t *testing.T) {
	recorder := &topicTitleRecorder{
		statuses: []int{http.StatusBadRequest},
		bodies:   []string{`{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_NOT_MODIFIED"}`},
	}
	bot := newTopicTitleBot(t, recorder)
	bot.setTopicTitle(topicTitleKey{chatID: 7, threadID: 5}, topicTitleImplicit)
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", true); err != nil {
		t.Fatalf("TOPIC_NOT_MODIFIED error = %v", err)
	}
	if got := bot.topicTitleState(topicTitleKey{chatID: 7, threadID: 5}); got != topicTitleExplicit {
		t.Fatalf("TOPIC_NOT_MODIFIED state = %v", got)
	}
}

func TestTelegramRenameConversationPropagatesOtherProviderErrors(t *testing.T) {
	recorder := &topicTitleRecorder{
		statuses: []int{http.StatusBadRequest},
		bodies:   []string{`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`},
	}
	bot := newTopicTitleBot(t, recorder)
	key := topicTitleKey{chatID: 7, threadID: 5}
	bot.setTopicTitle(key, topicTitleImplicit)
	err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", true)
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("unrelated provider error = %v", err)
	}
	if got := bot.topicTitleState(key); got != topicTitleImplicit {
		t.Fatalf("failed rename changed the tracked state to %v", got)
	}
}

func TestTelegramRenameConversationReauthorizesBeforeEveryProviderCall(t *testing.T) {
	t.Run("revoked authorization", func(t *testing.T) {
		recorder := &topicTitleRecorder{}
		bot := newTopicTitleBot(t, recorder)
		bot.setTopicTitle(topicTitleKey{chatID: 7, threadID: 5}, topicTitleImplicit)
		bot.SetAllowedUsersSource(func() []string { return nil })
		err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", true)
		if err == nil || !strings.Contains(err.Error(), "authorization") {
			t.Fatalf("revoked rename error = %v", err)
		}
		if requests := recorder.snapshot(); len(requests) != 0 {
			t.Fatalf("revoked rename reached the provider: %#v", requests)
		}
	})

	t.Run("retry rechecks authorization", func(t *testing.T) {
		recorder := &topicTitleRecorder{
			statuses: []int{http.StatusTooManyRequests},
			bodies:   []string{`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`},
		}
		bot := newTopicTitleBot(t, recorder)
		bot.setTopicTitle(topicTitleKey{chatID: 7, threadID: 5}, topicTitleImplicit)
		bot.retryWait = func(context.Context, time.Duration) error {
			bot.SetAllowedUsersSource(func() []string { return nil })
			return nil
		}
		err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", true)
		if err == nil || !strings.Contains(err.Error(), "authorization") {
			t.Fatalf("retry authorization error = %v", err)
		}
		if requests := recorder.snapshot(); len(requests) != 1 {
			t.Fatalf("retry after revocation reached the provider %d times", len(requests))
		}
	})

	t.Run("bounded retry succeeds after reauthorization", func(t *testing.T) {
		recorder := &topicTitleRecorder{
			statuses: []int{http.StatusTooManyRequests},
			bodies:   []string{`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`},
		}
		bot := newTopicTitleBot(t, recorder)
		bot.setTopicTitle(topicTitleKey{chatID: 7, threadID: 5}, topicTitleImplicit)
		bot.retryWait = func(context.Context, time.Duration) error { return nil }
		if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", true); err != nil {
			t.Fatalf("bounded retry error = %v", err)
		}
		if requests := recorder.snapshot(); len(requests) != 2 {
			t.Fatalf("bounded retry requests = %d, want 2", len(requests))
		}
	})
}
