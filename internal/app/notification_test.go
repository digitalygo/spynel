package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	validOutboxTestID      = "0123456789abcdef0123456789abcdef"
	otherValidOutboxTestID = "fedcba9876543210fedcba9876543210"
)

type outboxDeliveryRecorder struct {
	mu    sync.Mutex
	calls []string
	errs  []error
}

func (r *outboxDeliveryRecorder) Deliver(_ context.Context, origin Origin, eventID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, origin.Channel+"/"+origin.Conversation+"\x00"+eventID+"\x00"+text)
	if len(r.errs) >= len(r.calls) {
		return r.errs[len(r.calls)-1]
	}
	return nil
}

func (r *outboxDeliveryRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func writeRawOutboxFile(t *testing.T, directory, name string, entry OutboxEntry) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readOutboxTestBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeOutboxTestEntry(t *testing.T, path string) OutboxEntry {
	t.Helper()
	var entry OutboxEntry
	if err := json.Unmarshal(readOutboxTestBytes(t, path), &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

// processOutboxWithoutPanic fails the test if Process panics, turning a crash
// on corrupt durable state into an explicit regression assertion.
func processOutboxWithoutPanic(t *testing.T, outbox *Outbox) error {
	t.Helper()
	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("Process() panicked: %v", recovered)
			}
		}()
		err = outbox.Process(context.Background())
	}()
	return err
}

func TestValidOutboxAttempts(t *testing.T) {
	t.Parallel()
	for _, attempts := range []int{0, 1, 2, 8, 256, math.MaxInt - 1} {
		if !validOutboxAttempts(attempts) {
			t.Errorf("validOutboxAttempts(%d) = false, want true", attempts)
		}
	}
	for _, attempts := range []int{-1, -2, math.MinInt, math.MaxInt} {
		if validOutboxAttempts(attempts) {
			t.Errorf("validOutboxAttempts(%d) = true, want false", attempts)
		}
	}
}

func TestValidOutboxID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		validOutboxTestID,
		otherValidOutboxTestID,
		"00000000000000000000000000000000",
		"ffffffffffffffffffffffffffffffff",
	} {
		if !validOutboxID(id) {
			t.Errorf("validOutboxID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{
		"",
		"0",
		strings.Repeat("a", 31),
		strings.Repeat("a", 33),
		strings.ToUpper(validOutboxTestID),
		"0123456789abcdef0123456789abcdeg",
		"0123456789abcde-0123456789abcdef",
		"../" + otherValidOutboxTestID,
		" 0123456789abcdef0123456789abcde",
	} {
		if validOutboxID(id) {
			t.Errorf("validOutboxID(%q) = true, want false", id)
		}
	}
}

func TestOutboxProcessFailsClosedForCorruptIDs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fileID  string
		entryID string
	}{
		{name: "traversal entry ID", fileID: validOutboxTestID, entryID: "../../sentinel"},
		{name: "separator entry ID", fileID: validOutboxTestID, entryID: "sub/" + otherValidOutboxTestID},
		{name: "wrong but valid entry ID", fileID: validOutboxTestID, entryID: otherValidOutboxTestID},
		{name: "uppercase entry ID", fileID: validOutboxTestID, entryID: strings.ToUpper(validOutboxTestID)},
		{name: "short entry ID", fileID: validOutboxTestID, entryID: strings.Repeat("a", 31)},
		{name: "long entry ID", fileID: validOutboxTestID, entryID: strings.Repeat("a", 33)},
		{name: "non-hex entry ID", fileID: validOutboxTestID, entryID: "0123456789abcdef0123456789abcdeg"},
		{name: "malformed filename", fileID: strings.Repeat("a", 31), entryID: strings.Repeat("a", 31)},
		{name: "uppercase filename", fileID: strings.ToUpper(validOutboxTestID), entryID: strings.ToUpper(validOutboxTestID)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			base := t.TempDir()
			outboxDirectory := filepath.Join(base, "workspace", "outbox")
			sentinelPath := filepath.Join(base, "sentinel.json")
			sentinel := []byte("{\"sentinel\":\"untouched\"}\n")
			if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
				t.Fatal(err)
			}
			entryPath := writeRawOutboxFile(t, outboxDirectory, testCase.fileID+".json", OutboxEntry{
				ID:      testCase.entryID,
				Origin:  "tui/local",
				Message: "must not deliver",
				State:   "pending",
			})
			before := readOutboxTestBytes(t, entryPath)
			recorder := &outboxDeliveryRecorder{}
			outbox := Outbox{Directory: outboxDirectory, Deliver: recorder.Deliver}

			err := outbox.Process(context.Background())
			if !errors.Is(err, errOutboxEntryID) {
				t.Fatalf("Process() error = %v, want errOutboxEntryID", err)
			}
			if calls := recorder.snapshot(); len(calls) != 0 {
				t.Fatalf("corrupt entry was delivered = %#v", calls)
			}
			if after := readOutboxTestBytes(t, entryPath); !bytes.Equal(before, after) {
				t.Fatalf("corrupt entry was rewritten:\n before %s\n after  %s", before, after)
			}
			if after := readOutboxTestBytes(t, sentinelPath); !bytes.Equal(sentinel, after) {
				t.Fatalf("sentinel outside the outbox was modified: %s", after)
			}
			children, err := os.ReadDir(outboxDirectory)
			if err != nil {
				t.Fatal(err)
			}
			if len(children) != 1 || children[0].Name() != testCase.fileID+".json" {
				t.Fatalf("outbox directory changed = %#v", children)
			}
		})
	}
}

