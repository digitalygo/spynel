package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/digitalygo/spynel/internal/channel"
	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	markdownfmt "github.com/digitalygo/spynel/internal/markdown"
	"github.com/digitalygo/spynel/internal/media"
)

type Bot struct {
	config            config.Telegram
	token             string
	client            *http.Client
	baseURL           string
	report            channel.StatusReporter
	notice            channel.NoticeReporter
	store             *media.Store
	speech            media.Transcriber
	commandMenu       []telegramBotCommand
	transcriptEcho    bool
	me                telegramUser
	activity          *channel.ActivityIndicator[Route]
	activityMu        sync.Mutex
	proactiveActivity map[string][]func()
	identity          *IdentityStore
	log               io.Writer
	allowedUsers      func() []string
	revoked           atomic.Bool
	authLost          chan struct{}
	authLostOnce      sync.Once
	listen            func(string, string) (net.Listener, error)
	renamedTopicsMu   sync.Mutex
	renamedTopics     map[string]struct{}
	// replies is the durable final-reply queue. It stays nil for direct
	// constructions that never opted into durable delivery (unit tests), in
	// which case terminal replies keep the existing bounded direct send.
	replies *replyQueue
	// retryWait sleeps out a provider-requested or transport retry delay.
	// Tests replace it to observe waits without real sleeping.
	retryWait func(context.Context, time.Duration) error
}

var errTelegramRuntimeAuthorization = errors.New("Telegram runtime authorization is unavailable: allowed_users has no valid user")

// Topic rename bounds. The already-renamed set is deliberately in-memory: a
// fresh process starts empty, so the first automatic rename after a restart
// runs again; eviction can likewise re-enable one more automatic rename.
const (
	maxTopicLabelRunes = 128
	maxRenamedTopics   = 1024
)

// Text delivery bounds. One complete sendMessage reply, including every
// chunk, transport retry delay, rate-limit wait, and HTML parse fallback,
// must finish inside the delivery budget. Transient transport failures are
// retried at most twice with the fixed delays below.
const (
	telegramTextDeliveryBudget  = 3 * time.Minute
	maxTelegramTransportRetries = 2
)

var telegramTransportRetryDelays = [...]time.Duration{time.Second, 2 * time.Second}

func New(cfg config.Telegram, token string) *Bot {
	return NewWithIdentityStore(cfg, token, "")
}

func NewWithIdentityStore(cfg config.Telegram, token, identityPath string) *Bot {
	timeout := time.Duration(cfg.PollTimeoutSec+10) * time.Second
	if timeout < 20*time.Second {
		timeout = 20 * time.Second
	}
	bot := &Bot{config: cfg, token: token, client: &http.Client{Timeout: timeout}, baseURL: "https://api.telegram.org", identity: NewIdentityStore(identityPath), authLost: make(chan struct{}), listen: net.Listen, proactiveActivity: map[string][]func(){}, retryWait: waitWithContext, renamedTopics: map[string]struct{}{}}
	bot.allowedUsers = func() []string { return cfg.AllowedUsers }
	bot.activity = newTelegramActivity(bot, 4*time.Second)
	return bot
}

func newTelegramActivity(bot *Bot, interval time.Duration) *channel.ActivityIndicator[Route] {
	return channel.NewActivityIndicator(interval, func(ctx context.Context, route Route, active bool) error {
		if !active {
			return nil
		}
		return bot.action(ctx, route, "typing")
	})
}

func (b *Bot) Name() string { return "telegram" }

func (b *Bot) SetStatusReporter(report channel.StatusReporter) { b.report = report }

func (b *Bot) SetNoticeReporter(report channel.NoticeReporter) { b.notice = report }

func (b *Bot) SetLogWriter(writer io.Writer) { b.log = writer }

func (b *Bot) SetMedia(store *media.Store, speech media.Transcriber) {
	b.store = store
	b.speech = speech
}

// SetTranscriptEcho controls whether a successful voice or audio
// transcription is echoed back to the chat before the message is dispatched.
func (b *Bot) SetTranscriptEcho(enabled bool) { b.transcriptEcho = enabled }

// SetAllowedUsersSource installs the live configuration resolver used at
// startup and immediately before every inbound and outbound provider action.
func (b *Bot) SetAllowedUsersSource(source func() []string) { b.allowedUsers = source }

// AttachReplyWorker connects the bot to the primary-term-owned durable
// final-reply worker (.spynel/runtime/telegram-replies in production).
// Inbound terminal replies and direct handler failures are persisted before
// delivery, retried on the queue's exponential schedule, and removed after
// every chunk is confirmed or after the one-hour expiry. Attachments,
// welcome messages, transcript echoes, and proactive deliveries keep their
// existing one-shot semantics. The bot registers itself with the worker only
// after a successful getMe, and records are bound to that verified account.
// Without this call terminal replies keep the ordinary bounded direct send.
func (b *Bot) AttachReplyWorker(worker *ReplyWorker) {
	if worker == nil {
		b.replies = nil
		return
	}
	b.replies = worker.queue
}

// accountID reports the verified bot account or zero before getMe.
func (b *Bot) accountID() int64 { return b.me.ID }

// logReplyQueue records one content-free queue diagnostic for a refusal that
// happened before the queue could own the record.
func (b *Bot) logReplyQueue(event string, fields ...string) {
	if b.log == nil {
		return
	}
	_, _ = fmt.Fprintln(b.log, replyQueueLogLine(event, fields...))
}

// deliverFinal queues one terminal reply or handler failure for durable
// delivery through the primary-owned worker. A configured queue never falls
// back to a direct provider send: capacity, storage, or ownership refusals
// are reported and produce no provider traffic at all. A bot that has not
// completed getMe is refused for the same reason. text is already the exact
// selected and decorated final text, including the downstream Pi session
// notice when the application added one.
func (b *Bot) deliverFinal(ctx context.Context, route Route, text string, replyTo int64, sender telegramUser) error {
	if b.replies == nil {
		return b.send(ctx, route, text, replyTo)
	}
	if b.me.ID <= 0 {
		b.logReplyQueue("suppressed", "reason=unverified")
		return errReplyQueueGenerationLost
	}
	chunks := markdownfmt.TelegramChunks(text)
	if len(chunks) == 0 {
		return nil
	}
	plain := make([]string, len(chunks))
	for index, chunk := range chunks {
		plain[index] = markdownfmt.TelegramChunkPlainText(chunk)
	}
	err := b.replies.enqueue(replyRecord{
		BotID:          b.me.ID,
		Conversation:   route.Conversation(),
		SenderID:       sender.ID,
		SenderUsername: normalizeUsername(sender.Username),
		ReplyTo:        replyTo,
		HTML:           chunks,
		Plain:          plain,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errReplyQueueFull):
		b.logReplyQueue("suppressed", "reason=capacity")
	case errors.Is(err, errReplyQueueStorage):
		b.logReplyQueue("storage error", "reason=enqueue")
	case errors.Is(err, errReplyQueueOwnership):
		b.logReplyQueue("suppressed", "reason=generation")
	default:
		b.logReplyQueue("suppressed", "reason=invalid")
	}
	return err
}

