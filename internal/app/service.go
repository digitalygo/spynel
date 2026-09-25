package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalygo/spynel/internal/agentdocs"
	"github.com/digitalygo/spynel/internal/channel"
	"github.com/digitalygo/spynel/internal/channel/telegram"
	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/extensions"
	"github.com/digitalygo/spynel/internal/fsx"
	"github.com/digitalygo/spynel/internal/harness"
	"github.com/digitalygo/spynel/internal/history"
	"github.com/digitalygo/spynel/internal/media"
	"github.com/digitalygo/spynel/internal/shortid"
	"github.com/digitalygo/spynel/internal/theme"
	"github.com/digitalygo/spynel/internal/updater"
)

type Service struct {
	// Config is the immutable structural configuration used to construct
	// histories, hooks, routes, and workspace paths. Live scalar values are
	// always read through Settings, avoiding races with remote form commands.
	Config          config.Config
	Harness         harness.Harness
	History         *history.Store
	Hooks           extensions.Runner
	Runtime         *Runtime
	Settings        *config.Store
	PairingControl  channel.PairingManager
	DeliveryControl channel.DeliveryRouter
	// ConversationLabels routes best-effort conversation label renames to the
	// active channel generation. It is optional; naming still succeeds when no
	// router is installed.
	ConversationLabels channel.ConversationLabelRouter
	// ConversationDelivery is the ordinary channel response path; it is
	// intentionally separate from Notify.
	ConversationDelivery   channel.DeliveryRouter
	conversationDeliveryMu sync.RWMutex
	outbox                 Outbox
	Startup                interface {
		Sync(config.Config, bool) error
		Enabled(config.Config) (bool, error)
	}
	Updates              *updater.Manager
	configurationMu      sync.Mutex
	instanceMu           sync.RWMutex
	primaryInstance      string
	cleanupNotBefore     time.Time
	titleMu              sync.Mutex
	titleChanges         chan string
	themeChanges         chan theme.Theme
	connectionMu         sync.RWMutex
	connections          map[string]channel.ConnectionStatus
	pairingMu            sync.RWMutex
	pairing              map[string]channel.PairingEvent
	pairingEvents        chan channel.PairingEvent
	noticeMu             sync.RWMutex
	noticeSequence       uint64
	lastNotice           channel.Notice
	noticeEvents         chan channel.Notice
	restartRequests      chan struct{}
	updateRequests       chan updater.Result
	primaryRequests      chan string
	primaryRequestMu     sync.Mutex
	primaryRequested     bool
	streamMu             sync.Mutex
	admissionMu          sync.Mutex
	admissions           map[string]bool
	piNoticeMu           sync.Mutex
	piNoticeState        map[string]piSessionDisclosure
	liveTUIMu            sync.Mutex
	liveTUI              map[string]map[string]time.Time
	chatActivityMu       sync.Mutex
	chatActivity         map[int]map[*chatActivityEmitter]struct{}
	cleanupHistoryStep   func(string)
	resumeAdmissionStep  func(string)
	streamText           map[string]string
	telegramIdentity     *telegram.IdentityStore
	jobCancellationGrace time.Duration
	correlationMu        sync.Mutex
	correlations         map[string]*executionCorrelation
}

const jobCancellationGrace = 30 * time.Second

// errUnhandledCommand tells Handle that a slash input is not a deterministic
// framework command and must be delivered to a harness that expands its own
// native skills, prompt templates, and extension commands. It never reaches a
// caller and is never persisted as an error.
var errUnhandledCommand = errors.New("unhandled command")

func New(cfg config.Config, target harness.Harness) *Service {
	return NewWithRuntime(cfg, target, NewRuntime())
}

func NewWithRuntime(cfg config.Config, target harness.Harness, runtime *Runtime) *Service {
	runtime.ConfigureJobArchive(cfg.StatePath("jobs"))
	hooks := extensions.Runner{Directory: cfg.Resolve(cfg.Extensions.Directory), Timeout: cfg.Extensions.Timeout()}
	hooks.Log = runtime.Writer("extensions")
	store := history.New(cfg.StatePath("history"))
	connections := map[string]channel.ConnectionStatus{
		"telegram": {Name: "telegram", State: channel.ConnectionUnconfigured},
		"whatsapp": {Name: "whatsapp", State: channel.ConnectionUnconfigured},
	}
	if cfg.Channels.Telegram.Enabled {
		connections["telegram"] = channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionConnecting}
	}
	if cfg.Channels.WhatsApp.Enabled {
		connections["whatsapp"] = channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionConnecting}
	}
	service := &Service{
		Config:               cfg,
		Harness:              target,
		History:              store,
		Hooks:                hooks,
		Runtime:              runtime,
		Settings:             config.NewStore(cfg),
		titleChanges:         make(chan string, 1),
		themeChanges:         make(chan theme.Theme, 1),
		connections:          connections,
		pairing:              map[string]channel.PairingEvent{},
		pairingEvents:        make(chan channel.PairingEvent, 1),
		noticeEvents:         make(chan channel.Notice, 8),
		restartRequests:      make(chan struct{}, 1),
		updateRequests:       make(chan updater.Result, 1),
		primaryRequests:      make(chan string, 1),
		streamText:           map[string]string{},
		liveTUI:              map[string]map[string]time.Time{},
		chatActivity:         map[int]map[*chatActivityEmitter]struct{}{},
		telegramIdentity:     telegram.NewIdentityStore(cfg.StatePath("runtime", "telegram-identities.json")),
		jobCancellationGrace: jobCancellationGrace,
		correlations:         map[string]*executionCorrelation{},
		piNoticeState:        map[string]piSessionDisclosure{},
	}
	service.outbox = Outbox{Directory: cfg.StatePath("runtime", "outbox"), Deliver: service.deliverNotification}
	return service
}

// Close stops the harness while its final diagnostics can still be captured,
// then drains and closes the durable runtime log.
func (s *Service) Close() error {
	err := s.Harness.Close()
	s.stopAllChatActivity()
	s.Runtime.Close()
	return err
}

func (s *Service) validateOrigin(origin Origin) error {
	if _, err := os.Stat(s.History.Path(origin.Channel, origin.Conversation)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("origin %s/%s is not a known conversation", origin.Channel, origin.Conversation)
		}
		return err
	}
	cfg := s.Settings.Snapshot()
	switch origin.Channel {
	case "telegram":
		route, err := telegram.ParseConversation(origin.Conversation)
		if err != nil {
			return errors.New("invalid Telegram origin")
		}
		if route.IsGroup() {
			if cfg.Channels.Telegram.GroupMode == "off" {
				return errors.New("Telegram group origin is disabled")
			}
			return nil
		}
		// A private topic is authorized against its base numeric user.
		if s.telegramIdentity.AuthorizedPrivate(cfg.Channels.Telegram.AllowedUsers, strconv.FormatInt(route.ChatID(), 10)) {
			return nil
		}
		return errors.New("Telegram origin is not authorized by allowed_users")
	case "whatsapp":
		if strings.HasPrefix(origin.Conversation, "WA-group-") {
			if !cfg.Channels.WhatsApp.AllowGroups {
				return errors.New("WhatsApp group origin is disabled")
			}
			return nil
		}
		if !strings.HasPrefix(origin.Conversation, "WA-") {
			return errors.New("invalid WhatsApp origin")
		}
		number := config.NormalizeWhatsAppNumber(strings.TrimPrefix(origin.Conversation, "WA-"))
		for _, allowed := range cfg.Channels.WhatsApp.AllowedNumbers {
			if config.NormalizeAllowedWhatsAppNumber(allowed) == number {
				return nil
			}
		}
		return errors.New("WhatsApp origin is not authorized by allowed_numbers")
	case "tui", "cli":
		return nil
	}
	return errors.New("unsupported origin channel")
}

func (s *Service) deliverNotification(ctx context.Context, origin Origin, eventID, text string) error {
	if err := s.validateOrigin(origin); err != nil {
		return err
	}
	if origin.Channel == "telegram" || origin.Channel == "whatsapp" {
		state, stateErr := s.History.DeliveryState(origin.Channel, origin.Conversation, eventID)
		if stateErr != nil {
			return stateErr
		}
		if state == "sent" {
			return nil
		}
		if s.DeliveryControl == nil {
			return fmt.Errorf("%s is disconnected", origin.Channel)
		}
		if _, err := s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: "notification_sending", EventID: eventID}); err != nil {
			return err
		}
		err := s.DeliveryControl.Deliver(ctx, origin.Channel, origin.Conversation, eventID, text)
		if err != nil {
			_, _ = s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: "notification_failed", EventID: eventID, Content: err.Error()})
			return err
		}
	}
	role := "assistant"
	if origin.Channel == "tui" && s.Harness.IsActive(sessionKey(core.Message{Channel: origin.Channel, Conversation: origin.Conversation})) {
		role = "notification_pending"
	}
	_, err := s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: role, Sender: "Spy", Content: text, EventID: eventID})
	return err
}

