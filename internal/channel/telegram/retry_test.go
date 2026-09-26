package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	markdownfmt "github.com/digitalygo/spynel/internal/markdown"
)

// telegramSendProbe is a deterministic provider transport for the bounded
// text retry tests. It records every request payload and answers through the
// supplied responder, keyed by attempt number.
type telegramSendProbe struct {
	mu       sync.Mutex
	calls    int
	payloads []map[string]any
	respond  func(call int, payload map[string]any) (*http.Response, error)
}

func (p *telegramSendProbe) roundTrip(request *http.Request) (*http.Response, error) {
	var payload map[string]any
	if request.Body != nil {
		defer request.Body.Close()
		_ = json.NewDecoder(request.Body).Decode(&payload)
	}
	p.mu.Lock()
	p.calls++
	call := p.calls
	if payload != nil {
		p.payloads = append(p.payloads, payload)
	}
	respond := p.respond
	p.mu.Unlock()
	return respond(call, payload)
}

func (p *telegramSendProbe) snapshot() (int, []map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, append([]map[string]any(nil), p.payloads...)
}

func telegramJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func telegramOKResponse() *http.Response {
	return telegramJSONResponse(http.StatusOK, `{"ok":true,"result":{"message_id":9}}`)
}

func newSendRetryBot(probe *telegramSendProbe, allowed *[]string) *Bot {
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, GroupMode: "all"}, "SECRET-TOKEN")
	if allowed != nil {
		bot.SetAllowedUsersSource(func() []string { return *allowed })
	}
	bot.client.Transport = telegramRoundTripFunc(probe.roundTrip)
	return bot
}

// transientTransportFailure returns one pre-redaction transport-class failure.
func transientTransportFailure(kind string) error {
	switch kind {
	case "timeout":
		return &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	case "reset":
		return &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	case "closed":
		return net.ErrClosed
	case "eof":
		return io.ErrUnexpectedEOF
	default:
		panic("unknown transport failure: " + kind)
	}
}

func TestTelegramTextSendRetriesTransientTransportFailures(t *testing.T) {
	for _, kind := range []string{"timeout", "reset", "closed", "eof"} {
		t.Run(kind, func(t *testing.T) {
			failure := transientTransportFailure(kind)
			probe := &telegramSendProbe{respond: func(call int, _ map[string]any) (*http.Response, error) {
				if call <= 2 {
					return nil, failure
				}
				return telegramOKResponse(), nil
			}}
			bot := newSendRetryBot(probe, nil)
			var waits []time.Duration
			bot.retryWait = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			}
			var logs strings.Builder
			bot.SetLogWriter(&logs)
			route, err := ParseConversation("TG-7-topic-5")
			if err != nil {
				t.Fatal(err)
			}
			if err := bot.send(context.Background(), route, "secret reply text", 41); err != nil {
				t.Fatalf("send: %v", err)
			}
			calls, payloads := probe.snapshot()
			if calls != 3 {
				t.Fatalf("provider calls = %d, want 3", calls)
			}
			if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
				t.Fatalf("retry waits = %#v, want [1s 2s]", waits)
			}
			if len(payloads) != 3 {
				t.Fatalf("recorded payloads = %d, want 3", len(payloads))
			}
			for index, payload := range payloads {
				if payload["chat_id"] != "7" {
					t.Fatalf("attempt %d chat id = %#v", index, payload["chat_id"])
				}
				if payload["message_thread_id"] != float64(5) {
					t.Fatalf("attempt %d thread = %#v", index, payload["message_thread_id"])
				}
				reply, ok := payload["reply_parameters"].(map[string]any)
				if !ok || reply["message_id"] != float64(41) {
					t.Fatalf("attempt %d reply anchor = %#v", index, payload["reply_parameters"])
				}
				if payload["text"] != "secret reply text" {
					t.Fatalf("attempt %d text = %#v", index, payload["text"])
				}
			}
			if !strings.Contains(logs.String(), "recovered after 2 transport retries") {
				t.Fatalf("recovery log = %q", logs.String())
			}
			if strings.Contains(logs.String(), "secret reply text") || strings.Contains(logs.String(), "SECRET-TOKEN") {
				t.Fatalf("recovery log leaked content: %q", logs.String())
			}
		})
	}
}

