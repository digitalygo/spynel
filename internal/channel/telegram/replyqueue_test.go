package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/fsx"
	markdownfmt "github.com/digitalygo/spynel/internal/markdown"
)

// replyQueueClock is the deterministic injected clock for queue scheduling.
type replyQueueClock struct {
	mu  sync.Mutex
	now time.Time
}

func newReplyQueueClock() *replyQueueClock {
	return &replyQueueClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
}

func (c *replyQueueClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *replyQueueClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

// nonceGenerator returns deterministic, unique attempt nonces for one queue.
func nonceGenerator() func() string {
	var mu sync.Mutex
	counter := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("%016x", counter)
	}
}

// fakeElection simulates the workspace election mutation boundary. Every
// worker holds a token-bound guard, exactly like election.RunWhileOwner only
// admits the current lease holder; tests flip the owner token to simulate a
// takeover, and Held reports whether a guarded action is inside the lock so
// tests can prove provider requests run outside it.
type fakeElection struct {
	actionMu sync.Mutex
	stateMu  sync.Mutex
	owner    string
	held     int
}

func newFakeElection(owner string) *fakeElection {
	return &fakeElection{owner: owner}
}

// Guard returns the token-bound owner guard for one worker generation.
func (e *fakeElection) Guard(token string) OwnerGuard {
	return func(action func() error) (bool, error) {
		e.actionMu.Lock()
		defer e.actionMu.Unlock()
		e.stateMu.Lock()
		allowed := token != "" && e.owner == token
		if allowed {
			e.held++
		}
		e.stateMu.Unlock()
		if !allowed {
			return false, nil
		}
		err := action()
		e.stateMu.Lock()
		e.held--
		e.stateMu.Unlock()
		return true, err
	}
}

func (e *fakeElection) SetOwner(owner string) {
	e.stateMu.Lock()
	e.owner = owner
	e.stateMu.Unlock()
}

func (e *fakeElection) Held() bool {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.held > 0
}

// recordedReplySend is one observed queue delivery request.
type recordedReplySend struct {
	route   Route
	text    string
	replyTo int64
	html    bool
}

// replyQueueSender records every sent chunk and answers through an injected
// handler, so tests can schedule failures deterministically per call.
type replyQueueSender struct {
	mu    sync.Mutex
	sends []recordedReplySend
	fn    func(call int, send recordedReplySend) error
}

func (s *replyQueueSender) Send(_ context.Context, route Route, text string, replyTo int64, html bool) error {
	entry := recordedReplySend{route: route, text: text, replyTo: replyTo, html: html}
	s.mu.Lock()
	s.sends = append(s.sends, entry)
	call := len(s.sends)
	fn := s.fn
	s.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(call, entry)
}

func (s *replyQueueSender) snapshot() []recordedReplySend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedReplySend(nil), s.sends...)
}

func (s *replyQueueSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sends)
}

// fakeReplyBot is one verified adapter generation with an injected account
// ID, sender, and authorization behavior.
type fakeReplyBot struct {
	mu          sync.Mutex
	id          int64
	gen         uint64
	sender      *replyQueueSender
	authorizeFn func(replyRecord) error
}

func (f *fakeReplyBot) generation() uint64 { return f.gen }

func (f *fakeReplyBot) accountID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id
}

func (f *fakeReplyBot) authorizeQueuedRecord(record replyRecord) error {
	f.mu.Lock()
	fn := f.authorizeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(record)
	}
	return nil
}

func (f *fakeReplyBot) sendQueuedChunk(ctx context.Context, route Route, text string, replyTo int64, html bool) error {
	return f.sender.Send(ctx, route, text, replyTo, html)
}

// replyQueueFixture wires one queue to a deterministic clock, owner guard,
// sender, verified bot, and content-free log buffer.
type replyQueueFixture struct {
	queue    *replyQueue
	dir      string
	clock    *replyQueueClock
	logs     *strings.Builder
	sender   *replyQueueSender
	bot      *fakeReplyBot
	election *fakeElection
}

func newReplyQueueFixture(t *testing.T) *replyQueueFixture {
	t.Helper()
	return newReplyQueueFixtureIn(t, filepath.Join(t.TempDir(), "telegram-replies"), newReplyQueueClock())
}

func newReplyQueueFixtureIn(t *testing.T, dir string, clock *replyQueueClock) *replyQueueFixture {
	t.Helper()
	logs := &strings.Builder{}
	sender := &replyQueueSender{}
	election := newFakeElection("owner-1")
	queue := newReplyQueue(dir)
	queue.now = clock.Now
	queue.owner = "owner-1"
	queue.guard = election.Guard("owner-1")
	queue.newNonce = nonceGenerator()
	queue.logf = func(line string) { logs.WriteString(line + "\n") }
	bot := &fakeReplyBot{id: 7, gen: 1, sender: sender}
	queue.attachBot(context.Background(), bot)
	return &replyQueueFixture{queue: queue, dir: dir, clock: clock, logs: logs, sender: sender, bot: bot, election: election}
}

// record builds one valid enqueue record exactly as the adapter would: chunks
// rendered once with their plain-text fallbacks and the original route and
// anchor.
func (f *replyQueueFixture) record(conversation, text string, replyTo int64) replyRecord {
	chunks := markdownfmt.TelegramChunks(text)
	if len(chunks) == 0 {
		panic("test text produced no chunks")
	}
	plain := make([]string, len(chunks))
	for index, chunk := range chunks {
		plain[index] = markdownfmt.TelegramChunkPlainText(chunk)
	}
	return replyRecord{
		BotID:        f.bot.accountID(),
		Conversation: conversation,
		SenderID:     7,
		ReplyTo:      replyTo,
		HTML:         chunks,
		Plain:        plain,
	}
}

func (f *replyQueueFixture) enqueue(t *testing.T, conversation, text string, replyTo int64) {
	t.Helper()
	if err := f.queue.enqueue(f.record(conversation, text, replyTo)); err != nil {
		t.Fatalf("enqueue %q: %v", text, err)
	}
}

func (f *replyQueueFixture) records(t *testing.T) []replyHeader {
	t.Helper()
	headers, err := f.queue.scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return headers
}

// drain sweeps until no deliverable record remains, covering the bounded
// global concurrency limit without relying on wall-clock time.
func (f *replyQueueFixture) drain(t *testing.T) {
	t.Helper()
	for pass := 0; pass < 4*replyQueueMaxEntries; pass++ {
		f.queue.sweep(context.Background())
		f.queue.waitIdle()
		pending := false
		for _, header := range f.records(t) {
			record := header.record
			if record.Stopped || f.queue.claimActive(record, f.queue.now()) {
				continue
			}
			if replyDue(record, f.queue.now()) {
				pending = true
				break
			}
		}
		if !pending {
			return
		}
	}
	t.Fatal("reply queue did not quiesce")
}

func (f *replyQueueFixture) oneSweep(t *testing.T) {
	t.Helper()
	f.queue.sweep(context.Background())
	f.queue.waitIdle()
}

func assertQueueLog(t *testing.T, logs, want string) {
	t.Helper()
	if !strings.Contains(logs, want) {
		t.Fatalf("queue log = %q, want %q", logs, want)
	}
}

func assertQueueLogAbsent(t *testing.T, logs string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if value != "" && strings.Contains(logs, value) {
			t.Fatalf("queue log leaked %q: %q", value, logs)
		}
	}
}

func writeRawRecord(t *testing.T, directory, name string, record replyRecord) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.AtomicWriteFile(filepath.Join(directory, name), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestReplyQueueQueuesAndDeliversFinalText(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.enqueue(t, "TG-7", "hello **world**", 41)
	headers := f.records(t)
	if len(headers) != 1 {
		t.Fatalf("queued records = %d, want 1", len(headers))
	}
	if headers[0].record.BotID != 7 || headers[0].record.ReplyTo != 41 {
		t.Fatalf("queued record = %#v, want bot 7 anchored at 41", headers[0].record)
	}
	f.drain(t)
	sends := f.sender.snapshot()
	if len(sends) != 1 {
		t.Fatalf("delivery attempts = %d, want 1", len(sends))
	}
	wantChunk := markdownfmt.TelegramChunks("hello **world**")[0]
	if sends[0].text != wantChunk || !sends[0].html {
		t.Fatalf("delivered chunk = %#v, want %q as HTML", sends[0], wantChunk)
	}
	if sends[0].replyTo != 41 {
		t.Fatalf("first chunk reply anchor = %d, want 41", sends[0].replyTo)
	}
	if sends[0].route.Conversation() != "TG-7" {
		t.Fatalf("delivered route = %q, want TG-7", sends[0].route.Conversation())
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("records after delivery = %d, want 0", len(headers))
	}
	logs := f.logs.String()
	assertQueueLog(t, logs, "telegram: reply queue queued")
	assertQueueLog(t, logs, "telegram: reply queue delivered chunks=1")
	assertQueueLogAbsent(t, logs, "hello", "world", "TG-7", "41", "SECRET")
}

func TestReplyQueuePreservesTopicThreadAndAnchor(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.enqueue(t, "TG-7-topic-5", "topic reply", 41)
	f.drain(t)
	sends := f.sender.snapshot()
	if len(sends) != 1 {
		t.Fatalf("attempts = %d, want 1", len(sends))
	}
	if sends[0].route.ThreadID() != 5 || sends[0].route.Conversation() != "TG-7-topic-5" {
		t.Fatalf("delivered route = %q thread=%d, want TG-7-topic-5 thread 5", sends[0].route.Conversation(), sends[0].route.ThreadID())
	}
	if sends[0].replyTo != 41 {
		t.Fatalf("delivered reply anchor = %d, want 41", sends[0].replyTo)
	}
}

func TestReplyQueueInitialSendAndFiveScheduledRounds(t *testing.T) {
	f := newReplyQueueFixture(t)
	var times []time.Time
	f.sender.fn = func(_ int, _ recordedReplySend) error {
		times = append(times, f.clock.Now())
		return errors.New("provider outage with secret prose")
	}
	f.enqueue(t, "TG-7", "durable reply", 0)
	f.drain(t)
	if len(times) != 1 {
		t.Fatalf("initial attempt count = %d, want 1", len(times))
	}

	steps := []struct {
		advance time.Duration
		want    int
	}{
		{advance: 29 * time.Second, want: 1},
		{advance: time.Second, want: 2},
		{advance: 59 * time.Second, want: 2},
		{advance: time.Second, want: 3},
		{advance: time.Minute + 59*time.Second, want: 3},
		{advance: time.Second, want: 4},
		{advance: 3*time.Minute + 59*time.Second, want: 4},
		{advance: time.Second, want: 5},
		{advance: 7*time.Minute + 59*time.Second, want: 5},
		{advance: time.Second, want: 6},
	}
	for _, step := range steps {
		f.clock.Advance(step.advance)
		f.drain(t)
		if len(times) != step.want {
			t.Fatalf("attempts after advancing %s = %d, want %d", step.advance, len(times), step.want)
		}
	}

	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	wantTimes := []time.Time{
		start,
		start.Add(30 * time.Second),
		start.Add(90 * time.Second),
		start.Add(3*time.Minute + 30*time.Second),
		start.Add(7*time.Minute + 30*time.Second),
		start.Add(15*time.Minute + 30*time.Second),
	}
	if len(times) != len(wantTimes) {
		t.Fatalf("scheduled attempts = %d, want %d", len(times), len(wantTimes))
	}
	for index := range wantTimes {
		if !times[index].Equal(wantTimes[index]) {
			t.Fatalf("attempt %d at %s, want %s", index+1, times[index], wantTimes[index])
		}
	}

	// Exhaustion is persisted until the one-hour expiry instead of deleting
	// the record: no further send may happen, and the state survives rescans.
	headers := f.records(t)
	if len(headers) != 1 {
		t.Fatalf("records after exhaustion = %d, want 1 persisted exhausted state", len(headers))
	}
	if !headers[0].record.Stopped || headers[0].record.StopReason != replyStopExhausted || headers[0].record.Attempts != replyQueueMaxAttempts {
		t.Fatalf("persisted exhausted state = %#v", headers[0].record)
	}
	// The exhausted record is retained until its one-hour expiry without any
	// extra network traffic.
	f.clock.Advance(30 * time.Minute)
	f.drain(t)
	if len(times) != replyQueueMaxAttempts {
		t.Fatalf("stopped record issued %d extra sends", len(times)-replyQueueMaxAttempts)
	}
	if headers := f.records(t); len(headers) != 1 {
		t.Fatalf("exhausted record disappeared before expiry: %#v", headers)
	}
	f.clock.Advance(20 * time.Minute)
	f.drain(t)
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("exhausted record survived expiry: %#v", headers)
	}
	logs := f.logs.String()
	for round := 1; round <= 5; round++ {
		assertQueueLog(t, logs, fmt.Sprintf("telegram: reply queue retry round=%d", round))
	}
	assertQueueLog(t, logs, "telegram: reply queue exhausted attempts=6")
	assertQueueLog(t, logs, "telegram: reply queue expired")
	assertQueueLogAbsent(t, logs, "durable reply", "provider outage", "TG-7", "secret prose")
}