func (s *Service) AckNotification(originText, eventID string, afterChars int) error {
	origin, err := ParseOrigin(originText)
	if err != nil {
		return err
	}
	if origin.Channel != "tui" {
		return errors.New("notification interleave acknowledgements are TUI-only")
	}
	if eventID == "" || afterChars < 0 {
		return errors.New("invalid notification acknowledgement")
	}
	_, err = s.History.Append(origin.Channel, origin.Conversation, history.Entry{Role: "notification_ack", EventID: eventID, AfterChars: afterChars})
	return err
}

func (s *Service) Notify(ctx context.Context, originText, text string) (string, error) {
	origin, err := ParseOrigin(originText)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("notification message is required")
	}
	if err := s.validateOrigin(origin); err != nil {
		return "", err
	}
	deliveryKey := fmt.Sprintf("manual-%d", time.Now().UTC().UnixNano())
	entry, err := s.outbox.Enqueue(deliveryKey, "manual", originText, text)
	if err != nil {
		return "", err
	}
	if processErr := s.outbox.Process(ctx); processErr != nil {
		s.Runtime.LogEvent("error", "notify", "notification_retry", "Notification retained for retry: "+processErr.Error())
	}
	return entry.ID, nil
}

// NotifyRecentAuthorized resolves a destination inside the application-owned
// authorization boundary. Callers receive only the queued ID, never recipient
// or history data.
func (s *Service) NotifyRecentAuthorized(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("notification message is required")
	}
	origin, err := s.mostRecentAuthorizedOrigin()
	if err != nil {
		return "", err
	}
	deliveryKey := fmt.Sprintf("recent-%d", time.Now().UTC().UnixNano())
	entry, err := s.outbox.Enqueue(deliveryKey, "manual", origin.Channel+"/"+origin.Conversation, text)
	if err != nil {
		return "", err
	}
	if processErr := s.outbox.Process(ctx); processErr != nil {
		s.Runtime.LogEvent("error", "notify", "notification_retry", "Notification retained for retry: "+processErr.Error())
	}
	return entry.ID, nil
}

func (s *Service) mostRecentAuthorizedOrigin() (Origin, error) {
	cfg := s.Settings.Snapshot()
	if remoteAuthorizedPrincipalCount(cfg) > 1 {
		return Origin{}, errors.New("most-recent-authorized routing is ambiguous: multiple remote users are authorized")
	}
	activity, err := s.History.ListUserActivity(256)
	if err != nil {
		return Origin{}, err
	}
	var selected Origin
	var selectedAt time.Time
	for _, candidate := range activity {
		origin := Origin{Channel: candidate.Channel, Conversation: candidate.Conversation}
		if (origin.Channel == "telegram" && !cfg.Channels.Telegram.Enabled) || (origin.Channel == "whatsapp" && !cfg.Channels.WhatsApp.Enabled) {
			continue
		}
		if strings.Contains(origin.Conversation, "-group-") || s.validateOrigin(origin) != nil {
			continue
		}
		if candidate.UpdatedAt.Equal(selectedAt) && selected.Channel != "" && (selected.Channel != origin.Channel || selected.Conversation != origin.Conversation) {
			return Origin{}, errors.New("most-recent-authorized routing is ambiguous: recent activity timestamps are tied")
		}
		if selected.Channel == "" || candidate.UpdatedAt.After(selectedAt) {
			selected, selectedAt = origin, candidate.UpdatedAt
		}
	}
	if selected.Channel == "" {
		return Origin{}, errors.New("no unambiguous recently active authorized conversation is available")
	}
	return selected, nil
}

func remoteAuthorizedPrincipalCount(cfg config.Config) int {
	principals := map[string]struct{}{}
	if cfg.Channels.Telegram.Enabled {
		for _, value := range cfg.Channels.Telegram.AllowedUsers {
			if principal := config.NormalizeTelegramUser(value); principal != "" {
				principals["telegram:"+principal] = struct{}{}
			}
		}
	}
	if cfg.Channels.WhatsApp.Enabled {
		for _, value := range cfg.Channels.WhatsApp.AllowedNumbers {
			if principal := config.NormalizeAllowedWhatsAppNumber(value); principal != "" {
				principals["whatsapp:"+principal] = struct{}{}
			}
		}
	}
	return len(principals)
}

func (s *Service) Start(ctx context.Context) error {
	return s.Harness.Start(ctx)
}

// SetPrimaryInstanceID records the workspace server owner for shared status
// output. Loopback clients separately identify the process making a request.
func (s *Service) SetPrimaryInstanceID(id string) {
	id = strings.TrimSpace(id)
	s.instanceMu.Lock()
	if id != "" && id != s.primaryInstance {
		// A fresh owner has no process-local view of leases registered with its
		// predecessor. Fence destructive cleanup long enough for every live TUI
		// to renew before this owner can use its rebuilt lease set.
		s.cleanupNotBefore = time.Now().UTC().Add(liveTUILeaseDuration)
	} else if id == "" {
		s.cleanupNotBefore = time.Time{}
	}
	s.primaryInstance = id
	s.instanceMu.Unlock()
}

// FenceCleanupForLiveTUIReadmission restarts the owner-transition safety
// window immediately before the local API begins accepting client renewals.
// Primary construction can perform slower startup work after election, so the
// fence must be anchored to API availability rather than election alone.
func (s *Service) FenceCleanupForLiveTUIReadmission() {
	s.instanceMu.Lock()
	if s.primaryInstance != "" {
		s.cleanupNotBefore = time.Now().UTC().Add(liveTUILeaseDuration)
	}
	s.instanceMu.Unlock()
}

func (s *Service) primaryInstanceID() string {
	s.instanceMu.RLock()
	id := s.primaryInstance
	s.instanceMu.RUnlock()
	return id
}

func (s *Service) Handle(ctx context.Context, message core.Message, emit core.Emit) (resultErr error) {
	if message.Channel == "cli" || message.Channel == "tui" {
		if err := ValidateLocalMessage(message); err != nil {
			return err
		}
	}
	if message.ReceivedAt.IsZero() {
		message.ReceivedAt = time.Now().UTC()
	}
	message.Text = strings.TrimSpace(message.Text)
	if message.Text == "" {
		return nil
	}
	if message.SourceMessageID == "" {
		var err error
		message.SourceMessageID, err = core.NewSourceMessageID()
		if err != nil {
			return fmt.Errorf("create source message identity: %w", err)
		}
	}
	release, err := s.admitMessage(message)
	if err != nil {
		if errors.Is(err, ErrDuplicateMessage) && message.Channel != "cli" && message.Channel != "tui" {
			return nil
		}
		return err
	}
	defer release()
	duplicate, err := s.History.HasUserSourceID(message.Channel, message.Conversation, message.SourceMessageID)
	if err != nil {
		return fmt.Errorf("validate source message identity: %w", err)
	}
	if duplicate {
		if message.Channel != "cli" && message.Channel != "tui" {
			return nil
		}
		return ErrDuplicateMessage
	}
	if message.FollowupOnly && !s.Harness.IsActive(sessionKey(message)) {
		return errors.New("there is no active execution for this conversation")
	}
	recorded := false
	var recordErr error
	recordUser := func() error {
		_, recordErr = s.History.Append(message.Channel, message.Conversation, userHistoryEntry(message, time.Now().UTC()))
		recorded = recordErr == nil
		return recordErr
	}
	// Returned failures are rendered by the transport, while provider error
	// events are persisted by wrapEmit. Save rejected requests here so reloads
	// and the next agent prompt include the same error the user saw.
	defer func() {
		if resultErr != nil {
			if recordErr != nil {
				s.Runtime.LogEvent("error", "history", "append_failed", fmt.Sprintf("Persist incoming history failed (%T)", recordErr))
				return
			}
			if !recorded {
				if err := recordUser(); err != nil {
					s.Runtime.LogEvent("error", "history", "append_failed", fmt.Sprintf("Persist incoming history failed (%T)", err))
					resultErr = errors.Join(resultErr, fmt.Errorf("save request to conversation history: %w", err))
					return
				}
			}
			if _, err := s.History.Append(message.Channel, message.Conversation, history.Entry{
				Role: "error", Content: resultErr.Error(), SourceMessageID: message.SourceMessageID, Terminal: true,
			}); err != nil {
				s.Runtime.LogEvent("error", "history", "error_append_failed", fmt.Sprintf("Persist error history failed (%T)", err))
				resultErr = errors.Join(resultErr, fmt.Errorf("save error to conversation history: %w", err))
			}
		}
	}()
	if s.Config.Extensions.Enabled {
		output, err := s.Hooks.Run(ctx, "message.received", messageHookPayload(message))
		if err != nil {
			return err
		}
		if output.Cancel {
			if err := recordUser(); err != nil {
				return err
			}
			if output.Message != "" {
				return s.localReply(message, output.Message, emit)
			}
			return nil
		}
		if text, ok := output.Payload["text"].(string); ok {
			message.Text = text
		}
	}
	if err := recordUser(); err != nil {
		return err
	}
	if strings.HasPrefix(message.Text, "/") {
		// Known commands stay deterministic. An unrecognized command falls
		// through to the active harness only when it accepts native
		// conversation input; its raw spelling and arguments are preserved.
		if err := s.handleCommand(ctx, message, emit); !errors.Is(err, errUnhandledCommand) {
			return err
		}
	}
	prompt, err := s.chatPrompt(message)
	if err != nil {
		return err
	}
	return s.dispatchHarnessPrompt(ctx, message, prompt, emit)
}

