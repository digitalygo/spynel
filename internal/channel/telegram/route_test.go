package telegram

import (
	"errors"
	"math"
	"testing"
)

func TestParseConversationCanonicalForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		conversation string
		want         Route
	}{
		{name: "private base", conversation: "TG-7", want: Route{chatID: 7}},
		{name: "private base large", conversation: "TG-518743883", want: Route{chatID: 518743883}},
		{name: "private base maximum", conversation: "TG-9223372036854775807", want: Route{chatID: math.MaxInt64}},
		{name: "private topic", conversation: "TG-7-topic-2", want: Route{chatID: 7, threadID: 2}},
		{name: "private topic maximum", conversation: "TG-7-topic-9223372036854775807", want: Route{chatID: 7, threadID: math.MaxInt64}},
		{name: "group base one", conversation: "TG-group--1", want: Route{chatID: -1, group: true}},
		{name: "group base real", conversation: "TG-group--1001234567890", want: Route{chatID: -1001234567890, group: true}},
		{name: "group base minimum", conversation: "TG-group--9223372036854775808", want: Route{chatID: math.MinInt64, group: true}},
		{name: "group topic", conversation: "TG-group--1001234567890-topic-2", want: Route{chatID: -1001234567890, threadID: 2, group: true}},
		{name: "group topic maximum", conversation: "TG-group--1-topic-9223372036854775807", want: Route{chatID: -1, threadID: math.MaxInt64, group: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseConversation(test.conversation)
			if err != nil {
				t.Fatalf("ParseConversation(%q) returned error: %v", test.conversation, err)
			}
			if got != test.want {
				t.Fatalf("ParseConversation(%q) = %+v, want %+v", test.conversation, got, test.want)
			}
			if roundTrip := got.Conversation(); roundTrip != test.conversation {
				t.Fatalf("Conversation() = %q, want %q", roundTrip, test.conversation)
			}
			if got.ChatID() != test.want.chatID || got.IsGroup() != test.want.group || got.ThreadID() != test.want.threadID {
				t.Fatalf("accessors = chat %d group %v thread %d, want chat %d group %v thread %d",
					got.ChatID(), got.IsGroup(), got.ThreadID(), test.want.chatID, test.want.group, test.want.threadID)
			}
		})
	}
}

func TestParseConversationRejectsNonCanonicalInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		conversation string
		wantErr      error
	}{
		{name: "empty", conversation: "", wantErr: errInvalidConversation},
		{name: "private prefix only", conversation: "TG-", wantErr: errInvalidChatID},
		{name: "missing prefix", conversation: "telegram-7", wantErr: errInvalidConversation},
		{name: "lowercase prefix", conversation: "tg-7", wantErr: errInvalidConversation},
		{name: "no separator", conversation: "TG7", wantErr: errInvalidConversation},
		{name: "private leading whitespace", conversation: " TG-7", wantErr: errInvalidConversation},
		{name: "private trailing whitespace", conversation: "TG-7 ", wantErr: errInvalidChatID},
		{name: "private zero", conversation: "TG-0", wantErr: errInvalidChatID},
		{name: "private leading zero", conversation: "TG-007", wantErr: errInvalidChatID},
		{name: "private plus sign", conversation: "TG-+7", wantErr: errInvalidChatID},
		{name: "private negative", conversation: "TG--7", wantErr: errInvalidChatID},
		{name: "private suffix junk", conversation: "TG-7x", wantErr: errInvalidChatID},
		{name: "private overflow", conversation: "TG-9223372036854775808", wantErr: errInvalidChatID},
		{name: "private unicode digits", conversation: "TG-١٧", wantErr: errInvalidChatID},
		{name: "private fullwidth digits", conversation: "TG-１２３", wantErr: errInvalidChatID},
		{name: "private soft hyphen", conversation: "TG-\u00ad7", wantErr: errInvalidChatID},
		{name: "topic zero", conversation: "TG-7-topic-0", wantErr: errInvalidThreadID},
		{name: "topic general", conversation: "TG-7-topic-1", wantErr: errInvalidThreadID},
		{name: "topic leading zero", conversation: "TG-7-topic-02", wantErr: errInvalidThreadID},
		{name: "topic empty", conversation: "TG-7-topic-", wantErr: errInvalidThreadID},
		{name: "topic missing separator", conversation: "TG-7-topic2", wantErr: errInvalidChatID},
		{name: "topic repeated", conversation: "TG-7-topic-2-topic-3", wantErr: errInvalidThreadID},
		{name: "topic negative", conversation: "TG-7-topic--2", wantErr: errInvalidThreadID},
		{name: "topic plus sign", conversation: "TG-7-topic-+2", wantErr: errInvalidThreadID},
		{name: "topic suffix junk", conversation: "TG-7-topic-2x", wantErr: errInvalidThreadID},
		{name: "topic trailing whitespace", conversation: "TG-7-topic-2 ", wantErr: errInvalidThreadID},
		{name: "topic overflow", conversation: "TG-7-topic-9223372036854775808", wantErr: errInvalidThreadID},
		{name: "topic unicode digits", conversation: "TG-7-topic-٢", wantErr: errInvalidThreadID},
		{name: "group positive", conversation: "TG-group-100", wantErr: errInvalidChatID},
		{name: "group zero", conversation: "TG-group-0", wantErr: errInvalidChatID},
		{name: "group marker only", conversation: "TG-group", wantErr: errInvalidChatID},
		{name: "group empty", conversation: "TG-group-", wantErr: errInvalidChatID},
		{name: "group negative zero", conversation: "TG-group--0", wantErr: errInvalidChatID},
		{name: "group leading zero", conversation: "TG-group--0100", wantErr: errInvalidChatID},
		{name: "group plus sign", conversation: "TG-group-+100", wantErr: errInvalidChatID},
		{name: "group suffix junk", conversation: "TG-group--100x", wantErr: errInvalidChatID},
		{name: "group trailing whitespace", conversation: "TG-group--100 ", wantErr: errInvalidChatID},
		{name: "group overflow", conversation: "TG-group--9223372036854775809", wantErr: errInvalidChatID},
		{name: "group unicode digits", conversation: "TG-group--١٠٠", wantErr: errInvalidChatID},
		{name: "group topic zero", conversation: "TG-group--100-topic-0", wantErr: errInvalidThreadID},
		{name: "group topic general", conversation: "TG-group--100-topic-1", wantErr: errInvalidThreadID},
		{name: "group topic empty", conversation: "TG-group--100-topic-", wantErr: errInvalidThreadID},
		{name: "group topic repeated", conversation: "TG-group--100-topic-2-topic-3", wantErr: errInvalidThreadID},
		{name: "group topic overflow", conversation: "TG-group--100-topic-9223372036854775808", wantErr: errInvalidThreadID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseConversation(test.conversation)
			if err == nil {
				t.Fatalf("ParseConversation(%q) = %+v, want error", test.conversation, got)
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ParseConversation(%q) error = %v, want %v", test.conversation, err, test.wantErr)
			}
			if got != (Route{}) {
				t.Fatalf("ParseConversation(%q) returned non-zero route %+v", test.conversation, got)
			}
		})
	}
}

func TestRouteConstructorsNormalizeAndValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		group      bool
		chatID     int64
		threadID   int64
		want       string
		wantThread int64
		wantErr    error
	}{
		{name: "private base", chatID: 7, want: "TG-7"},
		{name: "private general topic", chatID: 7, threadID: 1, want: "TG-7"},
		{name: "private topic", chatID: 7, threadID: 2, want: "TG-7-topic-2", wantThread: 2},
		{name: "private maximum boundaries", chatID: math.MaxInt64, threadID: math.MaxInt64, want: "TG-9223372036854775807-topic-9223372036854775807", wantThread: math.MaxInt64},
		{name: "private zero id", chatID: 0, wantErr: errInvalidChatID},
		{name: "private negative id", chatID: -7, wantErr: errInvalidChatID},
		{name: "private negative thread", chatID: 7, threadID: -1, wantErr: errInvalidThreadID},
		{name: "private minimum thread", chatID: 7, threadID: math.MinInt64, wantErr: errInvalidThreadID},
		{name: "group base", group: true, chatID: -1001234567890, want: "TG-group--1001234567890"},
		{name: "group general topic", group: true, chatID: -1001234567890, threadID: 1, want: "TG-group--1001234567890"},
		{name: "group topic", group: true, chatID: -1001234567890, threadID: 2, want: "TG-group--1001234567890-topic-2", wantThread: 2},
		{name: "group minimum boundary", group: true, chatID: math.MinInt64, threadID: math.MaxInt64, want: "TG-group--9223372036854775808-topic-9223372036854775807", wantThread: math.MaxInt64},
		{name: "group zero id", group: true, chatID: 0, wantErr: errInvalidChatID},
		{name: "group positive id", group: true, chatID: 100, wantErr: errInvalidChatID},
		{name: "group negative thread", group: true, chatID: -100, threadID: math.MinInt64, wantErr: errInvalidThreadID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var (
				route Route
				err   error
			)
			if test.group {
				route, err = NewGroupRoute(test.chatID, test.threadID)
			} else {
				route, err = NewPrivateRoute(test.chatID, test.threadID)
			}
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("constructor error = %v, want %v", err, test.wantErr)
				}
				if route != (Route{}) {
					t.Fatalf("constructor returned non-zero route %+v", route)
				}
				return
			}
			if err != nil {
				t.Fatalf("constructor returned error: %v", err)
			}
			if got := route.Conversation(); got != test.want {
				t.Fatalf("Conversation() = %q, want %q", got, test.want)
			}
			if route.ChatID() != test.chatID || route.IsGroup() != test.group || route.ThreadID() != test.wantThread {
				t.Fatalf("accessors = chat %d group %v thread %d, want chat %d group %v thread %d",
					route.ChatID(), route.IsGroup(), route.ThreadID(), test.chatID, test.group, test.wantThread)
			}
			parsed, err := ParseConversation(test.want)
			if err != nil || parsed != route {
				t.Fatalf("ParseConversation(%q) = %+v, %v; want %+v", test.want, parsed, err, route)
			}
		})
	}
}

func TestRouteConversationFailsClosedForInvalidValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		route Route
	}{
		{name: "zero value", route: Route{}},
		{name: "private zero chat", route: Route{chatID: 0}},
		{name: "private negative chat", route: Route{chatID: -7}},
		{name: "group zero chat", route: Route{chatID: 0, group: true}},
		{name: "group positive chat", route: Route{chatID: 7, group: true}},
		{name: "negative thread", route: Route{chatID: 7, threadID: -1}},
		{name: "general thread", route: Route{chatID: 7, threadID: 1}},
		{name: "group negative thread", route: Route{chatID: -7, threadID: -2, group: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.route.Conversation(); got != "" {
				t.Fatalf("Conversation() = %q, want empty", got)
			}
		})
	}
}