// sendQueuedChunk performs exactly one provider request for one stored
// chunk. It deliberately bypasses the nested transport retry budget used by
// ordinary sends so the queue's initial send plus five scheduled rounds is
// the complete retry allowance; the ordinary short retry stays exclusive to
// non-queued sends.
func (b *Bot) sendQueuedChunk(ctx context.Context, route Route, text string, replyTo int64, html bool) error {
	if err := b.authorizeProviderRoute(route); err != nil {
		return err
	}
	_, err := b.callProviderOnce(ctx, "sendMessage", sendMessagePayload(route, text, replyTo, html))
	return err
}

// authorizeQueuedRecord reapplies the live allow-list, the current route
// policy, and the original group sender's authorization immediately before
// every queued chunk and HTML fallback. A lost bot generation is reported as
// a suspendable generation failure; a revoked recipient or group sender is
// reported as a permanent suppression. Neither spends a retry round.
func (b *Bot) authorizeQueuedRecord(record replyRecord) error {
	if b.me.ID <= 0 || record.BotID != b.me.ID {
		return errReplyQueueGenerationLost
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		return fmt.Errorf("%w: %v", errReplyQueueGenerationLost, err)
	}
	route, err := ParseConversation(record.Conversation)
	if err != nil {
		return fmt.Errorf("%w: invalid route", errReplyQueueRecipientRevoked)
	}
	if route.IsGroup() && !b.allowed(telegramUser{ID: record.SenderID, Username: record.SenderUsername}) {
		return errReplyQueueRecipientRevoked
	}
	if err := b.authorizeRoutePolicy(route); err != nil {
		return fmt.Errorf("%w: %v", errReplyQueueRecipientRevoked, err)
	}
	return nil
}

func (b *Bot) liveAllowedUsers() []string {
	if b.allowedUsers == nil {
		return nil
	}
	return b.allowedUsers()
}

func (b *Bot) ValidateRuntimeAuthorization() error {
	if b.revoked.Load() || !config.HasAllowedTelegramUser(b.liveAllowedUsers()) {
		return errTelegramRuntimeAuthorization
	}
	return nil
}

func (b *Bot) RevokeRuntimeAuthorization() {
	b.revoked.Store(true)
	b.signalAuthorizationLoss()
}

func (b *Bot) signalAuthorizationLoss() {
	b.authLostOnce.Do(func() { close(b.authLost) })
}

func (b *Bot) requireRuntimeAuthorization() error {
	if err := b.ValidateRuntimeAuthorization(); err != nil {
		b.reportStatus(channel.ConnectionError, err.Error())
		b.signalAuthorizationLoss()
		return err
	}
	return nil
}

func (b *Bot) Deliver(ctx context.Context, conversation, eventID, text string) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	route, err := b.deliveryRoute(conversation)
	if err != nil {
		return err
	}
	if err := b.send(ctx, route, text, 0); err != nil {
		b.logTextDeliveryFailure(deliveryProactive, err)
		return err
	}
	return nil
}

func (b *Bot) DeliverEvent(ctx context.Context, conversation, eventID string, event core.Event) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	route, err := b.deliveryRoute(conversation)
	if err != nil {
		return err
	}
	if event.Kind == core.EventActivity {
		b.activityMu.Lock()
		if event.Active {
			b.proactiveActivity[conversation] = append(b.proactiveActivity[conversation], b.activity.Start(ctx, route))
			b.activityMu.Unlock()
			return nil
		}
		stops := b.proactiveActivity[conversation]
		if len(stops) > 0 {
			stop := stops[len(stops)-1]
			stops = stops[:len(stops)-1]
			if len(stops) == 0 {
				delete(b.proactiveActivity, conversation)
			} else {
				b.proactiveActivity[conversation] = stops
			}
			b.activityMu.Unlock()
			stop()
			return nil
		}
		b.activityMu.Unlock()
		return nil
	}
	if event.Done && !event.Continues && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
		text := event.Text
		if event.Kind == core.EventFinal && event.FinalText != nil {
			text = *event.FinalText
		}
		if event.Kind == core.EventError {
			text = channel.ErrorResponse(text)
		}
		if err := b.send(ctx, route, text, 0); err != nil {
			b.logTextDeliveryFailure(deliveryProactiveEvent, err)
			return err
		}
		return nil
	}
	return nil
}

// deliveryRoute resolves and authorizes an outbound conversation through the
// strict canonical grammar before any provider call. Malformed conversations,
// disabled group delivery, and unauthorized private users fail closed. A
// private topic is authorized against its base numeric user.
func (b *Bot) deliveryRoute(conversation string) (Route, error) {
	route, err := ParseConversation(conversation)
	if err != nil {
		return Route{}, errors.New("invalid Telegram conversation origin")
	}
	if err := b.authorizeRoutePolicy(route); err != nil {
		return Route{}, err
	}
	return route, nil
}

// authorizeRoutePolicy reapplies the live recipient policy for one parsed
// route: malformed routes fail closed, group routes honor the current
// group-mode policy, and private routes re-check the live allow-list against
// their base numeric user.
func (b *Bot) authorizeRoutePolicy(route Route) error {
	if route.Conversation() == "" {
		return &telegramRouteAuthorizationError{message: "invalid Telegram conversation origin"}
	}
	if route.IsGroup() {
		if b.config.GroupMode == "off" {
			return &telegramRouteAuthorizationError{message: "Telegram group delivery is disabled"}
		}
		return nil
	}
	if !b.identity.AuthorizedPrivate(b.liveAllowedUsers(), strconv.FormatInt(route.ChatID(), 10)) {
		return &telegramRouteAuthorizationError{message: "Telegram origin is not in allowed_users"}
	}
	return nil
}

// telegramRouteAuthorizationError marks one route policy refusal while
// keeping the caller-visible message unchanged, so delivery diagnostics can
// classify the failure without inspecting or redacting its text.
type telegramRouteAuthorizationError struct{ message string }