func TestOutboxProcessFailsClosedForCorruptAttemptCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		attempts int
	}{
		{name: "negative one", attempts: -1},
		{name: "negative two", attempts: -2},
		{name: "minimum int", attempts: math.MinInt},
		{name: "maximum int overflow", attempts: math.MaxInt},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			outboxDirectory := filepath.Join(base, "workspace", "outbox")
			sentinelPath := filepath.Join(base, "sentinel.json")
			sentinel := []byte("{\"sentinel\":\"untouched\"}\n")
			if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
				t.Fatal(err)
			}
			entryPath := writeRawOutboxFile(t, outboxDirectory, validOutboxTestID+".json", OutboxEntry{
				ID:       validOutboxTestID,
				Origin:   "tui/local",
				Message:  "must not deliver",
				State:    "pending",
				Attempts: testCase.attempts,
			})
			before := readOutboxTestBytes(t, entryPath)
			recorder := &outboxDeliveryRecorder{}
			outbox := Outbox{Directory: outboxDirectory, Deliver: recorder.Deliver}

			err := processOutboxWithoutPanic(t, &outbox)
			if !errors.Is(err, errOutboxAttempts) {
				t.Fatalf("Process() error = %v, want errOutboxAttempts", err)
			}
			if !strings.Contains(err.Error(), "attempt count") {
				t.Fatalf("Process() error = %v, want a bounded attempt count problem", err)
			}
			if calls := recorder.snapshot(); len(calls) != 0 {
				t.Fatalf("corrupt attempt count was delivered = %#v", calls)
			}
			if after := readOutboxTestBytes(t, entryPath); !bytes.Equal(before, after) {
				t.Fatalf("corrupt entry was rewritten:\n before %s\n after  %s", before, after)
			}
			if after := readOutboxTestBytes(t, sentinelPath); !bytes.Equal(sentinel, after) {
				t.Fatalf("sentinel outside the outbox was modified: %s", after)
			}
			children, err := os.ReadDir(outboxDirectory)
			if err != nil {
				t.Fatal(err)
			}
			if len(children) != 1 || children[0].Name() != validOutboxTestID+".json" {
				t.Fatalf("outbox directory changed = %#v", children)
			}
		})
	}
}

func TestOutboxProcessDeliversValidSiblingBesideCorruptAttemptCount(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	corruptPath := writeRawOutboxFile(t, directory, validOutboxTestID+".json", OutboxEntry{
		ID:       validOutboxTestID,
		Origin:   "tui/local",
		Message:  "must not deliver",
		State:    "pending",
		Attempts: -1,
	})
	corruptBefore := readOutboxTestBytes(t, corruptPath)
	validEntry := OutboxEntry{
		ID:      otherValidOutboxTestID,
		Origin:  "tui/local",
		Message: "deliver me",
		State:   "pending",
	}
	validPath := writeRawOutboxFile(t, directory, validEntry.ID+".json", validEntry)
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Deliver: recorder.Deliver}

	err := processOutboxWithoutPanic(t, &outbox)
	if !errors.Is(err, errOutboxAttempts) {
		t.Fatalf("Process() error = %v, want errOutboxAttempts", err)
	}
	calls := recorder.snapshot()
	if len(calls) != 1 || calls[0] != "tui/local\x00"+validEntry.ID+"\x00deliver me" {
		t.Fatalf("valid sibling deliveries = %#v", calls)
	}
	if decoded := decodeOutboxTestEntry(t, validPath); decoded.State != "delivered" || decoded.Attempts != 1 {
		t.Fatalf("valid sibling = %+v, want delivered with one attempt", decoded)
	}
	if after := readOutboxTestBytes(t, corruptPath); !bytes.Equal(corruptBefore, after) {
		t.Fatalf("corrupt sibling was rewritten:\n before %s\n after  %s", corruptBefore, after)
	}
}

func TestOutboxProcessRefusesUnincrementableAttemptCount(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	pendingPath := writeRawOutboxFile(t, directory, validOutboxTestID+".json", OutboxEntry{
		ID:            validOutboxTestID,
		Origin:        "tui/local",
		Message:       "must not deliver",
		State:         "pending",
		Attempts:      math.MaxInt - 1,
		CreatedAt:     clock,
		UpdatedAt:     clock,
		NextAttemptAt: clock,
	})
	pendingBefore := readOutboxTestBytes(t, pendingPath)
	// A delivered entry at the last persisted value is a legitimate artifact of
	// the final safe increment and must stay inert rather than become a problem.
	deliveredPath := writeRawOutboxFile(t, directory, otherValidOutboxTestID+".json", OutboxEntry{
		ID:          otherValidOutboxTestID,
		Origin:      "tui/local",
		Message:     "already delivered",
		State:       "delivered",
		Attempts:    math.MaxInt - 1,
		CreatedAt:   clock,
		UpdatedAt:   clock,
		DeliveredAt: clock,
	})
	deliveredBefore := readOutboxTestBytes(t, deliveredPath)
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Now: func() time.Time { return clock }, Deliver: recorder.Deliver}

	err := processOutboxWithoutPanic(t, &outbox)
	if !errors.Is(err, errOutboxAttempts) {
		t.Fatalf("Process() error = %v, want errOutboxAttempts", err)
	}
	if !strings.Contains(err.Error(), "attempt count") {
		t.Fatalf("Process() error = %v, want a bounded attempt count problem", err)
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("unincrementable entry was delivered = %#v", calls)
	}
	if after := readOutboxTestBytes(t, pendingPath); !bytes.Equal(pendingBefore, after) {
		t.Fatalf("unincrementable entry was rewritten:\n before %s\n after  %s", pendingBefore, after)
	}
	if after := readOutboxTestBytes(t, deliveredPath); !bytes.Equal(deliveredBefore, after) {
		t.Fatalf("delivered MaxInt-1 entry was rewritten:\n before %s\n after  %s", deliveredBefore, after)
	}
	problemLines := 0
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.Contains(line, "attempt count") {
			problemLines++
		}
	}
	if problemLines != 1 {
		t.Fatalf("Process() reported %d attempt count problems = %v", problemLines, err)
	}
}

