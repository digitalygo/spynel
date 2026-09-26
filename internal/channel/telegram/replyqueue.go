package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitalygo/spynel/internal/fsx"
)

// The durable Telegram final-reply queue stores one bounded, private,
// atomically replaced record per inbound final response inside the
// workspace's .spynel/runtime/telegram-replies directory. The primary term
// owns the worker and joins it on shutdown, so bounded delivery and expiry
// cleanup survive adapter generations and run even while Telegram is disabled
// or disconnected. Delivery is at-least-once: a crash while a provider
// request is ambiguous may duplicate the chunk being sent, and the public
// contract makes no exactly-once promise.
const (
	// replyQueueVersion is the only accepted record schema. Obsolete or
	// unknown representations fail validation and are never delivered.
	replyQueueVersion = 2
	// replyQueueMaxEntries bounds the whole directory; replyQueueMaxPerRoute
	// bounds one canonical conversation; replyQueueMaxEntryBytes bounds one
	// rendered reply and replyQueueMaxTotalBytes bounds the directory size.
	replyQueueMaxEntries    = 128
	replyQueueMaxPerRoute   = 16
	replyQueueMaxTotalBytes = 16 << 20
	replyQueueMaxEntryBytes = 512 << 10
	// replyQueueExpiry deletes queued replies whose delivery could not be
	// completed in time, including those stopped by exhaustion, a permanent
	// provider refusal, or recipient revocation. Stopped records remain on
	// disk until this expiry so their state is never lost and never replayed.
	replyQueueExpiry = time.Hour
	// replyQueueMaxAttempts is the initial send plus the five scheduled
	// retry rounds in replyQueueRetryDelays.
	replyQueueMaxAttempts = 6
	// replyQueueMaxConcurrentRoutes bounds how many canonical conversations
	// may deliver at the same time. One route stays strictly FIFO.
	replyQueueMaxConcurrentRoutes = 4
	// replyQueueScanInterval is the ordinary rescan period of the worker.
	// Enqueue signals the worker immediately, so this only bounds how often
	// an idle worker looks for externally changed state.
	replyQueueScanInterval = 5 * time.Second
	// replyQueueRoundBudget bounds one reserved delivery round, including
	// every chunk, the HTML fallback, and each reauthorization. It matches
	// the ordinary text-delivery budget so a queued reply can never run
	// longer than a direct send.
	replyQueueRoundBudget = telegramTextDeliveryBudget
	// replyQueueClaimMargin is the fixed safety margin between the end of a
	// delivery round and the expiry of its cross-process claim.
	replyQueueClaimMargin = time.Minute
	// replyQueueClaimDuration bounds how long one delivery attempt may hold
	// its cross-process claim. It exceeds the complete delivery round budget
	// by the margin so a live attempt is never reclaimed while it can still
	// be sending.
	replyQueueClaimDuration = replyQueueRoundBudget + replyQueueClaimMargin
	// replyQueueMaxScanEntries bounds one directory enumeration so a hostile
	// or damaged directory cannot consume unbounded memory.
	replyQueueMaxScanEntries = 4096
	// replyQueueScanBatch bounds one ReadDir call.
	replyQueueScanBatch = 64
	// replyQueueMaxBrokenEntries bounds how many unusable entries are
	// remembered per process.
	replyQueueMaxBrokenEntries = 128
	// replyQueueNonceBytes is the width of one attempt nonce.
	replyQueueNonceBytes = 8
)

// Persisted stop reasons. A stopped record keeps its state until expiry and
// never issues another network send.
const (
	replyStopExhausted  = "exhausted"
	replyStopPermanent  = "permanent"
	replyStopSuppressed = "suppressed"
)

// replyQueueRetryDelays are the exponential waits after the initial send.
// The final scheduled round lands about 15m30s after enqueue, so an outage
// of fifteen minutes is recovered even when every attempt fails immediately.
var replyQueueRetryDelays = [...]time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	4 * time.Minute,
	8 * time.Minute,
}

var (
	// errReplyQueueFull reports that the bounded queue cannot admit one more
	// reply. A configured queue never falls back to a direct provider send.
	errReplyQueueFull = errors.New("telegram reply queue is at capacity")
	// errReplyQueueStorage reports an unusable, untrusted, or corrupt queue
	// storage boundary. The queue fails closed for delivery and mutation.
	errReplyQueueStorage = errors.New("telegram reply queue storage is unavailable")
	// errReplyQueueOwnership reports that the workspace primary election no
	// longer admits this worker's mutations.
	errReplyQueueOwnership = errors.New("telegram reply queue ownership was lost")
	// errReplyQueueSuspended lets an injected sender mark one attempt as
	// suspended. The attempt's retry reservation is rolled back and no round
	// is spent.
	errReplyQueueSuspended = errors.New("telegram reply queue attempt suspended")
	// errReplyQueueCorrupt marks a record that failed strict validation.
	errReplyQueueCorrupt = errors.New("invalid telegram reply queue record")
	// errReplyQueueSymlink marks a queue entry that is a symbolic link.
	errReplyQueueSymlink = errors.New("telegram reply queue entry is a symlink")
	// errReplyQueueSize marks a queue entry above the per-entry bound.
	errReplyQueueSize = errors.New("telegram reply queue entry exceeds its size bound")
	// errReplyQueuePermissions marks a queue entry readable by group or other.
	errReplyQueuePermissions = errors.New("telegram reply queue entry permissions are not private")
	// errReplyQueueGenerationLost marks a queue attempt refused because its
	// authenticated bot generation is no longer active. The record is
	// suspended without spending a round.
	errReplyQueueGenerationLost = errors.New("telegram reply queue generation is no longer active")
	// errReplyQueueRecipientRevoked marks a queue attempt refused because the
	// live route policy no longer admits the original recipient or group
	// sender. The record is permanently suppressed until expiry.
	errReplyQueueRecipientRevoked = errors.New("telegram reply queue recipient is no longer authorized")
	// errReplyQueueRoundDeadline marks one reserved delivery round that
	// reached its deadline. Unlike owner or generation cancellation, an
	// expired round spends its attempt and schedules the next one after the
	// failure completed.
	errReplyQueueRoundDeadline = errors.New("telegram reply queue delivery round expired")
)