func (e *telegramRouteAuthorizationError) Error() string { return e.message }

// isTelegramAuthorizationFailure reports whether one delivery failure came
// from the global runtime authorization or from the live route policy.
func isTelegramAuthorizationFailure(err error) bool {
	if errors.Is(err, errTelegramRuntimeAuthorization) {
		return true
	}
	var routeErr *telegramRouteAuthorizationError
	return errors.As(err, &routeErr)
}

// authorizeProviderRoute reapplies the global runtime authorization and the
// live route policy immediately before a route-bound provider call.
func (b *Bot) authorizeProviderRoute(route Route) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	return b.authorizeRoutePolicy(route)
}

func (b *Bot) Run(ctx context.Context, handler channel.Handler) error {
	if b.token == "" {
		err := errors.New("Telegram is enabled but no token is configured")
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	if b.config.Mode == "webhook" && strings.TrimSpace(b.config.WebhookSecret) == "" {
		err := errors.New("Telegram webhook mode requires a verification secret")
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	result, err := b.call(ctx, "getMe", map[string]any{})
	if err != nil {
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	_ = json.Unmarshal(result, &b.me)
	if b.me.ID <= 0 {
		err := errors.New("Telegram getMe returned no verified bot identity")
		b.reportStatus(channel.ConnectionError, err.Error())
		return err
	}
	// Only a getMe-verified generation registers with the primary-owned
	// worker, and only for its own Run lifetime. Queued records are bound to
	// this verified account and are never delivered through another bot.
	if b.replies != nil {
		b.replies.attachBot(b)
		defer b.replies.detachBot(b)
	}
	if err := b.registerCommands(ctx); err != nil {
		return err
	}
	if b.store != nil && b.config.AttachmentMaxAgeHours > 0 {
		if _, err := b.store.CleanupOlderThan(time.Duration(b.config.AttachmentMaxAgeHours) * time.Hour); err != nil {
			return fmt.Errorf("clean Telegram attachments: %w", err)
		}
		go b.cleanupAttachments(ctx)
	}
	if b.config.Mode == "webhook" {
		return b.runWebhook(ctx, handler)
	}
	return b.runPolling(ctx, handler)
}

func (b *Bot) cleanupAttachments(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = b.store.CleanupOlderThan(time.Duration(b.config.AttachmentMaxAgeHours) * time.Hour)
		}
	}
}

func (b *Bot) runPolling(ctx context.Context, handler channel.Handler) error {
	if err := b.post(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}); err != nil {
		return err
	}
	b.reportStatus(channel.ConnectionConnected, "")
	offset := int64(0)
	for {
		updates, err := b.updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.reportStatus(channel.ConnectionError, err.Error())
			if errors.Is(err, errTelegramRuntimeAuthorization) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}
		b.reportStatus(channel.ConnectionConnected, "")
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			b.processUpdate(ctx, handler, update)
			select {
			case <-b.authLost:
				return errTelegramRuntimeAuthorization
			default:
			}
		}
	}
}

func (b *Bot) runWebhook(ctx context.Context, handler channel.Handler) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	listener, err := b.listen("tcp", b.config.WebhookListen)
	if err != nil {
		return fmt.Errorf("listen for Telegram webhook on %s: %w", b.config.WebhookListen, err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(b.token)))
	path := "/spynel/telegram/" + hash[:16]
	updates := make(chan telegramUpdate, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-updates:
				b.processUpdate(ctx, handler, update)
			}
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(writer http.ResponseWriter, request *http.Request) {
		if err := b.requireRuntimeAuthorization(); err != nil {
			http.Error(writer, "channel authorization unavailable", http.StatusServiceUnavailable)
			return
		}
		if request.Method != http.MethodPost {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(request.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(b.config.WebhookSecret)) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 10*1024*1024)
		var update telegramUpdate
		if err := json.NewDecoder(request.Body).Decode(&update); err != nil {
			http.Error(writer, "invalid update", http.StatusBadRequest)
			return
		}
		select {
		case updates <- update:
			writer.WriteHeader(http.StatusOK)
		case <-ctx.Done():
			http.Error(writer, "shutting down", http.StatusServiceUnavailable)
		default:
			http.Error(writer, "update queue full", http.StatusServiceUnavailable)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	serveError := make(chan error, 1)
	go func() { serveError <- server.Serve(listener) }()
	publicURL := strings.TrimRight(b.config.WebhookURL, "/") + path
	payload := map[string]any{"url": publicURL, "allowed_updates": []string{"message"}, "secret_token": b.config.WebhookSecret}
	if err := b.post(ctx, "setWebhook", payload); err != nil {
		_ = server.Close()
		return err
	}
	b.reportStatus(channel.ConnectionConnected, "webhook connected via "+listener.Addr().String())
	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case <-b.authLost:
		runErr = errTelegramRuntimeAuthorization
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownContext)
	// Deleting an already-registered webhook is a teardown-only exception to
	// the normal provider boundary: revocation must block all useful traffic,
	// but it must not strand Telegram delivery at a listener that is gone.
	_, _ = b.callProvider(shutdownContext, "deleteWebhook", map[string]any{"drop_pending_updates": false}, b.requireRuntimeAuthorization)
	return runErr
}

func (b *Bot) processUpdate(ctx context.Context, handler channel.Handler, update telegramUpdate) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return
	}
	message := update.Message
	if message == nil || !b.allowed(message.From) {
		return
	}
	if !message.hasContent() {
		return
	}
	if message.Chat.Type == "private" && message.Chat.ID == message.From.ID {
		if err := b.requireRuntimeAuthorization(); err != nil {
			return
		}
		if err := b.identity.RecordVerifiedPrivate(message.From.ID, message.From.ID, message.From.Username); err != nil && b.log != nil {
			_, _ = fmt.Fprintln(b.log, "telegram: persist verified private identity:", err)
		}
	}
	if b.config.WelcomeEnabled && len(message.NewChatMembers) > 0 {
		b.welcome(ctx, message)
	}
	if (strings.TrimSpace(message.Text) != "" || strings.TrimSpace(message.Caption) != "" || message.hasMedia()) && b.groupAllowed(message) {
		b.handle(ctx, handler, message)
	}
}

func (b *Bot) reportStatus(state channel.ConnectionState, detail string) {
	if b.report != nil {
		if (state == channel.ConnectionConnecting || state == channel.ConnectionConnected) && b.ValidateRuntimeAuthorization() != nil {
			state = channel.ConnectionError
			detail = errTelegramRuntimeAuthorization.Error()
		}
		status := channel.ConnectionStatus{Name: b.Name(), State: state, Detail: detail}
		if username := telegramUsername(b.me.Username); username != "" {
			status.Identity = "@" + username
			status.Link = "https://t.me/" + username
		}
		b.report(status)
	}
}