func TestOutboxProcessIncrementsLastSafeAttemptThenRefusesRetry(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	entry := OutboxEntry{
		ID:            validOutboxTestID,
		Origin:        "tui/local",
		Message:       "retry me",
		State:         "pending",
		Attempts:      math.MaxInt - 2,
		CreatedAt:     clock,
		UpdatedAt:     clock,
		NextAttemptAt: clock,
	}
	entryPath := writeRawOutboxFile(t, directory, entry.ID+".json", entry)
	deliveries := 0
	outbox := Outbox{
		Directory: directory,
		Now:       func() time.Time { return clock },
		Deliver: func(context.Context, Origin, string, string) error {
			deliveries++
			return errors.New("upstream offline")
		},
	}

	if err := processOutboxWithoutPanic(t, &outbox); err == nil || !strings.Contains(err.Error(), "upstream offline") {
		t.Fatalf("first Process() error = %v, want the delivery failure", err)
	}
	if deliveries != 1 {
		t.Fatalf("first Process() deliveries = %d, want 1", deliveries)
	}
	incremented := decodeOutboxTestEntry(t, entryPath)
	if incremented.Attempts != math.MaxInt-1 || incremented.State != "pending" {
		t.Fatalf("incremented entry = %+v, want pending with the last persisted attempt", incremented)
	}
	if !incremented.NextAttemptAt.Equal(clock.Add(256 * time.Second)) {
		t.Fatalf("incremented backoff = %v, want %v", incremented.NextAttemptAt, clock.Add(256*time.Second))
	}
	incrementedBytes := readOutboxTestBytes(t, entryPath)

	clock = incremented.NextAttemptAt
	err := processOutboxWithoutPanic(t, &outbox)
	if !errors.Is(err, errOutboxAttempts) {
		t.Fatalf("retry Process() error = %v, want errOutboxAttempts", err)
	}
	if !strings.Contains(err.Error(), "attempt count") {
		t.Fatalf("retry Process() error = %v, want a bounded attempt count problem", err)
	}
	if deliveries != 1 {
		t.Fatalf("refused retry delivered again: deliveries = %d", deliveries)
	}
	if after := readOutboxTestBytes(t, entryPath); !bytes.Equal(incrementedBytes, after) {
		t.Fatalf("refused retry rewrote the entry:\n before %s\n after  %s", incrementedBytes, after)
	}
}

func TestOutboxProcessDeliversValidSiblingBesideUnincrementableAttemptCount(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	blockedPath := writeRawOutboxFile(t, directory, validOutboxTestID+".json", OutboxEntry{
		ID:            validOutboxTestID,
		Origin:        "tui/local",
		Message:       "must not deliver",
		State:         "pending",
		Attempts:      math.MaxInt - 1,
		CreatedAt:     clock,
		UpdatedAt:     clock,
		NextAttemptAt: clock,
	})
	blockedBefore := readOutboxTestBytes(t, blockedPath)
	validEntry := OutboxEntry{
		ID:            otherValidOutboxTestID,
		Origin:        "tui/local",
		Message:       "deliver me",
		State:         "pending",
		CreatedAt:     clock,
		UpdatedAt:     clock,
		NextAttemptAt: clock,
	}
	validPath := writeRawOutboxFile(t, directory, validEntry.ID+".json", validEntry)
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Now: func() time.Time { return clock }, Deliver: recorder.Deliver}

	err := processOutboxWithoutPanic(t, &outbox)
	if !errors.Is(err, errOutboxAttempts) {
		t.Fatalf("Process() error = %v, want errOutboxAttempts", err)
	}
	calls := recorder.snapshot()
	if len(calls) != 1 || calls[0] != "tui/local\x00"+validEntry.ID+"\x00deliver me" {
		t.Fatalf("valid sibling deliveries = %#v", calls)
	}
	if decoded := decodeOutboxTestEntry(t, validPath); decoded.State != "delivered" || decoded.Attempts != 1 {
		t.Fatalf("valid sibling = %+v, want delivered with one attempt", decoded)
	}
	if after := readOutboxTestBytes(t, blockedPath); !bytes.Equal(blockedBefore, after) {
		t.Fatalf("unincrementable sibling was rewritten:\n before %s\n after  %s", blockedBefore, after)
	}
}

func TestOutboxProcessDeliversValidEntriesBesideCorruptOnes(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	corruptPath := writeRawOutboxFile(t, directory, validOutboxTestID+".json", OutboxEntry{
		ID:      otherValidOutboxTestID,
		Origin:  "tui/local",
		Message: "must not deliver",
		State:   "pending",
	})
	validEntry := OutboxEntry{
		ID:      otherValidOutboxTestID,
		Origin:  "tui/local",
		Message: "deliver me",
		State:   "pending",
	}
	validPath := writeRawOutboxFile(t, directory, validEntry.ID+".json", validEntry)
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Deliver: recorder.Deliver}

	err := outbox.Process(context.Background())
	if !errors.Is(err, errOutboxEntryID) {
		t.Fatalf("Process() error = %v, want errOutboxEntryID", err)
	}
	calls := recorder.snapshot()
	if len(calls) != 1 || calls[0] != "tui/local\x00"+validEntry.ID+"\x00deliver me" {
		t.Fatalf("valid entry deliveries = %#v", calls)
	}
	if decoded := decodeOutboxTestEntry(t, validPath); decoded.State != "delivered" {
		t.Fatalf("valid entry state = %q, want delivered", decoded.State)
	}
	if after := readOutboxTestBytes(t, corruptPath); !bytes.Contains(after, []byte(otherValidOutboxTestID)) {
		t.Fatalf("corrupt entry was rewritten: %s", after)
	}
}