func TestReplyQueueTransientFailuresStayRetryable(t *testing.T) {
	cases := map[string]error{
		"timeout":  &telegramTransportError{err: errors.New("redacted timeout")},
		"reset":    &telegramTransportError{err: errors.New("redacted reset")},
		"eof":      &telegramTransportError{err: errors.New("redacted eof")},
		"five_xx":  &telegramAPIError{method: "sendMessage", code: http.StatusInternalServerError, description: "Internal Server Error"},
		"bad_gate": &telegramAPIError{method: "sendMessage", code: http.StatusBadGateway, description: "Bad Gateway"},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReplyQueueFixture(t)
			f.sender.fn = func(call int, _ recordedReplySend) error {
				if call == 1 {
					return failure
				}
				return nil
			}
			f.enqueue(t, "TG-7", "transient reply", 0)
			f.drain(t)
			headers := f.records(t)
			if len(headers) != 1 || headers[0].record.Stopped || headers[0].record.Attempts != 1 {
				t.Fatalf("record after transient failure = %#v, want retryable attempts:1", headers)
			}
			f.clock.Advance(30 * time.Second)
			f.drain(t)
			if f.sender.count() != 2 {
				t.Fatalf("attempts after wait = %d, want 2", f.sender.count())
			}
			if headers := f.records(t); len(headers) != 0 {
				t.Fatalf("records after recovery = %d, want 0", len(headers))
			}
			assertQueueLog(t, f.logs.String(), "telegram: reply queue retry round=1")
		})
	}
}

func TestReplyQueuePermanentFailuresStopUntilExpiry(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			f := newReplyQueueFixture(t)
			f.sender.fn = func(int, recordedReplySend) error {
				return &telegramAPIError{method: "sendMessage", code: code, description: "provider refusal"}
			}
			f.enqueue(t, "TG-7", "permanent reply", 0)
			f.drain(t)
			headers := f.records(t)
			if len(headers) != 1 {
				t.Fatalf("records = %d, want 1 persisted permanent state", len(headers))
			}
			if !headers[0].record.Stopped || headers[0].record.StopReason != replyStopPermanent {
				t.Fatalf("persisted state = %#v, want permanent", headers[0].record)
			}
			f.clock.Advance(replyQueueExpiry - time.Minute)
			f.drain(t)
			if f.sender.count() != 1 {
				t.Fatalf("permanent failure issued %d extra sends", f.sender.count()-1)
			}
			f.clock.Advance(2 * time.Minute)
			f.drain(t)
			if headers := f.records(t); len(headers) != 0 {
				t.Fatalf("permanent record survived expiry: %#v", headers)
			}
			logs := f.logs.String()
			assertQueueLog(t, logs, "telegram: reply queue stopped reason=permanent")
			assertQueueLog(t, logs, "telegram: reply queue expired")
			assertQueueLogAbsent(t, logs, "permanent reply", "provider refusal", "TG-7")
		})
	}
}

func TestReplyQueueRateLimitPolicy(t *testing.T) {
	t.Run("bounded retry_after defers without extra sends", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(call int, _ recordedReplySend) error {
			if call == 1 {
				return &telegramAPIError{method: "sendMessage", code: http.StatusTooManyRequests, retryAfter: 45 * time.Second}
			}
			return nil
		}
		f.enqueue(t, "TG-7", "rate limited", 0)
		f.drain(t)
		if f.sender.count() != 1 {
			t.Fatalf("attempts after 429 = %d, want 1", f.sender.count())
		}
		f.clock.Advance(30 * time.Second)
		f.drain(t)
		if f.sender.count() != 1 {
			t.Fatalf("429 wait was ignored: attempts = %d", f.sender.count())
		}
		f.clock.Advance(15 * time.Second)
		f.drain(t)
		if f.sender.count() != 2 {
			t.Fatalf("attempts after honoring retry_after = %d, want 2", f.sender.count())
		}
	})

	t.Run("retry_after above the cap fails without clipping", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(int, recordedReplySend) error {
			return &telegramAPIError{method: "sendMessage", code: http.StatusTooManyRequests, retryAfter: 30 * time.Minute}
		}
		f.enqueue(t, "TG-7", "rate limited", 0)
		f.drain(t)
		headers := f.records(t)
		if len(headers) != 1 || !headers[0].record.Stopped || headers[0].record.StopReason != replyStopPermanent {
			t.Fatalf("oversized retry_after state = %#v, want permanent stop", headers)
		}
		// The persisted next-attempt stays the ordinary scheduled round; the
		// provider's oversized delay is never clipped into a retry.
		if got := headers[0].record.NextAttempt; got.Sub(headers[0].record.CreatedAt) != 30*time.Second {
			t.Fatalf("next attempt = %s after the initial failure, want the ordinary 30s round", got)
		}
		f.clock.Advance(maxTelegramRetryAfter + time.Minute)
		f.drain(t)
		if f.sender.count() != 1 {
			t.Fatalf("oversized retry_after issued %d extra sends", f.sender.count()-1)
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue stopped reason=permanent")
	})

	t.Run("429 on the last round exhausts without another send", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(int, recordedReplySend) error {
			return &telegramAPIError{method: "sendMessage", code: http.StatusTooManyRequests, retryAfter: 10 * time.Second}
		}
		f.enqueue(t, "TG-7", "rate limited", 0)
		for _, wait := range []time.Duration{0, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute} {
			f.clock.Advance(wait)
			f.drain(t)
		}
		if f.sender.count() != replyQueueMaxAttempts {
			t.Fatalf("attempts = %d, want %d", f.sender.count(), replyQueueMaxAttempts)
		}
		headers := f.records(t)
		if len(headers) != 1 || !headers[0].record.Stopped || headers[0].record.StopReason != replyStopExhausted {
			t.Fatalf("records after exhausted 429 = %#v, want exhausted state", headers)
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue exhausted attempts=6")
	})
}

func TestReplyQueueHTMLFallbackUsesPlainChunkAndRevalidates(t *testing.T) {
	f := newReplyQueueFixture(t)
	authorizeCalls := 0
	f.bot.authorizeFn = func(replyRecord) error {
		authorizeCalls++
		return nil
	}
	wantPlain := markdownfmt.TelegramChunkPlainText(markdownfmt.TelegramChunks("**bold** plain")[0])
	if wantPlain == "" || strings.Contains(wantPlain, "<") {
		t.Fatalf("plain fallback fixture = %q, want stripped text", wantPlain)
	}
	f.sender.fn = func(call int, send recordedReplySend) error {
		switch call {
		case 1:
			if !send.html {
				return errors.New("first attempt must be HTML")
			}
			return &telegramAPIError{method: "sendMessage", code: http.StatusBadRequest, description: "Bad Request: can't parse entities", parse: true}
		case 2:
			if send.html {
				return errors.New("plain fallback must not use HTML")
			}
			if send.text != wantPlain {
				return fmt.Errorf("fallback sent %q, want the stored plain chunk %q", send.text, wantPlain)
			}
			return errors.New("plain fallback transport failure")
		default:
			if send.html {
				return errors.New("persisted plain mode regressed to HTML")
			}
			if send.text != wantPlain {
				return fmt.Errorf("persisted plain retry sent %q, want %q", send.text, wantPlain)
			}
			return nil
		}
	}
	f.enqueue(t, "TG-7", "**bold** plain", 0)
	f.drain(t)
	sends := f.sender.snapshot()
	if len(sends) != 2 {
		t.Fatalf("attempts = %d, want HTML then the plain fallback", len(sends))
	}
	// The fallback stays inside the first scheduled attempt and never resets
	// or extends the retry budget.
	if authorizeCalls != 2 {
		t.Fatalf("authorization checks = %d, want one before every chunk and fallback request", authorizeCalls)
	}
	headers := f.records(t)
	if len(headers) != 1 || !headers[0].record.PlainMode || headers[0].record.Attempts != 1 {
		t.Fatalf("record after persisted plain mode = %#v", headers)
	}
	f.clock.Advance(30 * time.Second)
	f.drain(t)
	sends = f.sender.snapshot()
	if len(sends) != 3 {
		t.Fatalf("attempts after the plain retry = %d, want the persisted plain round", len(sends))
	}
	if authorizeCalls != 3 {
		t.Fatalf("authorization checks = %d, want one before the retried chunk too", authorizeCalls)
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("records after plain delivery = %#v, logs: %s", headers, f.logs.String())
	}
}

func TestReplyQueueRecipientRevocationMidChunkSuppresses(t *testing.T) {
	f := newReplyQueueFixture(t)
	longText := strings.Repeat("b", markdownfmt.TelegramMaxVisiblePerMessage) + "tail"
	if len(markdownfmt.TelegramChunks(longText)) != 2 {
		t.Fatal("fixture must produce two chunks")
	}
	authorizeCalls := 0
	f.bot.authorizeFn = func(replyRecord) error {
		authorizeCalls++
		if authorizeCalls > 1 {
			return errReplyQueueRecipientRevoked
		}
		return nil
	}
	f.enqueue(t, "TG-7", longText, 0)
	f.drain(t)
	if f.sender.count() != 1 {
		t.Fatalf("sends after mid-chunk revocation = %d, want only the first chunk", f.sender.count())
	}
	headers := f.records(t)
	if len(headers) != 1 {
		t.Fatalf("records after revocation = %d, want 1 suppressed record", len(headers))
	}
	record := headers[0].record
	if !record.Stopped || record.StopReason != replyStopSuppressed || record.Acked != 1 || record.Attempts != 0 {
		t.Fatalf("suppressed state = %#v, want acked:1, unspent attempt, suppressed", record)
	}
	// Even a restored authorization never resumes a permanently suppressed
	// recipient; the record survives until expiry.
	f.bot.authorizeFn = func(replyRecord) error { return nil }
	f.clock.Advance(replyQueueExpiry - time.Minute)
	f.drain(t)
	if f.sender.count() != 1 {
		t.Fatalf("suppressed record resumed: %d sends", f.sender.count())
	}
	f.clock.Advance(2 * time.Minute)
	f.drain(t)
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("suppressed record survived expiry: %#v", headers)
	}
	assertQueueLog(t, f.logs.String(), "telegram: reply queue suppressed reason=authorization")
}