func messageHookPayload(message core.Message) map[string]any {
	return map[string]any{
		"channel": message.Channel, "conversation": message.Conversation,
		"sender": message.Sender, "text": message.Text, "reply_to": message.ReplyTo,
	}
}

// userHistoryEntry builds the authoritative user record. Admission persists it
// and prompt construction reuses the identical timestamp, role, sender,
// reply_to, redacted content, and source identity so the delivered current
// message can never drift from durable history.
func userHistoryEntry(message core.Message, acceptedAt time.Time) history.Entry {
	return history.Entry{
		At: message.ReceivedAt, AcceptedAt: acceptedAt, Role: "user",
		Sender: message.Sender, ReplyTo: message.ReplyTo,
		Content:         redactSensitiveCommand(message.Text),
		SourceMessageID: message.SourceMessageID,
	}
}

func (s *Service) dispatchHarnessPrompt(ctx context.Context, message core.Message, prompt string, emit core.Emit) error {
	if s.Config.Extensions.Enabled {
		output, err := s.Hooks.Run(ctx, "harness.before", map[string]any{
			"session_key": sessionKey(message), "prompt": prompt, "channel": message.Channel,
		})
		if err != nil {
			return err
		}
		if output.Cancel {
			return s.localReply(message, output.Message, emit)
		}
		if value, ok := output.Payload["prompt"].(string); ok {
			prompt = value
		}
	}
	key := sessionKey(message)
	jobID, jobCreated, err := s.Runtime.tryBeginJobWithDetails(key, message.Channel, message.Conversation, message.Text, JobDetails{Kind: "conversation", Provider: s.Settings.Snapshot().Harness.Name})
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}
	reservation, admission, err := s.reserveExecutionCorrelation(message)
	if err != nil {
		if jobCreated {
			s.Runtime.EndJob(jobID)
		}
		return err
	}
	executionID := reservation.executionID
	s.Runtime.LogEvent("info", "harness", "turn_started", "Harness turn started")
	activity := newChatActivityEmitter(emit)
	s.trackChatActivity(jobID, activity)
	activity.start()
	// Capture the pre-dispatch session identity on every surface so an empty
	// value always means the conversation had no prior session. Only the
	// surface-gated disclosure and notice helpers may reveal an identity.
	priorPiSession := s.currentPiSessionID(message)
	wrapped := s.wrapEmit(message, jobID, activity.emit)
	var threadID string
	var steered bool
	if sender, ok := s.Harness.(harness.ConversationSender); ok {
		threadID, steered, err = sender.SendConversation(ctx, key, prompt, message.Text, wrapped)
	} else {
		threadID, steered, err = s.Harness.Send(ctx, key, prompt, wrapped)
	}
	if err != nil {
		activity.stop()
		s.rollbackCorrelationReservation(message, reservation)
		s.Runtime.RecordJobEvent(jobID, core.Event{Kind: core.EventError, Text: err.Error(), Done: true})
		s.Runtime.LogEvent("error", "harness", "start_failed", "Harness turn failed to start ("+harnessFailureEvidence(err)+")")
		// A rejected follow-up does not end the original provider execution.
		if !s.Harness.IsActive(key) {
			s.Runtime.UpdateJob(jobID, core.ExecutionStatus{State: string(JobError), Detail: err.Error()})
			s.Runtime.EndJob(jobID)
		}
		return err
	}
	s.markPiSessionDisclosure(message, priorPiSession, threadID)
	s.nameNewPiSession(ctx, message, priorPiSession, threadID, steered)
	actualAdmission := "new"
	if steered {
		actualAdmission = "steered"
	}
	if admission == "followup" && admission != actualAdmission {
		_, _ = s.History.Append(message.Channel, message.Conversation, history.Entry{Role: "correlation", SourceMessageID: message.SourceMessageID, ExecutionID: executionID, Admission: actualAdmission})
	}
	s.Runtime.SetJobRunningIfStarting(jobID)
	if emit != nil {
		verb := "Harness turn started"
		if steered {
			verb = "Message steered into active harness turn"
		}
		emit(core.Event{Kind: core.EventStatus, Text: verb, ThreadID: threadID})
	}
	return nil
}

func harnessFailureEvidence(err error) string {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return fmt.Sprintf("exit_code=%d", exit.ExitCode())
	}
	return fmt.Sprintf("error_type=%T", err)
}

func (s *Service) chatPrompt(message core.Message) (string, error) {
	return s.chatPromptWithCurrent(message, userHistoryEntry(message, time.Now().UTC()))
}

// outboundAttachmentGuidance teaches provider harnesses the outbound media
// directive the application parses. Native-conversation harnesses never
// receive it; they own their own resources and system prompt.
const outboundAttachmentGuidance = "\n\nTo send a readable local file back through this channel, put one directive on its own line in the final response: `[Send attachment](</absolute/path/to/file>)`. For an image displayed as a native photo, use `[Send photo](</absolute/path/to/image.png>)`. Keep any user-facing caption as ordinary text. The path may be outside the active workspace, but it must be absolute and resolve to a regular file within the attachment size limit."

// nativeConversationInput reports whether the active harness receives the raw
// accepted conversation text for this session key and expands its own native
// skills, prompt templates, and extension commands. Absence or a false answer
// keeps the provider-neutral bounded-context fallback.
func (s *Service) nativeConversationInput(message core.Message) bool {
	provider, ok := s.Harness.(harness.NativeConversationInputProvider)
	if !ok {
		return false
	}
	return provider.NativeConversationInput(sessionKey(message))
}

// unhandledOrUnknownCommand keeps the deterministic framework refusal for a
// harness that cannot expand its own commands, and lets a
// native-conversation harness handle the exact raw input instead.
func (s *Service) unhandledOrUnknownCommand(message core.Message, command string, emit core.Emit) error {
	if s.nativeConversationInput(message) {
		return errUnhandledCommand
	}
	return s.localReply(message, "Unknown command /"+command+". Use /help.", emit)
}

// chatPromptWithCurrent renders the prompt around one explicit authoritative
// current user entry. A native-conversation harness receives only that entry,
// byte-for-byte, and expands its own native resources; every other harness
// receives the bounded provider-neutral history context.
func (s *Service) chatPromptWithCurrent(message core.Message, current history.Entry) (string, error) {
	if s.nativeConversationInput(message) {
		return current.Content, nil
	}
	cfg := s.Settings.Snapshot()
	// Query the retained-context capability as late as practical so a session
	// rotation shortly before dispatch is observed. Absence, uncertainty, or
	// unsupported providers keep the bounded seed; a retained provider session
	// receives only the current user entry.
	includePriorHistory := true
	if provider, ok := s.Harness.(harness.ConversationContextProvider); ok {
		includePriorHistory = !provider.ProvidesConversationContext(sessionKey(message))
	}
	recent, _, err := s.History.PromptContext(message.Channel, message.Conversation, current, includePriorHistory, cfg.Workspace.HistoryMaxMessages, cfg.Workspace.HistoryCharLimit)
	if err != nil {
		return "", err
	}
	if message.Channel == "telegram" || message.Channel == "whatsapp" {
		recent += outboundAttachmentGuidance
	}
	return recent, nil
}