func TestOutboxProcessDeliversValidEntryAndRetriesWithBackoff(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	recorder := &outboxDeliveryRecorder{errs: []error{errors.New("temporary upstream failure")}}
	outbox := Outbox{
		Directory: directory,
		Now:       func() time.Time { return clock },
		Deliver:   recorder.Deliver,
	}
	entry, err := outbox.Enqueue("delivery-key", "manual", "tui/local", "retry me")
	if err != nil {
		t.Fatal(err)
	}
	if !validOutboxID(entry.ID) {
		t.Fatalf("Enqueue() ID = %q, want a valid outbox ID", entry.ID)
	}
	entryPath := outbox.path(entry.ID)

	if err := outbox.Process(context.Background()); err == nil || !strings.Contains(err.Error(), "temporary upstream failure") {
		t.Fatalf("first Process() error = %v, want the delivery failure", err)
	}
	if calls := recorder.snapshot(); len(calls) != 1 {
		t.Fatalf("first Process() deliveries = %#v", calls)
	}
	pending := decodeOutboxTestEntry(t, entryPath)
	if pending.State != "pending" || pending.Attempts != 1 || !pending.NextAttemptAt.Equal(clock.Add(time.Second)) {
		t.Fatalf("pending entry = %+v, want one attempt and a one-second backoff", pending)
	}

	if err := outbox.Process(context.Background()); err != nil {
		t.Fatalf("backoff Process() error = %v", err)
	}
	if calls := recorder.snapshot(); len(calls) != 1 {
		t.Fatalf("backoff did not hold = %#v", calls)
	}

	clock = clock.Add(2 * time.Second)
	if err := outbox.Process(context.Background()); err != nil {
		t.Fatalf("retry Process() error = %v", err)
	}
	calls := recorder.snapshot()
	if len(calls) != 2 {
		t.Fatalf("retry deliveries = %#v", calls)
	}
	if expected := "tui/local\x00" + entry.ID + "\x00retry me"; calls[1] != expected {
		t.Fatalf("retry delivery = %q, want %q", calls[1], expected)
	}
	delivered := decodeOutboxTestEntry(t, entryPath)
	if delivered.State != "delivered" || delivered.Attempts != 2 || !delivered.DeliveredAt.Equal(clock) || delivered.LastError != "" {
		t.Fatalf("delivered entry = %+v", delivered)
	}
}

func TestOutboxEnqueueFailsClosedForCorruptExistingEntry(t *testing.T) {
	t.Parallel()
	const deliveryKey = "delivery-key"
	const class = "manual"
	computedID := notificationOutboxID(deliveryKey, class)
	cases := []struct {
		name    string
		entryID string
	}{
		{name: "traversal entry ID", entryID: "../" + otherValidOutboxTestID},
		{name: "wrong but valid entry ID", entryID: otherValidOutboxTestID},
		{name: "uppercase entry ID", entryID: strings.ToUpper(computedID)},
		{name: "malformed entry ID", entryID: strings.Repeat("a", 31)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "outbox")
			entryPath := writeRawOutboxFile(t, directory, computedID+".json", OutboxEntry{
				ID:      testCase.entryID,
				Origin:  "tui/local",
				Message: "corrupt",
				State:   "pending",
			})
			before := readOutboxTestBytes(t, entryPath)
			outbox := Outbox{Directory: directory}

			if _, err := outbox.Enqueue(deliveryKey, class, "tui/local", "replacement"); !errors.Is(err, errOutboxEntryID) {
				t.Fatalf("Enqueue() error = %v, want errOutboxEntryID", err)
			}
			if after := readOutboxTestBytes(t, entryPath); !bytes.Equal(before, after) {
				t.Fatalf("corrupt entry was rewritten:\n before %s\n after  %s", before, after)
			}
			children, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(children) != 1 || children[0].Name() != computedID+".json" {
				t.Fatalf("outbox directory changed = %#v", children)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(directory), otherValidOutboxTestID+".json")); !os.IsNotExist(err) {
				t.Fatalf("ID reached a path outside the outbox: err=%v", err)
			}
		})
	}
}

func TestOutboxEnqueueFailsClosedForCorruptExistingAttemptCounts(t *testing.T) {
	t.Parallel()
	const deliveryKey = "delivery-key"
	const class = "manual"
	computedID := notificationOutboxID(deliveryKey, class)
	cases := []struct {
		name     string
		attempts int
	}{
		{name: "negative one", attempts: -1},
		{name: "minimum int", attempts: math.MinInt},
		{name: "maximum int overflow", attempts: math.MaxInt},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			directory := filepath.Join(t.TempDir(), "outbox")
			entryPath := writeRawOutboxFile(t, directory, computedID+".json", OutboxEntry{
				ID:       computedID,
				Origin:   "tui/local",
				Message:  "corrupt attempts",
				State:    "pending",
				Attempts: testCase.attempts,
			})
			before := readOutboxTestBytes(t, entryPath)
			outbox := Outbox{Directory: directory}

			entry, err := outbox.Enqueue(deliveryKey, class, "tui/local", "replacement")
			if !errors.Is(err, errOutboxAttempts) {
				t.Fatalf("Enqueue() = %+v, %v; want errOutboxAttempts", entry, err)
			}
			if entry != (OutboxEntry{}) {
				t.Fatalf("Enqueue() returned %+v on corrupt attempts", entry)
			}
			if after := readOutboxTestBytes(t, entryPath); !bytes.Equal(before, after) {
				t.Fatalf("corrupt entry was rewritten:\n before %s\n after  %s", before, after)
			}
			children, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(children) != 1 || children[0].Name() != computedID+".json" {
				t.Fatalf("outbox directory changed = %#v", children)
			}
		})
	}
}

