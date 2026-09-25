package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/digitalygo/spynel/internal/fsx"
)

// Origin is one explicit notification destination parsed from the
// channel/conversation form accepted by the shared notify API.
type Origin struct{ Channel, Conversation string }

// ParseOrigin validates the strict channel/conversation grammar shared by
// explicit notification origins.
func ParseOrigin(value string) (Origin, error) {
	channel, conversation, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || conversation == "" {
		return Origin{}, errors.New("origin must be channel/conversation")
	}
	switch channel {
	case "telegram", "whatsapp", "tui", "cli":
	default:
		return Origin{}, fmt.Errorf("unsupported origin channel %q", channel)
	}
	if strings.ContainsAny(conversation, "\r\n\x00") {
		return Origin{}, errors.New("origin conversation contains invalid characters")
	}
	return Origin{Channel: channel, Conversation: conversation}, nil
}

// errOutboxEntryID reports a durable outbox identity that is malformed or
// does not match the filename holding it. Loading and processing fail closed
// before delivery or any write when they see one.
var errOutboxEntryID = errors.New("outbox entry ID is malformed or does not match its filename")

// errOutboxAttempts reports a durable outbox attempt counter that is negative
// or too large to increment without overflowing. Loading, processing, and
// writing fail closed before delivery or file mutation when they see one.
var errOutboxAttempts = errors.New("outbox entry attempt count is invalid")