func TestReplyQueueGenerationLossSuspendsWithoutSpend(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.bot.authorizeFn = func(replyRecord) error { return errReplyQueueGenerationLost }
	f.enqueue(t, "TG-7", "generation reply", 0)
	f.oneSweep(t)
	if f.sender.count() != 0 {
		t.Fatalf("lost generation contacted the provider %d times", f.sender.count())
	}
	headers := f.records(t)
	if len(headers) != 1 || headers[0].record.Stopped || headers[0].record.Attempts != 0 || headers[0].record.Claim != nil {
		t.Fatalf("state after generation loss = %#v, want suspended without spend", headers)
	}
	f.bot.authorizeFn = nil
	f.drain(t)
	if f.sender.count() != 1 {
		t.Fatalf("replacement generation attempts = %d, want 1", f.sender.count())
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("records after resume = %d, want 0", len(headers))
	}
	assertQueueLog(t, f.logs.String(), "telegram: reply queue suppressed reason=authorization")
}

func TestReplyQueueAccountChangeNeverUsesAnotherBot(t *testing.T) {
	f := newReplyQueueFixture(t)
	record := f.record("TG-7", "other account reply", 0)
	record.BotID = 100
	if err := f.queue.enqueue(record); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	f.oneSweep(t)
	if f.sender.count() != 0 {
		t.Fatalf("record bound to another bot was delivered %d times", f.sender.count())
	}
	headers := f.records(t)
	if len(headers) != 1 || headers[0].record.BotID != 100 {
		t.Fatalf("records after account change = %#v, want the original binding", headers)
	}
	f.clock.Advance(replyQueueExpiry + time.Second)
	f.oneSweep(t)
	if f.sender.count() != 0 {
		t.Fatalf("expired foreign-account record produced %d sends", f.sender.count())
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("foreign-account record survived expiry: %#v", headers)
	}
	assertQueueLog(t, f.logs.String(), "telegram: reply queue expired")
}

func TestReplyQueueOrphanedWhileDisabledCleansExpiry(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.queue.detachBot(f.bot)
	f.enqueue(t, "TG-7", "orphaned reply", 0)
	f.oneSweep(t)
	if f.sender.count() != 0 {
		t.Fatalf("orphaned record contacted the provider %d times", f.sender.count())
	}
	if headers := f.records(t); len(headers) != 1 {
		t.Fatalf("orphaned record disappeared before expiry: %#v", headers)
	}
	f.clock.Advance(replyQueueExpiry + time.Second)
	f.oneSweep(t)
	if f.sender.count() != 0 {
		t.Fatalf("expiry cleanup produced %d sends", f.sender.count())
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("orphaned record survived expiry: %#v", headers)
	}
	assertQueueLog(t, f.logs.String(), "telegram: reply queue expired")
}

func TestReplyQueueGuardIsNeverHeldDuringProviderRequests(t *testing.T) {
	f := newReplyQueueFixture(t)
	var violations int
	f.sender.fn = func(int, recordedReplySend) error {
		if f.election.Held() {
			violations++
		}
		return nil
	}
	f.bot.authorizeFn = func(replyRecord) error {
		if f.election.Held() {
			violations++
		}
		return nil
	}
	f.enqueue(t, "TG-7", "unlocked reply", 0)
	f.drain(t)
	if violations != 0 {
		t.Fatalf("provider or authorization work ran inside the election lock %d times", violations)
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("records after delivery = %d, want 0", len(headers))
	}
}

func TestReplyQueuePartialChunkAckAndRestart(t *testing.T) {
	longText := strings.Repeat("a", markdownfmt.TelegramMaxVisiblePerMessage) + "tail"
	chunks := markdownfmt.TelegramChunks(longText)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	f := newReplyQueueFixture(t)
	f.sender.fn = func(_ int, send recordedReplySend) error {
		if send.text == chunks[0] {
			return nil
		}
		return errors.New("crash after the first chunk")
	}
	f.enqueue(t, "TG-7", longText, 41)
	f.drain(t)
	sends := f.sender.snapshot()
	if len(sends) != 2 {
		t.Fatalf("attempts = %d, want 2 (first chunk then crashing second)", len(sends))
	}
	if sends[0].text != chunks[0] || sends[0].replyTo != 41 {
		t.Fatalf("first attempt = %#v, want chunk 1 anchored at 41", sends[0])
	}
	if sends[1].text != chunks[1] || sends[1].replyTo != 0 {
		t.Fatalf("second attempt = %#v, want chunk 2 without the anchor", sends[1])
	}
	headers := f.records(t)
	if len(headers) != 1 {
		t.Fatalf("records after crash = %d, want 1", len(headers))
	}
	if headers[0].record.Acked != 1 || headers[0].record.Attempts != 1 {
		t.Fatalf("persisted progress = acked:%d attempts:%d, want acked:1 attempts:1", headers[0].record.Acked, headers[0].record.Attempts)
	}

	f.clock.Advance(30 * time.Second)
	resumed := newReplyQueueFixtureIn(t, f.dir, f.clock)
	resumed.sender.fn = nil
	resumed.drain(t)
	resumedSends := resumed.sender.snapshot()
	if len(resumedSends) != 1 {
		t.Fatalf("resumed attempts = %d, want only the unacknowledged chunk", len(resumedSends))
	}
	if resumedSends[0].text != chunks[1] || resumedSends[0].replyTo != 0 {
		t.Fatalf("resumed attempt = %#v, want chunk 2 without the anchor", resumedSends[0])
	}
	if headers := resumed.records(t); len(headers) != 0 {
		t.Fatalf("records after resumed delivery = %d, want 0", len(headers))
	}
}

func TestReplyQueueFifteenMinuteOutageWithRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telegram-replies")
	clock := newReplyQueueClock()
	first := newReplyQueueFixtureIn(t, dir, clock)
	first.sender.fn = func(int, recordedReplySend) error { return errors.New("outage") }
	first.enqueue(t, "TG-7", "survives an outage", 41)
	first.drain(t)
	clock.Advance(30 * time.Second)
	first.drain(t)
	if first.sender.count() != 2 {
		t.Fatalf("pre-restart attempts = %d, want 2", first.sender.count())
	}

	// Simulate a process restart: the persisted record, cursor, and reserved
	// next attempt survive while a completely new queue instance resumes.
	second := newReplyQueueFixtureIn(t, dir, clock)
	outageEnd := clock.Now().Add(15 * time.Minute)
	second.sender.fn = func(_ int, _ recordedReplySend) error {
		if second.clock.Now().Before(outageEnd) {
			return errors.New("outage")
		}
		return nil
	}
	second.drain(t)
	if second.sender.count() != 0 {
		t.Fatalf("restart attempted before the persisted next-attempt time: %d", second.sender.count())
	}
	clock.Advance(time.Minute)
	second.drain(t)
	if second.sender.count() != 1 {
		t.Fatalf("attempt after first scheduled wait = %d, want 1", second.sender.count())
	}
	clock.Advance(2 * time.Minute)
	second.drain(t)
	clock.Advance(4 * time.Minute)
	second.drain(t)
	if second.sender.count() != 3 {
		t.Fatalf("attempts before the final round = %d, want 3", second.sender.count())
	}
	clock.Advance(8 * time.Minute)
	second.drain(t)
	if second.sender.count() != 4 {
		t.Fatalf("post-outage attempts = %d, want 4", second.sender.count())
	}
	if headers := second.records(t); len(headers) != 0 {
		t.Fatalf("records after recovery = %d, want 0", len(headers))
	}
	assertQueueLog(t, second.logs.String(), "telegram: reply queue delivered chunks=1")
}

func TestReplyQueueOverlappingGenerations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telegram-replies")
	clock := newReplyQueueClock()
	election := newFakeElection("gen-a")
	first := newReplyQueueFixtureIn(t, dir, clock)
	first.queue.owner = "gen-a"
	first.queue.guard = election.Guard("gen-a")

	longText := strings.Repeat("c", markdownfmt.TelegramMaxVisiblePerMessage) + "tail"
	chunks := markdownfmt.TelegramChunks(longText)
	if len(chunks) != 2 {
		t.Fatal("fixture must produce two chunks")
	}
	first.sender.fn = func(call int, send recordedReplySend) error {
		// The first chunk is confirmed by the transport, then ownership moves
		// on before the first generation can persist its acknowledgement.
		if call == 1 {
			election.SetOwner("gen-b")
		}
		return nil
	}
	first.enqueue(t, "TG-7", longText, 0)
	first.oneSweep(t)
	if first.sender.count() != 1 {
		t.Fatalf("first generation sends = %#v, want only the lost chunk", first.sender.snapshot())
	}
	headers := first.records(t)
	if len(headers) != 1 {
		t.Fatalf("records after takeover = %d, want 1", len(headers))
	}
	if headers[0].record.Acked != 0 || headers[0].record.Attempts != 1 || headers[0].record.Claim == nil {
		t.Fatalf("record after takeover = %#v, want unacked claim from the lost generation", headers[0].record)
	}

	// The successor cannot touch the record while the claim is live, and only
	// reclaims it after the claim expires.
	second := newReplyQueueFixtureIn(t, dir, clock)
	second.queue.owner = "gen-b"
	second.queue.guard = election.Guard("gen-b")
	second.oneSweep(t)
	if second.sender.count() != 0 {
		t.Fatalf("successor reclaimed a live claim: %d sends", second.sender.count())
	}
	clock.Advance(replyQueueClaimDuration + time.Second)
	second.drain(t)

	// The lost generation's reservation is rolled back: the successor starts
	// from the same round rather than spending a second one.
	sends := second.sender.snapshot()
	if len(sends) != 2 {
		t.Fatalf("successor sends = %d, want the unacknowledged chunks", len(sends))
	}
	if sends[0].text != chunks[0] || sends[1].text != chunks[1] {
		t.Fatalf("successor order = %#v, want both chunks in order", sends)
	}
	if first.sender.count() != 1 {
		t.Fatalf("old generation sent after losing ownership: %d", first.sender.count())
	}
	if headers := second.records(t); len(headers) != 0 {
		t.Fatalf("records after successor delivery = %d, want 0", len(headers))
	}
}

func TestReplyQueueFenceRefusesStaleRevisionAndNonce(t *testing.T) {
	t.Run("stale revision", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.enqueue(t, "TG-7", "fenced reply", 0)
		headers := f.records(t)
		header := headers[0]
		// A successor persisted a newer revision while this worker held an
		// older view.
		updated := header.record
		updated.Revision++
		updated.Acked = 1
		writeRawRecord(t, f.dir, header.name, updated)
		_, outcome := f.queue.fencedMutation(header.name, header.record.Revision, "", func(record *replyRecord) bool {
			record.Acked = 0
			return true
		})
		if outcome != replyFenceStale {
			t.Fatalf("stale mutation outcome = %v, want refused", outcome)
		}
		current := f.records(t)[0].record
		if current.Acked != 1 || current.Revision != updated.Revision {
			t.Fatalf("stale mutation overwrote successor state: %#v", current)
		}
	})

	t.Run("unknown nonce", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.enqueue(t, "TG-7", "fenced reply", 0)
		header := f.records(t)[0]
		_, outcome := f.queue.fencedMutation(header.name, header.record.Revision, "0000000000000000", func(record *replyRecord) bool {
			record.Acked = 1
			return true
		})
		if outcome != replyFenceStale {
			t.Fatalf("nonce mismatch outcome = %v, want refused", outcome)
		}
		if current := f.records(t)[0].record; current.Acked != 0 {
			t.Fatalf("nonce mismatch overwrote record: %#v", current)
		}
	})
}