func TestOutboxEnqueueLoadsValidExistingEntry(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	computedID := notificationOutboxID("delivery-key", "manual")
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	existing := OutboxEntry{
		ID:            computedID,
		Origin:        "tui/local",
		Message:       "already queued",
		State:         "pending",
		CreatedAt:     now,
		UpdatedAt:     now,
		NextAttemptAt: now,
	}
	entryPath := writeRawOutboxFile(t, directory, computedID+".json", existing)
	before := readOutboxTestBytes(t, entryPath)
	outbox := Outbox{Directory: directory, Now: func() time.Time { return now.Add(time.Minute) }}

	loaded, err := outbox.Enqueue("delivery-key", "manual", "tui/local", "ignored duplicate")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != computedID || loaded.Message != existing.Message || loaded.State != existing.State {
		t.Fatalf("loaded entry = %+v, want the existing entry", loaded)
	}
	if after := readOutboxTestBytes(t, entryPath); !bytes.Equal(before, after) {
		t.Fatalf("valid entry was rewritten:\n before %s\n after  %s", before, after)
	}
}

func TestParseOriginSplitsAndValidatesExplicitDestinations(t *testing.T) {
	t.Parallel()
	valid := []struct {
		value string
		want  Origin
	}{
		{value: "telegram/TG-7", want: Origin{Channel: "telegram", Conversation: "TG-7"}},
		{value: "telegram/TG-7-topic-42", want: Origin{Channel: "telegram", Conversation: "TG-7-topic-42"}},
		{value: "telegram/TG-group-123", want: Origin{Channel: "telegram", Conversation: "TG-group-123"}},
		{value: "telegram/TG-group-123-topic-9", want: Origin{Channel: "telegram", Conversation: "TG-group-123-topic-9"}},
		{value: "whatsapp/WA-15551234567", want: Origin{Channel: "whatsapp", Conversation: "WA-15551234567"}},
		{value: "whatsapp/WA-group-120363000000000000", want: Origin{Channel: "whatsapp", Conversation: "WA-group-120363000000000000"}},
		{value: "tui/local", want: Origin{Channel: "tui", Conversation: "local"}},
		{value: "cli/local", want: Origin{Channel: "cli", Conversation: "local"}},
		{value: "  tui/local  ", want: Origin{Channel: "tui", Conversation: "local"}},
	}
	for _, test := range valid {
		got, err := ParseOrigin(test.value)
		if err != nil {
			t.Fatalf("ParseOrigin(%q) error = %v", test.value, err)
		}
		if got != test.want {
			t.Fatalf("ParseOrigin(%q) = %#v, want %#v", test.value, got, test.want)
		}
	}
	invalid := []struct {
		value   string
		wantErr string
	}{
		{value: "", wantErr: "origin must be channel/conversation"},
		{value: "   ", wantErr: "origin must be channel/conversation"},
		{value: "telegram", wantErr: "origin must be channel/conversation"},
		{value: "telegram/", wantErr: "origin must be channel/conversation"},
		{value: "/local", wantErr: "unsupported origin channel"},
		{value: "email/me", wantErr: "unsupported origin channel"},
		{value: "Telegram/TG-7", wantErr: "unsupported origin channel"},
		{value: "telegram/line\nbreak", wantErr: "invalid characters"},
		{value: "telegram/carriage\rreturn", wantErr: "invalid characters"},
		{value: "telegram/nul\x00byte", wantErr: "invalid characters"},
	}
	for _, test := range invalid {
		got, err := ParseOrigin(test.value)
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Fatalf("ParseOrigin(%q) = %#v, %v; want error containing %q", test.value, got, err, test.wantErr)
		}
		if got != (Origin{}) {
			t.Fatalf("ParseOrigin(%q) returned %#v on error", test.value, got)
		}
	}
}

