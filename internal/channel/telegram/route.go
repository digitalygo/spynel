package telegram

import (
	"errors"
	"strconv"
	"strings"
)

// Canonical conversation grammar:
//
//	TG-<positive-user-id>
//	TG-<positive-user-id>-topic-<positive-thread-id>
//	TG-group-<negative-chat-id>
//	TG-group-<negative-chat-id>-topic-<positive-thread-id>
const (
	privateConversationPrefix = "TG-"
	groupConversationPrefix   = "TG-group-"
	conversationTopicMarker   = "-topic-"
)

// Route construction and parsing errors. Every failure fails closed.
var (
	errInvalidConversation = errors.New("invalid Telegram conversation")
	errInvalidChatID       = errors.New("invalid Telegram chat identifier")
	errInvalidThreadID     = errors.New("invalid Telegram message thread identifier")
)

// Route is a comparable, canonical Telegram conversation destination: a
// private user chat or a group chat, optionally inside a topic thread. The
// zero value is invalid; build values with NewPrivateRoute, NewGroupRoute, or
// ParseConversation.
type Route struct {
	chatID   int64
	threadID int64
	group    bool
}

// NewPrivateRoute builds a private chat route from the inbound sender user ID
// and message_thread_id. Absent threads and the General topic (0 and 1)
// canonicalize to the base conversation with outbound thread ID 0; only IDs
// of at least 2 address a topic thread.
func NewPrivateRoute(userID, threadID int64) (Route, error) {
	if userID <= 0 {
		return Route{}, errInvalidChatID
	}
	return newRoute(userID, threadID, false)
}

// NewGroupRoute builds a group or supergroup route from the inbound chat ID
// and message_thread_id. The chat ID must be negative. Absent threads and the
// General topic (0 and 1) canonicalize to the base conversation with outbound
// thread ID 0; only IDs of at least 2 address a topic thread.
func NewGroupRoute(chatID, threadID int64) (Route, error) {
	if chatID >= 0 {
		return Route{}, errInvalidChatID
	}
	return newRoute(chatID, threadID, true)
}

func newRoute(chatID, threadID int64, group bool) (Route, error) {
	normalized, err := normalizeThreadID(threadID)
	if err != nil {
		return Route{}, err
	}
	return Route{chatID: chatID, threadID: normalized, group: group}, nil
}

// normalizeThreadID maps an inbound message_thread_id to the outbound thread
// ID. The General topic and absent threads (0 and 1) become 0; negative
// identifiers fail closed.
func normalizeThreadID(threadID int64) (int64, error) {
	if threadID < 0 {
		return 0, errInvalidThreadID
	}
	if threadID < 2 {
		return 0, nil
	}
	return threadID, nil
}

// ParseConversation strictly parses a canonical conversation string. Any
// non-canonical form fails closed; there is no fallback parsing.
func ParseConversation(conversation string) (Route, error) {
	rest, ok := strings.CutPrefix(conversation, privateConversationPrefix)
	if !ok {
		return Route{}, errInvalidConversation
	}
	group := false
	if groupRest, ok := strings.CutPrefix(rest, "group-"); ok {
		group = true
		rest = groupRest
	}
	chatToken := rest
	threadToken := ""
	hasTopic := false
	if index := strings.Index(rest, conversationTopicMarker); index >= 0 {
		chatToken = rest[:index]
		threadToken = rest[index+len(conversationTopicMarker):]
		hasTopic = true
	}
	route := Route{group: group}
	if group {
		route.chatID, ok = parseCanonicalNegative(chatToken)
	} else {
		route.chatID, ok = parseCanonicalPositive(chatToken)
	}
	if !ok {
		return Route{}, errInvalidChatID
	}
	if !hasTopic {
		return route, nil
	}
	route.threadID, ok = parseCanonicalPositive(threadToken)
	if !ok || route.threadID < 2 {
		return Route{}, errInvalidThreadID
	}
	return route, nil
}

// parseCanonicalPositive parses [1-9][0-9]* exactly, rejecting leading
// zeroes, signs, whitespace, Unicode digits, suffix junk, and overflow.
func parseCanonicalPositive(token string) (int64, bool) {
	if token == "" || token[0] < '1' || token[0] > '9' {
		return 0, false
	}
	for index := 1; index < len(token); index++ {
		if token[index] < '0' || token[index] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(token, 10, 64)
	return value, err == nil
}

// parseCanonicalNegative parses -[1-9][0-9]* exactly, rejecting -0, leading
// zeroes, other signs, whitespace, Unicode digits, suffix junk, and overflow.
func parseCanonicalNegative(token string) (int64, bool) {
	if len(token) < 2 || token[0] != '-' || token[1] < '1' || token[1] > '9' {
		return 0, false
	}
	for index := 2; index < len(token); index++ {
		if token[index] < '0' || token[index] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(token, 10, 64)
	return value, err == nil
}

// Conversation returns the canonical conversation string, or "" when the
// route is invalid.
func (r Route) Conversation() string {
	if r.threadID < 0 || r.threadID == 1 {
		return ""
	}
	var base string
	if r.group {
		if r.chatID >= 0 {
			return ""
		}
		base = groupConversationPrefix + strconv.FormatInt(r.chatID, 10)
	} else {
		if r.chatID <= 0 {
			return ""
		}
		base = privateConversationPrefix + strconv.FormatInt(r.chatID, 10)
	}
	if r.threadID == 0 {
		return base
	}
	return base + conversationTopicMarker + strconv.FormatInt(r.threadID, 10)
}

// ChatID returns the signed Telegram chat identifier: positive for private
// chats and negative for groups.
func (r Route) ChatID() int64 { return r.chatID }

// IsGroup reports whether the route addresses a group or supergroup.
func (r Route) IsGroup() bool { return r.group }

// ThreadID returns the outbound message_thread_id, or 0 for the base
// conversation and General topic.
func (r Route) ThreadID() int64 { return r.threadID }