func (s *Service) wrapEmit(message core.Message, jobID int, downstream core.Emit) core.Emit {
	var terminalMu sync.Mutex
	terminalDelivered := false
	// The same surface-independent capture applies here; the notice helper
	// itself remains gated by piControlSurface.
	priorPiSession := s.currentPiSessionID(message)
	piNoticeSent := false
	return func(event core.Event) {
		terminal := event.Done && !event.Continues && (event.Kind == core.EventFinal || event.Kind == core.EventError)
		if terminal {
			terminalMu.Lock()
			if terminalDelivered {
				terminalMu.Unlock()
				return
			}
			terminalDelivered = true
			terminalMu.Unlock()
		}
		key := sessionKey(message)
		// A queued request can be cancelled while the shared provider turn is
		// still active. Persist and deliver its result without settling that turn.
		requestOnly := terminal && s.Harness.IsActive(key)
		if event.Execution != nil {
			s.Runtime.UpdateJob(jobID, *event.Execution)
		} else if event.Kind == core.EventDelta {
			s.Runtime.UpdateJob(jobID, core.ExecutionStatus{State: string(JobRunning)})
		}
		if terminal && !requestOnly {
			state := JobFinishing
			if event.Kind == core.EventError {
				state = JobError
			}
			s.Runtime.UpdateJob(jobID, core.ExecutionStatus{State: string(state), Detail: map[bool]string{true: event.Text, false: ""}[event.Kind == core.EventError]})
		}
		if event.Kind == core.EventDelta {
			s.streamMu.Lock()
			s.streamText[key] += event.Text
			s.streamMu.Unlock()
		}
		if event.Done && (event.Kind == core.EventFinal || event.Kind == core.EventError) {
			var streamed string
			if !requestOnly {
				s.streamMu.Lock()
				streamed = s.streamText[key]
				delete(s.streamText, key)
				s.streamMu.Unlock()
			}
			text := event.Text
			remoteChannel := message.Channel == "telegram" || message.Channel == "whatsapp"
			hookText := text
			if remoteChannel && event.FinalText != nil {
				hookText = *event.FinalText
			}
			if s.Config.Extensions.Enabled {
				output, err := s.Hooks.Run(context.Background(), "harness.after", map[string]any{
					"session_key": sessionKey(message), "text": hookText, "kind": event.Kind,
				})
				if err != nil {
					event.Kind = core.EventError
					text = "harness.after extension hook failed: " + err.Error()
					event.FinalText = &text
				} else if output.Cancel {
					text = output.Message
					event.FinalText = &text
				} else {
					if value, ok := output.Payload["text"].(string); ok {
						if remoteChannel && event.FinalText != nil {
							if value != hookText {
								text = replaceFinalAssistantItem(text, hookText, value)
								event.FinalText = &value
							}
						} else {
							text = value
						}
					}
				}
			}
			if event.Kind == core.EventFinal && remoteChannel {
				cfg := s.Settings.Snapshot()
				maxBytes := int64(cfg.Workspace.AttachmentMaxMB) * 1024 * 1024
				cleaned, attachments, err := media.ParseOutbound(text, maxBytes)
				if err != nil {
					event.Kind = core.EventError
					text = "Spynel outbound attachment error: " + err.Error()
					event.FinalText = &text
					event.Attachments = nil
				} else {
					text = cleaned
					event.Attachments = attachments
					if event.FinalText != nil {
						finalText, finalAttachments, finalErr := media.ParseOutbound(*event.FinalText, maxBytes)
						if finalErr != nil {
							event.Kind = core.EventError
							text = "Spynel outbound attachment error: " + finalErr.Error()
							event.FinalText = &text
							event.Attachments = nil
						} else {
							event.FinalText = &finalText
							event.Attachments = finalAttachments
						}
					}
				}
			}
			event.Text = text
			historyText := text
			for _, attachment := range event.Attachments {
				historyText = strings.TrimSpace(historyText + "\n\n[Sent " + attachment.Kind + " " + attachment.Name + "]")
			}
			if event.Kind == core.EventError && streamed != "" {
				if _, err := s.History.Append(message.Channel, message.Conversation, history.Entry{At: time.Now().UTC(), Role: "assistant", Content: streamed}); err != nil {
					s.Runtime.LogEvent("error", "history", "partial_append_failed", fmt.Sprintf("Persist partial history failed (%T)", err))
				}
			}
			entry := history.Entry{At: time.Now().UTC(), Role: map[bool]string{true: "error", false: "assistant"}[event.Kind == core.EventError], Content: historyText, Terminal: true, SourceMessageID: message.SourceMessageID, FinalText: event.FinalText, Continues: event.Continues}
			if _, err := s.History.Append(message.Channel, message.Conversation, entry); err != nil {
				s.Runtime.LogEvent("error", "history", "terminal_append_failed", fmt.Sprintf("Persist terminal history failed (%T)", err))
			}
			if !event.Continues && !requestOnly {
				s.finishExecutionCorrelation(message, map[bool]string{true: "terminal_error", false: "terminal_assistant"}[event.Kind == core.EventError])
			}
		}
		// Capture after extension and outbound-media processing so validated
		// attachment paths and objects never enter the archive.
		s.Runtime.RecordJobEvent(jobID, event)
		// The new-session disclosure is a downstream decoration only: durable
		// history, hook payloads, and job archives keep the provider response
		// unaffected, and prepending keeps it outside Telegram's tail
		// truncation. Only non-continuing finals are decorated because remote
		// channels suppress intermediate continuations.
		if event.Kind == core.EventFinal && !event.Continues && !piNoticeSent {
			piNoticeSent = true
			if notice := s.takePiSessionNotice(message, priorPiSession); notice != "" {
				event.Text = notice + "\n\n" + event.Text
				if event.FinalText != nil {
					final := notice + "\n\n" + *event.FinalText
					event.FinalText = &final
				}
			}
		}
		if downstream != nil {
			downstream(event)
		}
		if terminal && !requestOnly {
			level, outcome := "info", "completed"
			if event.Kind == core.EventError {
				level, outcome = "error", "failed"
			}
			s.Runtime.LogEvent(level, "harness", "turn_"+outcome, "Harness turn "+outcome)
			s.Runtime.EndJob(jobID)
		}
	}
}

func replaceFinalAssistantItem(aggregate, previous, replacement string) string {
	if aggregate == previous {
		return replacement
	}
	if previous != "" && strings.HasSuffix(aggregate, previous) {
		return strings.TrimSuffix(aggregate, previous) + replacement
	}
	return aggregate
}

func (s *Service) handleCommand(ctx context.Context, message core.Message, emit core.Emit) error {
	commandLine := strings.TrimSpace(strings.TrimPrefix(message.Text, "/"))
	parts := strings.Fields(commandLine)
	if len(parts) == 0 {
		return s.localReply(message, commandHelp, emit)
	}
	command := strings.ToLower(parts[0])
	if message.Channel == "telegram" {
		command = strings.SplitN(command, "@", 2)[0]
		if command == "start" {
			return s.localReply(message, "Spynel is running.", emit)
		}
	}
	remainder := strings.TrimSpace(strings.TrimPrefix(commandLine, parts[0]))
	switch command {
	case "help":
		return s.localReply(message, helpFor(remainder), emit)
	case "commands":
		return s.localReply(message, commandHelp, emit)
	case "welcome":
		return s.localReply(message, s.welcomeMessage(message.Channel), emit)
	case "new":
		if message.Channel != "tui" {
			return s.localReply(message, "Starting a separate conversation with /new is available in the TUI; this channel keeps its stable conversation identity.", emit)
		}
		id, err := shortid.New()
		if err != nil {
			return fmt.Errorf("allocate new conversation: %w", err)
		}
		conversation := "new-" + id
		if _, err := s.History.Ensure("tui", conversation); err != nil {
			return fmt.Errorf("create new conversation: %w", err)
		}
		screen := s.WelcomeScreen()
		screen.Conversation = conversation
		if emit != nil {
			emit(core.Event{Kind: core.EventScreen, Screen: &screen, Local: true})
		}
		return nil
	case "status":
		status, err := s.statusText(message)
		if err != nil {
			return err
		}
		return s.localReply(message, status, emit)
	case "primary":
		return s.primaryCommand(message, emit)
	case "pi":
		return s.piCommand(ctx, message, remainder, emit)
	case "config":
		return s.configurationCommand(message, "config", remainder, emit)
	case "telegram":
		if message.Channel == "telegram" {
			return s.localReply(message, "Telegram cannot be configured from Telegram itself. Use the TUI or WhatsApp so a bad setting cannot lock you out of this channel.", emit)
		}
		return s.configurationCommand(message, "telegram", remainder, emit)
	case "whatsapp":
		if message.Channel == "whatsapp" {
			return s.localReply(message, "WhatsApp cannot be configured from WhatsApp itself. Use the TUI or Telegram so a bad setting cannot lock you out of this channel.", emit)
		}
		return s.configurationCommand(message, "whatsapp", remainder, emit)
	case "harness":
		return s.harnessCommand(message, remainder, emit)
	case "model":
		return s.modelCommand(ctx, message, remainder, emit)
	case "effort":
		return s.modelPropertyCommand(ctx, message, "effort", remainder, emit)
	case "speed":
		return s.modelPropertyCommand(ctx, message, "speed", remainder, emit)
	case "theme":
		return s.themeCommand(message, remainder, emit)
	case "title":
		if remainder == "" {
			return s.localReply(message, "Usage: /title <name>", emit)
		}
		if len([]rune(remainder)) > maxTitleChars {
			return s.localReply(message, fmt.Sprintf("Title is too long (maximum %d characters).", maxTitleChars), emit)
		}
		if err := fsx.AtomicWriteFile(s.Config.StatePath("tui-title"), []byte(remainder+"\n"), 0o600); err != nil {
			return s.localReply(message, "Cannot save title: "+err.Error(), emit)
		}
		s.publishTitle(remainder)
		return s.localReply(message, "Title changed to "+remainder+".", emit)
	case "history":
		_, path, err := s.History.Recent(message.Channel, message.Conversation, 1)
		if err != nil {
			return err
		}
		return s.localReply(message, "Full independent history: "+path, emit)
	case "resume":
		return s.resumeCommand(message, emit)
	case "log", "logs":
		return s.logCommand(message, remainder, emit)
	case "jobs":
		if strings.EqualFold(strings.TrimSpace(remainder), "recent") {
			return s.jobsRecentCommand(message, emit)
		}
		if strings.TrimSpace(remainder) != "" {
			return s.localReply(message, "Usage: /jobs [recent]", emit)
		}
		return s.localReply(message, formatJobs(s.Runtime.NumericJobs()), emit)
	case "job":
		return s.jobCommand(ctx, message, remainder, emit)
	case "clear":
		if err := s.Harness.ResetSession(sessionKey(message)); err != nil {
			return s.localReply(message, "Cannot clear this conversation: "+err.Error(), emit)
		}
		if err := s.History.Clear(message.Channel, message.Conversation); err != nil {
			return err
		}
		if emit != nil {
			emit(core.Event{Kind: core.EventFinal, Text: "Conversation history and harness thread cleared.", Clear: true, Done: true, Local: true})
		}
		return nil
	case "stop":
		key := sessionKey(message)
		cancellation := s.correlationCancellationSnapshot(key)
		job, hasJob := s.Runtime.JobForSession(key)
		if hasJob {
			job, hasJob = s.Runtime.ReserveJobCancellation(job.ID)
		}
		stopped, err := s.Harness.Interrupt(ctx, key)
		if err != nil {
			if hasJob {
				s.Runtime.RestoreJobAfterFailedCancellation(job)
			}
			return s.localReply(message, "Cannot stop the active execution: "+err.Error(), emit)
		}
		if !stopped {
			if hasJob {
				s.Runtime.RestoreJobAfterFailedCancellation(job)
			}
			return s.localReply(message, "There is no active execution to stop.", emit)
		}
		if hasJob {
			s.finishCancelledJobAfterGrace(job)
		}
		s.commitCorrelationCancellation(key, message.Channel, message.Conversation, cancellation)
		return s.localReply(message, "Stop requested for the active execution.", emit)
	case "restart":
		if err := s.localReply(message, "Restarting Spynel...", emit); err != nil {
			return err
		}
		s.requestRestart()
		return nil
	case "update":
		return s.updateCommand(ctx, message, remainder, emit)
	case "cleanup":
		return s.cleanupCommand(message, remainder, emit)
	case "extension", "extensions":
		return s.extensionCommand(ctx, message, remainder, emit)
	case "quit", "exit":
		return s.localReply(message, "/quit exits an interactive TUI only; it does not stop the Telegram or WhatsApp server.", emit)
	default:
		return s.unhandledOrUnknownCommand(message, command, emit)
	}
}