func TestNormalizeNotificationTextFiltersTerminalProtocolTraffic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "pty query replies", input: "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1RReport ready.", want: "Report ready."},
		{name: "sgr styles and 8 bit csi", input: "A\x1b[31mred\x1b[0m B\u009b6nC", want: "Ared BC"},
		{name: "osc terminated by st", input: "before\x1b]0;unsafe title\x1b\\after", want: "beforeafter"},
		{name: "dcs apc pm sos and c1 st", input: "a\x1bPpayload\x1b\\b\x1b_hidden\x1b\\c\x1b^private\u009cd\u0098secret\u009ce", want: "abcde"},
		{name: "c0 and c1 controls preserve newline and tab", input: "one\x00\x08\ttwo\r\nthree\u0085four\x7f", want: "one\ttwo\nthreefour"},
		{name: "two character escapes", input: "a\x1b7b\x1b8c", want: "abc"},
		{name: "charset designation", input: "a\x1b(0b", want: "ab"},
		{name: "csi intermediate bytes", input: "a\x1b[ !p0b", want: "a0b"},
		{name: "csi parameter bytes", input: "a\x1b[?25hb", want: "ab"},
		{name: "truncated csi", input: "safe\x1b[12;", want: "safe"},
		{name: "truncated osc", input: "safe\x1b]11;rgb:ffff/ffff/ffff", want: "safe"},
		{name: "truncated string control", input: "safe\x1bPpayload", want: "safe"},
		{name: "truncated escape intermediate", input: "safe\x1b((", want: "safe"},
		{name: "truncated escape", input: "safe\x1b", want: "safe"},
		{name: "bare st escape", input: "a\x1b\\b", want: "ab"},
		{name: "escape intermediate then malformed", input: "a\x1b(\x7fb", want: "ab"},
		{name: "string control skips invalid bytes", input: "a\x1bPpay\xffload\x1b\\b", want: "ab"},
		{name: "malformed csi keeps following unicode", input: "safe\x1b[12;\U0001F6F0\uFE0F prose", want: "safe\U0001F6F0\uFE0F prose"},
		{name: "escape before unicode", input: "a\x1b\u00e9b", want: "a\u00e9b"},
		{name: "dcs bell is payload not terminator", input: "a\x1bPpay\x07load\x1b\\b", want: "ab"},
		{name: "osc c1 st terminator", input: "a\x1b]0;title\u009cb", want: "ab"},
		{name: "8 bit osc with bell", input: "a\x9d0;title\x07b", want: "ab"},
		{name: "8 bit dcs apc pm sos with c1 st", input: "a\x90x\x9cb\x9ex\x9cc\x98x\x9cd\x9fx\x9ce", want: "abcde"},
		{name: "standalone 8 bit st and other c1", input: "a\x9cb\x84c", want: "abc"},
		{name: "valid unicode c1 controls", input: "a\u0084\u0085\u0086b", want: "ab"},
		{name: "8 bit csi sequence", input: "\x9b?25l\x9b?25hvisible", want: "visible"},
		{name: "markdown unicode multiline", input: "  **Done** \u2014 caf\u00e9 \U0001F680\n\n- \u7b2c\u4e00\n\tindented  ", want: "**Done** \u2014 caf\u00e9 \U0001F680\n\n- \u7b2c\u4e00\n\tindented"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeNotificationText(test.input)
			if err != nil {
				t.Fatalf("NormalizeNotificationText(%q) error = %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("NormalizeNotificationText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeNotificationTextRejectsInvalidUTF8AndControlOnlyInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "whitespace only", input: " \t\n\r "},
		{name: "control only", input: "\x00\r\x7f"},
		{name: "pty query replies only", input: "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1R"},
		{name: "8 bit csi only", input: "\x9b1;1R"},
		{name: "invalid byte", input: "ok\xff"},
		{name: "overlong encoding", input: "\xc0\xaf"},
		{name: "utf16 surrogate", input: "\xed\xa0\x80"},
		{name: "escape then invalid byte", input: "safe\x1b\xff"},
		{name: "csi then invalid byte", input: "safe\x1b[1;\xff"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeNotificationText(test.input)
			if err == nil || got != "" {
				t.Fatalf("NormalizeNotificationText(%q) = %q, %v; want rejection", test.input, got, err)
			}
		})
	}
}

func TestOutboxProcessWithoutDeliverOrDirectoryIsNoOp(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "outbox")
	if err := (&Outbox{Directory: missing}).Process(context.Background()); err != nil {
		t.Fatalf("Process() without Deliver = %v", err)
	}
	delivered := false
	outbox := Outbox{Directory: missing, Deliver: func(context.Context, Origin, string, string) error {
		delivered = true
		return nil
	}}
	if err := outbox.Process(context.Background()); err != nil {
		t.Fatalf("Process() over a missing directory = %v", err)
	}
	if delivered {
		t.Fatal("delivery ran for a missing outbox directory")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("Process created the outbox directory: %v", err)
	}
}

