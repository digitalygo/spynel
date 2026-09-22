package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/channel"
	"github.com/digitalygo/spynel/internal/config"
)

// topicRenameRecorder captures every editForumTopic payload and replies with
// a configurable response per call so provider outcomes stay deterministic.
type topicRenameRecorder struct {
	mu       sync.Mutex
	requests []map[string]any
	statuses []int
	bodies   []string
}

func (r *topicRenameRecorder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
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

func (r *topicRenameRecorder) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.requests...)
}

func newTopicRenameBot(t *testing.T, recorder http.Handler) *Bot {
	t.Helper()
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	bot.baseURL = server.URL
	return bot
}

func TestTelegramAutomaticRenameFiresOncePerConversation(t *testing.T) {
	recorder := &topicRenameRecorder{}
	bot := newTopicRenameBot(t, recorder)
	// A first-session trigger always renames the private topic: there is no
	// title-state input to consult, so a freshly created topic that is still
	// called "New chat" is replaced without any transport-reported state.
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release candidate", false); err != nil {
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
	if !bot.topicRenamed("TG-7-topic-5") {
		t.Fatal("successful automatic rename did not mark the conversation renamed")
	}
	// A later automatic attempt for the same conversation, such as a new
	// session created after /clear, is a silent no-op with zero provider calls.
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release candidate", false); err != nil {
		t.Fatal(err)
	}
	if len(recorder.snapshot()) != 1 {
		t.Fatal("automatic rename repeated after success")
	}
	bot.renamedTopicsMu.Lock()
	_, secondConversation := bot.renamedTopics["TG-7-topic-6"]
	bot.renamedTopicsMu.Unlock()
	if secondConversation {
		t.Fatal("one conversation's rename marked another conversation")
	}
}

func TestTelegramForcedRenameAlwaysRenames(t *testing.T) {
	recorder := &topicRenameRecorder{}
	bot := newTopicRenameBot(t, recorder)
	// /pi name renames even a topic Spynel never touched automatically.
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Explicit", true); err != nil {
		t.Fatal(err)
	}
	// And it renames even after Spynel already renamed the topic.
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Forced", true); err != nil {
		t.Fatal(err)
	}
	if requests := recorder.snapshot(); len(requests) != 2 {
		t.Fatalf("forced rename requests = %d, want 2", len(requests))
	}
	// The forced renames keep the mark, so automatic logic stays silent.
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Automatic again", false); err != nil {
		t.Fatal(err)
	}
	if requests := recorder.snapshot(); len(requests) != 2 {
		t.Fatalf("automatic rename ran after forced renames: %d requests", len(requests))
	}
}

func TestTelegramAutomaticRenameSurvivesNothingAcrossRestart(t *testing.T) {
	recorder := &topicRenameRecorder{}
	first := newTopicRenameBot(t, recorder)
	if err := first.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false); err != nil {
		t.Fatal(err)
	}
	if len(recorder.snapshot()) != 1 {
		t.Fatal("first process did not rename the topic")
	}
	// A fresh Bot stands for a process restart: the once-set is empty, so the
	// next automatic trigger renames the topic again.
	restarted := newTopicRenameBot(t, recorder)
	if restarted.topicRenamed("TG-7-topic-5") {
		t.Fatal("fresh process retained a renamed topic")
	}
	if err := restarted.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false); err != nil {
		t.Fatal(err)
	}
	if requests := recorder.snapshot(); len(requests) != 2 {
		t.Fatalf("post-restart automatic rename requests = %d, want 2", len(requests))
	}
}

func TestTelegramRenamedTopicSetIsBounded(t *testing.T) {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}}, "test")
	for index := 0; index < maxRenamedTopics; index++ {
		bot.markTopicRenamed(fmt.Sprintf("TG-7-topic-%d", 1000+index))
	}
	bot.markTopicRenamed("TG-7-topic-9000")
	bot.renamedTopicsMu.Lock()
	size := len(bot.renamedTopics)
	_, kept := bot.renamedTopics["TG-7-topic-9000"]
	bot.renamedTopicsMu.Unlock()
	if size > maxRenamedTopics || !kept {
		t.Fatalf("bounded eviction: size=%d kept=%v", size, kept)
	}
}