func TestReplyQueueRemovalRefusesSuccessorState(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.enqueue(t, "TG-7", "successor state", 0)
	header := f.records(t)[0]

	f.election.SetOwner("")
	if outcome := f.queue.removeFenced(header.name, header.record.Revision, ""); outcome != replyFenceLost {
		t.Fatalf("removal without ownership outcome = %v, want lost", outcome)
	}
	if headers := f.records(t); len(headers) != 1 {
		t.Fatalf("ownership loss removed successor state: %d records", len(headers))
	}

	f.election.SetOwner("owner-1")
	if outcome := f.queue.removeFenced(header.name, header.record.Revision+5, ""); outcome != replyFenceStale {
		t.Fatalf("stale removal outcome = %v, want stale", outcome)
	}
	if headers := f.records(t); len(headers) != 1 {
		t.Fatalf("stale removal removed successor state: %d records", len(headers))
	}

	if outcome := f.queue.removeFenced(header.name, header.record.Revision, ""); outcome != replyFenceApplied {
		t.Fatalf("valid removal outcome = %v, want applied", outcome)
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("valid removal left records: %#v", headers)
	}
}

func TestReplyQueueExpiryDeletesWithoutDelivery(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.enqueue(t, "TG-7", "expired reply", 0)
	f.clock.Advance(replyQueueExpiry + time.Second)
	f.drain(t)
	if f.sender.count() != 0 {
		t.Fatalf("expired reply contacted the provider %d times", f.sender.count())
	}
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("records after expiry = %d, want 0", len(headers))
	}
	assertQueueLog(t, f.logs.String(), "telegram: reply queue expired")
	assertQueueLogAbsent(t, f.logs.String(), "expired reply", "TG-7")
}

func TestReplyQueueCapacityBounds(t *testing.T) {
	t.Run("per route", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		for index := 0; index < replyQueueMaxPerRoute; index++ {
			f.enqueue(t, "TG-7", fmt.Sprintf("reply %d", index), 0)
		}
		err := f.queue.enqueue(f.record("TG-7", "overflow", 0))
		if !errors.Is(err, errReplyQueueFull) {
			t.Fatalf("per-route overflow error = %v, want errReplyQueueFull", err)
		}
		if headers := f.records(t); len(headers) != replyQueueMaxPerRoute {
			t.Fatalf("records = %d, want %d", len(headers), replyQueueMaxPerRoute)
		}
	})

	t.Run("entry count", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		for route := 0; route < replyQueueMaxEntries/replyQueueMaxPerRoute; route++ {
			for index := 0; index < replyQueueMaxPerRoute; index++ {
				f.enqueue(t, fmt.Sprintf("TG-%d", 100+route), fmt.Sprintf("reply %d", index), 0)
			}
		}
		if headers := f.records(t); len(headers) != replyQueueMaxEntries {
			t.Fatalf("records = %d, want %d", len(headers), replyQueueMaxEntries)
		}
		err := f.queue.enqueue(f.record("TG-9999", "overflow", 0))
		if !errors.Is(err, errReplyQueueFull) {
			t.Fatalf("entry overflow error = %v, want errReplyQueueFull", err)
		}
	})

	t.Run("entry size", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		chunk := strings.Repeat("a", 300<<10)
		err := f.queue.enqueue(replyRecord{BotID: 7, Conversation: "TG-7", SenderID: 7, HTML: []string{chunk}, Plain: []string{chunk}})
		if !errors.Is(err, errReplyQueueFull) {
			t.Fatalf("oversized entry error = %v, want errReplyQueueFull", err)
		}
	})

	t.Run("total bytes", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		chunk := strings.Repeat("a", 250<<10)
		for index := 0; index < 33; index++ {
			record := replyRecord{
				Version:      replyQueueVersion,
				BotID:        7,
				Conversation: fmt.Sprintf("TG-%d", 1000+index),
				SenderID:     7,
				HTML:         []string{chunk},
				Plain:        []string{chunk},
				Revision:     1,
			}
			record.CreatedAt = f.clock.Now()
			record.ExpiresAt = record.CreatedAt.Add(replyQueueExpiry)
			writeRawRecord(t, f.dir, fmt.Sprintf("bulk-%02d.json", index), record)
		}
		err := f.queue.enqueue(f.record("TG-2000", "small", 0))
		if !errors.Is(err, errReplyQueueFull) {
			t.Fatalf("total-bytes overflow error = %v, want errReplyQueueFull", err)
		}
	})
}

func TestReplyQueueFailsClosedOnUntrustedStorage(t *testing.T) {
	t.Run("corrupt record", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, "corrupt.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("corrupt record produced %d sends", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue storage error reason=corrupt")
	})

	t.Run("symlinked record", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires privileges on Windows")
		}
		f := newReplyQueueFixture(t)
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target.json")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(f.dir, "linked.json")); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("symlinked record produced %d sends", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue storage error reason=symlink")
	})

	t.Run("non-private record permissions", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.enqueue(t, "TG-7", "private reply", 0)
		header := f.records(t)[0]
		if err := os.Chmod(filepath.Join(f.dir, header.name), 0o644); err != nil {
			t.Fatal(err)
		}
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("non-private record produced %d sends", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue storage error reason=permissions")
	})

	t.Run("oversized record", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, "huge.json"), make([]byte, replyQueueMaxEntryBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("oversized record produced %d sends", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue storage error reason=oversized")
	})

	t.Run("symlinked directory root", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires privileges on Windows")
		}
		f := newReplyQueueFixture(t)
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "telegram-replies")
		if err := os.Symlink(f.dir, link); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		linked := newReplyQueueFixtureIn(t, link, newReplyQueueClock())
		linked.oneSweep(t)
		if linked.sender.count() != 0 {
			t.Fatalf("symlinked directory produced %d sends", linked.sender.count())
		}
		assertQueueLog(t, linked.logs.String(), "telegram: reply queue storage error reason=directory")
		if err := linked.queue.enqueue(linked.record("TG-7", "overflow", 0)); !errors.Is(err, errReplyQueueStorage) {
			t.Fatalf("symlinked directory enqueue error = %v, want errReplyQueueStorage", err)
		}
	})

	t.Run("symlinked ancestor", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires privileges on Windows")
		}
		real := t.TempDir()
		link := filepath.Join(t.TempDir(), "linked-runtime")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		directory := filepath.Join(link, "runtime", "telegram-replies")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		f := newReplyQueueFixtureIn(t, directory, newReplyQueueClock())
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("symlinked ancestor produced %d sends", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue storage error reason=directory")
		if err := f.queue.enqueue(f.record("TG-7", "reply", 0)); !errors.Is(err, errReplyQueueStorage) {
			t.Fatalf("symlinked ancestor enqueue error = %v, want errReplyQueueStorage", err)
		}
	})

	t.Run("non-directory root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "telegram-replies")
		if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		f := newReplyQueueFixtureIn(t, root, newReplyQueueClock())
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("file root produced %d sends", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue storage error reason=directory")
	})

	t.Run("bounded directory enumeration", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for index := 0; index <= replyQueueMaxScanEntries; index++ {
			if err := os.WriteFile(filepath.Join(f.dir, fmt.Sprintf("f-%05d", index)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("over-bound directory produced %d sends", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue storage error reason=directory")
	})

	t.Run("bounded corrupt tracking", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < replyQueueMaxBrokenEntries+10; index++ {
			if err := os.WriteFile(filepath.Join(f.dir, fmt.Sprintf("bad-%04d.json", index)), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("corrupt corpus produced %d sends", f.sender.count())
		}
		logs := f.logs.String()
		if count := strings.Count(logs, "storage error reason=corrupt\n"); count > replyQueueMaxBrokenEntries {
			t.Fatalf("corrupt tracking exceeded its bound: %d diagnostics", count)
		}
		assertQueueLog(t, logs, "telegram: reply queue storage error reason=corrupt_limit")
	})
}

func TestReplyQueueUnlinkFailureNeverClaimsRemoval(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.enqueue(t, "TG-7", "unlink fault reply", 0)
	f.queue.removeFile = func(string) error { return errors.New("remove fault") }
	f.oneSweep(t)
	if f.sender.count() != 1 {
		t.Fatalf("sends = %d, want 1", f.sender.count())
	}
	headers := f.records(t)
	if len(headers) != 1 || headers[0].record.Acked != 1 {
		t.Fatalf("record after failed unlink = %#v, want fully acknowledged and retained", headers)
	}
	logs := f.logs.String()
	assertQueueLog(t, logs, "telegram: reply queue storage error reason=remove")
	if strings.Contains(logs, "telegram: reply queue delivered") {
		t.Fatalf("failed unlink claimed delivery removal: %q", logs)
	}

	// A later sweep retries the unlink and only then reports delivery.
	f.queue.removeFile = os.Remove
	f.drain(t)
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("records after unlink recovery = %d, want 0", len(headers))
	}
	assertQueueLog(t, f.logs.String(), "telegram: reply queue delivered chunks=1")
}

func TestReplyQueueStorageFaultDuringProgressKeepsCursor(t *testing.T) {
	longText := strings.Repeat("d", markdownfmt.TelegramMaxVisiblePerMessage) + "tail"
	chunks := markdownfmt.TelegramChunks(longText)
	if len(chunks) != 2 {
		t.Fatal("fixture must produce two chunks")
	}
	f := newReplyQueueFixture(t)
	f.sender.fn = func(_ int, send recordedReplySend) error {
		if send.text == chunks[1] {
			// The second chunk never confirms, so the first chunk's ack is
			// the only persisted progress.
			return errors.New("provider failure")
		}
		return nil
	}
	f.enqueue(t, "TG-7", longText, 0)
	f.drain(t)
	headers := f.records(t)
	if len(headers) != 1 || headers[0].record.Acked != 1 {
		t.Fatalf("record after partial progress = %#v, want acked:1", headers)
	}
	// A fresh generation resumes exactly at the unacknowledged chunk.
	resumed := newReplyQueueFixtureIn(t, f.dir, f.clock)
	resumed.sender.fn = nil
	resumed.clock.Advance(30 * time.Second)
	resumed.drain(t)
	sends := resumed.sender.snapshot()
	if len(sends) != 1 || sends[0].text != chunks[1] {
		t.Fatalf("resumed sends = %#v, want only the final chunk", sends)
	}
}

func TestReplyQueueConcurrentRoutesDoNotBlock(t *testing.T) {
	f := newReplyQueueFixture(t)
	release := make(chan struct{})
	f.sender.fn = func(_ int, send recordedReplySend) error {
		if send.route.Conversation() == "TG-1" {
			<-release
		}
		return nil
	}
	f.enqueue(t, "TG-1", "slow route", 0)
	f.enqueue(t, "TG-2", "fast route", 0)
	f.queue.sweep(context.Background())
	waitForCondition(t, "the fast route to finish while the slow route is blocked", func() bool {
		if f.sender.count() < 2 {
			return false
		}
		for _, header := range f.records(t) {
			if header.record.Conversation == "TG-2" {
				return false
			}
		}
		return true
	})
	close(release)
	f.queue.waitIdle()
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("records after both routes delivered = %d, want 0", len(headers))
	}
}