func TestTelegramTextSendStopsAfterExhaustedTransportRetries(t *testing.T) {
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return nil, transientTransportFailure("reset")
	}}
	bot := newSendRetryBot(probe, nil)
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	sendErr := bot.send(context.Background(), route, "hello", 0)
	if sendErr == nil {
		t.Fatal("exhausted transport retries reported success")
	}
	if !isTelegramTransportError(sendErr) {
		t.Fatalf("exhausted send error = %T %v, want a classified transport failure", sendErr, sendErr)
	}
	if strings.Contains(sendErr.Error(), "SECRET-TOKEN") {
		t.Fatalf("exhausted send error leaked the token: %v", sendErr)
	}
	calls, _ := probe.snapshot()
	if calls != 3 {
		t.Fatalf("provider calls = %d, want 3", calls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("retry waits = %#v, want [1s 2s]", waits)
	}
}

// TestTelegramTextSendDoesNotRetryDefinitiveNonOKStatus proves that a
// definitive non-200 status is never treated as an ambiguous transport send
// failure, even when the response body is empty or truncated, so the text
// send path performs exactly one request and reports a content-free provider
// classification.
func TestTelegramTextSendDoesNotRetryDefinitiveNonOKStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "400 empty body", status: http.StatusBadRequest},
		{name: "400 truncated body", status: http.StatusBadRequest, body: `{"ok":false,"error_code":400`},
		{name: "500 empty body", status: http.StatusInternalServerError},
		{name: "500 truncated body", status: http.StatusInternalServerError, body: `{"ok":false,"error_code":500`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
				return telegramJSONResponse(test.status, test.body), nil
			}}
			bot := newSendRetryBot(probe, nil)
			var waits []time.Duration
			bot.retryWait = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			}
			route, err := ParseConversation("TG-7")
			if err != nil {
				t.Fatal(err)
			}
			sendErr := bot.send(context.Background(), route, "hello", 0)
			if sendErr == nil {
				t.Fatal("definitive provider rejection reported success")
			}
			if isTelegramTransportError(sendErr) {
				t.Fatalf("non-200 response classified as transport: %T %v", sendErr, sendErr)
			}
			var apiErr *telegramAPIError
			if !errors.As(sendErr, &apiErr) || apiErr.code != test.status {
				t.Fatalf("send error = %T %v, want provider code %d", sendErr, sendErr, test.status)
			}
			if calls, _ := probe.snapshot(); calls != 1 {
				t.Fatalf("provider calls = %d, want exactly 1", calls)
			}
			if len(waits) != 0 {
				t.Fatalf("retry waits = %#v, want none", waits)
			}
			if strings.Contains(sendErr.Error(), "SECRET-TOKEN") {
				t.Fatalf("send error leaked the token: %v", sendErr)
			}
		})
	}

	t.Run("one content-free failure diagnostic", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return telegramJSONResponse(http.StatusBadRequest, ""), nil
		}}
		bot := newSendRetryBot(probe, nil)
		bot.retryWait = func(context.Context, time.Duration) error { return nil }
		var logs strings.Builder
		bot.SetLogWriter(&logs)
		bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
			finalText := "secret reply body"
			emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText})
			return nil
		}, privateMessage(8, "/unparseable"))
		if calls, _ := probe.snapshot(); calls != 1 {
			t.Fatalf("provider calls = %d, want exactly 1", calls)
		}
		assertDeliveryDiagnostic(t, logs.String(), "telegram: final reply delivery failed: category=provider code=400 attempts=1", "secret reply body", "SECRET-TOKEN")
	})
}