func telegramUsername(value string) string {
	username := strings.TrimPrefix(strings.TrimSpace(value), "@")
	if username == "" || strings.ContainsFunc(username, func(character rune) bool {
		return character != '_' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z')
	}) {
		return ""
	}
	return username
}

func (b *Bot) handle(ctx context.Context, handler channel.Handler, message *telegramMessage) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return
	}
	route, err := b.messageRoute(message)
	if err != nil {
		return
	}
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	var activityMu sync.Mutex
	var finishActivity func()
	var agentOwnedActivity bool
	setActivity := func(active bool) {
		activityMu.Lock()
		if active && finishActivity == nil {
			finishActivity = b.activity.Start(ctx, route)
			activityMu.Unlock()
			return
		}
		finish := finishActivity
		if !active {
			finishActivity = nil
		}
		activityMu.Unlock()
		if !active && finish != nil {
			finish()
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			setActivity(false)
			panic(recovered)
		}
	}()
	// Ordinary accepted messages own the visible communication turn from
	// arrival, including attachment preparation and transcription. Slash
	// commands wait for the application activity event so framework-only
	// commands never flash typing while agent-backed commands still activate it.
	rawText := strings.TrimSpace(firstNonempty(message.Text, message.Caption))
	if !strings.HasPrefix(rawText, "/") {
		setActivity(true)
	}
	text, echoes, err := b.prepareMessage(ctx, message)
	if err != nil {
		if sendErr := b.deliverFinal(ctx, route, channel.ErrorResponse("Spynel attachment error: "+err.Error()), message.MessageID, message.From); sendErr != nil {
			b.logTextDeliveryFailure(deliveryFinal, sendErr)
		}
		setActivity(false)
		return
	}
	// Echo each generated transcript back to the chat before dispatch so the
	// user can spot bad terms while the agent is still working. A failed echo
	// is non-fatal, never affects the turn, and never logs transcript content.
	for _, echo := range echoes {
		_ = b.sendPlain(ctx, route, echo, message.MessageID)
	}
	if b.config.NotifyMessages && b.notice != nil {
		if err := b.requireRuntimeAuthorization(); err != nil {
			setActivity(false)
			return
		}
		preview := strings.ReplaceAll(text, "\n", " ")
		if runes := []rune(preview); len(runes) > 120 {
			preview = string(runes[:120]) + "…"
		}
		b.notice(channel.Notice{Channel: b.Name(), Sender: b.sender(message.From), Text: preview})
	}
	emit := func(event core.Event) {
		if event.Kind == core.EventActivity {
			if event.Active {
				activityMu.Lock()
				agentOwnedActivity = true
				activityMu.Unlock()
			}
			setActivity(event.Active)
			return
		}
		if !event.Done || event.Continues || (event.Kind != core.EventFinal && event.Kind != core.EventError) {
			return
		}
		// Terminal events are a fallback cleanup boundary for handlers that do
		// not yet emit the explicit activity-off event. Stop before delivery.
		setActivity(false)
		text := event.Text
		if event.Kind == core.EventFinal && event.FinalText != nil {
			text = *event.FinalText
		}
		if event.Kind == core.EventError {
			text = channel.ErrorResponse(text)
		}
		if text != "" {
			if err := b.deliverFinal(ctx, route, text, message.MessageID, message.From); err != nil {
				b.logTextDeliveryFailure(deliveryFinal, err)
			}
			message.MessageID = 0
		}
		for _, attachment := range event.Attachments {
			if err := b.sendAttachment(ctx, route, attachment, message.MessageID); err != nil {
				if sendErr := b.deliverFinal(ctx, route, channel.ErrorResponse("Spynel attachment delivery error: "+err.Error()), message.MessageID, message.From); sendErr != nil {
					b.logTextDeliveryFailure(deliveryFinal, sendErr)
				}
			}
			message.MessageID = 0
		}
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		setActivity(false)
		return
	}
	err = handler(ctx, core.Message{
		Channel: b.Name(), Conversation: route.Conversation(), Sender: b.sender(message.From),
		SourceMessageID: fmt.Sprintf("telegram:%s:%d", chatID, message.MessageID), ReplyTo: telegramReplyTo(message), Text: text, ReceivedAt: time.Unix(message.Date, 0).UTC(),
	}, emit)
	if err != nil {
		setActivity(false)
		if sendErr := b.deliverFinal(ctx, route, channel.ErrorResponse(err.Error()), message.MessageID, message.From); sendErr != nil {
			b.logTextDeliveryFailure(deliveryFinal, sendErr)
		}
		return
	}
	activityMu.Lock()
	owned := agentOwnedActivity
	activityMu.Unlock()
	if !owned {
		// A synchronous handler that neither completed nor transferred activity
		// to an asynchronous main-agent turn cannot retain arrival activity.
		setActivity(false)
	}
}

func telegramReplyTo(message *telegramMessage) string {
	if message == nil || message.ReplyToMessage == nil || message.ReplyToMessage.MessageID <= 0 {
		return ""
	}
	replied := message.ReplyToMessage
	return channel.ReplyReference(strconv.FormatInt(replied.MessageID, 10), firstNonempty(replied.Text, replied.Caption))
}

// messageRoute classifies an inbound message into its canonical route.
// Groups and supergroups key on the chat ID and message thread; private chats
// key on the sender so a private topic stays bound to its authorized user.
// Thread IDs 0 and 1 (absent or General) canonicalize to the base
// conversation; IDs of at least 2 address a `-topic-<id>` conversation.
func (b *Bot) messageRoute(message *telegramMessage) (Route, error) {
	if message.Chat.Type == "group" || message.Chat.Type == "supergroup" {
		return NewGroupRoute(message.Chat.ID, message.MessageThreadID)
	}
	return NewPrivateRoute(message.From.ID, message.MessageThreadID)
}

// markTopicRenamed records that Spynel renamed one conversation's topic. The
// set is bounded with simple eviction so a long-lived process cannot grow it
// without limit; a restart starts empty.
func (b *Bot) markTopicRenamed(conversation string) {
	b.renamedTopicsMu.Lock()
	defer b.renamedTopicsMu.Unlock()
	if b.renamedTopics == nil {
		b.renamedTopics = map[string]struct{}{}
	}
	if _, exists := b.renamedTopics[conversation]; !exists && len(b.renamedTopics) >= maxRenamedTopics {
		for existing := range b.renamedTopics {
			delete(b.renamedTopics, existing)
			break
		}
	}
	b.renamedTopics[conversation] = struct{}{}
}

