package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/digitalygo/spynel/internal/channel/telegram"
	"github.com/digitalygo/spynel/internal/core"
	"github.com/digitalygo/spynel/internal/harness"
)

// piCommandUsage is the exact usage reply for a missing or unknown
// subcommand; every `/pi` surface returns it unchanged.
const piCommandUsage = "Usage: /pi session | /pi name <name> | /pi compact [instructions] | /pi import <full-session-id>"

// piSessionNameMaxRunes bounds one explicit session name after edge
// trimming. The Telegram topic adapter independently enforces its own label
// bound when the same name is applied to a topic.
const piSessionNameMaxRunes = 128

const (
	piControlsUnavailable = "Pi session controls are available only in the local TUI and private Telegram conversations."
	piControlsUnsupported = "The active coding harness does not provide Pi session controls."
)

// piControlSurface reports whether one conversation may receive full Pi
// session identities or control operations. Telegram classification uses the
// canonical route parser, so groups, topics of groups, foreign routes, and
// every other channel are refused before any capability call.
func piControlSurface(message core.Message) bool {
	switch message.Channel {
	case "tui":
		return true
	case "telegram":
		route, err := telegram.ParseConversation(message.Conversation)
		return err == nil && !route.IsGroup()
	default:
		return false
	}
}

// currentPiSessionID returns the conversation's session identity without
// starting a provider process. It fails closed to an empty string.
func (s *Service) currentPiSessionID(message core.Message) string {
	inspector, ok := s.Harness.(harness.SessionInspector)
	if !ok {
		return ""
	}
	info, found, err := inspector.SessionInfo(sessionKey(message))
	if err != nil || !found {
		return ""
	}
	return info.ID
}

// piSessionDisclosure tracks the one-time new-session disclosure per
// conversation. announced is the session identity already disclosed in this
// process, and pending is a session identity returned by a successful
// dispatch that has not yet reached a terminal final on any emitter.
type piSessionDisclosure struct {
	announced string
	pending   string
}

// markPiSessionDisclosure records that one successful dispatch returned a
// session identity different from the pre-dispatch identity. The marker is
// claimed by the next terminal final on any emitter for the conversation, so
// a session created by a turn that is steered before its own response still
// receives exactly one disclosure.
func (s *Service) markPiSessionDisclosure(message core.Message, prior, threadID string) {
	if !piControlSurface(message) || threadID == "" || threadID == prior {
		return
	}
	key := sessionKey(message)
	s.piNoticeMu.Lock()
	state := s.piNoticeState[key]
	state.pending = threadID
	s.piNoticeState[key] = state
	s.piNoticeMu.Unlock()
}

// takePiSessionNotice builds the one-time new-session disclosure for an
// allowed surface. It stays empty when the session is unchanged, the harness
// has no session inspection, the surface is not allowed, or the lookup fails
// closed.
func (s *Service) takePiSessionNotice(message core.Message, prior string) string {
	if !piControlSurface(message) {
		return ""
	}
	inspector, ok := s.Harness.(harness.SessionInspector)
	if !ok {
		return ""
	}
	key := sessionKey(message)
	info, found, err := inspector.SessionInfo(key)
	if err != nil || !found || info.ID == "" {
		return ""
	}
	s.piNoticeMu.Lock()
	state := s.piNoticeState[key]
	switch {
	case state.announced == info.ID:
		if state.pending == info.ID {
			state.pending = ""
			s.piNoticeState[key] = state
		}
		s.piNoticeMu.Unlock()
		return ""
	case state.pending == info.ID || prior != info.ID:
		state.announced = info.ID
		state.pending = ""
		s.piNoticeState[key] = state
		s.piNoticeMu.Unlock()
		return "Pi session `" + info.ID + "`. Run `/pi session` for direct-access details."
	default:
		s.piNoticeMu.Unlock()
		return ""
	}
}