func (s *Service) finishCancelledJobAfterGrace(job Job) {
	grace := s.jobCancellationGrace
	if grace <= 0 {
		grace = jobCancellationGrace
	}
	time.AfterFunc(grace, func() {
		current, ok := s.Runtime.Job(job.ID)
		if ok && current.StableID == job.StableID && current.Execution == JobCancelling {
			s.stopJobChatActivity(job.ID)
			s.Runtime.EndJob(job.ID)
		}
	})
}

func (s *Service) trackChatActivity(jobID int, activity *chatActivityEmitter) {
	activity.onStop = func() {
		s.chatActivityMu.Lock()
		delete(s.chatActivity[jobID], activity)
		if len(s.chatActivity[jobID]) == 0 {
			delete(s.chatActivity, jobID)
		}
		s.chatActivityMu.Unlock()
	}
	s.chatActivityMu.Lock()
	if s.chatActivity[jobID] == nil {
		s.chatActivity[jobID] = map[*chatActivityEmitter]struct{}{}
	}
	s.chatActivity[jobID][activity] = struct{}{}
	s.chatActivityMu.Unlock()
}

func (s *Service) stopJobChatActivity(jobID int) {
	s.chatActivityMu.Lock()
	activities := s.chatActivity[jobID]
	delete(s.chatActivity, jobID)
	s.chatActivityMu.Unlock()
	for activity := range activities {
		activity.stop()
	}
}

func (s *Service) stopAllChatActivity() {
	s.chatActivityMu.Lock()
	activities := s.chatActivity
	s.chatActivity = map[int]map[*chatActivityEmitter]struct{}{}
	s.chatActivityMu.Unlock()
	for _, jobActivities := range activities {
		for activity := range jobActivities {
			activity.stop()
		}
	}
}

// chatActivityEmitter publishes the canonical main communication-agent
// activity boundary. It stops before forwarding a terminal response so remote
// channels never keep native activity visible during final delivery. A done
// status releases an emitter whose output ownership moved to a newer turn.
type chatActivityEmitter struct {
	downstream core.Emit
	mu         sync.Mutex
	started    bool
	stopped    bool
	onStop     func()
}

func newChatActivityEmitter(downstream core.Emit) *chatActivityEmitter {
	return &chatActivityEmitter{downstream: downstream}
}

func (e *chatActivityEmitter) start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return
	}
	e.started = true
	if e.downstream != nil {
		e.downstream(core.Event{Kind: core.EventActivity, Active: true})
	}
}

func (e *chatActivityEmitter) stop() {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	if e.started && e.downstream != nil {
		e.downstream(core.Event{Kind: core.EventActivity})
	}
	onStop := e.onStop
	e.mu.Unlock()
	if onStop != nil {
		onStop()
	}
}

func (e *chatActivityEmitter) emit(event core.Event) {
	terminal := event.Done && !event.Continues && (event.Kind == core.EventFinal || event.Kind == core.EventError)
	released := event.Done && event.Kind == core.EventStatus
	if terminal || released {
		e.stop()
	}
	if e.downstream != nil {
		e.downstream(event)
	}
}

func (s *Service) logCommand(message core.Message, remainder string, emit core.Emit) error {
	usage := "Usage: /log [page <number>|page <start>-<end>|search <text>|clear]"
	parts := strings.Fields(remainder)
	if len(parts) == 0 {
		return s.localReply(message, formatLogs(s.Runtime.Logs()), emit)
	}
	switch strings.ToLower(parts[0]) {
	case "page":
		if len(parts) != 2 {
			return s.localReply(message, usage, emit)
		}
		firstPage, lastPage, err := parseLogPageSpec(parts[1])
		if err != nil {
			return s.localReply(message, err.Error()+"\n\n"+usage, emit)
		}
		return s.localReply(message, formatLogPages(s.Runtime.Logs(), firstPage, lastPage), emit)
	case "search":
		query := strings.TrimSpace(strings.TrimPrefix(remainder, parts[0]))
		if query == "" {
			return s.localReply(message, "Usage: /log search <text>", emit)
		}
		return s.localReply(message, formatLogSearch(s.Runtime.Logs(), query), emit)
	case "clear":
		if len(parts) != 1 {
			return s.localReply(message, usage, emit)
		}
		count, err := s.Runtime.ClearLogsResult()
		if err != nil {
			return s.localReply(message, fmt.Sprintf("Cleared %d in-memory runtime log entries, but retained files could not be fully cleared: %v", count, err), emit)
		}
		return s.localReply(message, fmt.Sprintf("Cleared %d runtime log entries.", count), emit)
	default:
		return s.localReply(message, usage, emit)
	}
}

func parseLogPageSpec(spec string) (int, int, error) {
	if !strings.Contains(spec, "-") {
		page, err := strconv.Atoi(spec)
		if err != nil || page < 1 {
			return 0, 0, fmt.Errorf("log page must be a positive number or inclusive range such as 1-5")
		}
		return page, page, nil
	}
	if strings.Count(spec, "-") != 1 {
		return 0, 0, fmt.Errorf("log page must be a positive number or inclusive range such as 1-5")
	}
	bounds := strings.SplitN(spec, "-", 2)
	firstPage, firstErr := strconv.Atoi(bounds[0])
	lastPage, lastErr := strconv.Atoi(bounds[1])
	if firstErr != nil || lastErr != nil || firstPage < 1 || lastPage < 1 {
		return 0, 0, fmt.Errorf("log page range bounds must be positive numbers")
	}
	if firstPage > lastPage {
		return 0, 0, fmt.Errorf("log page range must run from a lower page to a higher page")
	}
	return firstPage, lastPage, nil
}

