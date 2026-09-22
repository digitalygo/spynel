package media

import (
	"context"
	"errors"
)

// ErrSpeechAPIKeyMissing reports that the configured cloud API key
// environment variable was missing or blank when transcription started. It is
// the only substitutable transcription failure: every other provider outcome,
// including invalid keys, rate limits, timeouts, network errors, and empty
// transcripts, is real evidence that must surface unchanged.
var ErrSpeechAPIKeyMissing = errors.New("speech API key is not configured")

// fallbackTranscriber delegates to the fallback backend only when the primary
// error reports ErrSpeechAPIKeyMissing. A primary success and every other
// primary error pass through unchanged, so an invalid key, rate limit,
// timeout, network failure, or empty transcript is never silently replaced by
// a local transcript.
type fallbackTranscriber struct {
	primary  Transcriber
	fallback Transcriber
}

// NewFallback wraps one primary backend with an explicit missing-API-key
// fallback backend. Only errors satisfying errors.Is(err,
// ErrSpeechAPIKeyMissing) reach the fallback; its result, including its
// errors, is returned unchanged.
func NewFallback(primary, fallback Transcriber) Transcriber {
	return &fallbackTranscriber{primary: primary, fallback: fallback}
}

// Transcribe runs the primary backend and substitutes the fallback backend
// exactly when the primary reports a missing API key.
func (f *fallbackTranscriber) Transcribe(ctx context.Context, request TranscriptionRequest) (string, error) {
	transcript, err := f.primary.Transcribe(ctx, request)
	if err == nil || !errors.Is(err, ErrSpeechAPIKeyMissing) {
		return transcript, err
	}
	return f.fallback.Transcribe(ctx, request)
}