// TestTelegramTextSendRetriesTruncatedOKBody keeps the ambiguous HTTP 200
// case retryable: an ok response whose body is truncated remains a premature
// EOF that the bounded transport retry may resolve.
func TestTelegramTextSendRetriesTruncatedOKBody(t *testing.T) {
	probe := &telegramSendProbe{respond: func(call int, _ map[string]any) (*http.Response, error) {
		if call <= 2 {
			return telegramJSONResponse(http.StatusOK, `{"ok":true,"result":{"message_id":9}`), nil
		}
		return telegramOKResponse(), nil
	}}
	bot := newSendRetryBot(probe, nil)
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.send(context.Background(), route, "hello", 0); err != nil {
		t.Fatalf("send: %v", err)
	}
	if calls, _ := probe.snapshot(); calls != 3 {
		t.Fatalf("provider calls = %d, want 3", calls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("retry waits = %#v, want [1s 2s]", waits)
	}
}

func TestTelegramTextSendDoesNotReplayAcknowledgedChunks(t *testing.T) {
	longText := strings.Repeat("a", markdownfmt.TelegramMaxVisiblePerMessage) + strings.Repeat("b", markdownfmt.TelegramMaxVisiblePerMessage) + "c"
	chunks := markdownfmt.TelegramChunks(longText)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	probe := &telegramSendProbe{respond: func(_ int, payload map[string]any) (*http.Response, error) {
		if payload["text"] == chunks[0] {
			return telegramOKResponse(), nil
		}
		return nil, transientTransportFailure("reset")
	}}
	bot := newSendRetryBot(probe, nil)
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.send(context.Background(), route, longText, 41); err == nil {
		t.Fatal("send with a permanently failing middle chunk reported success")
	}
	calls, payloads := probe.snapshot()
	if calls != 4 {
		t.Fatalf("provider calls = %d, want 4 (one first chunk plus three middle-chunk attempts)", calls)
	}
	firstChunkAttempts := 0
	for index, payload := range payloads {
		switch payload["text"] {
		case chunks[0]:
			firstChunkAttempts++
			if payload["reply_parameters"] == nil {
				t.Fatalf("first chunk attempt %d lost its reply anchor", index)
			}
		case chunks[1]:
			if payload["reply_parameters"] != nil {
				t.Fatalf("middle chunk attempt %d replayed the reply anchor", index)
			}
		default:
			t.Fatalf("attempt %d sent an unexpected chunk", index)
		}
	}
	if firstChunkAttempts != 1 {
		t.Fatalf("acknowledged first chunk attempts = %d, want 1", firstChunkAttempts)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("retry waits = %#v, want [1s 2s]", waits)
	}
}

func TestTelegramTextTransportRetryReauthorizesLiveRoute(t *testing.T) {
	t.Run("global revocation", func(t *testing.T) {
		allowed := []string{"7"}
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, &allowed)
		bot.retryWait = func(context.Context, time.Duration) error {
			allowed = nil
			return nil
		}
		route, err := ParseConversation("TG-7-topic-5")
		if err != nil {
			t.Fatal(err)
		}
		if err := bot.send(context.Background(), route, "hello", 0); !errors.Is(err, errTelegramRuntimeAuthorization) {
			t.Fatalf("revoked retry error = %v", err)
		}
		calls, _ := probe.snapshot()
		if calls != 1 {
			t.Fatalf("revoked retry contacted Telegram %d times, want 1", calls)
		}
	})

	t.Run("private topic recipient replacement", func(t *testing.T) {
		allowed := []string{"7"}
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, &allowed)
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
		calls, _ := probe.snapshot()
		if calls != 1 {
			t.Fatalf("revoked private topic contacted Telegram %d times, want 1", calls)
		}
	})

	t.Run("group delivery disabled during wait", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, nil)
		bot.retryWait = func(context.Context, time.Duration) error {
			bot.config.GroupMode = "off"
			return nil
		}
		route, err := ParseConversation("TG-group--100-topic-9")
		if err != nil {
			t.Fatal(err)
		}
		err = bot.send(context.Background(), route, "hello", 0)
		if err == nil || !strings.Contains(err.Error(), "group delivery is disabled") {
			t.Fatalf("group retry error = %v", err)
		}
		calls, _ := probe.snapshot()
		if calls != 1 {
			t.Fatalf("disabled group contacted Telegram %d times, want 1", calls)
		}
	})
}

