package media

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxTranscriptBytes is the shared transcript safety bound for every speech
// backend. A backend that produces more text fails explicitly instead of
// delivering a silently truncated transcript.
const maxTranscriptBytes = 1024 * 1024

// maxTranscriptionErrorRunes bounds one sanitized diagnostic fragment that a
// transcription marker may carry to the harness.
const maxTranscriptionErrorRunes = 200

// TranscriptionRequest describes one audio attachment to transcribe.
// DurationSeconds is the transport-declared duration in seconds; backends that
// require it fail closed when it is missing or malformed.
type TranscriptionRequest struct {
	Path            string
	DurationSeconds int
}

// Transcriber turns one stored audio attachment into plain text. Every
// implementation returns an error instead of a marker; transports render the
// outcome through the shared marker constructors below.
type Transcriber interface {
	Transcribe(context.Context, TranscriptionRequest) (string, error)
}

// TranscriptionDisabledMarker is the message line shown when no speech
// transcription backend is active.
func TranscriptionDisabledMarker() string {
	return "[Speech transcription is disabled; inspect the attached audio manually]"
}

// TranscriptionFailedMarker is the message line shown when transcription
// failed. The error text is sanitized to one bounded line and cannot close the
// marker early or smuggle attachment directives into the message.
func TranscriptionFailedMarker(err error) string {
	detail := "unknown error"
	if err != nil {
		if sanitized := sanitizeTranscriptionText(err.Error(), maxTranscriptionErrorRunes); sanitized != "" {
			detail = sanitized
		}
	}
	return "[Speech transcription failed; inspect the attached audio manually: " + detail + "]"
}

// TranscriptionGeneratedMarker prefixes a generated transcript with its
// explicit label. The transcript is trimmed but never altered or truncated.
func TranscriptionGeneratedMarker(transcript string) string {
	return "[Generated speech transcription; may contain errors]\n" + strings.TrimSpace(transcript)
}

// sanitizeTranscriptionText reduces untrusted provider or decoder text to one
// safe display line: control and format characters are stripped, whitespace
// collapses to single spaces, square brackets are neutralized so the text can
// never form a marker or attachment directive, and the result is capped at
// maxRunes Unicode code points with a trailing ellipsis.
func sanitizeTranscriptionText(value string, maxRunes int) string {
	var builder strings.Builder
	pendingSpace := false
	for _, r := range value {
		switch {
		case unicode.IsSpace(r):
			pendingSpace = builder.Len() > 0
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		default:
			if pendingSpace {
				builder.WriteByte(' ')
				pendingSpace = false
			}
			switch r {
			case '[':
				r = '('
			case ']':
				r = ')'
			}
			builder.WriteRune(r)
		}
	}
	text := builder.String()
	if maxRunes > 0 && utf8.RuneCountInString(text) > maxRunes {
		runes := []rune(text)
		text = string(runes[:maxRunes-1]) + "…"
	}
	return text
}