func TestTelegramRenameConversationRejectsInapplicableRoutesAndLabels(t *testing.T) {
	recorder := &topicRenameRecorder{}
	bot := newTopicRenameBot(t, recorder)
	for _, conversation := range []string{"TG-7", "TG-group--100", "TG-group--100-topic-2", "TG-not-canonical", "WA-15551234567"} {
		err := bot.RenameConversation(context.Background(), conversation, "Release", true)
		if err == nil {
			t.Fatalf("%s was accepted as a private Telegram topic", conversation)
		}
		if conversation != "TG-not-canonical" && conversation != "WA-15551234567" && !errors.Is(err, channel.ErrConversationLabelUnsupported) {
			t.Fatalf("%s rejection did not wrap the unsupported sentinel: %v", conversation, err)
		}
	}
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
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", strings.Repeat("a", maxTopicLabelRunes), true); err != nil {
		t.Fatalf("boundary-length label was rejected: %v", err)
	}
	if requests := recorder.snapshot(); len(requests) != 1 {
		t.Fatalf("rejected routes or labels reached the provider: %#v", requests)
	}
}

func TestTelegramRenameConversationTreatsTopicNotModifiedAsSuccess(t *testing.T) {
	recorder := &topicRenameRecorder{
		statuses: []int{http.StatusBadRequest},
		bodies:   []string{`{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_NOT_MODIFIED"}`},
	}
	bot := newTopicRenameBot(t, recorder)
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false); err != nil {
		t.Fatalf("TOPIC_NOT_MODIFIED error = %v", err)
	}
	if !bot.topicRenamed("TG-7-topic-5") {
		t.Fatal("TOPIC_NOT_MODIFIED did not mark the conversation renamed")
	}
	if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false); err != nil {
		t.Fatal(err)
	}
	if requests := recorder.snapshot(); len(requests) != 1 {
		t.Fatalf("automatic rename repeated after TOPIC_NOT_MODIFIED: %d requests", len(requests))
	}
}

func TestTelegramRenameConversationPropagatesOtherProviderErrors(t *testing.T) {
	recorder := &topicRenameRecorder{
		statuses: []int{http.StatusBadRequest},
		bodies:   []string{`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`},
	}
	bot := newTopicRenameBot(t, recorder)
	err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false)
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("unrelated provider error = %v", err)
	}
	if bot.topicRenamed("TG-7-topic-5") {
		t.Fatal("failed rename marked the conversation renamed")
	}
}

func TestTelegramRenameConversationReauthorizesBeforeEveryProviderCall(t *testing.T) {
	t.Run("revoked authorization", func(t *testing.T) {
		recorder := &topicRenameRecorder{}
		bot := newTopicRenameBot(t, recorder)
		bot.SetAllowedUsersSource(func() []string { return nil })
		err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false)
		if err == nil || !strings.Contains(err.Error(), "authorization") {
			t.Fatalf("revoked rename error = %v", err)
		}
		if requests := recorder.snapshot(); len(requests) != 0 {
			t.Fatalf("revoked rename reached the provider: %#v", requests)
		}
	})

	t.Run("retry rechecks authorization", func(t *testing.T) {
		recorder := &topicRenameRecorder{
			statuses: []int{http.StatusTooManyRequests},
			bodies:   []string{`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`},
		}
		bot := newTopicRenameBot(t, recorder)
		bot.retryWait = func(context.Context, time.Duration) error {
			bot.SetAllowedUsersSource(func() []string { return nil })
			return nil
		}
		err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false)
		if err == nil || !strings.Contains(err.Error(), "authorization") {
			t.Fatalf("retry authorization error = %v", err)
		}
		if requests := recorder.snapshot(); len(requests) != 1 {
			t.Fatalf("retry after revocation reached the provider %d times", len(requests))
		}
		if bot.topicRenamed("TG-7-topic-5") {
			t.Fatal("failed retry marked the conversation renamed")
		}
	})

	t.Run("bounded retry succeeds after reauthorization", func(t *testing.T) {
		recorder := &topicRenameRecorder{
			statuses: []int{http.StatusTooManyRequests},
			bodies:   []string{`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`},
		}
		bot := newTopicRenameBot(t, recorder)
		bot.retryWait = func(context.Context, time.Duration) error { return nil }
		if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false); err != nil {
			t.Fatalf("bounded retry error = %v", err)
		}
		if requests := recorder.snapshot(); len(requests) != 2 {
			t.Fatalf("bounded retry requests = %d, want 2", len(requests))
		}
		if !bot.topicRenamed("TG-7-topic-5") {
			t.Fatal("successful retry did not mark the conversation renamed")
		}
	})
}