func TestReplyQueuePreservesRouteFIFO(t *testing.T) {
	f := newReplyQueueFixture(t)
	f.enqueue(t, "TG-7", "first", 0)
	f.enqueue(t, "TG-7", "second", 0)
	f.queue.sweep(context.Background())
	f.queue.waitIdle()
	sends := f.sender.snapshot()
	if len(sends) != 1 {
		t.Fatalf("first sweep attempts = %d, want 1 (one route at a time)", len(sends))
	}
	f.drain(t)
	sends = f.sender.snapshot()
	if len(sends) != 2 {
		t.Fatalf("attempts after second sweep = %d, want 2", len(sends))
	}
	if sends[0].text != markdownfmt.TelegramChunks("first")[0] || sends[1].text != markdownfmt.TelegramChunks("second")[0] {
		t.Fatalf("FIFO order violated: %#v", sends)
	}
}

// TestReplyQueueTakeoverKeepsClaimedHeadAheadOfYoungerReplies proves that a
// cross-process takeover never lets a younger same-route reply overtake the
// claimed head, and that stopped or expired heads release the route once
// their state is resolved.
func TestReplyQueueTakeoverKeepsClaimedHeadAheadOfYoungerReplies(t *testing.T) {
	t.Run("live claim blocks the younger reply until reclaimed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "telegram-replies")
		clock := newReplyQueueClock()
		election := newFakeElection("gen-a")
		first := newReplyQueueFixtureIn(t, dir, clock)
		first.queue.owner = "gen-a"
		first.queue.guard = election.Guard("gen-a")
		first.sender.fn = func(int, recordedReplySend) error {
			// Ownership moves on while the head attempt is still in flight, so
			// the first generation can neither finish nor persist its failure.
			election.SetOwner("gen-b")
			return errors.New("crash")
		}
		first.enqueue(t, "TG-7", "older reply", 0)
		first.enqueue(t, "TG-7", "younger reply", 0)
		first.oneSweep(t)
		if first.sender.count() != 1 {
			t.Fatalf("first generation sends = %d, want only the claimed head", first.sender.count())
		}

		second := newReplyQueueFixtureIn(t, dir, clock)
		second.queue.owner = "gen-b"
		second.queue.guard = election.Guard("gen-b")
		second.oneSweep(t)
		if second.sender.count() != 0 {
			t.Fatalf("successor dispatched the younger reply behind a live claim: %#v", second.sender.snapshot())
		}

		clock.Advance(replyQueueClaimDuration + time.Second)
		second.drain(t)
		sends := second.sender.snapshot()
		if len(sends) != 2 {
			t.Fatalf("successor sends = %d, want the head then the younger reply", len(sends))
		}
		if sends[0].text != markdownfmt.TelegramChunks("older reply")[0] || sends[1].text != markdownfmt.TelegramChunks("younger reply")[0] {
			t.Fatalf("post-takeover order = %#v, want the head before the younger reply", sends)
		}
		if first.sender.count() != 1 {
			t.Fatalf("old generation sent after losing ownership: %d", first.sender.count())
		}
		if headers := second.records(t); len(headers) != 0 {
			t.Fatalf("records after successor delivery = %#v, want none", headers)
		}
	})

	t.Run("stopped head never blocks the younger reply", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(call int, _ recordedReplySend) error {
			if call == 1 {
				return &telegramAPIError{method: "sendMessage", code: http.StatusBadRequest, description: "provider refusal"}
			}
			return nil
		}
		f.enqueue(t, "TG-7", "older reply", 0)
		f.enqueue(t, "TG-7", "younger reply", 0)
		f.drain(t)
		sends := f.sender.snapshot()
		if len(sends) != 2 {
			t.Fatalf("sends = %d, want the stopped head plus the younger reply", len(sends))
		}
		if sends[1].text != markdownfmt.TelegramChunks("younger reply")[0] {
			t.Fatalf("younger reply = %#v, want delivery after the stopped head", sends[1])
		}
		headers := f.records(t)
		if len(headers) != 1 || headers[0].record.StopReason != replyStopPermanent {
			t.Fatalf("records after stopped head = %#v, want only the retained permanent head", headers)
		}
	})

	t.Run("expired head cleanup releases the younger reply", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.enqueue(t, "TG-7", "older reply", 0)
		f.clock.Advance(replyQueueExpiry + time.Second)
		f.enqueue(t, "TG-7", "younger reply", 0)
		f.drain(t)
		sends := f.sender.snapshot()
		if len(sends) != 1 {
			t.Fatalf("sends = %d, want only the younger reply after head expiry", len(sends))
		}
		if sends[0].text != markdownfmt.TelegramChunks("younger reply")[0] {
			t.Fatalf("delivered reply = %#v, want the younger reply", sends[0])
		}
		if headers := f.records(t); len(headers) != 0 {
			t.Fatalf("records after expiry = %#v, want none", headers)
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue expired")
	})

	t.Run("expired record with a live claim still blocks until the claim clears", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.enqueue(t, "TG-7", "older reply", 0)
		f.enqueue(t, "TG-7", "younger reply", 0)
		headers := f.records(t)
		older := headers[0]
		if older.record.Conversation != "TG-7" || older.record.Acked != 0 {
			t.Fatalf("unexpected head record %#v", older.record)
		}
		// Simulate a generation that died mid-attempt just before the record
		// expiry: the claim outlives the record's expiry window.
		older.record.Revision++
		older.record.ExpiresAt = f.clock.Now().Add(time.Second)
		older.record.Attempts = 1
		older.record.Claim = &replyClaim{
			Owner:            "gen-a",
			Nonce:            "0000000000000001",
			Until:            f.clock.Now().Add(replyQueueClaimDuration),
			PreviousAttempts: 0,
		}
		writeRawRecord(t, f.dir, older.name, older.record)
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("successor bypassed a claimed head: %#v", f.sender.snapshot())
		}
		f.clock.Advance(2 * time.Second)
		f.oneSweep(t)
		if f.sender.count() != 0 {
			t.Fatalf("expired head released the route before its claim cleared: %#v", f.sender.snapshot())
		}
		f.clock.Advance(replyQueueClaimDuration)
		f.drain(t)
		sends := f.sender.snapshot()
		if len(sends) != 1 || sends[0].text != markdownfmt.TelegramChunks("younger reply")[0] {
			t.Fatalf("sends after claim clearing = %#v, want the younger reply only", sends)
		}
		if headers := f.records(t); len(headers) != 0 {
			t.Fatalf("records after cleanup = %#v, want none", headers)
		}
	})
}

// TestReplyQueueRoundDeadlineSpendsOnlyExpiredAttempts proves one reserved
// delivery round shares the ordinary text budget: expiry spends the attempt
// and schedules the next round from the failure, while owner cancellation
// refunds the unspent reservation.
func TestReplyQueueRoundDeadlineSpendsOnlyExpiredAttempts(t *testing.T) {
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	t.Run("deadline expiry spends the round and schedules after completion", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(call int, _ recordedReplySend) error {
			if call == 1 {
				f.clock.Advance(replyQueueRoundBudget + time.Second)
				return &telegramTransportError{err: errors.New("redacted timeout")}
			}
			return nil
		}
		f.enqueue(t, "TG-7", "deadline reply", 0)
		f.drain(t)
		headers := f.records(t)
		if len(headers) != 1 {
			t.Fatalf("records after deadline = %d, want 1", len(headers))
		}
		record := headers[0].record
		if record.Attempts != 1 || record.Stopped {
			t.Fatalf("record after deadline = %#v, want one spent retryable attempt", record)
		}
		wantNext := start.Add(replyQueueRoundBudget + time.Second + 30*time.Second)
		if !record.NextAttempt.Equal(wantNext) {
			t.Fatalf("next attempt = %s, want %s", record.NextAttempt, wantNext)
		}
		f.clock.Advance(29 * time.Second)
		f.drain(t)
		if f.sender.count() != 1 {
			t.Fatalf("retry fired before the post-deadline delay: %d sends", f.sender.count())
		}
		f.clock.Advance(time.Second)
		f.drain(t)
		if f.sender.count() != 2 {
			t.Fatalf("attempts after the scheduled round = %d, want 2", f.sender.count())
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue retry round=1")
	})

	t.Run("no new chunk starts after the deadline", func(t *testing.T) {
		longText := strings.Repeat("e", markdownfmt.TelegramMaxVisiblePerMessage) + "tail"
		chunks := markdownfmt.TelegramChunks(longText)
		if len(chunks) != 2 {
			t.Fatal("fixture must produce two chunks")
		}
		f := newReplyQueueFixture(t)
		f.sender.fn = func(call int, _ recordedReplySend) error {
			if call == 1 {
				f.clock.Advance(replyQueueRoundBudget + time.Second)
			}
			return nil
		}
		f.enqueue(t, "TG-7", longText, 0)
		f.drain(t)
		sends := f.sender.snapshot()
		if len(sends) != 1 || sends[0].text != chunks[0] {
			t.Fatalf("sends after the deadline = %#v, want only the pre-deadline chunk", sends)
		}
		headers := f.records(t)
		if len(headers) != 1 || headers[0].record.Acked != 1 || headers[0].record.Attempts != 1 {
			t.Fatalf("record after the deadline = %#v, want acked:1 with one spent attempt", headers)
		}
	})

	t.Run("owner cancellation refunds the round", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		f.sender.fn = func(int, recordedReplySend) error {
			cancel()
			return context.Canceled
		}
		f.enqueue(t, "TG-7", "cancelled reply", 0)
		f.queue.sweep(ctx)
		f.queue.waitIdle()
		headers := f.records(t)
		if len(headers) != 1 {
			t.Fatalf("records after cancellation = %d, want 1", len(headers))
		}
		record := headers[0].record
		if record.Attempts != 0 || record.Claim != nil || record.Stopped {
			t.Fatalf("record after owner cancellation = %#v, want an unspent refunded reservation", record)
		}
		if !record.NextAttempt.IsZero() {
			t.Fatalf("refunded next attempt = %s, want the previous unset value", record.NextAttempt)
		}
		assertQueueLog(t, f.logs.String(), "telegram: reply queue suppressed reason=suspended")
	})
}