// topicRenamed reports whether Spynel already renamed one conversation's
// topic in this process.
func (b *Bot) topicRenamed(conversation string) bool {
	b.renamedTopicsMu.Lock()
	defer b.renamedTopicsMu.Unlock()
	_, renamed := b.renamedTopics[conversation]
	return renamed
}

// RenameConversation renames one private Telegram topic through
// editForumTopic. Base chats, groups, malformed routes, and invalid labels
// are rejected before any provider call. An automatic rename (force=false)
// runs at most once per conversation in this process: Spynel only needs to
// replace a freshly created topic, so a conversation Spynel already renamed
// is a silent no-op success. A forced rename (/pi name) always renames even a
// topic Spynel touched before. Every provider call re-checks live
// authorization through callRoute, including on its bounded retry.
func (b *Bot) RenameConversation(ctx context.Context, conversation, label string, force bool) error {
	route, err := ParseConversation(conversation)
	if err != nil {
		return errors.New("invalid Telegram conversation origin")
	}
	if route.IsGroup() || route.ThreadID() < 2 {
		return fmt.Errorf("conversation labels apply only to private Telegram topics: %w", channel.ErrConversationLabelUnsupported)
	}
	label = strings.TrimSpace(label)
	if !utf8.ValidString(label) || label == "" || utf8.RuneCountInString(label) > maxTopicLabelRunes {
		return fmt.Errorf("Telegram topic names must be nonempty valid UTF-8 of at most %d characters", maxTopicLabelRunes)
	}
	if strings.ContainsFunc(label, unicode.IsControl) {
		return errors.New("Telegram topic names must be a single line without control characters")
	}
	if !force && b.topicRenamed(conversation) {
		return nil
	}
	payload := map[string]any{
		"chat_id":           strconv.FormatInt(route.ChatID(), 10),
		"message_thread_id": route.ThreadID(),
		"name":              label,
	}
	if _, err := b.callRoute(ctx, route, "editForumTopic", payload); err != nil {
		if !isTopicNotModified(err) {
			return err
		}
	}
	b.markTopicRenamed(conversation)
	return nil
}

// isTopicNotModified reports the provider's idempotent re-rename rejection,
// which is the only 400 that already proves the requested title is live.
func isTopicNotModified(err error) bool {
	var apiErr *telegramAPIError
	return errors.As(err, &apiErr) && apiErr.code == http.StatusBadRequest && strings.Contains(apiErr.description, "TOPIC_NOT_MODIFIED")
}

func (b *Bot) welcome(ctx context.Context, message *telegramMessage) {
	route, err := b.messageRoute(message)
	if err != nil {
		return
	}
	for _, member := range message.NewChatMembers {
		welcome := b.config.WelcomeMessage
		if welcome == "" {
			welcome = "Welcome, {name}!"
		}
		name := firstNonempty(member.FirstName, b.sender(member))
		_ = b.send(ctx, route, strings.ReplaceAll(welcome, "{name}", name), message.MessageID)
	}
}

// messageText returns the agent-facing text for one inbound message. Use
// prepareMessage when the generated transcripts must also be echoed.
func (b *Bot) messageText(ctx context.Context, message *telegramMessage) (string, error) {
	text, _, err := b.prepareMessage(ctx, message)
	return text, err
}

// prepareMessage downloads and transcribes attachments. It returns the
// agent-facing text plus, in order, the generated transcripts to echo back to
// the chat before dispatch. Echoes exist only for successful transcriptions
// while transcript echo is enabled; disabled or failed transcription never
// produces one.
func (b *Bot) prepareMessage(ctx context.Context, message *telegramMessage) (string, []string, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return "", nil, err
	}
	parts := []string{firstNonempty(message.Text, message.Caption)}
	var echoes []string
	for _, file := range message.files() {
		attachment, err := b.download(ctx, file)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, attachment.Token())
		if file.Speech && b.speech == nil {
			parts = append(parts, media.TranscriptionDisabledMarker())
		}
		if file.Speech && b.speech != nil {
			transcript, err := b.speech.Transcribe(ctx, media.TranscriptionRequest{Path: attachment.Path, DurationSeconds: file.DurationSeconds})
			if err != nil {
				parts = append(parts, media.TranscriptionFailedMarker(err))
				continue
			}
			generated := media.TranscriptionGeneratedMarker(transcript)
			parts = append(parts, generated)
			if b.transcriptEcho {
				echoes = append(echoes, generated)
			}
		}
	}
	return joinNonempty(parts), echoes, nil
}

func (b *Bot) download(ctx context.Context, file telegramFile) (media.Attachment, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return media.Attachment{}, err
	}
	if b.store == nil {
		return media.Attachment{}, errors.New("attachment storage is not configured")
	}
	result, err := b.call(ctx, "getFile", map[string]any{"file_id": file.FileID})
	if err != nil {
		return media.Attachment{}, err
	}
	var remote struct {
		Path string `json:"file_path"`
	}
	if err := json.Unmarshal(result, &remote); err != nil || remote.Path == "" {
		return media.Attachment{}, errors.New("Telegram returned an invalid attachment path")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(b.baseURL, "/")+"/file/bot"+b.token+"/"+strings.TrimLeft(remote.Path, "/"), nil)
	if err != nil {
		return media.Attachment{}, err
	}
	if err := b.requireRuntimeAuthorization(); err != nil {
		return media.Attachment{}, err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return media.Attachment{}, b.redact(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return media.Attachment{}, fmt.Errorf("Telegram attachment download returned HTTP %d", response.StatusCode)
	}
	return b.store.Save(ctx, file.Name, response.Body)
}

func (b *Bot) allowed(user telegramUser) bool {
	allowedUsers := b.liveAllowedUsers()
	if !config.HasAllowedTelegramUser(allowedUsers) {
		return false
	}
	id := strconv.FormatInt(user.ID, 10)
	username := normalizeUsername(user.Username)
	for _, allowed := range allowedUsers {
		allowed = normalizeAllowedUser(allowed)
		if allowed == id || (username != "" && allowed == username) {
			return true
		}
	}
	return false
}

func (b *Bot) sender(user telegramUser) string {
	if user.Username != "" {
		return "@" + user.Username
	}
	return strconv.FormatInt(user.ID, 10)
}