func (s *Service) piCommand(ctx context.Context, message core.Message, remainder string, emit core.Emit) error {
	if !piControlSurface(message) {
		return s.localReply(message, piControlsUnavailable, emit)
	}
	fields := strings.Fields(remainder)
	if len(fields) == 0 {
		return s.localReply(message, piCommandUsage, emit)
	}
	switch strings.ToLower(fields[0]) {
	case "session":
		if len(fields) != 1 {
			return s.localReply(message, piCommandUsage, emit)
		}
		return s.piSessionCommand(message, emit)
	case "name":
		return s.piNameCommand(ctx, message, strings.TrimSpace(strings.TrimPrefix(remainder, fields[0])), emit)
	case "compact":
		return s.piCompactCommand(ctx, message, strings.TrimSpace(strings.TrimPrefix(remainder, fields[0])), emit)
	case "import":
		if len(fields) != 2 {
			return s.localReply(message, piCommandUsage, emit)
		}
		return s.piImportCommand(ctx, message, fields[1], emit)
	default:
		return s.localReply(message, piCommandUsage, emit)
	}
}

func (s *Service) piSessionCommand(message core.Message, emit core.Emit) error {
	inspector, ok := s.Harness.(harness.SessionInspector)
	if !ok {
		return s.localReply(message, piControlsUnsupported, emit)
	}
	info, found, err := inspector.SessionInfo(sessionKey(message))
	if err != nil {
		if errors.Is(err, harness.ErrSessionControlsUnsupported) {
			return s.localReply(message, piControlsUnsupported, emit)
		}
		return s.piControlFailure(message, "Cannot inspect the Pi session: ", err, emit)
	}
	if !found {
		return s.localReply(message, "No Pi session exists for this conversation yet. The first ordinary prompt creates one.", emit)
	}
	return s.localReply(message, piSessionReply(info), emit)
}

// piNameCommand assigns one explicit display name to an existing Pi session.
// It never creates a session, is safe during an active turn, and additionally
// renames a private Telegram topic so an explicitly named session can replace
// an implicit topic title. A failed topic rename reports partial success and
// never rolls the provider name back.
func (s *Service) piNameCommand(ctx context.Context, message core.Message, name string, emit core.Emit) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return s.localReply(message, piCommandUsage, emit)
	}
	if !utf8.ValidString(name) {
		return s.localReply(message, "Pi session names must be valid UTF-8.", emit)
	}
	if utf8.RuneCountInString(name) > piSessionNameMaxRunes {
		return s.localReply(message, fmt.Sprintf("Pi session names must be at most %d characters.", piSessionNameMaxRunes), emit)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return s.localReply(message, "Pi session names must be a single line without control characters.", emit)
	}
	inspector, ok := s.Harness.(harness.SessionInspector)
	if !ok {
		return s.localReply(message, piControlsUnsupported, emit)
	}
	info, found, err := inspector.SessionInfo(sessionKey(message))
	if err != nil {
		if errors.Is(err, harness.ErrSessionControlsUnsupported) {
			return s.localReply(message, piControlsUnsupported, emit)
		}
		return s.piControlFailure(message, "Cannot inspect the Pi session: ", err, emit)
	}
	if !found {
		return s.localReply(message, "No Pi session exists for this conversation yet. The first ordinary prompt creates one.", emit)
	}
	namer, ok := s.Harness.(harness.SessionNamer)
	if !ok {
		return s.localReply(message, piControlsUnsupported, emit)
	}
	result, err := namer.SetSessionName(ctx, sessionKey(message), info.ID, name, false)
	if err != nil {
		if errors.Is(err, harness.ErrSessionControlsUnsupported) {
			return s.localReply(message, piControlsUnsupported, emit)
		}
		return s.piControlFailure(message, "Cannot name the Pi session: ", err, emit)
	}
	effective := result.Name
	if effective == "" {
		effective = name
	}
	reply := "Pi session renamed to `" + effective + "`."
	if route, routeErr := telegram.ParseConversation(message.Conversation); message.Channel == "telegram" && routeErr == nil && !route.IsGroup() && route.ThreadID() >= 2 && s.ConversationLabels != nil {
		if renameErr := s.ConversationLabels.RenameConversation(ctx, message.Channel, message.Conversation, effective, false); renameErr != nil {
			reply = "Pi session renamed to `" + effective + "`, but the Telegram topic could not be renamed: " + harness.SafeControlErrorText(renameErr)
		} else {
			reply = "Pi session renamed to `" + effective + "` and the Telegram topic was renamed."
		}
	}
	return s.localReply(message, reply, emit)
}