// TestReplyQueueRetryDelayStartsAfterFailure proves the scheduled wait is
// measured from the failed attempt's completion instead of its reservation,
// so a slow attempt never shortens its own backoff, and that a provider
// retry_after is honored only when it lands later than that wait.
func TestReplyQueueRetryDelayStartsAfterFailure(t *testing.T) {
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	t.Run("slow transport failure waits from completion", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(call int, _ recordedReplySend) error {
			if call == 1 {
				f.clock.Advance(40 * time.Second)
				return &telegramTransportError{err: errors.New("redacted timeout")}
			}
			return nil
		}
		f.enqueue(t, "TG-7", "slow failure", 0)
		f.drain(t)
		headers := f.records(t)
		if len(headers) != 1 {
			t.Fatalf("records after slow failure = %d, want 1", len(headers))
		}
		wantNext := start.Add(70 * time.Second)
		if got := headers[0].record.NextAttempt; !got.Equal(wantNext) {
			t.Fatalf("next attempt = %s, want %s (completion + the 30s first delay)", got, wantNext)
		}
		// The old reservation-relative schedule would have retried at start+30s.
		f.clock.Advance(29 * time.Second)
		f.drain(t)
		if f.sender.count() != 1 {
			t.Fatalf("retry fired before completion + delay: %d sends", f.sender.count())
		}
		f.clock.Advance(time.Second)
		f.drain(t)
		if f.sender.count() != 2 {
			t.Fatalf("retry after completion + delay = %d sends, want 2", f.sender.count())
		}
		if headers := f.records(t); len(headers) != 0 {
			t.Fatalf("records after recovery = %#v, want none", headers)
		}
	})

	t.Run("bounded 429 waits from completion and honors the later delay", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(call int, _ recordedReplySend) error {
			if call == 1 {
				f.clock.Advance(40 * time.Second)
				return &telegramAPIError{method: "sendMessage", code: http.StatusTooManyRequests, retryAfter: 45 * time.Second}
			}
			return nil
		}
		f.enqueue(t, "TG-7", "slow rate limit", 0)
		f.drain(t)
		headers := f.records(t)
		if len(headers) != 1 {
			t.Fatalf("records after 429 = %d, want 1", len(headers))
		}
		wantNext := start.Add(time.Minute + 25*time.Second) // completion + the later 45s retry_after
		if got := headers[0].record.NextAttempt; !got.Equal(wantNext) {
			t.Fatalf("next attempt = %s, want %s", got, wantNext)
		}
		f.clock.Advance(44 * time.Second)
		f.drain(t)
		if f.sender.count() != 1 {
			t.Fatalf("retry fired before the provider retry_after: %d sends", f.sender.count())
		}
		f.clock.Advance(time.Second)
		f.drain(t)
		if f.sender.count() != 2 {
			t.Fatalf("retry after retry_after = %d sends, want 2", f.sender.count())
		}
	})

	t.Run("short retry_after yields to the scheduled round", func(t *testing.T) {
		f := newReplyQueueFixture(t)
		f.sender.fn = func(call int, _ recordedReplySend) error {
			if call == 1 {
				f.clock.Advance(40 * time.Second)
				return &telegramAPIError{method: "sendMessage", code: http.StatusTooManyRequests, retryAfter: 10 * time.Second}
			}
			return nil
		}
		f.enqueue(t, "TG-7", "short rate limit", 0)
		f.drain(t)
		headers := f.records(t)
		if len(headers) != 1 {
			t.Fatalf("records after 429 = %d, want 1", len(headers))
		}
		wantNext := start.Add(70 * time.Second) // completion + the later 30s scheduled round
		if got := headers[0].record.NextAttempt; !got.Equal(wantNext) {
			t.Fatalf("next attempt = %s, want %s", got, wantNext)
		}
		f.clock.Advance(29 * time.Second)
		f.drain(t)
		if f.sender.count() != 1 {
			t.Fatalf("short retry_after fired early: %d sends", f.sender.count())
		}
		f.clock.Advance(time.Second)
		f.drain(t)
		if f.sender.count() != 2 {
			t.Fatalf("scheduled round after slow 429 = %d sends, want 2", f.sender.count())
		}
	})
}

func TestReplyQueueWorkerLoopsAndJoins(t *testing.T) {
	f := newReplyQueueFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.queue.run(ctx)
	}()
	f.enqueue(t, "TG-7", "worker reply", 0)
	waitForCondition(t, "the worker to deliver the attached reply", func() bool {
		return f.sender.count() >= 1 && len(f.records(t)) == 0
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reply queue worker did not join after cancellation")
	}
}

// The following tests exercise the adapter-facing integration through the
// real Bot transport boundary.

func newQueuedBot(t *testing.T, probe *telegramSendProbe, allowed []string, directory string, clock *replyQueueClock) (*Bot, *ReplyWorker) {
	t.Helper()
	bot := New(config.Telegram{AllowedUsers: allowed, GroupMode: "all", PollTimeoutSec: 30}, "SECRET-TOKEN")
	bot.client.Transport = telegramRoundTripFunc(probe.roundTrip)
	bot.me = telegramUser{ID: 42, Username: "spynel_bot"}
	worker := NewReplyWorker(directory, "test-owner", nil, nil)
	worker.queue.now = clock.Now
	worker.queue.newNonce = nonceGenerator()
	bot.AttachReplyWorker(worker)
	worker.queue.attachBot(context.Background(), bot)
	return bot, worker
}

func TestTelegramQueuedFinalBypassesNestedTransportRetry(t *testing.T) {
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return nil, transientTransportFailure("reset")
	}}
	clock := newReplyQueueClock()
	var logs strings.Builder
	bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
	worker.queue.logf = func(line string) { logs.WriteString(line + "\n") }
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.deliverFinal(context.Background(), route, "queued reply text", 41, telegramUser{ID: 7}); err != nil {
		t.Fatalf("deliverFinal: %v", err)
	}
	worker.queue.sweep(context.Background())
	worker.queue.waitIdle()
	calls, _ := probe.snapshot()
	if calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (no nested transport retry)", calls)
	}
	headers, err := worker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || headers[0].record.Attempts != 1 || headers[0].record.Stopped {
		t.Fatalf("record after failed attempt = %#v, want attempts:1 retryable", headers)
	}
	assertQueueLog(t, logs.String(), "telegram: reply queue retry round=1")
	assertQueueLogAbsent(t, logs.String(), "queued reply text", "SECRET-TOKEN", "TG-7")
}

func TestTelegramQueuedFinalPreservesDecoratedText(t *testing.T) {
	const piSession = "11111111-2222-3333-4444-555555555555"
	notice := "Pi session `" + piSession + "`."
	body := strings.Repeat("Telegram rich response line\n\n", 400)
	full := notice + "\n\n" + body
	chunks := markdownfmt.TelegramChunks(full)
	if len(chunks) < 2 {
		t.Fatalf("expected a multi-chunk reply, got %d chunks", len(chunks))
	}
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return telegramOKResponse(), nil
	}}
	clock := newReplyQueueClock()
	bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.deliverFinal(context.Background(), route, full, 41, telegramUser{ID: 7}); err != nil {
		t.Fatalf("deliverFinal: %v", err)
	}
	headers, err := worker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 {
		t.Fatalf("records = %d, want 1", len(headers))
	}
	stored := headers[0].record
	if stored.BotID != 42 {
		t.Fatalf("stored bot ID = %d, want the verified account 42", stored.BotID)
	}
	if len(stored.HTML) != len(chunks) {
		t.Fatalf("stored chunks = %d, want %d", len(stored.HTML), len(chunks))
	}
	for index := range chunks {
		if stored.HTML[index] != chunks[index] {
			t.Fatalf("stored chunk %d changed the decorated text", index)
		}
		if stored.Plain[index] != markdownfmt.TelegramChunkPlainText(chunks[index]) {
			t.Fatalf("stored plain chunk %d changed the decorated text", index)
		}
	}
	if first := markdownfmt.TelegramChunkPlainText(stored.HTML[0]); !strings.HasPrefix(first, "Pi session "+piSession+".") {
		t.Fatalf("first stored chunk lost the leading Pi notice: %q", first)
	}
	worker.queue.sweep(context.Background())
	worker.queue.waitIdle()
	_, payloads := probe.snapshot()
	if len(payloads) != len(chunks) {
		t.Fatalf("delivered chunks = %d, want %d", len(payloads), len(chunks))
	}
	for index, payload := range payloads {
		if payload["text"] != chunks[index] {
			t.Fatalf("delivered chunk %d = %#v, want %q", index, payload["text"], chunks[index])
		}
	}
	headers, err = worker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 0 {
		t.Fatalf("records after delivery = %d, want 0", len(headers))
	}
}

func TestTelegramQueuedFinalRevalidatesGroupSenderAndPolicy(t *testing.T) {
	allowed := []string{"7"}
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return telegramOKResponse(), nil
	}}
	clock := newReplyQueueClock()
	var logs strings.Builder
	bot, worker := newQueuedBot(t, probe, allowed, filepath.Join(t.TempDir(), "telegram-replies"), clock)
	worker.queue.logf = func(line string) { logs.WriteString(line + "\n") }
	ctx := context.Background()
	message := &telegramMessage{MessageID: 5, From: telegramUser{ID: 7, Username: "alice"}, Chat: telegramChat{ID: -100, Type: "supergroup"}, Text: "/diag"}
	bot.handle(ctx, func(_ context.Context, _ core.Message, emit core.Emit) error {
		finalText := "group reply"
		emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText})
		return nil
	}, message)
	if calls, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("terminal reply bypassed the queue: %d calls", calls)
	}

	// The original group sender lost authorization: fail closed, permanently
	// suppress, and never spend an attempt.
	bot.SetAllowedUsersSource(func() []string { return []string{"8"} })
	worker.queue.sweep(ctx)
	worker.queue.waitIdle()
	if calls, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("revoked group sender produced %d calls", calls)
	}
	headers, err := worker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || !headers[0].record.Stopped || headers[0].record.StopReason != replyStopSuppressed {
		t.Fatalf("record after sender revocation = %#v, want suppressed", headers)
	}

	// Even restored authorization and policy never resume a suppressed
	// record; it waits for expiry.
	bot.SetAllowedUsersSource(func() []string { return []string{"7"} })
	bot.config.GroupMode = "off"
	worker.queue.sweep(ctx)
	worker.queue.waitIdle()
	if calls, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("disabled group policy produced %d calls", calls)
	}
	bot.config.GroupMode = "all"
	worker.queue.sweep(ctx)
	worker.queue.waitIdle()
	if calls, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("suppressed group reply resumed: %d calls", calls)
	}
	assertQueueLog(t, logs.String(), "telegram: reply queue suppressed reason=authorization")
	assertQueueLogAbsent(t, logs.String(), "group reply", "-100", "alice", "SECRET-TOKEN")
}

func TestTelegramQueuedFinalSurvivesBotReplacement(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "telegram-replies")
	clock := newReplyQueueClock()
	failing := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return nil, transientTransportFailure("reset")
	}}
	first, firstWorker := newQueuedBot(t, failing, []string{"7"}, directory, clock)
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.deliverFinal(context.Background(), route, "replacement-safe reply", 41, telegramUser{ID: 7}); err != nil {
		t.Fatalf("deliverFinal: %v", err)
	}
	firstWorker.queue.sweep(context.Background())
	firstWorker.queue.waitIdle()
	if calls, _ := failing.snapshot(); calls != 1 {
		t.Fatalf("first generation calls = %d, want 1", calls)
	}

	// A replacement bot generation for the same verified account resumes the
	// same record after its persisted next-attempt time.
	clock.Advance(time.Minute)
	succeeding := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return telegramOKResponse(), nil
	}}
	_, secondWorker := newQueuedBot(t, succeeding, []string{"7"}, directory, clock)
	secondWorker.queue.sweep(context.Background())
	secondWorker.queue.waitIdle()
	if calls, _ := succeeding.snapshot(); calls != 1 {
		t.Fatalf("replacement calls = %d, want 1", calls)
	}
	headers, err := secondWorker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 0 {
		t.Fatalf("records after replacement delivery = %d, want 0", len(headers))
	}
	_, payloads := succeeding.snapshot()
	if len(payloads) != 1 {
		t.Fatalf("replacement payloads = %d, want 1", len(payloads))
	}
	if payloads[0]["chat_id"] != "7" {
		t.Fatalf("replacement chat_id = %#v, want 7", payloads[0]["chat_id"])
	}
	reply, ok := payloads[0]["reply_parameters"].(map[string]any)
	if !ok || reply["message_id"] != float64(41) {
		t.Fatalf("replacement reply anchor = %#v, want 41", payloads[0]["reply_parameters"])
	}
}