func (s *Service) jobCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	parts := strings.Fields(remainder)
	usage := "Usage: /job info <number> | /job output <number> [tail <bytes>] | /job kill <number>\n\nUse /jobs for live jobs or /jobs recent for archived jobs."
	if len(parts) < 2 {
		return s.localReply(message, usage, emit)
	}
	id, numericErr := strconv.Atoi(parts[1])
	switch strings.ToLower(parts[0]) {
	case "info":
		if len(parts) != 2 {
			return s.localReply(message, usage, emit)
		}
		if numericErr != nil || id < 1 || id > maxJobNumber {
			return s.localReply(message, fmt.Sprintf("Job number must be from 1 to %d. Use /jobs or /jobs recent to list jobs.", maxJobNumber), emit)
		}
		return s.jobInfoCommand(message, id, emit)
	case "output":
		if len(parts) != 2 && len(parts) != 4 || len(parts) == 4 && !strings.EqualFold(parts[2], "tail") {
			return s.localReply(message, usage, emit)
		}
		tailBytes := 16 << 10
		if len(parts) == 4 {
			value, err := strconv.Atoi(parts[3])
			if err != nil || value < 1 || value > 64<<10 {
				return s.localReply(message, "Job output tail must be from 1 to 65536 bytes.", emit)
			}
			tailBytes = value
		}
		if numericErr != nil || id < 1 || id > maxJobNumber {
			return s.localReply(message, fmt.Sprintf("Job number must be from 1 to %d. Use /jobs or /jobs recent to list jobs.", maxJobNumber), emit)
		}
		return s.jobOutputCommand(message, id, tailBytes, emit)
	case "kill":
		if numericErr != nil || id < 1 || id > maxJobNumber {
			return s.localReply(message, fmt.Sprintf("Job control requires a live job number from 1 to %d.", maxJobNumber), emit)
		}
		if len(parts) != 2 {
			return s.localReply(message, usage, emit)
		}
	default:
		return s.localReply(message, usage, emit)
	}
	liveJob, live := s.Runtime.JobByNumber(id)
	var job Job
	ok := false
	if live {
		job, ok = s.Runtime.ReserveJobCancellation(liveJob.ID)
	}
	if !ok {
		if _, _, err := s.Runtime.ArchivedJob(id); err == nil {
			return s.localReply(message, fmt.Sprintf("Job %d is completed and cannot be killed; killing is available only for live jobs.", id), emit)
		}
		return s.localReply(message, fmt.Sprintf("Job %d is not running. Use /jobs to list running jobs.", id), emit)
	}
	cancellation := s.correlationCancellationSnapshot(job.SessionKey)
	stopped, err := s.Harness.Interrupt(ctx, job.SessionKey)
	if err != nil {
		s.Runtime.RestoreJobAfterFailedCancellation(job)
		return s.localReply(message, fmt.Sprintf("Cannot kill job %d: %v", id, err), emit)
	}
	if !stopped {
		s.Runtime.RestoreJobAfterFailedCancellation(job)
		if s.Harness.IsActive(job.SessionKey) {
			return s.localReply(message, fmt.Sprintf("Cannot kill job %d: the provider did not accept the interrupt request.", id), emit)
		}
		s.stopJobChatActivity(job.ID)
		s.Runtime.EndJob(job.ID)
		return s.localReply(message, fmt.Sprintf("Job %d was already finished.", id), emit)
	}
	s.Runtime.LogEvent("info", "jobs", "job_stop_requested", fmt.Sprintf("job_id=%d channel=%s kind=%s", job.Number, logField(job.Channel, "unknown"), logField(job.Kind, "chat")))
	s.commitCorrelationCancellation(job.SessionKey, job.Channel, job.Conversation, cancellation)
	s.finishCancelledJobAfterGrace(job)
	return s.localReply(message, fmt.Sprintf("Kill requested for job %d.", id), emit)
}

// TitleChanges publishes persisted title updates so a running TUI can reflect
// commands received from any channel.
func (s *Service) TitleChanges() <-chan string {
	return s.titleChanges
}

func (s *Service) publishTitle(title string) {
	s.titleMu.Lock()
	defer s.titleMu.Unlock()
	select {
	case <-s.titleChanges:
	default:
	}
	s.titleChanges <- title
}

// RestartRequests publishes process-wide restart requests after the command
// acknowledgment has been persisted and emitted to the issuing channel.
func (s *Service) RestartRequests() <-chan struct{} {
	return s.restartRequests
}

func (s *Service) requestRestart() {
	select {
	case s.restartRequests <- struct{}{}:
	default:
	}
}

// UpdateRequests publishes source-specific update/restart requests after the
// acknowledgement is persisted. The CLI owns complete runtime shutdown.
func (s *Service) UpdateRequests() <-chan updater.Result {
	return s.updateRequests
}

func (s *Service) requestUpdate(result updater.Result) {
	select {
	case s.updateRequests <- result:
	default:
	}
}

func (s *Service) updateCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	if s.Updates == nil {
		return s.localReply(message, "Updates are unavailable in this build. Install Spynel through npm or the install script to use `/update`.", emit)
	}
	action := strings.ToLower(strings.TrimSpace(remainder))
	if action != "" && action != "install" && action != "check" {
		return s.localReply(message, "Usage: /update [check]", emit)
	}
	result, err := s.Updates.Check(ctx)
	if err != nil {
		return s.localReply(message, "Update check skipped: "+err.Error()+". Try `/update` again later.", emit)
	}
	if result.Source == "" {
		return s.localReply(message, "This Spynel binary is unmanaged. Download a newer release using the same installation method.", emit)
	}
	if !result.Available && action == "check" {
		latest := result.Latest
		if latest == "" {
			latest = result.Current
		}
		return s.localReply(message, fmt.Sprintf("Spynel %s is current; %s also reports %s.", result.Current, result.Source, latest), emit)
	}
	if action == "check" {
		if result.CanAutoInstall {
			return s.localReply(message, fmt.Sprintf("Spynel %s is installed through %s; %s is available. Run `/update` to update and restart all instances of this installation.", result.Current, result.Source, result.Latest), emit)
		}
		return s.localReply(message, fmt.Sprintf("Spynel %s is installed through npm; %s is available. Run `%s`, then `/restart`. This process was not launched by the npm wrapper, so it cannot replace itself safely.", result.Current, result.Latest, result.Command), emit)
	}
	if !result.CanAutoInstall {
		return s.localReply(message, fmt.Sprintf("Run `%s`, then `/restart`. Updating in place is unavailable because this process was not launched by the npm wrapper.", result.Command), emit)
	}
	if err := s.Updates.PrepareUpdate(ctx, result); err != nil {
		return err
	}
	notice := fmt.Sprintf("Restarting all instances of this installation at Spynel %s.", result.Current)
	if result.Available {
		notice = fmt.Sprintf("Updating Spynel from %s to %s with %s, then restarting all instances of this installation.", result.Current, result.Latest, result.Source)
	}
	if err := s.localReply(message, notice, emit); err != nil {
		return err
	}
	s.requestUpdate(result)
	return nil
}

// PrimaryRequests publishes a target-specific request after /primary has
// acknowledged the requesting local TUI and verified that no jobs are active.
func (s *Service) PrimaryRequests() <-chan string {
	return s.primaryRequests
}

func (s *Service) primaryCommand(message core.Message, emit core.Emit) error {
	if message.Channel != "tui" {
		return s.localReply(message, "/primary is available only from a local TUI instance.", emit)
	}
	primaryID := s.primaryInstanceID()
	if message.InstanceID == "" || primaryID == "" {
		return s.localReply(message, "Cannot identify both this instance and the current primary instance.", emit)
	}
	if message.InstanceID == primaryID {
		return s.localReply(message, "This instance is already the primary instance.", emit)
	}
	if jobs := s.Runtime.Status().Jobs; jobs != 0 {
		activity := fmt.Sprintf("%d agent jobs are", jobs)
		if jobs == 1 {
			activity = "1 agent job is"
		}
		return s.localReply(message, "Cannot transfer primary ownership while "+activity+" running. Use `/jobs` to inspect active work and retry when the workspace is idle.", emit)
	}
	s.primaryRequestMu.Lock()
	if s.primaryRequested {
		s.primaryRequestMu.Unlock()
		return s.localReply(message, "A primary handoff is already in progress.", emit)
	}
	s.primaryRequested = true
	s.primaryRequestMu.Unlock()
	if err := s.localReply(message, "Primary handoff requested. This instance will take ownership after the current primary safely stops its owner-only services.", emit); err != nil {
		s.primaryRequestMu.Lock()
		s.primaryRequested = false
		s.primaryRequestMu.Unlock()
		return err
	}
	s.primaryRequests <- message.InstanceID
	return nil
}

// SetConnectionStatus keeps shared channel health available to commands on
// every transport, not only to the TUI that renders the live badges.
func (s *Service) SetConnectionStatus(status channel.ConnectionStatus) {
	if status.Name == "" {
		return
	}
	s.connectionMu.Lock()
	s.connections[status.Name] = status
	s.connectionMu.Unlock()
}

func (s *Service) connectionStatus(name string) channel.ConnectionStatus {
	s.connectionMu.RLock()
	status, ok := s.connections[name]
	s.connectionMu.RUnlock()
	if !ok {
		return channel.ConnectionStatus{Name: name, State: channel.ConnectionUnconfigured}
	}
	return status
}

// SetConversationDelivery installs the ordinary remote response router.
func (s *Service) SetConversationDelivery(router channel.DeliveryRouter) {
	s.conversationDeliveryMu.Lock()
	s.ConversationDelivery = router
	s.conversationDeliveryMu.Unlock()
}

func (s *Service) conversationDelivery() channel.DeliveryRouter {
	s.conversationDeliveryMu.RLock()
	router := s.ConversationDelivery
	s.conversationDeliveryMu.RUnlock()
	return router
}

// StatusSnapshot is the bounded, non-secret operational state exposed to
// plain CLI clients and rendered by /status on every channel.
type StatusSnapshot struct {
	Title           string                     `json:"title"`
	Instance        string                     `json:"instance,omitempty"`
	PrimaryInstance string                     `json:"primary_instance,omitempty"`
	Connections     []channel.ConnectionStatus `json:"connections"`
	Runtime         core.RuntimeStatus         `json:"runtime"`
	Harness         string                     `json:"harness,omitempty"`
	HarnessState    string                     `json:"harness_state"`
	HarnessDetail   string                     `json:"harness_detail,omitempty"`
	Model           string                     `json:"model,omitempty"`
	ReasoningEffort string                     `json:"reasoning_effort,omitempty"`
	ServiceMode     string                     `json:"service_mode,omitempty"`
	Sandbox         string                     `json:"sandbox"`
	StartupEnabled  bool                       `json:"startup_enabled"` // Saved preference, not observed OS registration.
	TurnActive      bool                       `json:"turn_active"`
}