// replyRecord is one durable final reply. HTML and Plain carry the exact
// rendered chunks produced once at enqueue time; Acked is the number of
// leading chunks the provider has confirmed. Revision, Claim, Attempts, and
// NextAttempt form the cross-process delivery state: every short mutation
// compares the revision and attempt nonce under the workspace election
// fence, so concurrent generations can neither overwrite acknowledged
// progress nor remove successor state.
type replyRecord struct {
	Version        int         `json:"version"`
	BotID          int64       `json:"bot_id"`
	Conversation   string      `json:"conversation"`
	SenderID       int64       `json:"sender_id"`
	SenderUsername string      `json:"sender_username,omitempty"`
	ReplyTo        int64       `json:"reply_to,omitempty"`
	HTML           []string    `json:"html"`
	Plain          []string    `json:"plain"`
	Acked          int         `json:"acked"`
	PlainMode      bool        `json:"plain_mode,omitempty"`
	Attempts       int         `json:"attempts"`
	NextAttempt    time.Time   `json:"next_attempt,omitempty"`
	Revision       int64       `json:"revision"`
	Claim          *replyClaim `json:"claim,omitempty"`
	Stopped        bool        `json:"stopped,omitempty"`
	StopReason     string      `json:"stop_reason,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	ExpiresAt      time.Time   `json:"expires_at"`
}

// replyClaim is the cross-process in-flight state of one delivery attempt.
// Until bounds how long the claim may block every other generation; Previous
// carries the reservation a lost generation rolls back, so a generation that
// disappears mid-attempt never spends a scheduled round.
type replyClaim struct {
	Owner            string    `json:"owner"`
	Nonce            string    `json:"nonce"`
	Until            time.Time `json:"until"`
	PreviousAttempts int       `json:"previous_attempts,omitempty"`
	PreviousNext     time.Time `json:"previous_next,omitempty"`
}

// replyHeader is one validated record plus its private filename and size.
type replyHeader struct {
	name   string
	size   int64
	record replyRecord
}

// replyFence identifies one guarded mutation target: the private filename,
// the attempt nonce that owns the in-flight claim, and the last revision this
// worker persisted. Revision and nonce are compared against a fresh read
// inside the owner guard, so a stale or superseded worker never writes.
type replyFence struct {
	name   string
	nonce  string
	record replyRecord
}

// replyFenceOutcome reports one guarded mutation result.
type replyFenceOutcome int

const (
	// replyFenceApplied means the mutation was written.
	replyFenceApplied replyFenceOutcome = iota
	// replyFenceLost means the owner guard refused; the generation is gone.
	replyFenceLost
	// replyFenceStale means revision or nonce no longer matched; a newer
	// state owns the record.
	replyFenceStale
	// replyFenceFailed means the mutation could not be persisted.
	replyFenceFailed
)

// replyFailureClass classifies one failed provider request.
type replyFailureClass int

const (
	replyFailureRetryable replyFailureClass = iota
	replyFailurePermanent
	replyFailureRateLimited
	replyFailureGeneration
)

// OwnerGuard fences one short durable mutation with the workspace primary
// election. Production wires instance.Election.RunWhileOwner; tests inject a
// deterministic fake. The guard is never held during a provider request.
type OwnerGuard func(action func() error) (bool, error)

// replyQueueBot is the verified live adapter the worker may deliver through.
// Records carry the bot account they were created for, so a replacement
// generation with a different account never receives another bot's reply.
type replyQueueBot interface {
	accountID() int64
	generation() uint64
	authorizeQueuedRecord(record replyRecord) error
	sendQueuedChunk(ctx context.Context, route Route, text string, replyTo int64, html bool) error
}

// ReplyWorker owns the durable Telegram final-reply worker for one primary
// term. The primary term creates it, runs Run for the term lifetime, and
// joins it on shutdown, independently of any Telegram adapter generation.
type ReplyWorker struct {
	queue *replyQueue
}

// NewReplyWorker creates the primary-owned durable reply worker. directory is
// the private queue root, ownerID identifies this primary term for claim
// fencing, guard fences short record mutations with the election, and logf
// receives content-free diagnostics.
func NewReplyWorker(directory, ownerID string, guard OwnerGuard, logf func(string)) *ReplyWorker {
	queue := newReplyQueue(directory)
	queue.owner = ownerID
	if guard != nil {
		queue.guard = guard
	}
	queue.logf = logf
	return &ReplyWorker{queue: queue}
}

// Run owns the worker loop until ctx is cancelled and joins every in-flight
// attempt before returning.
func (w *ReplyWorker) Run(ctx context.Context) { w.queue.run(ctx) }

// replyQueue is the Telegram-specific bounded durable final-reply store and
// delivery scheduler. Its send callback performs exactly one provider
// request per chunk so the queue's own scheduled rounds are the complete
// retry allowance; the ordinary nested transport retry stays exclusive to
// non-queued sends.
type replyQueue struct {
	directory  string
	now        func() time.Time
	guard      OwnerGuard
	owner      string
	newNonce   func() string
	removeFile func(string) error
	logf       func(string)

	wake chan struct{}
	sem  chan struct{}

	enqueueMu sync.Mutex
	sequence  uint64

	botMu         sync.Mutex
	bot           replyQueueBot
	botGeneration uint64

	mu            sync.Mutex
	active        map[string]struct{}
	broken        map[string]struct{}
	brokenOmitted bool
	storageLogged bool
	wg            sync.WaitGroup
}

func newReplyQueue(directory string) *replyQueue {
	return &replyQueue{
		directory:  directory,
		now:        time.Now,
		guard:      func(action func() error) (bool, error) { return true, action() },
		newNonce:   replyQueueNonce,
		removeFile: os.Remove,
		wake:       make(chan struct{}, 1),
		sem:        make(chan struct{}, replyQueueMaxConcurrentRoutes),
		active:     map[string]struct{}{},
		broken:     map[string]struct{}{},
	}
}

// attachBot registers one verified adapter generation after getMe. A stale
// generation whose run context was already cancelled, or whose generation is
// older than the registered one, is refused so a delayed getMe can never
// displace a live replacement. The successful caller owns the matching
// detachBot call.
func (q *replyQueue) attachBot(ctx context.Context, bot replyQueueBot) bool {
	if bot == nil {
		return false
	}
	q.botMu.Lock()
	defer q.botMu.Unlock()
	if ctx.Err() != nil || bot.generation() < q.botGeneration {
		return false
	}
	q.bot = bot
	q.botGeneration = bot.generation()
	q.wakeWorker()
	return true
}

// detachBot removes exactly the registered generation. Both the pointer and
// the generation must still match, so an older generation's exit can never
// clear a newer replacement.
func (q *replyQueue) detachBot(bot replyQueueBot) {
	q.botMu.Lock()
	if q.bot == bot && q.botGeneration == bot.generation() {
		q.bot = nil
	}
	q.botMu.Unlock()
}

func (q *replyQueue) activeBot() replyQueueBot {
	q.botMu.Lock()
	defer q.botMu.Unlock()
	return q.bot
}

// replyQueueLogLine builds one content-free diagnostic line. Callers pass
// only fixed event names and numeric or duration derived fields; reply text,
// conversation identifiers, message identifiers, URLs, tokens, and provider
// prose never reach this boundary.
func replyQueueLogLine(event string, fields ...string) string {
	parts := append([]string{"telegram: reply queue", event}, fields...)
	return strings.Join(parts, " ")
}

func (q *replyQueue) event(event string, fields ...string) {
	if q.logf == nil {
		return
	}
	q.logf(replyQueueLogLine(event, fields...))
}

// run owns the queue worker for one primary term. Enqueue and bot
// registration wake it immediately; the ticker only covers state changed by
// another process or a restarted generation.
func (q *replyQueue) run(ctx context.Context) {
	q.sweep(ctx)
	ticker := time.NewTicker(replyQueueScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			q.wg.Wait()
			return
		case <-ticker.C:
		case <-q.wake:
		}
		q.sweep(ctx)
	}
}

// wakeWorker requests one immediate sweep without blocking the caller.
func (q *replyQueue) wakeWorker() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// sweep dispatches at most one due record per canonical conversation and
// bounds total concurrency. The oldest live record of a route keeps every
// later record behind it, whether it is not due yet or still claimed by an
// in-flight attempt from another generation, preserving FIFO order across
// takeovers. Expiry, stopped-state retention, and confirmed-delivery cleanup
// proceed without any verified bot, so an orphaned queue is still bounded
// while Telegram is disabled.
func (q *replyQueue) sweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	headers, err := q.scan()
	if err != nil {
		q.reportStorage("directory")
		return
	}
	now := q.now()
	byRoute := make(map[string][]replyHeader)
	var order []string
	for _, header := range headers {
		record := header.record
		if q.claimActive(record, now) {
			// A live cross-process claim owns its route until it is released,
			// expires, or is reclaimed. The claimed head stays in the route
			// list so a younger same-route reply can never overtake it, while a
			// terminal record neither blocks nor mutates under another
			// generation.
			if record.Stopped || record.Acked >= len(record.HTML) {
				continue
			}
			if _, exists := byRoute[record.Conversation]; !exists {
				order = append(order, record.Conversation)
			}
			byRoute[record.Conversation] = append(byRoute[record.Conversation], header)
			continue
		}
		if !record.ExpiresAt.After(now) {
			q.expire(header)
			continue
		}
		if record.Stopped {
			// Persisted exhausted, permanent, or suppressed state stays until
			// expiry without another network send.
			continue
		}
		if record.Acked >= len(record.HTML) {
			// The final cursor was persisted; only the unlink may remain.
			q.cleanupDelivered(header)
			continue
		}
		if _, exists := byRoute[record.Conversation]; !exists {
			order = append(order, record.Conversation)
		}
		byRoute[record.Conversation] = append(byRoute[record.Conversation], header)
	}
	for _, conversation := range order {
		if ctx.Err() != nil {
			return
		}
		header := byRoute[conversation][0]
		if !q.botEligible(header.record) {
			// The head is bound to another account or no verified bot is
			// registered. Leave it queued, still blocking younger same-route
			// replies for FIFO, without claiming the route or spending one of
			// the bounded concurrency slots on a dispatch that must refuse.
			continue
		}
		if !q.claim(conversation) {
			continue
		}
		select {
		case q.sem <- struct{}{}:
		default:
			q.release(conversation)
			continue
		}
		q.wg.Add(1)
		go func(header replyHeader, conversation string) {
			defer q.wg.Done()
			defer func() { <-q.sem }()
			defer q.release(conversation)
			q.dispatch(ctx, header)
		}(header, conversation)
	}
}

// waitIdle blocks until every dispatched attempt has finished. Tests use it
// to make concurrent delivery deterministic; the production worker never
// needs it.
func (q *replyQueue) waitIdle() { q.wg.Wait() }

func (q *replyQueue) claim(conversation string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, busy := q.active[conversation]; busy {
		return false
	}
	q.active[conversation] = struct{}{}
	return true
}

func (q *replyQueue) release(conversation string) {
	q.mu.Lock()
	delete(q.active, conversation)
	q.mu.Unlock()
}

func (q *replyQueue) claimActive(record replyRecord, now time.Time) bool {
	return record.Claim != nil && record.Claim.Until.After(now)
}

// botEligible reports whether the currently registered verified generation
// can deliver one record. sweep checks it before spending a bounded
// concurrency slot, so foreign-account and bot-less route heads never starve
// a deliverable route; dispatch rechecks it under the owner guard.
func (q *replyQueue) botEligible(record replyRecord) bool {
	bot := q.activeBot()
	return bot != nil && bot.accountID() == record.BotID
}

// roundExpired reports whether one reserved delivery round passed its
// deadline. The injected clock is authoritative so deterministic scheduling
// tests can drive expiry, while the derived context additionally cancels any
// in-flight provider request at the same boundary.
func (q *replyQueue) roundExpired(roundCtx context.Context, deadline time.Time) bool {
	return !q.now().Before(deadline) || roundCtx.Err() != nil
}

// replyRequestDeadline returns the earlier of one round's deadline and the
// record's expiry. No provider request may outlive either boundary.
func replyRequestDeadline(roundDeadline, expiresAt time.Time) time.Time {
	if expiresAt.Before(roundDeadline) {
		return expiresAt
	}
	return roundDeadline
}

// sendChunkBounded performs exactly one provider request bounded by the
// earlier of the round deadline and the record expiry. The remaining budget
// is recomputed at dispatch time, so a clock that advanced past the boundary
// after the caller's preflight still issues no request.
func (q *replyQueue) sendChunkBounded(roundCtx context.Context, bot replyQueueBot, route Route, text string, replyTo int64, html bool, deadline time.Time) error {
	remaining := deadline.Sub(q.now())
	if remaining <= 0 {
		return errReplyQueueRoundDeadline
	}
	requestCtx, cancel := context.WithTimeout(roundCtx, remaining)
	defer cancel()
	return bot.sendQueuedChunk(requestCtx, route, text, replyTo, html)
}

// expireAttempt resolves one reserved round whose record expired before the
// remaining chunks could be sent. Confirmed chunks and the spent attempt stay
// persisted, and the record itself stays on disk for the ordinary sweep
// expiry cleanup, which removes it on the next pass.
func (q *replyQueue) expireAttempt(fence replyFence) {
	_, outcome := q.fencedMutation(fence.name, fence.record.Revision, fence.nonce, func(record *replyRecord) bool {
		if claim := record.Claim; claim == nil || claim.Nonce != fence.nonce {
			return false
		}
		record.Claim = nil
		return true
	})
	switch outcome {
	case replyFenceApplied:
		q.event("expired")
	case replyFenceLost:
		q.event("suppressed", "reason=generation")
	case replyFenceFailed:
		q.reportStorage("expire")
	}
}

// replyDue reports whether one record's persisted next-attempt time arrived.
func replyDue(record replyRecord, now time.Time) bool {
	if record.Attempts == 0 {
		return true
	}
	return !now.Before(record.NextAttempt)
}

// expire removes one record whose one-hour expiry arrived, unless another
// generation still holds a live in-flight claim.
func (q *replyQueue) expire(header replyHeader) {
	if q.claimActive(header.record, q.now()) {
		return
	}
	outcome := q.removeFenced(header.name, header.record.Revision, "")
	switch outcome {
	case replyFenceApplied:
		q.event("expired")
	case replyFenceFailed:
		q.reportStorage("expire")
	}
}

// cleanupDelivered removes one record whose complete delivery was already
// persisted. The unlink is retried on later sweeps when it fails, and a
// failed unlink never reports delivery.
func (q *replyQueue) cleanupDelivered(header replyHeader) {
	outcome := q.removeFenced(header.name, header.record.Revision, "")
	switch outcome {
	case replyFenceApplied:
		q.event("delivered", fmt.Sprintf("chunks=%d", header.record.Acked))
	case replyFenceFailed:
		q.reportStorage("remove")
	}
}

// dispatch reserves one attempt under the owner guard and starts the network
// loop. The guard action is a short revision compare-and-swap; provider
// requests run entirely outside the election lock.
func (q *replyQueue) dispatch(ctx context.Context, header replyHeader) {
	now := q.now()
	record, outcome := q.fencedMutation(header.name, header.record.Revision, "", func(record *replyRecord) bool {
		if record.Stopped || !record.ExpiresAt.After(now) {
			return false
		}
		bot := q.activeBot()
		if bot == nil || bot.accountID() != record.BotID {
			// No verified adapter for this record's account: never deliver
			// through another bot.
			return false
		}
		if q.claimActive(*record, now) {
			return false
		}
		if record.Acked >= len(record.HTML) {
			return false
		}
		if record.Claim != nil {
			// An expired claim belongs to a generation that vanished. Roll
			// back its reservation so the round is not spent twice.
			record.Attempts = record.Claim.PreviousAttempts
			record.NextAttempt = record.Claim.PreviousNext
		}
		if record.Attempts >= replyQueueMaxAttempts {
			record.Claim = nil
			record.Stopped = true
			record.StopReason = replyStopExhausted
			return true
		}
		if !replyDue(*record, now) {
			return false
		}
		previousAttempts, previousNext := record.Attempts, record.NextAttempt
		record.Attempts++
		// The next-attempt time is persisted by the failure handler from the
		// failed attempt's completion, so a slow attempt never shortens its own
		// backoff. Until then the claim alone tracks the reservation.
		record.Claim = &replyClaim{
			Owner:            q.owner,
			Nonce:            q.newNonce(),
			Until:            now.Add(replyQueueClaimDuration),
			PreviousAttempts: previousAttempts,
			PreviousNext:     previousNext,
		}
		return true
	})
	switch outcome {
	case replyFenceApplied:
		if record.Stopped {
			q.event("exhausted", fmt.Sprintf("attempts=%d", record.Attempts))
			return
		}
		q.attempt(ctx, replyFence{name: header.name, nonce: record.Claim.Nonce, record: record})
	case replyFenceLost:
		q.event("suppressed", "reason=generation")
	case replyFenceFailed:
		q.reportStorage("reserve")
	}
}

// attempt delivers the remaining chunks of one reserved record inside one
// bounded delivery round. Every provider request is bounded by the earlier
// of the round deadline and the record's expiry, so no chunk or fallback
// starts after the record expires. It revalidates authorization immediately
// before every chunk and before the HTML fallback, advances the acknowledged
// cursor only after a confirmed chunk, and removes the record only after the
// final acknowledgement was persisted and the unlink succeeded. The round
// shares the ordinary text-delivery budget: reaching it spends the attempt
// and schedules the next round from the failure's completion, while owner or
// generation cancellation rolls the reservation back instead. An expiry
// mid-round preserves every confirmed chunk for the ordinary cleanup.
func (q *replyQueue) attempt(ctx context.Context, fence replyFence) {
	record := fence.record
	claim := fence.record.Claim
	if claim == nil {
		// A reservation always carries its claim; without one the record was
		// already resolved by another mutation.
		return
	}
	if ctx.Err() != nil {
		// Owner shutdown is a refunded cancellation, never a spent round.
		q.suspend(fence, "cancelled")
		return
	}
	roundDeadline := claim.Until.Add(-replyQueueClaimMargin)
	deadline := replyRequestDeadline(roundDeadline, record.ExpiresAt)
	now := q.now()
	if !now.Before(record.ExpiresAt) {
		// The record expired before this round could send anything. Keep its
		// acknowledged progress for the ordinary expiry cleanup.
		q.expireAttempt(fence)
		return
	}
	if !now.Before(roundDeadline) {
		q.failed(fence, errReplyQueueRoundDeadline)
		return
	}
	roundCtx, cancel := context.WithTimeout(ctx, deadline.Sub(now))
	defer cancel()
	route, err := ParseConversation(record.Conversation)
	if err != nil {
		q.suppress(fence, "invalid")
		return
	}
	for record.Acked < len(record.HTML) {
		if ctx.Err() != nil {
			q.suspend(fence, "cancelled")
			return
		}
		if !q.now().Before(record.ExpiresAt) {
			// The record expired between chunks. No later chunk may be sent;
			// keep every confirmed chunk for the expiry cleanup.
			q.expireAttempt(fence)
			return
		}
		if q.roundExpired(roundCtx, roundDeadline) {
			q.failed(fence, errReplyQueueRoundDeadline)
			return
		}
		bot := q.activeBot()
		if bot == nil || bot.accountID() != record.BotID {
			q.suspend(fence, "generation")
			return
		}
		if err := bot.authorizeQueuedRecord(record); err != nil {
			q.authorizationFailure(fence, err)
			return
		}
		replyTo := int64(0)
		if record.Acked == 0 {
			replyTo = record.ReplyTo
		}
		// A persisted plain mode must send the exact stored plain chunk, never
		// the raw HTML markup with parsing disabled.
		html := !record.PlainMode
		chunk := record.HTML[record.Acked]
		if record.PlainMode {
			chunk = record.Plain[record.Acked]
		}
		sendErr := q.sendChunkBounded(roundCtx, bot, route, chunk, replyTo, html, deadline)
		if sendErr != nil && html && isTelegramEntityParseFailure(sendErr) {
			// Persist the downgraded mode before the fallback request so a
			// later round never attempts HTML again. The fallback stays inside
			// the same attempt and never refreshes the retry budget.
			updated, outcome := q.fencedMutation(fence.name, fence.record.Revision, fence.nonce, func(record *replyRecord) bool {
				record.PlainMode = true
				return true
			})
			if outcome != replyFenceApplied {
				q.fenceFailure(outcome, "mode")
				return
			}
			fence.record = updated
			record = updated
			// The fallback is its own provider request: prove the round is
			// still open, the record is unexpired, and the full authorization
			// still admits it immediately before dispatch.
			if ctx.Err() != nil {
				q.suspend(fence, "cancelled")
				return
			}
			if !q.now().Before(record.ExpiresAt) {
				q.expireAttempt(fence)
				return
			}
			if q.roundExpired(roundCtx, roundDeadline) {
				q.failed(fence, errReplyQueueRoundDeadline)
				return
			}
			if err := bot.authorizeQueuedRecord(record); err != nil {
				q.authorizationFailure(fence, err)
				return
			}
			sendErr = q.sendChunkBounded(roundCtx, bot, route, record.Plain[record.Acked], replyTo, false, deadline)
		}
		if sendErr != nil {
			if ctx.Err() != nil || errors.Is(sendErr, errReplyQueueSuspended) {
				q.suspend(fence, "suspended")
				return
			}
			if !q.now().Before(record.ExpiresAt) {
				q.expireAttempt(fence)
				return
			}
			if q.roundExpired(roundCtx, roundDeadline) {
				q.failed(fence, errReplyQueueRoundDeadline)
				return
			}
			q.failed(fence, sendErr)
			return
		}
		updated, outcome := q.fencedMutation(fence.name, fence.record.Revision, fence.nonce, func(record *replyRecord) bool {
			record.Acked++
			record.PlainMode = false
			return true
		})
		if outcome != replyFenceApplied {
			q.fenceFailure(outcome, "progress")
			return
		}
		fence.record = updated
		record = updated
		if record.Acked >= len(record.HTML) {
			q.finish(fence)
			return
		}
	}
}

// authorizationFailure separates a revoked recipient from a lost generation:
// a revoked recipient is permanently suppressed until expiry, while a
// generation-level refusal suspends the record without spending a round so a
// replacement generation can resume it.
func (q *replyQueue) authorizationFailure(fence replyFence, err error) {
	if isTelegramRecipientRevocationError(err) {
		q.suppress(fence, "authorization")
		return
	}
	q.suspend(fence, "authorization")
}

// fenceFailure reports one guarded mutation that could not be applied.
func (q *replyQueue) fenceFailure(outcome replyFenceOutcome, reason string) {
	switch outcome {
	case replyFenceLost:
		q.event("suppressed", "reason=generation")
	case replyFenceFailed:
		q.reportStorage(reason)
	}
}

// suspend rolls back one attempt's retry reservation after cancellation, a
// lost generation, or an unavailable verified bot. Acknowledged chunks stay
// acknowledged; the replacement generation retries the current chunk as soon
// as it holds the claim. When the election already moved on, the write is
// refused and the successor rolls the reservation back when it reclaims the
// expired claim.
func (q *replyQueue) suspend(fence replyFence, reason string) {
	_, outcome := q.fencedMutation(fence.name, fence.record.Revision, fence.nonce, func(record *replyRecord) bool {
		claim := record.Claim
		if claim == nil || claim.Nonce != fence.nonce {
			return false
		}
		record.Attempts = claim.PreviousAttempts
		record.NextAttempt = claim.PreviousNext
		record.Claim = nil
		return true
	})
	switch outcome {
	case replyFenceApplied:
		q.event("suppressed", "reason="+reason)
	case replyFenceLost:
		q.event("suppressed", "reason=generation")
	case replyFenceFailed:
		q.reportStorage("suspend")
	}
}

// suppress permanently stops one record whose recipient or original group
// sender lost authorization. The record keeps its acknowledged cursor and
// stays on disk until expiry without any further network send.
func (q *replyQueue) suppress(fence replyFence, reason string) {
	_, outcome := q.fencedMutation(fence.name, fence.record.Revision, fence.nonce, func(record *replyRecord) bool {
		if claim := record.Claim; claim != nil && claim.Nonce == fence.nonce {
			record.Attempts = claim.PreviousAttempts
			record.NextAttempt = claim.PreviousNext
		}
		record.Claim = nil
		record.Stopped = true
		record.StopReason = replyStopSuppressed
		return true
	})
	switch outcome {
	case replyFenceApplied:
		q.event("suppressed", "reason="+reason)
	case replyFenceLost:
		q.event("suppressed", "reason=generation")
	case replyFenceFailed:
		q.reportStorage("suppress")
	}
}

// failed persists the outcome of one failed provider request. Transient
// transport failures and API 5xx stay retryable; permanent 4xx refuses and
// 429 with an unusable retry_after stop network sends until expiry; an
// exhausted retry budget persists the exhausted state until expiry.
func (q *replyQueue) failed(fence replyFence, sendErr error) {
	if isTelegramRecipientRevocationError(sendErr) {
		q.suppress(fence, "authorization")
		return
	}
	if isTelegramGenerationLossError(sendErr) {
		q.suspend(fence, "authorization")
		return
	}
	class, retryAfter := classifyReplySendFailure(sendErr)
	now := q.now()
	updated, outcome := q.fencedMutation(fence.name, fence.record.Revision, fence.nonce, func(record *replyRecord) bool {
		previousAttempts, previousNext := record.Attempts, record.NextAttempt
		if claim := record.Claim; claim != nil && claim.Nonce == fence.nonce {
			previousAttempts, previousNext = claim.PreviousAttempts, claim.PreviousNext
		}
		record.Claim = nil
		switch class {
		case replyFailureGeneration:
			record.Attempts = previousAttempts
			record.NextAttempt = previousNext
		case replyFailurePermanent:
			// The wait is persisted from this failure's completion so a later
			// round can never be scheduled before the failure was observed.
			record.NextAttempt = now.Add(replyQueueRetryDelay(record.Attempts))
			record.Stopped = true
			record.StopReason = replyStopPermanent
		default:
			// Retryable failures and bounded rate limits both wait from the
			// completion of the failed attempt. A usable retry_after is honored
			// only when it lands later than the scheduled round.
			next := now.Add(replyQueueRetryDelay(record.Attempts))
			if class == replyFailureRateLimited {
				if retryAt := now.Add(retryAfter); retryAt.After(next) {
					next = retryAt
				}
			}
			record.NextAttempt = next
			if record.Attempts >= replyQueueMaxAttempts {
				record.Stopped = true
				record.StopReason = replyStopExhausted
			}
		}
		return true
	})
	if outcome != replyFenceApplied {
		q.fenceFailure(outcome, "failure")
		return
	}
	switch {
	case updated.Stopped && updated.StopReason == replyStopExhausted:
		q.event("exhausted", fmt.Sprintf("attempts=%d", updated.Attempts))
	case updated.Stopped:
		q.event("stopped", "reason="+updated.StopReason)
	default:
		q.event("retry", fmt.Sprintf("round=%d", updated.Attempts))
	}
}

// finish removes one record whose every chunk was acknowledged and persisted.
// A failed unlink leaves the final cursor for the next sweep to retry, never
// reports delivery, and releases the in-flight claim so the retry does not
// wait out the claim window.
func (q *replyQueue) finish(fence replyFence) {
	outcome := q.removeFenced(fence.name, fence.record.Revision, fence.nonce)
	switch outcome {
	case replyFenceApplied:
		q.event("delivered", fmt.Sprintf("chunks=%d", fence.record.Acked))
	case replyFenceLost:
		q.event("suppressed", "reason=generation")
	case replyFenceFailed:
		q.reportStorage("remove")
		q.releaseClaim(fence)
	}
}

// releaseClaim clears one owner's in-flight claim after its final confirmed
// chunk could not be unlinked, so a later sweep can retry the unlink promptly.
func (q *replyQueue) releaseClaim(fence replyFence) {
	_, outcome := q.fencedMutation(fence.name, fence.record.Revision, fence.nonce, func(record *replyRecord) bool {
		if record.Claim == nil || record.Claim.Nonce != fence.nonce {
			return false
		}
		record.Claim = nil
		return true
	})
	if outcome == replyFenceFailed {
		q.reportStorage("remove")
	}
}

// fencedMutation runs one short compare-and-swap mutation under the owner
// guard. The action re-reads the record, verifies the observed revision and
// the attempt nonce, then persists an incremented revision. Provider requests
// never run inside this boundary.
func (q *replyQueue) fencedMutation(name string, revision int64, nonce string, apply func(*replyRecord) bool) (replyRecord, replyFenceOutcome) {
	outcome := replyFenceStale
	var result replyRecord
	ran, err := q.guard(func() error {
		record, found, readErr := q.readRecord(name)
		if readErr != nil {
			return readErr
		}
		if !found || record.Revision != revision {
			return nil
		}
		if nonce != "" && (record.Claim == nil || record.Claim.Nonce != nonce) {
			return nil
		}
		if !apply(&record) {
			return nil
		}
		record.Revision++
		if err := q.persist(name, record); err != nil {
			return err
		}
		result = record
		outcome = replyFenceApplied
		return nil
	})
	if err != nil {
		return replyRecord{}, replyFenceFailed
	}
	if !ran {
		return replyRecord{}, replyFenceLost
	}
	return result, outcome
}

// removeFenced unlinks one record under the owner guard after verifying the
// observed revision and, when set, the owning attempt nonce. A record that
// already vanished is a benign race with a concurrent generation.
func (q *replyQueue) removeFenced(name string, revision int64, nonce string) replyFenceOutcome {
	outcome := replyFenceStale
	ran, err := q.guard(func() error {
		record, found, readErr := q.readRecord(name)
		if readErr != nil {
			return readErr
		}
		if !found {
			outcome = replyFenceApplied
			return nil
		}
		if record.Revision != revision {
			return nil
		}
		if nonce != "" && (record.Claim == nil || record.Claim.Nonce != nonce) {
			return nil
		}
		if err := q.removeFile(filepath.Join(q.directory, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		outcome = replyFenceApplied
		return nil
	})
	if err != nil {
		return replyFenceFailed
	}
	if !ran {
		return replyFenceLost
	}
	return outcome
}

// classifyReplySendFailure maps one provider or transport failure to the
// approved queue policy. An expired delivery round spends its attempt like
// any retryable failure, while a cancelled owner context stays a generation
// refund. Timeouts, resets, premature EOF, and API 5xx are retryable; a 429
// inside the bounded retry_after window defers without an extra request; a
// 429 outside it fails like the ordinary delivery policy, without clipping;
// 400 and 403 are permanent.
func classifyReplySendFailure(err error) (replyFailureClass, time.Duration) {
	if err == nil {
		return replyFailureRetryable, 0
	}
	if errors.Is(err, errReplyQueueRoundDeadline) {
		return replyFailureRetryable, 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errReplyQueueSuspended) {
		return replyFailureGeneration, 0
	}
	var apiErr *telegramAPIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.code == 429:
			if apiErr.retryAfter > 0 && apiErr.retryAfter <= maxTelegramRetryAfter {
				return replyFailureRateLimited, apiErr.retryAfter
			}
			return replyFailurePermanent, 0
		case apiErr.code == 401:
			return replyFailureGeneration, 0
		case apiErr.code >= 500:
			return replyFailureRetryable, 0
		case apiErr.code >= 400:
			return replyFailurePermanent, 0
		}
		return replyFailureRetryable, 0
	}
	// Transport-class timeouts, resets, closed connections, and premature EOF
	// are already typed; every other pre-response failure stays retryable
	// under the bounded attempt budget.
	return replyFailureRetryable, 0
}

// isTelegramRecipientRevocationError reports whether one queue authorization
// failure revoked the message's recipient or original group sender.
func isTelegramRecipientRevocationError(err error) bool {
	if errors.Is(err, errReplyQueueRecipientRevoked) {
		return true
	}
	var routeErr *telegramRouteAuthorizationError
	return errors.As(err, &routeErr)
}

// isTelegramGenerationLossError reports whether one queue authorization
// failure lost the authenticated bot generation instead of the recipient.
func isTelegramGenerationLossError(err error) bool {
	return errors.Is(err, errReplyQueueGenerationLost) || errors.Is(err, errTelegramRuntimeAuthorization)
}

// replyQueueRetryDelay returns the wait after the given one-based attempt
// number. The sixth attempt has no successor.
func replyQueueRetryDelay(attempt int) time.Duration {
	if attempt < 1 || attempt > len(replyQueueRetryDelays) {
		return 0
	}
	return replyQueueRetryDelays[attempt-1]
}

// enqueue validates and atomically publishes one new record inside the
// bounds. The capacity check and the private create run inside the owner
// guard so a former owner can never publish after a takeover. The initial
// provider attempt happens on the worker's first sweep.
func (q *replyQueue) enqueue(record replyRecord) error {
	q.enqueueMu.Lock()
	defer q.enqueueMu.Unlock()
	if err := q.ensureDirectory(); err != nil {
		return fmt.Errorf("%w: %v", errReplyQueueStorage, err)
	}
	now := q.now().UTC()
	record.Version = replyQueueVersion
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	if record.ExpiresAt.IsZero() {
		record.ExpiresAt = record.CreatedAt.Add(replyQueueExpiry)
	}
	if record.Revision <= 0 {
		record.Revision = 1
	}
	if err := validateReplyRecord(record); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if int64(len(data)) > replyQueueMaxEntryBytes {
		return errReplyQueueFull
	}
	q.sequence++
	name := fmt.Sprintf("%019d-%012d-%s.json", record.CreatedAt.UnixNano(), q.sequence, replyQueueSuffix())
	var enqueueErr error
	ran, guardErr := q.guard(func() error {
		headers, err := q.scan()
		if err != nil {
			enqueueErr = fmt.Errorf("%w: %v", errReplyQueueStorage, err)
			return nil
		}
		count, total, perRoute := 0, int64(0), 0
		for _, header := range headers {
			// Stopped and expired records still occupy private storage until
			// the next sweep, so they keep counting toward the hard bounds.
			count++
			total += header.size
			if header.record.Conversation == record.Conversation {
				perRoute++
			}
		}
		if count >= replyQueueMaxEntries || perRoute >= replyQueueMaxPerRoute || total+int64(len(data)) > replyQueueMaxTotalBytes {
			enqueueErr = errReplyQueueFull
			return nil
		}
		if err := fsx.AtomicCreateFile(filepath.Join(q.directory, name), append(data, '\n'), 0o600); err != nil {
			enqueueErr = fmt.Errorf("%w: %v", errReplyQueueStorage, err)
		}
		return nil
	})
	if !ran {
		return errReplyQueueOwnership
	}
	if guardErr != nil {
		return fmt.Errorf("%w: %v", errReplyQueueStorage, guardErr)
	}
	if enqueueErr != nil {
		return enqueueErr
	}
	q.event("queued")
	q.wakeWorker()
	return nil
}

// validateReplyRecord strictly validates one stored record. Any unexpected
// field, non-canonical route, unbalanced chunk set, impossible progress
// value, or unknown stop state fails closed rather than delivering untrusted
// content or rewriting an ambiguous record.
func validateReplyRecord(record replyRecord) error {
	if record.Version != replyQueueVersion {
		return errReplyQueueCorrupt
	}
	if len(record.HTML) == 0 || len(record.HTML) != len(record.Plain) {
		return errReplyQueueCorrupt
	}
	if record.Acked < 0 || record.Acked > len(record.HTML) {
		return errReplyQueueCorrupt
	}
	if record.Attempts < 0 || record.Attempts > replyQueueMaxAttempts {
		return errReplyQueueCorrupt
	}
	if record.Revision <= 0 {
		return errReplyQueueCorrupt
	}
	if record.BotID <= 0 {
		return errReplyQueueCorrupt
	}
	if record.SenderID <= 0 {
		return errReplyQueueCorrupt
	}
	if record.SenderUsername != normalizeUsername(record.SenderUsername) {
		return errReplyQueueCorrupt
	}
	if record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
		return errReplyQueueCorrupt
	}
	route, err := ParseConversation(record.Conversation)
	if err != nil || route.Conversation() != record.Conversation {
		return errReplyQueueCorrupt
	}
	for index := range record.HTML {
		if record.HTML[index] == "" || record.Plain[index] == "" {
			return errReplyQueueCorrupt
		}
	}
	if record.Stopped {
		switch record.StopReason {
		case replyStopExhausted, replyStopPermanent, replyStopSuppressed:
		default:
			return errReplyQueueCorrupt
		}
	} else if record.StopReason != "" {
		return errReplyQueueCorrupt
	}
	if claim := record.Claim; claim != nil {
		if len(claim.Nonce) != replyQueueNonceBytes*2 || !isLowerHex(claim.Nonce) {
			return errReplyQueueCorrupt
		}
		if claim.Owner == "" || claim.Until.IsZero() {
			return errReplyQueueCorrupt
		}
		if claim.PreviousAttempts < 0 || claim.PreviousAttempts > replyQueueMaxAttempts {
			return errReplyQueueCorrupt
		}
	}
	return nil
}

func isLowerHex(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// scan returns every strictly valid record ordered oldest first. A corrupt,
// symlinked, unreadable, or non-private entry is skipped with one bounded
// content-free storage diagnostic; a symlinked or non-directory queue root,
// or an over-bound directory, fails the whole scan closed.
func (q *replyQueue) scan() ([]replyHeader, error) {
	info, err := os.Lstat(q.directory)
	if err != nil {
		if os.IsNotExist(err) {
			q.clearStorageLogged()
			return nil, nil
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errReplyQueueStorage
	}
	// Reads refuse a symlinked ancestor chain exactly like writes, so a link
	// planted above the private runtime directory can never redirect them.
	if err := refuseSymlinkedAncestors(q.directory); err != nil {
		return nil, err
	}
	file, err := os.Open(q.directory)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []os.DirEntry
	for {
		batch, readErr := file.ReadDir(replyQueueScanBatch)
		entries = append(entries, batch...)
		if len(entries) > replyQueueMaxScanEntries {
			return nil, errReplyQueueStorage
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	headers := make([]replyHeader, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		header, err := q.readEntry(name)
		if err != nil {
			q.noteBroken(name, replyQueueFailureReason(err))
			continue
		}
		headers = append(headers, header)
	}
	sort.Slice(headers, func(left, right int) bool {
		if !headers[left].record.CreatedAt.Equal(headers[right].record.CreatedAt) {
			return headers[left].record.CreatedAt.Before(headers[right].record.CreatedAt)
		}
		return headers[left].name < headers[right].name
	})
	q.clearStorageLogged()
	return headers, nil
}

// readRecord reads one fresh record inside a guarded mutation.
func (q *replyQueue) readRecord(name string) (replyRecord, bool, error) {
	header, err := q.readEntry(name)
	if err != nil {
		if os.IsNotExist(err) {
			return replyRecord{}, false, nil
		}
		return replyRecord{}, false, err
	}
	return header.record, true, nil
}

// readEntry loads one regular, non-symlinked, private record inside its size
// bound. The read itself is bounded so a racing grow cannot exhaust memory.
func (q *replyQueue) readEntry(name string) (replyHeader, error) {
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return replyHeader{}, errReplyQueueCorrupt
	}
	path := filepath.Join(q.directory, name)
	info, err := os.Lstat(path)
	if err != nil {
		return replyHeader{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return replyHeader{}, errReplyQueueSymlink
	}
	if !info.Mode().IsRegular() {
		return replyHeader{}, errReplyQueueCorrupt
	}
	if info.Size() > replyQueueMaxEntryBytes {
		return replyHeader{}, errReplyQueueSize
	}
	if info.Mode().Perm()&0o077 != 0 {
		return replyHeader{}, errReplyQueuePermissions
	}
	file, err := os.Open(path)
	if err != nil {
		return replyHeader{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, replyQueueMaxEntryBytes+1))
	if err != nil {
		return replyHeader{}, err
	}
	if int64(len(data)) > replyQueueMaxEntryBytes {
		return replyHeader{}, errReplyQueueSize
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record replyRecord
	if err := decoder.Decode(&record); err != nil {
		return replyHeader{}, errReplyQueueCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return replyHeader{}, errReplyQueueCorrupt
	}
	if err := validateReplyRecord(record); err != nil {
		return replyHeader{}, err
	}
	return replyHeader{name: name, size: info.Size(), record: record}, nil
}

// persist atomically replaces one existing record. A record that vanished
// concurrently is never resurrected.
func (q *replyQueue) persist(name string, record replyRecord) error {
	path := filepath.Join(q.directory, name)
	if _, err := os.Lstat(path); err != nil {
		return fmt.Errorf("%w: %v", errReplyQueueStorage, err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if int64(len(data)) > replyQueueMaxEntryBytes {
		return errReplyQueueFull
	}
	if err := fsx.AtomicWriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("%w: %v", errReplyQueueStorage, err)
	}
	q.clearStorageLogged()
	return nil
}

// ensureDirectory creates the private queue directory, refusing symlinked
// ancestors and a symlink or non-directory at its own path before any write,
// and tightens an existing directory to 0700.
func (q *replyQueue) ensureDirectory() error {
	if err := refuseSymlinkedAncestors(q.directory); err != nil {
		return err
	}
	if err := os.MkdirAll(q.directory, 0o700); err != nil {
		return err
	}
	if err := refuseSymlinkedAncestors(q.directory); err != nil {
		return err
	}
	info, err := os.Lstat(q.directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errReplyQueueStorage
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(q.directory, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// refuseSymlinkedAncestors rejects any existing path component that is a
// symbolic link, so queue writes never escape through an attacker-controlled
// link placed above the private runtime directory.
func refuseSymlinkedAncestors(path string) error {
	for current := filepath.Clean(path); ; {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errReplyQueueStorage
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// reportStorage logs one deduplicated content-free storage diagnostic. A
// later successful scan or write resets the deduplication.
func (q *replyQueue) reportStorage(reason string) {
	q.mu.Lock()
	if q.storageLogged {
		q.mu.Unlock()
		return
	}
	q.storageLogged = true
	q.mu.Unlock()
	q.event("storage error", "reason="+reason)
}

func (q *replyQueue) clearStorageLogged() {
	q.mu.Lock()
	q.storageLogged = false
	q.mu.Unlock()
}

// noteBroken logs one content-free storage diagnostic per unusable entry,
// bounded so a damaged directory cannot grow process memory without limit.
func (q *replyQueue) noteBroken(name, reason string) {
	q.mu.Lock()
	if _, known := q.broken[name]; known {
		q.mu.Unlock()
		return
	}
	if len(q.broken) >= replyQueueMaxBrokenEntries {
		omitted := q.brokenOmitted
		q.brokenOmitted = true
		q.mu.Unlock()
		if !omitted {
			q.event("storage error", "reason=corrupt_limit")
		}
		return
	}
	q.broken[name] = struct{}{}
	q.mu.Unlock()
	q.event("storage error", "reason="+reason)
}

func replyQueueFailureReason(err error) string {
	switch {
	case errors.Is(err, errReplyQueueSymlink):
		return "symlink"
	case errors.Is(err, errReplyQueueSize):
		return "oversized"
	case errors.Is(err, errReplyQueuePermissions):
		return "permissions"
	case errors.Is(err, errReplyQueueCorrupt):
		return "corrupt"
	default:
		return "unreadable"
	}
}

func replyQueueSuffix() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func replyQueueNonce() string {
	buffer := make([]byte, replyQueueNonceBytes)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