func TestOutboxProcessSkipsForeignMalformedAndTerminalFiles(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	if err := os.MkdirAll(filepath.Join(directory, validOutboxTestID+".json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	malformedPath := filepath.Join(directory, strings.Repeat("a", 32)+".json")
	if err := os.WriteFile(malformedPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	delivered := OutboxEntry{ID: otherValidOutboxTestID, Origin: "tui/local", Message: "already handled", State: "delivered"}
	deliveredPath := writeRawOutboxFile(t, directory, delivered.ID+".json", delivered)
	cancelled := OutboxEntry{ID: "00000000000000000000000000000000", Origin: "tui/local", Message: "cancelled", State: "cancelled"}
	cancelledPath := writeRawOutboxFile(t, directory, cancelled.ID+".json", cancelled)
	deliveredBefore := readOutboxTestBytes(t, deliveredPath)
	cancelledBefore := readOutboxTestBytes(t, cancelledPath)
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Deliver: recorder.Deliver}

	if err := outbox.Process(context.Background()); err != nil {
		t.Fatalf("Process() = %v, want malformed and terminal entries skipped", err)
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("terminal or malformed entries were delivered = %#v", calls)
	}
	if after := readOutboxTestBytes(t, deliveredPath); !bytes.Equal(deliveredBefore, after) {
		t.Fatalf("delivered entry was rewritten:\n before %s\n after  %s", deliveredBefore, after)
	}
	if after := readOutboxTestBytes(t, cancelledPath); !bytes.Equal(cancelledBefore, after) {
		t.Fatalf("cancelled entry was rewritten:\n before %s\n after  %s", cancelledBefore, after)
	}
	if after := readOutboxTestBytes(t, malformedPath); !bytes.Equal(after, []byte("{not json")) {
		t.Fatalf("malformed entry was rewritten: %s", after)
	}
}

func TestOutboxProcessReportsUnreadableNamedEntry(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "missing-target")
	if err := os.Symlink(target, filepath.Join(directory, otherValidOutboxTestID+".json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Deliver: recorder.Deliver}

	err := outbox.Process(context.Background())
	if err == nil {
		t.Fatal("Process() = nil, want the unreadable entry error")
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("unreadable entry was delivered = %#v", calls)
	}
}

func TestOutboxProcessCancelsEntryWhoseMessageNormalizesToNothing(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	entry := OutboxEntry{ID: validOutboxTestID, Origin: "tui/local", Message: "\x1b[6n", State: "pending", CreatedAt: clock, UpdatedAt: clock, NextAttemptAt: clock}
	entryPath := writeRawOutboxFile(t, directory, entry.ID+".json", entry)
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Now: func() time.Time { return clock }, Deliver: recorder.Deliver}

	err := outbox.Process(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty after removing terminal controls") {
		t.Fatalf("Process() error = %v, want the normalization rejection", err)
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("control-only entry was delivered = %#v", calls)
	}
	cancelled := decodeOutboxTestEntry(t, entryPath)
	if cancelled.State != "cancelled" || cancelled.Attempts != 1 || !strings.Contains(cancelled.LastError, "empty after removing terminal controls") {
		t.Fatalf("cancelled entry = %+v", cancelled)
	}
}

func TestOutboxProcessRetriesEntryWithInvalidOrigin(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	entry := OutboxEntry{ID: validOutboxTestID, Origin: "email/me", Message: "keep me", State: "pending", CreatedAt: clock, UpdatedAt: clock, NextAttemptAt: clock}
	entryPath := writeRawOutboxFile(t, directory, entry.ID+".json", entry)
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Now: func() time.Time { return clock }, Deliver: recorder.Deliver}

	err := outbox.Process(context.Background())
	if err == nil || !strings.Contains(err.Error(), `unsupported origin channel "email"`) {
		t.Fatalf("Process() error = %v, want the invalid origin error", err)
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("invalid origin reached delivery = %#v", calls)
	}
	retried := decodeOutboxTestEntry(t, entryPath)
	if retried.State != "pending" || retried.Attempts != 1 || !strings.Contains(retried.LastError, "unsupported origin channel") {
		t.Fatalf("retried entry = %+v", retried)
	}
	if !retried.NextAttemptAt.Equal(clock.Add(time.Second)) {
		t.Fatalf("retry schedule = %v, want one second after the attempt", retried.NextAttemptAt)
	}
}

func TestOutboxProcessReportsDirectoryReadFailure(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	path := filepath.Join(base, "outbox-file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: path, Deliver: recorder.Deliver}

	if err := outbox.Process(context.Background()); err == nil {
		t.Fatal("Process() over a file path = nil, want a read error")
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("unreadable outbox root delivered = %#v", calls)
	}
}

func TestOutboxEnqueueRejectsInvalidInputBeforeTouchingDisk(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	outbox := Outbox{Directory: directory}

	if _, err := outbox.Enqueue("delivery-key", "manual", "email/me", "hello"); err == nil {
		t.Fatal("Enqueue accepted an unsupported origin channel")
	}
	if _, err := outbox.Enqueue("delivery-key", "manual", "tui/local", "\x1b[6n"); err == nil {
		t.Fatal("Enqueue accepted a control-only message")
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("rejected Enqueue touched the outbox directory: %v", err)
	}
}

func TestOutboxEnqueueNormalizesBeforePersistenceAndDelivery(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	recorder := &outboxDeliveryRecorder{}
	outbox := Outbox{Directory: directory, Deliver: recorder.Deliver}
	unsafe := "\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1R**Ready** \U0001F680\nnext"

	entry, err := outbox.Enqueue("delivery-key", "manual", "cli/local", unsafe)
	if err != nil {
		t.Fatal(err)
	}
	want := "**Ready** \U0001F680\nnext"
	if entry.Message != want {
		t.Fatalf("enqueued message = %q, want %q", entry.Message, want)
	}
	data := readOutboxTestBytes(t, outbox.path(entry.ID))
	if strings.Contains(string(data), "rgb:0000") {
		t.Fatalf("terminal query reply reached durable outbox: %s", data)
	}
	if stored := decodeOutboxTestEntry(t, outbox.path(entry.ID)); stored.Message != want {
		t.Fatalf("stored message = %q, want %q", stored.Message, want)
	}

	if err := outbox.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := recorder.snapshot()
	if len(calls) != 1 || calls[0] != "cli/local\x00"+entry.ID+"\x00"+want {
		t.Fatalf("deliveries = %#v", calls)
	}
}

func TestOutboxEnqueueNormalizesExistingEntryAndRejectsControlOnlyMessage(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	computedID := notificationOutboxID("delivery-key", "manual")
	existing := OutboxEntry{
		ID: computedID, Origin: "tui/local", Message: "\x1b[?1;2cQueued text\x1b[0m", State: "pending",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), NextAttemptAt: now.Add(-time.Hour),
	}
	entryPath := writeRawOutboxFile(t, directory, computedID+".json", existing)
	outbox := Outbox{Directory: directory, Now: func() time.Time { return now }}

	loaded, err := outbox.Enqueue("delivery-key", "manual", "tui/local", "ignored duplicate")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Message != "Queued text" || !loaded.UpdatedAt.Equal(now) || !loaded.NextAttemptAt.Equal(existing.NextAttemptAt) {
		t.Fatalf("normalized duplicate = %+v", loaded)
	}
	stored := decodeOutboxTestEntry(t, entryPath)
	if stored.Message != "Queued text" || !stored.UpdatedAt.Equal(now) {
		t.Fatalf("stored duplicate = %+v", stored)
	}

	controlOnlyID := notificationOutboxID("other-key", "manual")
	writeRawOutboxFile(t, directory, controlOnlyID+".json", OutboxEntry{ID: controlOnlyID, Origin: "tui/local", Message: "\x1b]0;title\x07", State: "pending"})
	if _, err := outbox.Enqueue("other-key", "manual", "tui/local", "replacement"); err == nil || !strings.Contains(err.Error(), "empty after removing terminal controls") {
		t.Fatalf("Enqueue() error = %v, want the normalization rejection", err)
	}
}

func TestOutboxWriteFailsClosedForCorruptAttemptCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		attempts int
	}{
		{name: "negative one", attempts: -1},
		{name: "minimum int", attempts: math.MinInt},
		{name: "maximum int overflow", attempts: math.MaxInt},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			directory := filepath.Join(t.TempDir(), "outbox")
			outbox := Outbox{Directory: directory}
			entry := OutboxEntry{
				ID:       validOutboxTestID,
				Origin:   "tui/local",
				Message:  "must not persist",
				State:    "pending",
				Attempts: testCase.attempts,
			}
			if err := outbox.write(entry); !errors.Is(err, errOutboxAttempts) {
				t.Fatalf("write() error = %v, want errOutboxAttempts", err)
			}
			if _, err := os.Stat(directory); !os.IsNotExist(err) {
				t.Fatalf("write with corrupt attempts touched the outbox directory: %v", err)
			}
		})
	}
}