// Status returns the same status contract used by /status without requiring a
// caller to parse presentation Markdown. Opaque identifiers stay shortened.
func (s *Service) Status(message core.Message) (StatusSnapshot, error) {
	cfg := s.Settings.Snapshot()
	primaryInstanceID := s.primaryInstanceID()
	instanceID := message.InstanceID
	if instanceID == "" {
		// Telegram and WhatsApp execute inside the primary process rather than
		// crossing the loopback API, so their caller is the owner itself.
		instanceID = primaryInstanceID
	}
	harnessState := "connected"
	harnessDetail := ""
	if availability, ok := s.Harness.(harness.Availability); ok {
		if ready, detail := availability.Available(); !ready {
			harnessState = "unavailable"
			harnessDetail = detail
		}
	}
	return StatusSnapshot{
		Title:    s.currentTitle(),
		Instance: shortid.Display(instanceID), PrimaryInstance: shortid.Display(primaryInstanceID),
		Connections: []channel.ConnectionStatus{s.connectionStatus("telegram"), s.connectionStatus("whatsapp")},
		Runtime:     s.Runtime.Status(), Harness: cfg.Harness.Name, HarnessState: harnessState,
		HarnessDetail: harnessDetail, Model: cfg.Harness.Model, ReasoningEffort: cfg.Harness.ReasoningEffort, ServiceMode: cfg.Harness.ServiceMode, Sandbox: cfg.Harness.Sandbox,
		StartupEnabled: cfg.Startup.Enabled, TurnActive: s.Harness.IsActive(sessionKey(message)),
	}, nil
}

func (s *Service) statusText(message core.Message) (string, error) {
	status, err := s.Status(message)
	if err != nil {
		return "", err
	}
	return FormatStatus(status), nil
}

// FormatStatus renders a StatusSnapshot for text terminals and chat channels.
func FormatStatus(status StatusSnapshot) string {
	harnessStatus := status.HarnessState
	if status.HarnessDetail != "" {
		harnessStatus += " — " + status.HarnessDetail
	}
	turn := "idle"
	if status.TurnActive {
		turn = "active"
	}
	telegram := channel.ConnectionStatus{Name: "telegram", State: channel.ConnectionUnconfigured}
	whatsapp := channel.ConnectionStatus{Name: "whatsapp", State: channel.ConnectionUnconfigured}
	for _, connection := range status.Connections {
		switch connection.Name {
		case "telegram":
			telegram = connection
		case "whatsapp":
			whatsapp = connection
		}
	}
	lines := []string{
		"# Status",
		"",
		"- Title: " + status.Title,
		"- Instance ID: " + statusID(status.Instance),
		"- Primary instance ID: " + statusID(status.PrimaryInstance),
		fmt.Sprintf("- Jobs: %d — `/jobs`", status.Runtime.Jobs),
		"- Telegram: " + connectionIndicator(telegram),
		"- WhatsApp: " + connectionIndicator(whatsapp),
		"- Coding harness: " + emptyAs(status.Harness, "not selected") + " (" + harnessStatus + ")",
		"- Model: " + emptyAs(status.Model, "harness default"),
		"- Reasoning effort: " + emptyAs(status.ReasoningEffort, "inherit"),
		"- Service mode: " + emptyAs(status.ServiceMode, "inherit"),
		"- Agent filesystem access: " + status.Sandbox,
		"- Autostart preference: " + enabledText(status.StartupEnabled) + " (use /configure to verify registration)",
		fmt.Sprintf("- Logs: %d — `/log`", status.Runtime.Logs),
		"- Turn: " + turn,
	}
	return strings.Join(lines, "\n")
}

func statusID(value string) string {
	display := shortid.Display(value)
	if display == "" {
		return "not available"
	}
	return "`" + display + "`"
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (s *Service) currentTitle() string {
	title := s.Settings.Snapshot().Channels.TUI.Title
	data, err := os.ReadFile(s.Config.StatePath("tui-title"))
	if err == nil && strings.TrimSpace(string(data)) != "" {
		title = strings.TrimSpace(string(data))
	}
	return title
}

func connectionIndicator(status channel.ConnectionStatus) string {
	switch status.State {
	case channel.ConnectionConnected:
		return "● connected"
	case channel.ConnectionConnecting:
		return "◐ connecting"
	case channel.ConnectionError:
		if detail := strings.TrimSpace(strings.ReplaceAll(status.Detail, "\n", " ")); detail != "" {
			return "▲ error — " + detail
		}
		return "▲ error"
	default:
		return "○ not configured"
	}
}

func (s *Service) extensionCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	parts := strings.Fields(remainder)
	directory := s.Config.Resolve(s.Config.Extensions.Directory)
	if len(parts) == 0 || parts[0] == "list" {
		names, err := extensions.List(directory)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return s.localReply(message, "No project extensions are installed.", emit)
		}
		return s.localReply(message, "Installed extensions:\n- "+strings.Join(names, "\n- "), emit)
	}
	switch parts[0] {
	case "install":
		if len(parts) < 2 {
			return s.localReply(message, "Usage: /extension install <git-url> [name]", emit)
		}
		name := ""
		if len(parts) > 2 {
			name = parts[2]
		}
		path, err := extensions.Install(ctx, directory, parts[1], name, s.Runtime.Writer("extension.install"))
		if err != nil {
			return err
		}
		return s.localReply(message, "Installed trusted extension: "+path, emit)
	case "remove":
		if len(parts) != 2 {
			return s.localReply(message, "Usage: /extension remove <name>", emit)
		}
		if err := extensions.Remove(directory, parts[1]); err != nil {
			return err
		}
		return s.localReply(message, "Removed extension "+parts[1]+". This cannot be recovered from Spynel; reinstall the Git repository if needed.", emit)
	default:
		return s.localReply(message, "Usage: /extension [list|install <git-url> [name]|remove <name>]", emit)
	}
}

func (s *Service) localReply(message core.Message, text string, emit core.Emit) error {
	if text == "" {
		text = "Command completed."
	}
	_, err := s.History.Append(message.Channel, message.Conversation, history.Entry{
		At: time.Now().UTC(), Role: "assistant", Content: text, SourceMessageID: message.SourceMessageID, Terminal: true,
	})
	if err != nil {
		return err
	}
	if emit != nil {
		emit(core.Event{Kind: core.EventFinal, Text: text, Done: true, Local: true})
	}
	return nil
}