func (s *Service) piCompactCommand(ctx context.Context, message core.Message, instructions string, emit core.Emit) error {
	if utf8.RuneCountInString(instructions) > harness.SessionCompactMaxInstructions {
		return s.localReply(message, fmt.Sprintf("Custom compaction instructions are too long (maximum %d characters).", harness.SessionCompactMaxInstructions), emit)
	}
	compactor, ok := s.Harness.(harness.SessionCompactor)
	if !ok {
		return s.localReply(message, piControlsUnsupported, emit)
	}
	result, err := compactor.CompactSession(ctx, sessionKey(message), instructions)
	if err != nil {
		if errors.Is(err, harness.ErrSessionControlsUnsupported) {
			return s.localReply(message, piControlsUnsupported, emit)
		}
		return s.piControlFailure(message, "Cannot compact the Pi session: ", err, emit)
	}
	return s.localReply(message, piCompactReply(result), emit)
}

func (s *Service) piImportCommand(ctx context.Context, message core.Message, sessionID string, emit core.Emit) error {
	importer, ok := s.Harness.(harness.SessionImporter)
	if !ok {
		return s.localReply(message, piControlsUnsupported, emit)
	}
	info, err := importer.ImportSession(ctx, sessionKey(message), sessionID)
	if err != nil {
		if errors.Is(err, harness.ErrSessionControlsUnsupported) {
			return s.localReply(message, piControlsUnsupported, emit)
		}
		return s.piControlFailure(message, "Cannot import the Pi session: ", err, emit)
	}
	return s.localReply(message, "Imported Pi session `"+info.ID+"`. This conversation now forks that session inside Spynel; the direct Pi session was not modified. Use `/clear` before importing a different session.", emit)
}

// piControlFailure bounds and sanitizes one adapter error before it becomes a
// local `/pi` reply, so provider prose can never leak workspace or session
// paths, credentials, or control characters. The optional session path is
// collected only to redact it from the reply.
func (s *Service) piControlFailure(message core.Message, prefix string, err error, emit core.Emit) error {
	sensitive := []string{s.Config.Root, s.Config.StatePath()}
	if inspector, ok := s.Harness.(harness.SessionInspector); ok {
		if info, found, inspectErr := inspector.SessionInfo(sessionKey(message)); inspectErr == nil && found {
			sensitive = append(sensitive, info.Path)
		}
	}
	return s.localReply(message, prefix+harness.SafeControlErrorText(err, sensitive...), emit)
}

// piSessionReply renders the full session identity plus a shell-safe command
// that forks the exact file instead of sharing it with a live direct Pi.
func piSessionReply(info harness.SessionInfo) string {
	command := strings.TrimSpace(info.Command)
	if command == "" {
		command = "pi"
	}
	return "Current Pi session: `" + info.ID + "`\n\n" +
		"Fork it into a direct Pi session without opening this file concurrently:\n\n" +
		"```bash\n" + shellQuote(command) + " --fork " + shellQuote(info.Path) + "\n```"
}

// piCompactReply renders one bounded compaction result without provider prose.
func piCompactReply(result harness.CompactResult) string {
	before := max(0, result.TokensBefore)
	if result.TokensAfterKnown {
		return fmt.Sprintf("Pi compaction complete: %d tokens before, about %d tokens after.", before, max(0, result.TokensAfter))
	}
	return fmt.Sprintf("Pi compaction complete: %d tokens before.", before)
}

// shellQuote renders one literal value as a POSIX shell word. The result is
// safe to paste verbatim and never interprets shell metacharacters.
func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	plain := true
	for _, r := range value {
		if r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '@' || r == '%' || r == '+' || r == '=' || r == ',' ||
			r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		plain = false
		break
	}
	if plain {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
