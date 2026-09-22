package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitalygo/spynel/internal/config"
)

// recordingTranscriber is a deterministic fallback or primary backend that
// counts how often it was consulted.
type recordingTranscriber struct {
	text  string
	err   error
	calls atomic.Int32
}

func (r *recordingTranscriber) Transcribe(context.Context, TranscriptionRequest) (string, error) {
	r.calls.Add(1)
	return r.text, r.err
}

func TestFallbackUsesFallbackOnlyWhenAPIKeyIsMissing(t *testing.T) {
	path := elevenLabsFile(t, "voice.ogg", []byte("audio bytes"))

	for _, test := range []struct {
		name  string
		key   string
		blank bool
	}{
		{name: "missing env", key: "ELEVENLABS_TEST_KEY_DEFINITELY_ABSENT"},
		{name: "blank env", key: config.DefaultElevenLabsAPIKeyEnv, blank: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, requests := rawServer(http.StatusOK, nil, `{"text":"unexpected"}`)
			defer server.Close()
			if test.blank {
				t.Setenv(test.key, "   ")
			}
			primary := elevenLabsFixture(t, func(speech *config.Speech) {
				speech.ElevenLabsAPIKeyEnv = test.key
			})
			primary.endpoint = server.URL
			fallback := &recordingTranscriber{text: "local words"}

			text, err := NewFallback(primary, fallback).Transcribe(context.Background(), TranscriptionRequest{Path: path, DurationSeconds: 5})
			if err != nil || text != "local words" {
				t.Fatalf("missing API key fallback = %q, %v", text, err)
			}
			if fallback.calls.Load() != 1 {
				t.Fatalf("fallback calls = %d, want 1", fallback.calls.Load())
			}
			// The missing key fails before any upload, so the fallback
			// transcript never masks a request that reached the provider.
			if requests.Load() != 0 {
				t.Fatalf("network requests = %d, want 0", requests.Load())
			}
		})
	}
}

func TestFallbackPassesThroughPrimarySuccessAndNonKeyFailures(t *testing.T) {
	path := elevenLabsFile(t, "voice.ogg", []byte("audio bytes"))
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)

	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
		fail   string
	}{
		{name: "primary success", status: http.StatusOK, body: `{"text":"cloud words"}`, want: "cloud words"},
		{
			name: "invalid key", status: http.StatusUnauthorized, body: `{"detail":{"code":"invalid_api_key","message":"bad key"}}`,
			fail: "HTTP 401",
		},
		{
			name: "non-retryable 429", status: http.StatusTooManyRequests, body: `{"detail":{"code":"rate_limit_exceeded","message":"slow down"}}`,
			fail: "HTTP 429",
		},
		{name: "empty transcript", status: http.StatusOK, body: `{"text":"   "}`, fail: "empty transcription"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, requests := rawServer(test.status, nil, test.body)
			defer server.Close()
			primary := elevenLabsFixture(t, nil)
			primary.endpoint = server.URL
			fallback := &recordingTranscriber{text: "local words"}

			text, err := NewFallback(primary, fallback).Transcribe(context.Background(), TranscriptionRequest{Path: path, DurationSeconds: 5})
			if fallback.calls.Load() != 0 {
				t.Fatalf("fallback consulted %d times for a non-key outcome", fallback.calls.Load())
			}
			switch {
			case test.fail == "":
				if err != nil || text != test.want {
					t.Fatalf("primary success = %q, %v, want %q", text, err, test.want)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), test.fail) {
					t.Fatalf("primary failure = %q, %v, want %q", text, err, test.fail)
				}
				if text != "" {
					t.Fatalf("failed transcription produced text %q", text)
				}
			}
			// A non-retryable 429 must reach the fallback after exactly one
			// attempt; every other case is one attempt as well.
			if requests.Load() != 1 {
				t.Fatalf("network requests = %d, want 1", requests.Load())
			}
		})
	}
}

func TestFallbackSurfacesPrimaryAndFallbackErrorsUnchanged(t *testing.T) {
	cloudFailure := errors.New("ElevenLabs transcription request failed: connection reset")
	localFailure := fmt.Errorf("parakeet model unavailable: %w", errors.New("cache unreadable"))

	t.Run("non-key primary error passes through", func(t *testing.T) {
		primary := &recordingTranscriber{err: cloudFailure}
		fallback := &recordingTranscriber{text: "local words"}
		_, err := NewFallback(primary, fallback).Transcribe(context.Background(), TranscriptionRequest{})
		if err != cloudFailure {
			t.Fatalf("primary error = %v, want the exact primary error", err)
		}
		if fallback.calls.Load() != 0 {
			t.Fatalf("fallback consulted %d times", fallback.calls.Load())
		}
	})

	t.Run("primary success with empty text passes through", func(t *testing.T) {
		primary := &recordingTranscriber{text: ""}
		fallback := &recordingTranscriber{text: "local words"}
		text, err := NewFallback(primary, fallback).Transcribe(context.Background(), TranscriptionRequest{})
		if err != nil || text != "" {
			t.Fatalf("empty success = %q, %v, want an unchanged pass-through", text, err)
		}
		if fallback.calls.Load() != 0 {
			t.Fatalf("fallback consulted %d times", fallback.calls.Load())
		}
	})

	t.Run("fallback failure surfaces unchanged", func(t *testing.T) {
		primary := &recordingTranscriber{err: fmt.Errorf("ElevenLabs API key environment variable TEST_KEY is not set: %w", ErrSpeechAPIKeyMissing)}
		fallback := &recordingTranscriber{err: localFailure}
		_, err := NewFallback(primary, fallback).Transcribe(context.Background(), TranscriptionRequest{})
		if err != localFailure {
			t.Fatalf("fallback error = %v, want the exact fallback error", err)
		}
		if fallback.calls.Load() != 1 {
			t.Fatalf("fallback calls = %d, want 1", fallback.calls.Load())
		}
	})

	t.Run("wrapped missing-key error still triggers the fallback", func(t *testing.T) {
		primary := &recordingTranscriber{err: fmt.Errorf("outer context: %w", fmt.Errorf("inner: %w", ErrSpeechAPIKeyMissing))}
		fallback := &recordingTranscriber{text: "local words"}
		text, err := NewFallback(primary, fallback).Transcribe(context.Background(), TranscriptionRequest{})
		if err != nil || text != "local words" {
			t.Fatalf("wrapped missing key = %q, %v", text, err)
		}
	})
}