func (b *Bot) groupAllowed(message *telegramMessage) bool {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return true
	}
	switch b.config.GroupMode {
	case "all":
		return true
	case "off":
		return false
	default:
		username := strings.ToLower(strings.TrimPrefix(b.me.Username, "@"))
		body := strings.ToLower(firstNonempty(message.Text, message.Caption))
		mentioned := username != "" && strings.Contains(body, "@"+username)
		replied := message.ReplyToMessage != nil && message.ReplyToMessage.From.ID == b.me.ID
		return mentioned || replied
	}
}

func (b *Bot) updates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("offset", strconv.FormatInt(offset, 10))
	timeout := b.config.PollTimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	query.Set("timeout", strconv.Itoa(timeout))
	query.Set("allowed_updates", `["message"]`)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.endpoint("getUpdates")+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return nil, b.redact(err)
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool             `json:"ok"`
		Result      []telegramUpdate `json:"result"`
		Description string           `json:"description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || !envelope.OK {
		return nil, fmt.Errorf("Telegram getUpdates: %s", envelope.Description)
	}
	return envelope.Result, nil
}

// send delivers a complete response as valid Telegram HTML chunks. Each
// chunk stays within Telegram's parsed-visible message limit and the complete
// reply within the 32K reply budget; delivery preserves order and stops on
// the first unrecoverable chunk error.
func (b *Bot) send(ctx context.Context, route Route, text string, replyTo int64) error {
	ctx, cancel := context.WithTimeout(ctx, telegramTextDeliveryBudget)
	defer cancel()
	for _, chunk := range markdownfmt.TelegramChunks(text) {
		if err := b.sendChunk(ctx, route, chunk, replyTo); err != nil {
			return err
		}
		// Reply parameters anchor only the first delivered text chunk.
		replyTo = 0
	}
	return nil
}

// sendPlain delivers literal user content without Markdown interpretation.
// Echoed transcripts are user content, so their characters must render
// exactly as spoken instead of becoming formatting.
func (b *Bot) sendPlain(ctx context.Context, route Route, text string, replyTo int64) error {
	ctx, cancel := context.WithTimeout(ctx, telegramTextDeliveryBudget)
	defer cancel()
	for _, chunk := range markdownfmt.TelegramPlainChunks(text) {
		if err := b.sendChunk(ctx, route, chunk, replyTo); err != nil {
			return err
		}
		replyTo = 0
	}
	return nil
}

// sendChunk delivers one HTML chunk. If and only if Telegram rejects the
// chunk's entities or HTML parsing, the same chunk is retried once as plain
// text without a parse mode; unrelated failures are never downgraded.
// Transient transport failures share one bounded retry budget across the
// HTML attempt and its plain-text fallback, so the fallback can never
// refresh the retry allowance.
func (b *Bot) sendChunk(ctx context.Context, route Route, chunk string, replyTo int64) error {
	text := chunk
	html := true
	budget := &textSendBudget{}
	for {
		_, err := b.callRouteSend(ctx, route, "sendMessage", sendMessagePayload(route, text, replyTo, html), budget)
		if err == nil {
			if budget.transportRetries > 0 {
				b.logTextDeliveryRecovered(budget.transportRetries)
			}
			return nil
		}
		if html && isTelegramEntityParseFailure(err) {
			text = markdownfmt.TelegramChunkPlainText(chunk)
			html = false
			continue
		}
		return &textDeliveryError{err: err, attempts: budget.attempts}
	}
}

// Text delivery diagnostic labels. A failure is attributed either to one
// inbound turn's final reply or to proactive outbound delivery, so operators
// can distinguish the two call sites without any conversation content.
const (
	deliveryFinal          = "final reply"
	deliveryProactive      = "proactive message"
	deliveryProactiveEvent = "proactive event"
)

// textDeliveryError wraps one unsuccessful text delivery with the number of
// provider requests that were actually executed for the failing chunk.
// Diagnostics use the executed count rather than any planned retry count.
type textDeliveryError struct {
	err      error
	attempts int
}

func (e *textDeliveryError) Error() string { return e.err.Error() }

func (e *textDeliveryError) Unwrap() error { return e.err }

// logTextDeliveryFailure records exactly one content-free diagnostic for one
// unsuccessful text delivery. It reports the executed request count and a
// fixed category, never payloads, chat or message identifiers, URLs,
// provider descriptions, or credentials. Routine shutdown cancellation is
// already surfaced through channel lifecycle state and is not logged.
func (b *Bot) logTextDeliveryFailure(kind string, err error) {
	if b.log == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	attempts := 0
	var deliveryErr *textDeliveryError
	if errors.As(err, &deliveryErr) {
		attempts = deliveryErr.attempts
	}
	category := "unknown"
	var apiErr *telegramAPIError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = "deadline"
	case isTelegramTransportError(err):
		category = "transport"
	case isTelegramAuthorizationFailure(err):
		category = "authorization"
	case errors.As(err, &apiErr):
		category = "provider"
	}
	if errors.As(err, &apiErr) {
		_, _ = fmt.Fprintf(b.log, "telegram: %s delivery failed: category=%s code=%d attempts=%d\n", kind, category, apiErr.code, attempts)
		return
	}
	_, _ = fmt.Fprintf(b.log, "telegram: %s delivery failed: category=%s attempts=%d\n", kind, category, attempts)
}

// logTextDeliveryRecovered records one content-free recovery after bounded
// transport retries.
func (b *Bot) logTextDeliveryRecovered(retries int) {
	if b.log == nil || retries <= 0 {
		return
	}
	_, _ = fmt.Fprintf(b.log, "telegram: sendMessage delivery recovered after %d transport retries\n", retries)
}

// sendMessagePayload builds one sendMessage payload. Topic routes carry
// message_thread_id; base and General routes omit it. html selects the HTML
// parse mode; plain-text fallbacks omit it entirely.
func sendMessagePayload(route Route, text string, replyTo int64, html bool) map[string]any {
	payload := map[string]any{"chat_id": strconv.FormatInt(route.ChatID(), 10), "text": text, "disable_web_page_preview": true}
	if html {
		payload["parse_mode"] = "HTML"
	}
	if threadID := route.ThreadID(); threadID != 0 {
		payload["message_thread_id"] = threadID
	}
	if replyTo > 0 {
		payload["reply_parameters"] = map[string]any{"message_id": replyTo}
	}
	return payload
}

func (b *Bot) sendAttachment(ctx context.Context, route Route, attachment core.OutboundAttachment, replyTo int64) error {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return err
	}
	file, err := media.OpenOutbound(attachment)
	if err != nil {
		return err
	}
	defer file.Close()
	method, field := "sendDocument", "document"
	if attachment.Kind == "photo" {
		method, field = "sendPhoto", "photo"
	}
	reader, pipe := io.Pipe()
	writer := multipart.NewWriter(pipe)
	contentType := writer.FormDataContentType()
	go func() {
		writeErr := writer.WriteField("chat_id", strconv.FormatInt(route.ChatID(), 10))
		if writeErr == nil && route.ThreadID() != 0 {
			writeErr = writer.WriteField("message_thread_id", strconv.FormatInt(route.ThreadID(), 10))
		}
		if writeErr == nil && replyTo > 0 {
			writeErr = writer.WriteField("reply_parameters", fmt.Sprintf(`{"message_id":%d}`, replyTo))
		}
		if writeErr == nil {
			var part io.Writer
			part, writeErr = writer.CreateFormFile(field, filepath.Base(attachment.Name))
			if writeErr == nil {
				_, writeErr = io.Copy(part, file)
			}
		}
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = pipe.CloseWithError(writeErr)
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint(method), reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", contentType)
	if err := b.authorizeProviderRoute(route); err != nil {
		return err
	}
	response, err := b.client.Do(request)
	if err != nil {
		return b.redact(err)
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || !envelope.OK {
		return fmt.Errorf("Telegram %s: %s", method, envelope.Description)
	}
	return nil
}

func (b *Bot) action(ctx context.Context, route Route, action string) error {
	payload := map[string]any{"chat_id": strconv.FormatInt(route.ChatID(), 10), "action": action}
	if threadID := route.ThreadID(); threadID != 0 {
		payload["message_thread_id"] = threadID
	}
	_, err := b.callRoute(ctx, route, "sendChatAction", payload)
	return err
}

func (b *Bot) post(ctx context.Context, method string, payload any) error {
	_, err := b.call(ctx, method, payload)
	return err
}

func (b *Bot) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	if err := b.requireRuntimeAuthorization(); err != nil {
		return nil, err
	}
	return b.callProvider(ctx, method, payload, b.requireRuntimeAuthorization)
}

// callRoute authorizes one route-bound provider request immediately before
// dispatch and again before its single bounded rate-limit retry.
func (b *Bot) callRoute(ctx context.Context, route Route, method string, payload any) (json.RawMessage, error) {
	authorize := func() error { return b.authorizeProviderRoute(route) }
	if err := authorize(); err != nil {
		return nil, err
	}
	return b.callProvider(ctx, method, payload, authorize)
}

// callRouteSend authorizes one route-bound sendMessage request immediately
// before every attempt and runs it under a shared text-delivery retry
// budget. It is the only provider path allowed to retry transient transport
// failures, and every retry reauthorizes the exact route, including private
// topic recipients and the current group policy.
func (b *Bot) callRouteSend(ctx context.Context, route Route, method string, payload any, budget *textSendBudget) (json.RawMessage, error) {
	return b.callProviderWithBudget(ctx, method, payload, func() error { return b.authorizeProviderRoute(route) }, budget)
}

// telegramAPIError is a typed Bot API response failure. It carries the
// provider error code, description, and derived retry guidance without the
// request URL, token, or message content. retryAfter is positive only for
// rate-limit responses that supply a usable retry_after parameter; parse
// marks an entity/HTML parse rejection.
type telegramAPIError struct {
	method      string
	code        int
	description string
	retryAfter  time.Duration
	parse       bool
}

func (e *telegramAPIError) Error() string {
	return fmt.Sprintf("Telegram %s: %s", e.method, e.description)
}

// maxTelegramRetryAfter caps the retry delay accepted from a rate-limit
// response. Larger or unusable delays fail closed instead of stalling
// delivery, and the wait never loops: one bounded retry per provider call.
const maxTelegramRetryAfter = 60 * time.Second

// isTelegramEntityParseFailure reports whether the provider rejected the
// message's HTML entities or tags. Only that failure class may downgrade a
// chunk to plain text.
func isTelegramEntityParseFailure(err error) bool {
	var apiErr *telegramAPIError
	return errors.As(err, &apiErr) && apiErr.parse
}

// telegramTransportError marks one provider request that failed before any
// definitive Bot API response: a timeout, connection reset, closed
// connection, or premature EOF. The stored message is already redacted,
// and classification happens on the raw error before redaction, so a
// retried send never has to inspect a token-bearing error.
type telegramTransportError struct {
	err error
}

func (e *telegramTransportError) Error() string { return e.err.Error() }

func (e *telegramTransportError) Unwrap() error { return e.err }

// isTelegramTransportError reports whether err was classified as a
// transient transport failure while its raw cause was still available.
func isTelegramTransportError(err error) bool {
	var transportErr *telegramTransportError
	return errors.As(err, &transportErr)
}

// isTransientTelegramTransport classifies one raw transport failure as a
// retryable timeout, connection reset, closed connection, or premature EOF.
// It must run before redaction, which replaces the error with an opaque
// message that no longer exposes the cause.
func isTransientTelegramTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// textSendBudget tracks the retry allowances shared by one text chunk's
// sendMessage delivery: at most maxTelegramTransportRetries transient
// transport retries and one provider rate-limit retry across the HTML
// attempt and its plain-text fallback. attempts counts provider requests
// actually executed, so diagnostics never report planned retries.
type textSendBudget struct {
	transportRetries int
	rateLimitRetried bool
	attempts         int
}

func (b *Bot) callProvider(ctx context.Context, method string, payload any, reauthorize func() error) (json.RawMessage, error) {
	return b.callProviderWithBudget(ctx, method, payload, reauthorize, nil)
}

// callProviderWithBudget executes one provider request and applies the
// existing single bounded rate-limit retry to every method unchanged. When
// budget is set (the text sendMessage path only), transient transport
// failures are additionally retried up to the shared bound, and every
// attempt rechecks the context and reauthorizes the exact route immediately
// before dispatch. Cancellation, authorization loss, and definitive API
// errors are never retried. Caller-side preauthorization and the documented
// post-revocation deleteWebhook teardown exception are unchanged for calls
// without a retry budget.
func (b *Bot) callProviderWithBudget(ctx context.Context, method string, payload any, reauthorize func() error, budget *textSendBudget) (json.RawMessage, error) {
	rateLimitRetried := budget != nil && budget.rateLimitRetried
	for {
		// Text sendMessage attempts are dispatched only while the caller's
		// context is live and the latest authorization admits the exact route.
		if budget != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if authErr := reauthorize(); authErr != nil {
				return nil, authErr
			}
			budget.attempts++
		}
		result, err := b.callProviderOnce(ctx, method, payload)
		if err == nil {
			return result, nil
		}
		if budget != nil && budget.transportRetries < maxTelegramTransportRetries && isTelegramTransportError(err) && ctx.Err() == nil {
			delay := telegramTransportRetryDelays[budget.transportRetries]
			budget.transportRetries++
			if waitErr := b.waitRetry(ctx, delay); waitErr != nil {
				return nil, waitErr
			}
			// The injected waiter may deliberately ignore cancellation, so the
			// context is rechecked before any further request is dispatched.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			continue
		}
		var apiErr *telegramAPIError
		if errors.As(err, &apiErr) && apiErr.retryAfter > 0 && apiErr.retryAfter <= maxTelegramRetryAfter && !rateLimitRetried {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			rateLimitRetried = true
			if budget != nil {
				budget.rateLimitRetried = true
			}
			if waitErr := b.waitRetry(ctx, apiErr.retryAfter); waitErr != nil {
				return nil, waitErr
			}
			// Reapply the caller's authorization after the wait so a revocation
			// or a recipient change during the delay cannot complete the retried
			// provider call. Cancellation wins over any further attempt. Text
			// sends reauthorize again at the top of the next iteration.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if budget == nil {
				if authErr := reauthorize(); authErr != nil {
					return nil, authErr
				}
			}
			continue
		}
		return nil, err
	}
}

// waitRetry sleeps for one bounded retry delay through the injected waiter,
// honoring caller cancellation and channel shutdown.
func (b *Bot) waitRetry(ctx context.Context, delay time.Duration) error {
	wait := b.retryWait
	if wait == nil {
		wait = waitWithContext
	}
	return wait(ctx, delay)
}

func (b *Bot) callProviderOnce(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint(method), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := b.client.Do(request)
	if err != nil {
		return nil, b.providerRequestError(ctx, err)
	}
	defer response.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		// A non-200 status is a definitive provider response, so an empty,
		// truncated, or malformed body must never be reclassified as an
		// ambiguous transport failure that the text send path may retry.
		if response.StatusCode != http.StatusOK {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, &telegramAPIError{method: method, code: response.StatusCode, description: http.StatusText(response.StatusCode)}
		}
		return nil, b.providerRequestError(ctx, err)
	}
	if response.StatusCode == http.StatusOK && envelope.OK {
		return envelope.Result, nil
	}
	apiErr := &telegramAPIError{method: method, code: envelope.ErrorCode, description: envelope.Description}
	if envelope.ErrorCode == http.StatusTooManyRequests && envelope.Parameters.RetryAfter > 0 {
		apiErr.retryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
	}
	if envelope.ErrorCode == http.StatusBadRequest && strings.Contains(strings.ToLower(envelope.Description), "can't parse entities") {
		apiErr.parse = true
	}
	return nil, apiErr
}

// waitWithContext sleeps for the delay while honoring cancellation.
func waitWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *Bot) endpoint(method string) string {
	return strings.TrimRight(b.baseURL, "/") + "/bot" + b.token + "/" + method
}

func (b *Bot) redact(err error) error {
	return errors.New(strings.ReplaceAll(err.Error(), b.token, "<redacted>"))
}

// providerRequestError redacts one provider request failure and preserves
// its transient-transport classification. A caller cancellation or deadline
// is returned as the context error before redaction, so it is never retried
// and callers can detect it with errors.Is. Classification otherwise runs
// on the raw error before redaction replaces it with an opaque message, so
// the returned error never contains the bot token, request URL contents, or
// payload while still being retryable when appropriate.
func (b *Bot) providerRequestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	redacted := b.redact(err)
	if !isTransientTelegramTransport(err) {
		return redacted
	}
	return &telegramTransportError{err: redacted}
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID       int64             `json:"message_id"`
	From            telegramUser      `json:"from"`
	Chat            telegramChat      `json:"chat"`
	Date            int64             `json:"date"`
	Text            string            `json:"text"`
	Caption         string            `json:"caption"`
	Document        *telegramDocument `json:"document"`
	Photo           []telegramPhoto   `json:"photo"`
	Video           *telegramMedia    `json:"video"`
	Audio           *telegramMedia    `json:"audio"`
	Voice           *telegramMedia    `json:"voice"`
	ReplyToMessage  *telegramMessage  `json:"reply_to_message"`
	NewChatMembers  []telegramUser    `json:"new_chat_members"`
	MessageThreadID int64             `json:"message_thread_id"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramDocument struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
}