func TestTelegramQueuedFinalPreservesTopicThread(t *testing.T) {
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return telegramOKResponse(), nil
	}}
	clock := newReplyQueueClock()
	bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
	route, err := ParseConversation("TG-7-topic-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.deliverFinal(context.Background(), route, "topic reply", 41, telegramUser{ID: 7}); err != nil {
		t.Fatalf("deliverFinal: %v", err)
	}
	worker.queue.sweep(context.Background())
	worker.queue.waitIdle()
	_, payloads := probe.snapshot()
	if len(payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(payloads))
	}
	if payloads[0]["message_thread_id"] != float64(5) {
		t.Fatalf("queued topic delivery thread = %#v, want 5", payloads[0]["message_thread_id"])
	}
	reply, ok := payloads[0]["reply_parameters"].(map[string]any)
	if !ok || reply["message_id"] != float64(41) {
		t.Fatalf("queued topic reply anchor = %#v, want 41", payloads[0]["reply_parameters"])
	}
}

func TestTelegramAttachmentsAreNotQueued(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := &telegramSendProbe{respond: func(_ int, payload map[string]any) (*http.Response, error) {
		if _, hasText := payload["text"]; hasText {
			return nil, errors.New("the final text must not be sent directly")
		}
		return telegramOKResponse(), nil
	}}
	clock := newReplyQueueClock()
	bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
	bot.handle(context.Background(), func(_ context.Context, _ core.Message, emit core.Emit) error {
		finalText := "delayed text"
		emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText, Attachments: []core.OutboundAttachment{{
			Kind: "attachment", Name: "report.txt", Path: path, MediaType: "text/plain", MaxBytes: 1024,
		}}})
		return nil
	}, privateMessage(8, "/attachment"))
	if calls, _ := probe.snapshot(); calls != 1 {
		t.Fatalf("provider calls = %d, want 1 attachment upload before the queued text", calls)
	}
	headers, err := worker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || headers[0].record.HTML[0] != "delayed text" {
		t.Fatalf("queued final text = %#v, want exactly one record", headers)
	}
}

func TestTelegramTerminalAndHandlerFailuresAreQueued(t *testing.T) {
	tests := []struct {
		name    string
		message *telegramMessage
		handler func(context.Context, core.Message, core.Emit) error
	}{
		{
			name:    "terminal final text",
			message: privateMessage(8, "/final"),
			handler: func(_ context.Context, _ core.Message, emit core.Emit) error {
				finalText := "terminal answer"
				emit(core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText})
				return nil
			},
		},
		{
			name:    "handler failure",
			message: privateMessage(8, "/failure"),
			handler: func(context.Context, core.Message, core.Emit) error {
				return errors.New("handler exploded")
			},
		},
		{
			name: "attachment preparation failure",
			message: func() *telegramMessage {
				message := privateMessage(8, "/attachment")
				message.Document = &telegramDocument{FileID: "file-id", FileUniqueID: "unique", FileName: "report.pdf"}
				return message
			}(),
			handler: func(context.Context, core.Message, core.Emit) error { return nil },
		},
		{
			name:    "attachment delivery failure",
			message: privateMessage(8, "/attachment-delivery"),
			handler: func(_ context.Context, _ core.Message, emit core.Emit) error {
				emit(core.Event{Kind: core.EventFinal, Done: true, Attachments: []core.OutboundAttachment{{
					Kind: "attachment", Name: "missing.txt", Path: filepath.Join(t.TempDir(), "missing.txt"), MediaType: "text/plain", MaxBytes: 1024,
				}}})
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
				return nil, errors.New("provider must not be reached before a queued sweep")
			}}
			clock := newReplyQueueClock()
			bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
			wantReplyTo := test.message.MessageID
			bot.handle(context.Background(), test.handler, test.message)
			if calls, _ := probe.snapshot(); calls != 0 {
				t.Fatalf("terminal failure was delivered directly: %d calls", calls)
			}
			headers, err := worker.queue.scan()
			if err != nil {
				t.Fatal(err)
			}
			if len(headers) != 1 {
				t.Fatalf("queued records = %d, want 1", len(headers))
			}
			if headers[0].record.Conversation != "TG-7" || headers[0].record.ReplyTo != wantReplyTo {
				t.Fatalf("queued record = %#v, want TG-7 anchored at %d", headers[0].record, wantReplyTo)
			}
		})
	}
}

func TestTelegramProactiveDeliveryStaysNonDurable(t *testing.T) {
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return nil, transientTransportFailure("reset")
	}}
	clock := newReplyQueueClock()
	bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
	bot.retryWait = func(context.Context, time.Duration) error { return nil }
	if err := bot.Deliver(context.Background(), "TG-7", "event", "proactive message"); err == nil {
		t.Fatal("failed proactive delivery reported success")
	}
	finalText := "proactive final"
	if err := bot.DeliverEvent(context.Background(), "TG-7", "event", core.Event{Kind: core.EventFinal, Done: true, FinalText: &finalText}); err == nil {
		t.Fatal("failed proactive event reported success")
	}
	if calls, _ := probe.snapshot(); calls != 6 {
		t.Fatalf("proactive calls = %d, want 6 ordinary retried sends (3 per message)", calls)
	}
	headers, err := worker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 0 {
		t.Fatalf("proactive delivery entered the durable queue: %d records", len(headers))
	}
}

func TestTelegramQueueRefusalNeverDirectSends(t *testing.T) {
	respond := func(int, map[string]any) (*http.Response, error) {
		return telegramOKResponse(), nil
	}
	t.Run("capacity", func(t *testing.T) {
		probe := &telegramSendProbe{respond: respond}
		clock := newReplyQueueClock()
		bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
		for index := 0; index < replyQueueMaxPerRoute; index++ {
			if err := worker.queue.enqueue(queuedRecordForTest(bot, "TG-7", fmt.Sprintf("full %d", index))); err != nil {
				t.Fatal(err)
			}
		}
		route, err := ParseConversation("TG-7")
		if err != nil {
			t.Fatal(err)
		}
		if err := bot.deliverFinal(context.Background(), route, "overflow reply", 0, telegramUser{ID: 7}); !errors.Is(err, errReplyQueueFull) {
			t.Fatalf("capacity refusal error = %v, want errReplyQueueFull", err)
		}
		if calls, _ := probe.snapshot(); calls != 0 {
			t.Fatalf("capacity refusal produced %d provider sends", calls)
		}
	})

	t.Run("storage", func(t *testing.T) {
		probe := &telegramSendProbe{respond: respond}
		clock := newReplyQueueClock()
		root := filepath.Join(t.TempDir(), "telegram-replies")
		if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		bot, _ := newQueuedBot(t, probe, []string{"7"}, root, clock)
		route, err := ParseConversation("TG-7")
		if err != nil {
			t.Fatal(err)
		}
		if err := bot.deliverFinal(context.Background(), route, "storage reply", 0, telegramUser{ID: 7}); !errors.Is(err, errReplyQueueStorage) {
			t.Fatalf("storage refusal error = %v, want errReplyQueueStorage", err)
		}
		if calls, _ := probe.snapshot(); calls != 0 {
			t.Fatalf("storage refusal produced %d provider sends", calls)
		}
	})

	t.Run("ownership", func(t *testing.T) {
		probe := &telegramSendProbe{respond: respond}
		clock := newReplyQueueClock()
		bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
		worker.queue.guard = func(func() error) (bool, error) { return false, nil }
		route, err := ParseConversation("TG-7")
		if err != nil {
			t.Fatal(err)
		}
		if err := bot.deliverFinal(context.Background(), route, "ownership reply", 0, telegramUser{ID: 7}); !errors.Is(err, errReplyQueueOwnership) {
			t.Fatalf("ownership refusal error = %v, want errReplyQueueOwnership", err)
		}
		if calls, _ := probe.snapshot(); calls != 0 {
			t.Fatalf("ownership refusal produced %d provider sends", calls)
		}
	})

	t.Run("unverified bot", func(t *testing.T) {
		probe := &telegramSendProbe{respond: respond}
		clock := newReplyQueueClock()
		bot, worker := newQueuedBot(t, probe, []string{"7"}, filepath.Join(t.TempDir(), "telegram-replies"), clock)
		bot.me = telegramUser{}
		worker.queue.detachBot(bot)
		route, err := ParseConversation("TG-7")
		if err != nil {
			t.Fatal(err)
		}
		if err := bot.deliverFinal(context.Background(), route, "unverified reply", 0, telegramUser{ID: 7}); err == nil {
			t.Fatal("unverified bot accepted a durable final reply")
		}
		if calls, _ := probe.snapshot(); calls != 0 {
			t.Fatalf("unverified refusal produced %d provider sends", calls)
		}
		headers, err := worker.queue.scan()
		if err != nil {
			t.Fatal(err)
		}
		if len(headers) != 0 {
			t.Fatalf("unverified bot stored %d records", len(headers))
		}
	})
}

// TestTelegramQueuedFinalUsesLiveAllowedUsersSource proves the production
// wiring: a bot whose allowed users come from a live source authorizes
// queued sends against the current value rather than its construction
// snapshot.
func TestTelegramQueuedFinalUsesLiveAllowedUsersSource(t *testing.T) {
	allowed := []string{"7"}
	probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
		return telegramOKResponse(), nil
	}}
	clock := newReplyQueueClock()
	bot := New(config.Telegram{GroupMode: "all"}, "SECRET-TOKEN")
	bot.client.Transport = telegramRoundTripFunc(probe.roundTrip)
	bot.me = telegramUser{ID: 42, Username: "spynel_bot"}
	bot.SetAllowedUsersSource(func() []string { return allowed })
	worker := NewReplyWorker(filepath.Join(t.TempDir(), "telegram-replies"), "test-owner", nil, nil)
	worker.queue.now = clock.Now
	worker.queue.newNonce = nonceGenerator()
	bot.AttachReplyWorker(worker)
	worker.queue.attachBot(context.Background(), bot)
	route, err := ParseConversation("TG-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := bot.deliverFinal(context.Background(), route, "live source reply", 0, telegramUser{ID: 7}); err != nil {
		t.Fatalf("deliverFinal: %v", err)
	}
	allowed = []string{"8"}
	worker.queue.sweep(context.Background())
	worker.queue.waitIdle()
	if calls, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("revoked live source produced %d calls", calls)
	}
	headers, err := worker.queue.scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || !headers[0].record.Stopped || headers[0].record.StopReason != replyStopSuppressed {
		t.Fatalf("record after live source revocation = %#v, want suppressed", headers)
	}
	allowed = []string{"7"}
	worker.queue.sweep(context.Background())
	worker.queue.waitIdle()
	if calls, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("restored live source resumed a suppressed record: %d calls", calls)
	}
}