func TestOutboxWriteRejectsMalformedIdentityAndUnusableDirectory(t *testing.T) {
	t.Parallel()
	outbox := Outbox{Directory: filepath.Join(t.TempDir(), "outbox")}
	if err := outbox.write(OutboxEntry{ID: "not-an-outbox-id", Message: "x"}); !errors.Is(err, errOutboxEntryID) {
		t.Fatalf("write() error = %v, want errOutboxEntryID", err)
	}

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockedOutbox := Outbox{Directory: filepath.Join(blocked, "outbox")}
	if _, err := blockedOutbox.Enqueue("delivery-key", "manual", "tui/local", "hello"); err == nil {
		t.Fatal("Enqueue succeeded under a regular file path")
	}
}

func TestOutboxProcessRedeliveryBackoffIsBounded(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "outbox")
	clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	outbox := Outbox{
		Directory: directory,
		Now:       func() time.Time { return clock },
		Deliver: func(context.Context, Origin, string, string) error {
			return errors.New("upstream offline")
		},
	}
	entry, err := outbox.Enqueue("bounded-retry", "manual", "tui/local", "retry me")
	if err != nil {
		t.Fatal(err)
	}
	entryPath := outbox.path(entry.ID)
	for attempt := 1; attempt <= 10; attempt++ {
		if err := outbox.Process(context.Background()); err == nil {
			t.Fatalf("attempt %d: Process() = nil, want failure", attempt)
		}
		stored := decodeOutboxTestEntry(t, entryPath)
		if stored.Attempts != attempt {
			t.Fatalf("attempt %d: attempts = %d", attempt, stored.Attempts)
		}
		wantDelay := time.Second * time.Duration(1<<min(attempt-1, 8))
		if !stored.NextAttemptAt.Equal(clock.Add(wantDelay)) {
			t.Fatalf("attempt %d: next attempt = %v, want %v", attempt, stored.NextAttemptAt, clock.Add(wantDelay))
		}
		clock = stored.NextAttemptAt
	}
	last := decodeOutboxTestEntry(t, entryPath)
	if delay := last.NextAttemptAt.Sub(last.UpdatedAt); delay != 256*time.Second {
		t.Fatalf("bounded retry delay = %v, want 256s after ten failures", delay)
	}
}

func TestOutboxProcessKeepsBackoffForValidAttemptCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		attempts int
		want     time.Duration
	}{
		{name: "initial attempt", attempts: 0, want: time.Second},
		{name: "second attempt", attempts: 1, want: 2 * time.Second},
		{name: "eighth attempt", attempts: 7, want: 128 * time.Second},
		{name: "capped attempt", attempts: 8, want: 256 * time.Second},
		{name: "cap retained", attempts: 9, want: 256 * time.Second},
		{name: "large valid count", attempts: 5000, want: 256 * time.Second},
		{name: "last safe attempt", attempts: math.MaxInt - 2, want: 256 * time.Second},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			directory := filepath.Join(t.TempDir(), "outbox")
			clock := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
			entry := OutboxEntry{
				ID:            validOutboxTestID,
				Origin:        "tui/local",
				Message:       "retry me",
				State:         "pending",
				Attempts:      testCase.attempts,
				CreatedAt:     clock,
				UpdatedAt:     clock,
				NextAttemptAt: clock,
			}
			entryPath := writeRawOutboxFile(t, directory, entry.ID+".json", entry)
			outbox := Outbox{
				Directory: directory,
				Now:       func() time.Time { return clock },
				Deliver: func(context.Context, Origin, string, string) error {
					return errors.New("upstream offline")
				},
			}
			if err := outbox.Process(context.Background()); err == nil || !strings.Contains(err.Error(), "upstream offline") {
				t.Fatalf("Process() error = %v, want the delivery failure", err)
			}
			stored := decodeOutboxTestEntry(t, entryPath)
			if stored.Attempts != testCase.attempts+1 {
				t.Fatalf("attempts = %d, want %d", stored.Attempts, testCase.attempts+1)
			}
			if !stored.NextAttemptAt.Equal(clock.Add(testCase.want)) {
				t.Fatalf("next attempt = %v, want %v", stored.NextAttemptAt, clock.Add(testCase.want))
			}
		})
	}
}