func sessionKey(message core.Message) string {
	return "chat:" + message.Channel + ":" + message.Conversation
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var slashCommands = []core.SlashCommand{
	{Value: "/help", Usage: "/help", Description: "Show the help topic index"},
	{Value: "/help about", Usage: "/help about", Description: "Learn what Spynel does and where it stores state"},
	{Value: "/help commands", Usage: "/help commands", Description: "Show the complete slash-command reference"},
	{Value: "/help extensions", Usage: "/help extensions", Description: "Learn how trusted project extensions work"},
	{Value: "/help config", Usage: "/help config", Description: "Understand .spynel/config.yaml and path resolution"},
	{Value: "/help channels", Usage: "/help channels", Description: "Learn about the TUI, Telegram, and WhatsApp"},
	{Value: "/status", Usage: "/status", Description: "Show runtime, channel, and harness state"},
	{Value: "/primary", Usage: "/primary", Description: "Safely make this TUI instance the workspace primary"},
	{Value: "/welcome", Usage: "/welcome", Description: "Print the Spynel welcome guide in this conversation"},
	{Value: "/config", Usage: "/config", Description: "Open or show Spynel configuration"},
	{Value: "/config get ", Usage: "/config get <key>", Description: "Read one configuration value"},
	{Value: "/config set ", Usage: "/config set <key> <value>", Description: "Persist one configuration value"},
	{Value: "/config unset ", Usage: "/config unset <key>", Description: "Clear one stored configuration value"},
	{Value: "/harness ", Usage: "/harness [name]", Description: "Show or select the coding harness"},
	{Value: "/model ", Usage: "/model [name]", Description: "Show or select the harness model"},
	{Value: "/effort ", Usage: "/effort [level|inherit]", Description: "Show, select, or reset reasoning effort"},
	{Value: "/speed ", Usage: "/speed [mode|inherit]", Description: "Show, select, or reset a model service mode"},
	{Value: "/theme", Usage: "/theme [name]", Description: "Preview or select a color theme"},
	{Value: "/telegram", Usage: "/telegram", Description: "Open or show Telegram configuration"},
	{Value: "/telegram on", Usage: "/telegram on", Description: "Enable Telegram"},
	{Value: "/telegram off", Usage: "/telegram off", Description: "Disable Telegram"},
	{Value: "/telegram set ", Usage: "/telegram set <key> <value>", Description: "Persist a Telegram setting"},
	{Value: "/whatsapp", Usage: "/whatsapp", Description: "Open or show WhatsApp configuration"},
	{Value: "/whatsapp on", Usage: "/whatsapp on", Description: "Enable WhatsApp"},
	{Value: "/whatsapp off", Usage: "/whatsapp off", Description: "Disable WhatsApp"},
	{Value: "/whatsapp set ", Usage: "/whatsapp set <key> <value>", Description: "Persist a WhatsApp setting"},
	{Value: "/title ", Usage: "/title <name>", Description: "Rename and persist this TUI window"},
	{Value: "/new", Usage: "/new", Description: "Start a distinct TUI conversation and preserve this one"},
	{Value: "/stop", Usage: "/stop", Description: "Stop the active execution for this conversation"},
	{Value: "/pi", Usage: "/pi session | /pi name <name> | /pi compact [instructions] | /pi import <full-session-id>", Description: "Inspect or manage the Pi session from the TUI or a private Telegram chat"},
	{Value: "/pi session", Usage: "/pi session", Description: "Show the full Pi session ID and a safe direct Pi command"},
	{Value: "/pi name ", Usage: "/pi name <name>", Description: "Name this conversation's Pi session and private Telegram topic"},
	{Value: "/pi compact ", Usage: "/pi compact [instructions]", Description: "Compact this conversation's Pi session context"},
	{Value: "/pi import ", Usage: "/pi import <full-session-id>", Description: "Fork an existing direct Pi session into this conversation"},
	{Value: "/restart", Usage: "/restart", Description: "Restart Spynel and restore saved state"},
	{Value: "/update", Usage: "/update", Description: "Update and restart all instances of this installation"},
	{Value: "/update check", Usage: "/update check", Description: "Check versions without updating or restarting"},
	{Value: "/history", Usage: "/history", Description: "Show the complete history file"},
	{Value: "/resume", Usage: "/resume", Description: "Browse saved conversations and branch one into the TUI"},
	{Value: "/log", Usage: "/log", Description: "Show the newest page of captured runtime logs"},
	{Value: "/log page ", Usage: "/log page <number|start-end>", Description: "Show one or a range of available runtime log pages"},
	{Value: "/log search ", Usage: "/log search <text>", Description: "Search captured runtime logs"},
	{Value: "/log clear", Usage: "/log clear", Description: "Clear captured runtime logs"},
	{Value: "/jobs", Usage: "/jobs", Description: "List running agent jobs"},
	{Value: "/jobs recent", Usage: "/jobs recent", Description: "List recent job archives by number"},
	{Value: "/job info ", Usage: "/job info <number>", Description: "Show bounded live or archived job metadata"},
	{Value: "/job output ", Usage: "/job output <number> [tail <bytes>]", Description: "Show bounded captured job output"},
	{Value: "/job kill ", Usage: "/job kill <number>", Description: "Stop a running agent job by number"},
	{Value: "/clear", Usage: "/clear", Description: "Clear this conversation's history and harness thread"},
	{Value: "/cleanup ", Usage: "/cleanup [days]", Description: "Remove old conversations and job archives"},
	{Value: "/extension list", Usage: "/extension list", Description: "List installed project extensions"},
	{Value: "/extension install ", Usage: "/extension install URL", Description: "Install a trusted Git extension"},
	{Value: "/extension remove ", Usage: "/extension remove NAME", Description: "Remove an installed extension"},
	{Value: "/quit", Usage: "/quit", Description: "Exit the TUI only (Telegram and WhatsApp use server lifecycle)"},
}

const maxTitleChars = 80

var commandHelp = formatCommandHelp(slashCommands)

var helpTopics = []struct {
	name        string
	description string
	body        string
}{
	{
		name:        "about",
		description: "What Spynel does and where it stores state",
		body:        "# About Spynel\n\n**Simplicity at scale.** Spynel is a classic, non-AI program that coordinates and oversees external AI/coding harnesses through one assistant-facing relationship. The harness supplies intelligence and tools; Spynel supplies the transports, durable history, runtime oversight, and deterministic commands.\n\n**One human → one agent → infinite agents.** The middle agent is the assistant relationship, not Spynel itself, and infinite expresses scalable leverage rather than a literal resource guarantee. Use Spynel from a terminal or channels such as Telegram on a phone.\n\nThe project configuration is `.spynel/config.yaml`. Runtime state, histories, harness sessions, attachments, and local UI preferences live beside it in the fixed private `.spynel` directory.\n\n**Simplicity. Leverage. Quality.**",
	},
	{
		name:        "commands",
		description: "The complete slash-command reference",
		body:        commandHelp,
	},
	{
		name:        "extensions",
		description: "Trusted project extensions and their hooks",
		body:        "# Extensions\n\nExtensions are explicitly installed Git repositories that can run the supported message and harness hooks. Manifests using unknown hook names are rejected so dead configuration cannot be silently retained. Their directory and whether hooks are enabled are configured under `extensions` in `.spynel/config.yaml`.\n\n- `/extension list` lists installed extensions.\n- `/extension install <git-url> [name]` installs a repository you trust.\n- `/extension remove <name>` removes an installed extension.",
	},
	{
		name:        "config",
		description: ".spynel/config.yaml settings and path resolution",
		body:        "# Configuration\n\n`.spynel/config.yaml` controls the workspace, harness, channels, speech processing, and extensions. `/config` shows the shared settings, `/config get <key>` reads one value, and `/config set <key> <value>` atomically validates and persists a change from any channel. `/harness [name]` and `/model [name]` are concise selectors; model and supported-harness effort lists end with Custom text entry even if detection fails; `/effort` accepts detected or manually entered reasoning identifiers, `/speed` requires detected service support, and `inherit` resets either property. Model-property changes can be saved during active work: the current turn keeps its captured model, effort, and service mode, while subsequent provider dispatches use the new atomic selection. `/theme [name]` previews/lists or selects a semantic palette from `.spynel/themes`. All harness settings live in the `harness` group. `harness.sandbox` accepts `danger-full-access`, `workspace-write`, or `read-only`; unrestricted access is the default. Relative paths resolve from the workspace root, one directory above `.spynel`, so a project can be moved without rewriting local paths.",
	},
	{
		name:        "channels",
		description: "The TUI, Telegram, and WhatsApp",
		body:        "# Channels\n\nThe TUI, each Telegram chat, and each WhatsApp chat keep independent durable histories and harness threads. All channels share the application slash commands and Markdown-aware responses.\n\nUse `/status` to inspect shared connection, runtime, harness, and instance indicators. From an idle local TUI, `/primary` safely hands workspace ownership to that TUI instance. Use `/history` to locate the current conversation's history file, `/clear` to erase that history and discard its harness thread, `/stop` to interrupt its active execution, and `/new` to switch the TUI to a distinct conversation while preserving the prior one for `/resume`. `/restart` acknowledges the request, cleanly stops the current runtime, and relaunches Spynel with saved configuration and histories intact. `/update` updates the owning installation and restarts all its running instances across workspaces. `/update check` only checks versions, with a ten-second deadline. The shell command `spynel killall` stops all running Spynel instances, preserving saved workspace state and future autostart registrations. `/log` shows bounded runtime diagnostics. `/jobs` lists active executions and `/jobs recent` lists archived executions by the same numeric reference; `/job info <number>` and `/job output <number>` inspect bounded metadata or captured output, and `/job kill <number>` stops one live job. Pi session controls are available only in the TUI and private Telegram conversations: `/pi session` shows the full Pi session ID and a shell-safe direct Pi command, `/pi compact [instructions]` compacts the existing idle session, and `/pi import <full-session-id>` forks a validated direct Pi session into Spynel. `/clear` resets this conversation's session before a different import.",
	},
}

var helpOverview = formatHelpOverview()

// SlashCommands returns the canonical command catalog used by interactive
// channel affordances. The returned slice is safe for the caller to modify.
func SlashCommands() []core.SlashCommand {
	return append([]core.SlashCommand(nil), slashCommands...)
}

func formatCommandHelp(commands []core.SlashCommand) string {
	lines := []string{"# Commands", ""}
	for _, command := range commands {
		lines = append(lines, fmt.Sprintf("- `%s` — %s", command.Usage, command.Description))
	}
	return strings.Join(lines, "\n")
}

func formatHelpOverview() string {
	lines := []string{
		"# Spynel help",
		"",
		"Spynel is a classic, non-AI program that coordinates and oversees external AI/coding harnesses through one assistant relationship.",
		"",
		"Choose a help topic:",
		"",
	}
	// The concise human index and the agent-facing index share curated topic
	// metadata even though /help retains its shorter channel-specific bodies.
	for _, topic := range agentdocs.HelpTopics() {
		lines = append(lines, fmt.Sprintf("- `/help %s` — %s", topic.ID, topic.Summary))
	}
	return strings.Join(lines, "\n")
}

func helpFor(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return helpOverview
	}
	if name == "configuration" {
		name = "config"
	}
	for _, topic := range helpTopics {
		if topic.name == name {
			return topic.body
		}
	}
	return fmt.Sprintf("Unknown help topic `%s`.\n\n%s", name, helpOverview)
}

func (s *Service) HistoryPath(channel, conversation string) string {
	return filepath.Clean(s.History.Path(channel, conversation))
}