// OutboxEntry is one durable explicit notification delivery attempt.
type OutboxEntry struct {
	ID            string    `json:"id"`
	Origin        string    `json:"origin"`
	Message       string    `json:"message"`
	State         string    `json:"state"`
	Attempts      int       `json:"attempts"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
	DeliveredAt   time.Time `json:"delivered_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

// Outbox persists and retries explicit notification deliveries. It is the
// classic at-least-once delivery primitive behind `spynel notify` and the
// local API; it carries no reminder, scan, or agent policy.
type Outbox struct {
	Directory string
	Deliver   func(context.Context, Origin, string, string) error
	Now       func() time.Time
	mu        sync.Mutex
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

func (o *Outbox) path(id string) string { return filepath.Join(o.Directory, id+".json") }

func notificationOutboxID(deliveryKey, class string) string {
	hash := sha256.Sum256([]byte(deliveryKey + "\x00" + class))
	return hex.EncodeToString(hash[:16])
}

// outboxIDLength is the exact length of every notificationOutboxID: 16
// SHA-256 bytes rendered as lowercase hexadecimal.
const outboxIDLength = 32

// validOutboxID reports whether id is exactly one outbox file identity as
// produced by notificationOutboxID. A durable ID reaches a filesystem path
// only after this check passes.
func validOutboxID(id string) bool {
	if len(id) != outboxIDLength {
		return false
	}
	for index := 0; index < len(id); index++ {
		character := id[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// validOutboxAttempts reports whether attempts is a nonnegative persisted
// delivery counter that can be incremented without integer overflow. Every
// existing-entry load, delivery, and write checks it before touching the
// entry further, so corrupt durable counts cannot panic or be perpetuated.
func validOutboxAttempts(attempts int) bool {
	return attempts >= 0 && attempts < math.MaxInt
}

// NormalizeNotificationText removes terminal protocol traffic before a
// notification can enter any durable or visible boundary. A PTY returns
// terminal query replies on the same input stream used by an stdin-based
// notification action, so treating that stream as authored prose is unsafe.
func NormalizeNotificationText(value string) (string, error) {
	var normalized strings.Builder
	normalized.Grow(len(value))
	for index := 0; index < len(value); {
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == utf8.RuneError && size == 1 {
			if value[index] >= 0x80 && value[index] <= 0x9f {
				index = skipC1NotificationControl(value, index, 1)
				continue
			}
			return "", errors.New("notification message must be valid UTF-8")
		}
		switch {
		case character == 0x1b:
			index = skipESCNotificationControl(value, index+size)
		case character >= 0x80 && character <= 0x9f:
			index = skipC1NotificationControl(value, index, size)
		case character < 0x20 || character == 0x7f:
			if character == '\n' || character == '\t' {
				normalized.WriteString(value[index : index+size])
			}
			index += size
		default:
			normalized.WriteString(value[index : index+size])
			index += size
		}
	}
	text := strings.TrimSpace(normalized.String())
	if text == "" {
		return "", errors.New("notification message is empty after removing terminal controls")
	}
	return text, nil
}

func skipESCNotificationControl(value string, index int) int {
	if index >= len(value) {
		return len(value)
	}
	character, size := utf8.DecodeRuneInString(value[index:])
	if character == utf8.RuneError && size == 1 {
		return index
	}
	switch character {
	case '[':
		return skipCSINotificationControl(value, index+size)
	case ']':
		return skipStringNotificationControl(value, index+size, true)
	case 'P', 'X', '^', '_':
		return skipStringNotificationControl(value, index+size, false)
	case '\\':
		return index + size
	}
	if character >= 0x20 && character <= 0x2f {
		index += size
		for index < len(value) {
			character, size = utf8.DecodeRuneInString(value[index:])
			if character >= 0x20 && character <= 0x2f {
				index += size
				continue
			}
			if character >= 0x30 && character <= 0x7e {
				return index + size
			}
			return index
		}
		return len(value)
	}
	if character >= 0x30 && character <= 0x7e {
		return index + size
	}
	return index
}

func skipC1NotificationControl(value string, index, size int) int {
	character, _ := utf8.DecodeRuneInString(value[index:])
	if size == 1 {
		character = rune(value[index])
	}
	next := index + size
	switch character {
	case 0x90, 0x98, 0x9e, 0x9f:
		return skipStringNotificationControl(value, next, false)
	case 0x9b:
		return skipCSINotificationControl(value, next)
	case 0x9d:
		return skipStringNotificationControl(value, next, true)
	default:
		return next
	}
}

func skipCSINotificationControl(value string, index int) int {
	intermediates := false
	for index < len(value) {
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == utf8.RuneError && size == 1 {
			return index
		}
		switch {
		case character >= 0x30 && character <= 0x3f && !intermediates:
			index += size
		case character >= 0x20 && character <= 0x2f:
			intermediates = true
			index += size
		case character >= 0x40 && character <= 0x7e:
			return index + size
		default:
			return index
		}
	}
	return len(value)
}

func skipStringNotificationControl(value string, index int, bellTerminates bool) int {
	for index < len(value) {
		if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
			return index + 2
		}
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == utf8.RuneError && size == 1 {
			if value[index] == 0x9c {
				return index + 1
			}
			index++
			continue
		}
		if character == 0x9c || (bellTerminates && character == 0x07) {
			return index + size
		}
		index += size
	}
	return len(value)
}

func (o *Outbox) Enqueue(deliveryKey, class, origin, message string) (OutboxEntry, error) {
	if _, err := ParseOrigin(origin); err != nil {
		return OutboxEntry{}, err
	}
	message, err := NormalizeNotificationText(message)
	if err != nil {
		return OutboxEntry{}, err
	}
	id := notificationOutboxID(deliveryKey, class)
	o.mu.Lock()
	defer o.mu.Unlock()
	if data, err := os.ReadFile(o.path(id)); err == nil {
		var existing OutboxEntry
		if json.Unmarshal(data, &existing) == nil {
			if !validOutboxID(existing.ID) || existing.ID != id {
				return OutboxEntry{}, fmt.Errorf("%w: %q", errOutboxEntryID, id)
			}
			if !validOutboxAttempts(existing.Attempts) {
				return OutboxEntry{}, fmt.Errorf("%w: %q has attempt count %d", errOutboxAttempts, id, existing.Attempts)
			}
			normalized, normalizeErr := NormalizeNotificationText(existing.Message)
			if normalizeErr != nil {
				return OutboxEntry{}, normalizeErr
			}
			if normalized != existing.Message {
				existing.Message = normalized
				existing.UpdatedAt = o.now()
				if err := o.write(existing); err != nil {
					return OutboxEntry{}, err
				}
			}
			return existing, nil
		}
	}
	now := o.now()
	entry := OutboxEntry{ID: id, Origin: origin, Message: message, State: "pending", CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
	return entry, o.write(entry)
}

func (o *Outbox) write(entry OutboxEntry) error {
	if !validOutboxID(entry.ID) {
		return fmt.Errorf("%w: %q", errOutboxEntryID, entry.ID)
	}
	if !validOutboxAttempts(entry.Attempts) {
		return fmt.Errorf("%w: %q has attempt count %d", errOutboxAttempts, entry.ID, entry.Attempts)
	}
	if err := os.MkdirAll(o.Directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(o.path(entry.ID), append(data, '\n'), 0o600)
}

func (o *Outbox) Process(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Deliver == nil {
		return nil
	}
	entries, err := os.ReadDir(o.Directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var problems []error
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		fileID := strings.TrimSuffix(item.Name(), ".json")
		if !validOutboxID(fileID) {
			problems = append(problems, fmt.Errorf("%w: %q", errOutboxEntryID, item.Name()))
			continue
		}
		data, readErr := os.ReadFile(o.path(fileID))
		if readErr != nil {
			problems = append(problems, readErr)
			continue
		}
		var entry OutboxEntry
		if json.Unmarshal(data, &entry) != nil {
			continue
		}
		if !validOutboxID(entry.ID) || entry.ID != fileID {
			problems = append(problems, fmt.Errorf("%w: %q", errOutboxEntryID, item.Name()))
			continue
		}
		if !validOutboxAttempts(entry.Attempts) {
			problems = append(problems, fmt.Errorf("%w: %q has attempt count %d", errOutboxAttempts, item.Name(), entry.Attempts))
			continue
		}
		if entry.State == "delivered" || entry.State == "cancelled" || entry.NextAttemptAt.After(o.now()) {
			continue
		}
		// A persisted count that cannot advance to another valid persisted
		// value would be delivered on every maintenance cycle without ever
		// recording the attempt. Refuse it before normalization, delivery, or
		// any file mutation.
		if !validOutboxAttempts(entry.Attempts + 1) {
			problems = append(problems, fmt.Errorf("%w: %q has attempt count %d that cannot be incremented", errOutboxAttempts, item.Name(), entry.Attempts))
			continue
		}
		normalized, normalizeErr := NormalizeNotificationText(entry.Message)
		if normalizeErr != nil {
			entry.State = "cancelled"
			entry.Attempts++
			entry.UpdatedAt = o.now()
			entry.LastError = normalizeErr.Error()
			if writeErr := o.write(entry); writeErr != nil {
				problems = append(problems, writeErr)
			}
			problems = append(problems, normalizeErr)
			continue
		}
		entry.Message = normalized
		origin, parseErr := ParseOrigin(entry.Origin)
		deliveryErr := parseErr
		if deliveryErr == nil {
			deliveryErr = o.Deliver(ctx, origin, entry.ID, entry.Message)
		}
		entry.Attempts++
		entry.UpdatedAt = o.now()
		if deliveryErr == nil {
			entry.State = "delivered"
			entry.DeliveredAt = entry.UpdatedAt
			entry.LastError = ""
		} else {
			entry.State = "pending"
			entry.LastError = deliveryErr.Error()
			delay := time.Second * time.Duration(1<<min(entry.Attempts-1, 8))
			entry.NextAttemptAt = entry.UpdatedAt.Add(delay)
			problems = append(problems, deliveryErr)
		}
		if writeErr := o.write(entry); writeErr != nil {
			problems = append(problems, writeErr)
		}
	}
	return errors.Join(problems...)
}