func TestTelegramTextTransportRetryHonorsCancellation(t *testing.T) {
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return nil, transientTransportFailure("reset")
	}}
	bot := newSendRetryBot(probe, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot.retryWait = func(_ context.Context, _ time.Duration) error {
		// Deliberately ignore cancellation: the retry loop itself must
		// recheck the context before dispatching another request. A waiter
		// that returns nil while the context is cancelled must still stop
		// the delivery.
		cancel()
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.send(ctx, route, "hello", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry error = %v", err)
	}
	calls, _ := probe.snapshot()
	if calls != 1 {
		t.Fatalf("cancelled retry contacted Telegram %d times, want 1", calls)
	}
}

func TestTelegramTextSendHonorsParentDeadline(t *testing.T) {
	var calls atomic.Int32
	bot := New(config.Telegram{AllowedUsers: []string{"7"}, GroupMode: "all"}, "SECRET-TOKEN")
	bot.client.Transport = telegramRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = bot.send(ctx, route, "hello", 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send deadline error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestTelegramTextTransportRetryBudgetSharedWithPlainFallback(t *testing.T) {
	probe := &telegramSendProbe{respond: func(_ int, payload map[string]any) (*http.Response, error) {
		if _, html := payload["parse_mode"]; html {
			return telegramJSONResponse(http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unexpected end tag"}`), nil
		}
		return nil, transientTransportFailure("reset")
	}}
	bot := newSendRetryBot(probe, nil)
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	sendErr := bot.send(context.Background(), route, "**bold**", 0)
	if sendErr == nil {
		t.Fatal("failing plain fallback reported success")
	}
	if !isTelegramTransportError(sendErr) {
		t.Fatalf("plain fallback error = %T %v, want a classified transport failure", sendErr, sendErr)
	}
	calls, payloads := probe.snapshot()
	if calls != 4 {
		t.Fatalf("provider calls = %d, want 4 (one HTML attempt plus three plain attempts)", calls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("retry waits = %#v, want [1s 2s] shared across the fallback", waits)
	}
	if payloads[0]["parse_mode"] != "HTML" {
		t.Fatalf("first attempt was not HTML: %#v", payloads[0])
	}
	for index, payload := range payloads[1:] {
		if _, html := payload["parse_mode"]; html {
			t.Fatalf("fallback attempt %d retained parse mode: %#v", index+1, payload)
		}
	}
}

func TestTelegramTextTransportRetrySharesRateLimitBudget(t *testing.T) {
	probe := &telegramSendProbe{respond: func(call int, _ map[string]any) (*http.Response, error) {
		switch call {
		case 2:
			return nil, transientTransportFailure("reset")
		default:
			return telegramJSONResponse(http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`), nil
		}
	}}
	bot := newSendRetryBot(probe, nil)
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	sendErr := bot.send(context.Background(), route, "hello", 0)
	var apiErr *telegramAPIError
	if !errors.As(sendErr, &apiErr) || apiErr.code != http.StatusTooManyRequests {
		t.Fatalf("shared-budget send error = %T %v, want a 429 provider error", sendErr, sendErr)
	}
	calls, _ := probe.snapshot()
	if calls != 3 {
		t.Fatalf("provider calls = %d, want 3", calls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != time.Second {
		t.Fatalf("retry waits = %#v, want one rate-limit wait and one transport wait", waits)
	}
}

func TestTelegramTransportRetriesApplyOnlyToSendMessage(t *testing.T) {
	route, err := ParseConversation("TG-7-topic-5")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("typing action", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, nil)
		waited := false
		bot.retryWait = func(context.Context, time.Duration) error {
			waited = true
			return nil
		}
		if err := bot.action(context.Background(), route, "typing"); err == nil {
			t.Fatal("typing action error = nil")
		}
		calls, _ := probe.snapshot()
		if calls != 1 || waited {
			t.Fatalf("typing calls = %d, waited = %v; want one attempt without retry", calls, waited)
		}
	})

	t.Run("topic rename", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, nil)
		waited := false
		bot.retryWait = func(context.Context, time.Duration) error {
			waited = true
			return nil
		}
		if err := bot.RenameConversation(context.Background(), "TG-7-topic-5", "Release", false); err == nil {
			t.Fatal("rename provider error = nil")
		}
		calls, _ := probe.snapshot()
		if calls != 1 || waited {
			t.Fatalf("rename calls = %d, waited = %v; want one attempt without retry", calls, waited)
		}
	})

	t.Run("getUpdates", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, nil)
		waited := false
		bot.retryWait = func(context.Context, time.Duration) error {
			waited = true
			return nil
		}
		if _, err := bot.updates(context.Background(), 0); err == nil {
			t.Fatal("getUpdates transport error = nil")
		}
		calls, _ := probe.snapshot()
		if calls != 1 || waited {
			t.Fatalf("getUpdates calls = %d, waited = %v; want one attempt without retry", calls, waited)
		}
	})

	t.Run("attachment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "report.txt")
		if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
			t.Fatal(err)
		}
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, nil)
		waited := false
		bot.retryWait = func(context.Context, time.Duration) error {
			waited = true
			return nil
		}
		err := bot.sendAttachment(context.Background(), route, core.OutboundAttachment{
			Kind: "attachment", Name: "report.txt", Path: path, MediaType: "text/plain", MaxBytes: 1024,
		}, 0)
		if err == nil {
			t.Fatal("attachment transport error = nil")
		}
		calls, _ := probe.snapshot()
		if calls != 1 || waited {
			t.Fatalf("attachment calls = %d, waited = %v; want one attempt without retry", calls, waited)
		}
	})
}

func TestTelegramTextTransportRetryBudgetSurvivesParseFallback(t *testing.T) {
	probe := &telegramSendProbe{respond: func(call int, _ map[string]any) (*http.Response, error) {
		switch call {
		case 1:
			return nil, transientTransportFailure("reset")
		case 2:
			return telegramJSONResponse(http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unexpected end tag"}`), nil
		default:
			return nil, transientTransportFailure("reset")
		}
	}}
	bot := newSendRetryBot(probe, nil)
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	sendErr := bot.send(context.Background(), route, "**bold**", 0)
	if sendErr == nil {
		t.Fatal("exhausted transport retries reported success")
	}
	if !isTelegramTransportError(sendErr) {
		t.Fatalf("send error = %T %v, want a classified transport failure", sendErr, sendErr)
	}
	calls, payloads := probe.snapshot()
	if calls != 4 {
		t.Fatalf("provider calls = %d, want 4 (transport, HTML rejection, then two exhausted plain attempts)", calls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("retry waits = %#v, want [1s 2s] shared across the fallback", waits)
	}
	if payloads[0]["parse_mode"] != "HTML" || payloads[1]["parse_mode"] != "HTML" {
		t.Fatalf("first two attempts were not HTML: %#v %#v", payloads[0], payloads[1])
	}
	for index, payload := range payloads[2:] {
		if _, html := payload["parse_mode"]; html {
			t.Fatalf("fallback attempt %d retained parse mode: %#v", index+2, payload)
		}
	}
}

func TestTelegramTextRateLimitBudgetSurvivesParseFallback(t *testing.T) {
	probe := &telegramSendProbe{respond: func(call int, _ map[string]any) (*http.Response, error) {
		if call == 2 {
			return telegramJSONResponse(http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unexpected end tag"}`), nil
		}
		return telegramJSONResponse(http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`), nil
	}}
	bot := newSendRetryBot(probe, nil)
	var waits []time.Duration
	bot.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	sendErr := bot.send(context.Background(), route, "**bold**", 0)
	var apiErr *telegramAPIError
	if !errors.As(sendErr, &apiErr) || apiErr.code != http.StatusTooManyRequests {
		t.Fatalf("send error = %T %v, want the second 429 provider error", sendErr, sendErr)
	}
	calls, payloads := probe.snapshot()
	if calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (HTML 429, HTML rejection, plain 429)", calls)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("retry waits = %#v, want one rate-limit wait only", waits)
	}
	if payloads[0]["parse_mode"] != "HTML" || payloads[1]["parse_mode"] != "HTML" {
		t.Fatalf("first two attempts were not HTML: %#v %#v", payloads[0], payloads[1])
	}
	if _, html := payloads[2]["parse_mode"]; html {
		t.Fatalf("fallback attempt retained parse mode: %#v", payloads[2])
	}
}

func TestTelegramTextRateLimitRetryHonorsCancellationInWaiter(t *testing.T) {
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return telegramJSONResponse(http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`), nil
	}}
	bot := newSendRetryBot(probe, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot.retryWait = func(_ context.Context, _ time.Duration) error {
		cancel()
		return nil
	}
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.send(ctx, route, "hello", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rate-limit retry error = %v", err)
	}
	calls, _ := probe.snapshot()
	if calls != 1 {
		t.Fatalf("cancelled rate-limit retry contacted Telegram %d times, want 1", calls)
	}
}

func TestTelegramHandlerTerminalSendsHonorChannelCancellation(t *testing.T) {
	missingAttachment := filepath.Join(t.TempDir(), "missing.txt")
	tests := []struct {
		name    string
		message *telegramMessage
		handler func(context.Context, core.Message, core.Emit) error
	}{
		{
			name:    "terminal emit text",
			message: privateMessage(8, "/cancel"),
			handler: func(_ context.Context, _ core.Message, emit core.Emit) error {
				finalText := "answer"
				emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText})
				return nil
			},
		},
		{
			name:    "handler error text",
			message: privateMessage(8, "/cancel"),
			handler: func(context.Context, core.Message, core.Emit) error {
				return errors.New("handler failed")
			},
		},
		{
			name: "attachment preparation error text",
			message: func() *telegramMessage {
				message := privateMessage(8, "/cancel")
				message.Document = &telegramDocument{FileID: "file-id", FileUniqueID: "unique", FileName: "report.pdf"}
				return message
			}(),
			handler: func(context.Context, core.Message, core.Emit) error { return nil },
		},
		{
			name:    "attachment delivery error text",
			message: privateMessage(8, "/cancel"),
			handler: func(_ context.Context, _ core.Message, emit core.Emit) error {
				emit(core.Event{Kind: core.EventFinal, Done: true, Attachments: []core.OutboundAttachment{{
					Kind: "attachment", Name: "missing.txt", Path: missingAttachment, MediaType: "text/plain", MaxBytes: 1024,
				}}})
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
				return nil, transientTransportFailure("reset")
			}}
			bot := newSendRetryBot(probe, nil)
			var logs strings.Builder
			bot.SetLogWriter(&logs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bot.retryWait = func(context.Context, time.Duration) error {
				// Ignore cancellation to prove each handler send path checks the
				// channel lifetime context itself before another request.
				cancel()
				return nil
			}
			bot.handle(ctx, test.handler, test.message)
			calls, _ := probe.snapshot()
			if calls != 1 {
				t.Fatalf("provider calls after channel cancellation = %d, want 1", calls)
			}
			if log := logs.String(); strings.Contains(log, "delivery failed") {
				t.Fatalf("routine shutdown cancellation produced a delivery diagnostic: %q", log)
			}
		})
	}
}

func TestTelegramFinalReplyDeliveryFailureDiagnostics(t *testing.T) {
	const secretText = "secret reply body"
	const token = "SECRET-TOKEN"
	tests := []struct {
		name       string
		respond    func(int, map[string]any) (*http.Response, error)
		want       string
		wantCalls  int
		wantAbsent []string
	}{
		{
			name: "transient transport exhaustion",
			respond: func(int, map[string]any) (*http.Response, error) {
				return nil, transientTransportFailure("reset")
			},
			want:      "telegram: final reply delivery failed: category=transport attempts=3",
			wantCalls: 3,
		},
		{
			name: "definitive provider rejection",
			respond: func(int, map[string]any) (*http.Response, error) {
				return telegramJSONResponse(http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`), nil
			},
			want:       "telegram: final reply delivery failed: category=provider code=400 attempts=1",
			wantCalls:  1,
			wantAbsent: []string{"message is too long"},
		},
		{
			name: "unknown failure",
			respond: func(int, map[string]any) (*http.Response, error) {
				return nil, errors.New("unexpected provider failure")
			},
			want:       "telegram: final reply delivery failed: category=unknown attempts=1",
			wantCalls:  1,
			wantAbsent: []string{"unexpected provider failure"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &telegramSendProbe{respond: test.respond}
			bot := newSendRetryBot(probe, nil)
			bot.retryWait = func(context.Context, time.Duration) error { return nil }
			var logs strings.Builder
			bot.SetLogWriter(&logs)
			bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
				finalText := secretText
				emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText})
				return nil
			}, privateMessage(8, "/diagnose"))
			calls, _ := probe.snapshot()
			if calls != test.wantCalls {
				t.Fatalf("provider calls = %d, want %d", calls, test.wantCalls)
			}
			forbidden := append([]string{secretText, token, "allowed_users", "api.telegram.org"}, test.wantAbsent...)
			assertDeliveryDiagnostic(t, logs.String(), test.want, forbidden...)
		})
	}
}

func TestTelegramFinalReplyDeadlineAndAuthorizationDiagnostics(t *testing.T) {
	const secretText = "secret reply body"
	const token = "SECRET-TOKEN"

	t.Run("deadline exhaustion", func(t *testing.T) {
		var calls atomic.Int32
		bot := New(config.Telegram{AllowedUsers: []string{"7"}, GroupMode: "all"}, token)
		bot.client.Transport = telegramRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		var logs strings.Builder
		bot.SetLogWriter(&logs)
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		bot.handle(ctx, func(_ context.Context, _ core.Message, emit core.Emit) error {
			finalText := secretText
			emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText})
			return nil
		}, privateMessage(8, "/deadline"))
		if got := calls.Load(); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
		assertDeliveryDiagnostic(t, logs.String(), "telegram: final reply delivery failed: category=deadline attempts=1", secretText, token)
	})

	t.Run("route authorization loss", func(t *testing.T) {
		allowed := []string{"7"}
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, &allowed)
		bot.retryWait = func(context.Context, time.Duration) error {
			allowed = nil
			return nil
		}
		var logs strings.Builder
		bot.SetLogWriter(&logs)
		bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
			finalText := secretText
			emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText})
			return nil
		}, privateMessage(8, "/authorize"))
		calls, _ := probe.snapshot()
		if calls != 1 {
			t.Fatalf("revoked retry contacted Telegram %d times, want 1", calls)
		}
		assertDeliveryDiagnostic(t, logs.String(), "telegram: final reply delivery failed: category=authorization attempts=1", secretText, token, "allowed_users")
	})
}

func TestTelegramProactiveDeliveryFailureAttribution(t *testing.T) {
	const secretText = "secret reply body"
	const token = "SECRET-TOKEN"

	t.Run("proactive message", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, transientTransportFailure("reset")
		}}
		bot := newSendRetryBot(probe, nil)
		bot.retryWait = func(context.Context, time.Duration) error { return nil }
		var logs strings.Builder
		bot.SetLogWriter(&logs)
		if err := bot.Deliver(context.Background(), "TG-7", "event-1", secretText); err == nil {
			t.Fatal("failed proactive delivery reported success")
		}
		calls, _ := probe.snapshot()
		if calls != 3 {
			t.Fatalf("provider calls = %d, want 3", calls)
		}
		assertDeliveryDiagnostic(t, logs.String(), "telegram: proactive message delivery failed: category=transport attempts=3", secretText, token)
	})

	t.Run("proactive event", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return nil, errors.New("unexpected event failure")
		}}
		bot := newSendRetryBot(probe, nil)
		var logs strings.Builder
		bot.SetLogWriter(&logs)
		if err := bot.DeliverEvent(context.Background(), "TG-7", "event-2", core.Event{Kind: core.EventFinal, Done: true, Text: secretText}); err == nil {
			t.Fatal("failed proactive event reported success")
		}
		calls, _ := probe.snapshot()
		if calls != 1 {
			t.Fatalf("provider calls = %d, want 1", calls)
		}
		assertDeliveryDiagnostic(t, logs.String(), "telegram: proactive event delivery failed: category=unknown attempts=1", secretText, token, "unexpected event failure")
	})
}

// privateMessage builds one authorized private inbound message for the
// handler tests. Slash-prefixed text keeps arrival typing activity out of
// the provider call count so each test observes delivery requests only.
func privateMessage(messageID int64, text string) *telegramMessage {
	return &telegramMessage{MessageID: messageID, From: telegramUser{ID: 7}, Chat: telegramChat{ID: 7, Type: "private"}, Date: 1, Text: text}
}

// assertDeliveryDiagnostic requires exactly one content-free delivery
// failure line containing want and none of the forbidden raw values.
func assertDeliveryDiagnostic(t *testing.T, log, want string, forbidden ...string) {
	t.Helper()
	if got := strings.Count(log, "delivery failed"); got != 1 {
		t.Fatalf("delivery diagnostics = %d, want exactly 1: %q", got, log)
	}
	if !strings.Contains(log, want) {
		t.Fatalf("delivery log = %q, want %q", log, want)
	}
	for _, value := range forbidden {
		if value != "" && strings.Contains(log, value) {
			t.Fatalf("delivery log leaked %q: %q", value, log)
		}
	}
}