type telegramMedia struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	Duration     int    `json:"duration"`
}

type telegramPhoto struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
}

type telegramFile struct {
	FileID string
	Name   string
	// Speech marks Telegram voice notes and audio files for transcription.
	// Video, video notes, documents (even audio-named ones), photos, and
	// stickers are never transcribed.
	Speech          bool
	DurationSeconds int
}

func (m *telegramMessage) hasMedia() bool {
	return m.Document != nil || len(m.Photo) > 0 || m.Video != nil || m.Audio != nil || m.Voice != nil
}

func (m *telegramMessage) hasContent() bool {
	return strings.TrimSpace(m.Text) != "" || strings.TrimSpace(m.Caption) != "" || m.hasMedia() || len(m.NewChatMembers) > 0
}

func (m *telegramMessage) files() []telegramFile {
	var files []telegramFile
	if m.Document != nil {
		files = append(files, telegramFile{FileID: m.Document.FileID, Name: firstNonempty(m.Document.FileName, "document-"+m.Document.FileUniqueID)})
	}
	if len(m.Photo) > 0 {
		photo := m.Photo[len(m.Photo)-1]
		files = append(files, telegramFile{FileID: photo.FileID, Name: "photo-" + photo.FileUniqueID + ".jpg"})
	}
	if m.Video != nil {
		files = append(files, telegramFile{FileID: m.Video.FileID, Name: firstNonempty(m.Video.FileName, "video-"+m.Video.FileUniqueID+".mp4")})
	}
	if m.Audio != nil {
		files = append(files, telegramFile{FileID: m.Audio.FileID, Name: firstNonempty(m.Audio.FileName, "audio-"+m.Audio.FileUniqueID+".mp3"), Speech: true, DurationSeconds: m.Audio.Duration})
	}
	if m.Voice != nil {
		files = append(files, telegramFile{FileID: m.Voice.FileID, Name: "voice-" + m.Voice.FileUniqueID + ".ogg", Speech: true, DurationSeconds: m.Voice.Duration})
	}
	return files
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func joinNonempty(values []string) string {
	result := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return strings.Join(result, "\n\n")
}
