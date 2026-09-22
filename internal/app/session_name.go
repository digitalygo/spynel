package app

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/harness"
)

const (
	// piSessionNamingTimeout bounds the synchronous best-effort naming call
	// that follows a successful dispatch. Naming is metadata only, so the turn
	// never waits longer than this and always continues without it.
	piSessionNamingTimeout = 8 * time.Second
	// sessionLabelMaxRunes is the complete derived-label budget, including the
	// trailing ellipsis of a truncated label.
	sessionLabelMaxRunes = 32
)

// nameNewPiSession assigns the first user message's derived label to a newly
// created Pi session and, when the conversation has a transport label, to the
// conversation itself. It is deliberately best-effort: every failure is
// non-fatal, logged without content, and never affects the turn or reply. The
// session label applies to every surface, while only the transport decides
// whether its conversation has a renameable label.
func (s *Service) nameNewPiSession(ctx context.Context, message core.Message, priorSession, threadID string, steered bool) {
	if steered || threadID == "" || priorSession != "" {
		return
	}
	label := conversationSessionLabel(message.Text)
	if label == "" {
		return
	}
	namer, ok := s.Harness.(harness.SessionNamer)
	if !ok {
		return
	}
	nameContext, cancel := context.WithTimeout(ctx, piSessionNamingTimeout)
	defer cancel()
	result, err := namer.SetSessionName(nameContext, sessionKey(message), threadID, label, true)
	if err != nil {
		if errors.Is(err, harness.ErrSessionControlsUnsupported) {
			return
		}
		s.Runtime.LogEvent("error", "harness", "session_name_failed", "Pi session naming failed ("+harnessFailureEvidence(err)+")")
		return
	}
	if result.Name == "" || s.ConversationLabels == nil {
		return
	}
	// The active transport decides whether this conversation has a label at
	// all; an inapplicable route or disconnected channel is a silent no-op,
	// and the transport suppresses any automatic repeat rename.
	_ = s.ConversationLabels.RenameConversation(nameContext, message.Channel, message.Conversation, result.Name, false)
}

// conversationSessionLabel derives one short session label from the accepted
// user content. Transport-generated attachment tokens and diagnostic lines
// are dropped, a leading slash command contributes only its argument, every
// whitespace run collapses to one space, and remaining control characters are
// removed while emoji joiners, variation selectors, combining marks,
// punctuation, and casing are preserved.
func conversationSessionLabel(text string) string {
	text = redactSensitiveCommand(text)
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if sessionLabelGeneratedLine(line) {
			continue
		}
		kept = append(kept, line)
	}
	text = strings.TrimSpace(strings.Join(kept, "\n"))
	if strings.HasPrefix(text, "/") {
		if index := strings.IndexFunc(text, unicode.IsSpace); index >= 0 {
			text = strings.TrimSpace(text[index:])
		} else {
			text = ""
		}
	}
	text = collapseSessionLabelWhitespace(text)
	if text == "" {
		return ""
	}
	return truncateSessionLabel(text)
}

// sessionLabelGeneratedLine reports one transport-generated attachment or
// diagnostic line that must never become a session label. Captions and
// generated transcripts are user-visible prose and stay eligible. The legacy
// voice prefixes stay recognized so old history remains safe.
func sessionLabelGeneratedLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	for _, prefix := range []string{
		"[Attachment ",
		"[Speech transcription is disabled",
		"[Speech transcription failed",
		"[Generated speech transcription",
		"[Voice transcription is disabled",
		"[Voice transcription failed",
		"[Generated voice transcription",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// collapseSessionLabelWhitespace replaces every Unicode whitespace run with
// one ASCII space, trims the edges, and drops non-whitespace control
// characters. Emoji joiners, variation selectors, combining marks,
// punctuation, and casing pass through unchanged.
func collapseSessionLabelWhitespace(text string) string {
	var builder strings.Builder
	pendingSpace := false
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			pendingSpace = builder.Len() > 0
		case unicode.IsControl(r):
			continue
		default:
			if pendingSpace {
				builder.WriteByte(' ')
				pendingSpace = false
			}
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// truncateSessionLabel bounds one label to the complete extended-grapheme
// prefix that fits 31 code points and appends one ellipsis, so the result
// never exceeds the 32-code-point budget and never splits a grapheme.
func truncateSessionLabel(label string) string {
	if utf8.RuneCountInString(label) <= sessionLabelMaxRunes {
		return label
	}
	var builder strings.Builder
	remaining := sessionLabelMaxRunes - 1
	graphemes := uniseg.NewGraphemes(label)
	for graphemes.Next() {
		cluster := graphemes.Str()
		count := utf8.RuneCountInString(cluster)
		if count > remaining {
			break
		}
		builder.WriteString(cluster)
		remaining -= count
	}
	return builder.String() + "…"
}