func TestTelegramBotRegistersOnlyAfterVerifiedGetMe(t *testing.T) {
	t.Run("getMe failure never registers", func(t *testing.T) {
		probe := &telegramSendProbe{respond: func(int, map[string]any) (*http.Response, error) {
			return telegramJSONResponse(http.StatusUnauthorized, `{"ok":false,"description":"Unauthorized"}`), nil
		}}
		bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 30}, "SECRET-TOKEN")
		bot.client.Transport = telegramRoundTripFunc(probe.roundTrip)
		worker := NewReplyWorker(filepath.Join(t.TempDir(), "telegram-replies"), "test-owner", nil, nil)
		bot.AttachReplyWorker(worker)
		if err := bot.Run(context.Background(), func(context.Context, core.Message, core.Emit) error { return nil }); err == nil {
			t.Fatal("Run succeeded without a verified bot identity")
		}
		if worker.queue.activeBot() != nil {
			t.Fatal("unverified bot registered with the durable worker")
		}
	})

	t.Run("registration is live during polling and removed on exit", func(t *testing.T) {
		registered := make(chan bool, 1)
		bot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 30}, "SECRET-TOKEN")
		worker := NewReplyWorker(filepath.Join(t.TempDir(), "telegram-replies"), "test-owner", nil, nil)
		bot.AttachReplyWorker(worker)
		bot.client.Transport = telegramRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := request.Context().Err(); err != nil {
				return nil, err
			}
			switch {
			case strings.HasSuffix(request.URL.Path, "/getMe"):
				return telegramJSONResponse(http.StatusOK, `{"ok":true,"result":{"id":4242,"is_bot":true,"username":"probe_bot"}}`), nil
			case strings.HasSuffix(request.URL.Path, "/setMyCommands"):
				return telegramOKResponse(), nil
			case strings.HasSuffix(request.URL.Path, "/deleteWebhook"):
				return telegramOKResponse(), nil
			default:
				select {
				case registered <- worker.queue.activeBot() != nil:
				default:
				}
				return telegramJSONResponse(http.StatusOK, `{"ok":true,"result":[]}`), nil
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- bot.Run(ctx, func(context.Context, core.Message, core.Emit) error { return nil }) }()
		select {
		case active := <-registered:
			if !active {
				cancel()
				<-done
				t.Fatal("polling ran before the verified bot registered")
			}
		case <-time.After(5 * time.Second):
			cancel()
			<-done
			t.Fatal("verified bot never reached polling")
		}
		if bot.me.ID != 4242 {
			cancel()
			<-done
			t.Fatalf("verified bot ID = %d, want 4242", bot.me.ID)
		}
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}
		if worker.queue.activeBot() != nil {
			t.Fatal("bot remained registered after Run returned")
		}
	})
}

// queuedRecordForTest builds the queue-visible record that the adapter would
// create for one bot final reply.
func queuedRecordForTest(bot *Bot, conversation, text string) replyRecord {
	chunks := markdownfmt.TelegramChunks(text)
	plain := make([]string, len(chunks))
	for index, chunk := range chunks {
		plain[index] = markdownfmt.TelegramChunkPlainText(chunk)
	}
	return replyRecord{BotID: bot.me.ID, Conversation: conversation, SenderID: 7, HTML: chunks, Plain: plain}
}

// TestReplyQueueSkipsIneligibleHeadsBeforeConcurrencySlot proves an older
// foreign-account or bot-less head never consumes one of the bounded
// concurrency slots, so a deliverable route still dispatches in the same
// sweep while per-route FIFO is preserved.
func TestReplyQueueSkipsIneligibleHeadsBeforeConcurrencySlot(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)

	f := newReplyQueueFixture(t)
	foreign := func(conversation string) {
		t.Helper()
		record := f.record(conversation, "foreign "+conversation, 0)
		record.BotID = 99
		if err := f.queue.enqueue(record); err != nil {
			t.Fatalf("enqueue foreign %s: %v", conversation, err)
		}
		f.clock.Advance(time.Millisecond)
	}
	// Four older foreign-account routes would occupy every concurrency slot
	// if sweep admitted ineligible heads.
	for _, conversation := range []string{"TG-1", "TG-2", "TG-3", "TG-4"} {
		foreign(conversation)
	}
	// A current-account record behind a foreign head stays FIFO-blocked on
	// its route and must not be dispatched either.
	foreign("TG-5")
	f.enqueue(t, "TG-5", "current behind foreign head", 0)
	f.enqueue(t, "TG-7", "current route", 0)

	f.oneSweep(t)
	sends := f.sender.snapshot()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want only the deliverable route", len(sends))
	}
	if got := sends[0].route.Conversation(); got != "TG-7" {
		t.Fatalf("delivered route = %q, want TG-7", got)
	}
	headers := f.records(t)
	if len(headers) != 6 {
		t.Fatalf("records after sweep = %d, want four foreign routes plus the foreign head and its follower", len(headers))
	}
	for _, header := range headers {
		if header.record.Attempts != 0 || header.record.Claim != nil {
			t.Fatalf("ineligible record spent delivery state: %#v", header.record)
		}
	}
}

// TestReplyQueueBotGenerationProtectsReplacement proves the queue refuses a
// stale or cancelled registration and that an older generation's detach can
// never clear a newer replacement.
func TestReplyQueueBotGenerationProtectsReplacement(t *testing.T) {
	q := newReplyQueue(filepath.Join(t.TempDir(), "telegram-replies"))
	q.now = newReplyQueueClock().Now
	q.newNonce = nonceGenerator()
	older := &fakeReplyBot{id: 11, gen: 1, sender: &replyQueueSender{}}
	newer := &fakeReplyBot{id: 22, gen: 2, sender: &replyQueueSender{}}

	if !q.attachBot(context.Background(), older) {
		t.Fatal("initial generation was refused")
	}
	if !q.attachBot(context.Background(), newer) {
		t.Fatal("replacement generation was refused")
	}
	if got := q.activeBot(); got != newer {
		t.Fatalf("active bot = %#v, want the replacement", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if q.attachBot(cancelled, older) {
		t.Fatal("a cancelled stale generation registered over the replacement")
	}
	if got := q.activeBot(); got != newer {
		t.Fatalf("active bot after stale attach = %#v, want the replacement", got)
	}
	q.detachBot(older)
	if got := q.activeBot(); got != newer {
		t.Fatalf("stale detach removed the replacement: %#v", got)
	}
	q.detachBot(newer)
	if got := q.activeBot(); got != nil {
		t.Fatalf("active bot after replacement detach = %#v, want none", got)
	}
}

// TestBotRunRefusesDelayedGetMeAfterReplacement drives the real Run boundary:
// a first run whose getMe response arrives after a replacement registered
// (and after the stale run was cancelled or revoked) must neither displace
// the replacement nor clear it on exit.
func TestBotRunRefusesDelayedGetMeAfterReplacement(t *testing.T) {
	run := func(t *testing.T, cancelStale bool) {
		t.Helper()
		worker := NewReplyWorker(filepath.Join(t.TempDir(), "telegram-replies"), "test-owner", nil, nil)
		worker.queue.newNonce = nonceGenerator()

		oldStarted := make(chan struct{})
		releaseOld := make(chan struct{})
		var startedOnce sync.Once
		transport := telegramRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			path := request.URL.Path
			switch {
			case strings.HasSuffix(path, "/getMe") && strings.HasPrefix(path, "/botold/"):
				startedOnce.Do(func() { close(oldStarted) })
				<-releaseOld
				return telegramJSONResponse(http.StatusOK, `{"ok":true,"result":{"id":11,"is_bot":true,"username":"stale_bot"}}`), nil
			case strings.HasSuffix(path, "/getMe"):
				return telegramJSONResponse(http.StatusOK, `{"ok":true,"result":{"id":22,"is_bot":true,"username":"current_bot"}}`), nil
			case strings.HasSuffix(path, "/getUpdates"):
				<-request.Context().Done()
				return nil, request.Context().Err()
			default:
				return telegramOKResponse(), nil
			}
		})

		oldBot := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 30}, "old")
		oldBot.client.Transport = transport
		oldBot.AttachReplyWorker(worker)
		replacement := New(config.Telegram{AllowedUsers: []string{"7"}, PollTimeoutSec: 30}, "new")
		replacement.client.Transport = transport
		replacement.AttachReplyWorker(worker)
		if replacement.generation() <= oldBot.generation() {
			t.Fatalf("generations = stale %d replacement %d, want the replacement to be newer", oldBot.generation(), replacement.generation())
		}

		oldCtx, cancelOld := context.WithCancel(context.Background())
		defer cancelOld()
		oldDone := make(chan error, 1)
		go func() {
			oldDone <- oldBot.Run(oldCtx, func(context.Context, core.Message, core.Emit) error { return nil })
		}()
		select {
		case <-oldStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("stale getMe never reached the transport")
		}

		newCtx, cancelNew := context.WithCancel(context.Background())
		newDone := make(chan error, 1)
		go func() {
			newDone <- replacement.Run(newCtx, func(context.Context, core.Message, core.Emit) error { return nil })
		}()
		waitForCondition(t, "replacement registration", func() bool { return worker.queue.activeBot() == replacement })

		// The stale getMe response is released only after the replacement is
		// registered, and only after the stale run was cancelled or revoked.
		if cancelStale {
			cancelOld()
		} else {
			oldBot.RevokeRuntimeAuthorization()
		}
		close(releaseOld)

		select {
		case err := <-oldDone:
			if cancelStale && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled stale Run error = %v, want context cancellation", err)
			}
			if !cancelStale && !errors.Is(err, errReplyQueueGenerationLost) {
				t.Fatalf("revoked stale Run error = %v, want a refused registration", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stale Run did not exit after its delayed getMe")
		}
		if got := worker.queue.activeBot(); got != replacement {
			t.Fatalf("active bot after delayed getMe = %#v, want the replacement", got)
		}

		cancelNew()
		select {
		case <-newDone:
		case <-time.After(5 * time.Second):
			t.Fatal("replacement Run did not exit")
		}
		waitForCondition(t, "replacement detach", func() bool { return worker.queue.activeBot() == nil })
	}

	t.Run("cancelled stale run", func(t *testing.T) { run(t, true) })
	t.Run("revoked stale run", func(t *testing.T) { run(t, false) })
}

// TestReplyQueueStopsAtRecordExpiry proves no later chunk is sent once the
// record's one-hour expiry passes mid-round, while the confirmed prefix and
// the spent round survive until the ordinary cleanup removes the record.
func TestReplyQueueStopsAtRecordExpiry(t *testing.T) {
	longText := strings.Repeat("e", markdownfmt.TelegramMaxVisiblePerMessage) + "tail"
	chunks := markdownfmt.TelegramChunks(longText)
	if len(chunks) != 2 {
		t.Fatal("fixture must produce two chunks")
	}

	f := newReplyQueueFixture(t)
	f.sender.fn = func(call int, _ recordedReplySend) error {
		if call == 1 {
			// The first acknowledgement arrives after the record expired.
			f.clock.Advance(20 * time.Second)
		}
		return nil
	}
	f.enqueue(t, "TG-7", longText, 0)
	f.clock.Advance(replyQueueExpiry - 10*time.Second) // age 59m50s

	f.oneSweep(t)
	sends := f.sender.snapshot()
	if len(sends) != 1 || sends[0].text != chunks[0] {
		t.Fatalf("sends after expiry crossing = %#v, want only the first chunk", sends)
	}
	headers := f.records(t)
	if len(headers) != 1 {
		t.Fatalf("records after expiry crossing = %d, want the retained record", len(headers))
	}
	record := headers[0].record
	if record.Acked != 1 || record.Attempts != 1 || record.Stopped || record.Claim != nil {
		t.Fatalf("record after expiry crossing = %#v, want acked:1 with one spent non-stopped attempt", record)
	}

	// The ordinary expiry sweep removes the retained record without replaying
	// or erasing the confirmed prefix.
	f.oneSweep(t)
	if headers := f.records(t); len(headers) != 0 {
		t.Fatalf("expired record survived cleanup: %#v", headers)
	}
	if f.sender.count() != 1 {
		t.Fatalf("post-expiry sends = %d, want no second chunk", f.sender.count())
	}
	assertQueueLog(t, f.logs.String(), "telegram: reply queue expired")
}
