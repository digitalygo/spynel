package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitalygo/spynel/internal/config"
)

const (
	// elevenLabsSpeechToTextURL is the fixed production endpoint. A
	// configurable base URL would let configuration redirect the API key and
	// the audio to an arbitrary server, so tests override the field instead.
	elevenLabsSpeechToTextURL = "https://api.elevenlabs.io/v1/speech-to-text"

	// cloudTranscriptionTimeout bounds one complete upload, processing, and
	// optional retry. A shorter caller deadline still wins.
	cloudTranscriptionTimeout = 15 * time.Minute

	maxCloudResponseBytes = 8 << 20
	maxCloudErrorBytes    = 64 << 10

	maxProviderMessageRunes = 200
	maxProviderCodeRunes    = 64

	// maxRetryAfter is the largest provider-requested wait that may trigger the
	// single bounded retry.
	maxRetryAfter = 60 * time.Second
)

// cloudAPIError is one sanitized provider failure. It carries only the HTTP
// status, a bounded provider code, and a bounded one-line message: never the
// API key, headers, raw bodies, environment values, or local paths.
type cloudAPIError struct {
	status          int
	code            string
	message         string
	retryAfter      time.Duration
	retryAfterValid bool
}

func (e *cloudAPIError) Error() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "ElevenLabs speech-to-text returned HTTP %d", e.status)
	if e.code != "" {
		builder.WriteString(" (" + e.code + ")")
	}
	if e.message != "" {
		builder.WriteString(": " + e.message)
	}
	return builder.String()
}

// ElevenLabs is the default cloud speech-to-text backend. It streams the stored
// attachment to the ElevenLabs Speech-to-Text endpoint with the standard
// library only, serializes one process-wide upload at a time, and never
// initializes or downloads a local model.
type ElevenLabs struct {
	settings *config.Store
	client   *http.Client
	endpoint string
	timeout  time.Duration
	serial   chan struct{}
}

// NewElevenLabs constructs the cloud transcription backend. The API key is not
// stored here: at the start of every transcription it is resolved from the
// stored workspace key first, otherwise from the configured environment
// variable.
func NewElevenLabs(settings *config.Store) *ElevenLabs {
	return &ElevenLabs{
		settings: settings,
		client: &http.Client{
			// A redirect must never receive the xi-api-key header or the audio.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		endpoint: elevenLabsSpeechToTextURL,
		timeout:  cloudTranscriptionTimeout,
		serial:   make(chan struct{}, 1),
	}
}

// Transcribe uploads one stored attachment and returns the provider transcript.
// The configured file-size and duration limits are enforced from the validated
// file descriptor and the transport-declared duration before any network
// request; a missing or malformed declared duration fails closed here.
func (e *ElevenLabs) Transcribe(ctx context.Context, request TranscriptionRequest) (string, error) {
	select {
	case e.serial <- struct{}{}:
		defer func() { <-e.serial }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// One snapshot per call fixes the effective key and the provider settings
	// for this transcription; a later store change applies to the next call
	// without reconstructing the client.
	current := e.settings.Snapshot()
	cfg := current.Speech
	if !cfg.Enabled {
		return "", errors.New("speech transcription is disabled")
	}
	keyName := strings.TrimSpace(cfg.ElevenLabsAPIKeyEnv)
	key := current.ElevenLabsAPIKey()
	if key == "" {
		return "", fmt.Errorf("ElevenLabs API key is not set and environment variable %s is missing or blank: %w", sanitizeTranscriptionText(keyName, maxProviderCodeRunes), ErrSpeechAPIKeyMissing)
	}
	if request.DurationSeconds <= 0 {
		return "", errors.New("ElevenLabs transcription requires the declared audio duration")
	}
	if request.DurationSeconds > cfg.MaxDurationSec {
		return "", fmt.Errorf("audio duration exceeds speech.max_duration_seconds (%d seconds)", cfg.MaxDurationSec)
	}
	file, err := os.Open(request.Path)
	if err != nil {
		return "", errors.New("cannot open the audio attachment")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", errors.New("cannot inspect the audio attachment")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("audio attachment is not a regular file")
	}
	size := info.Size()
	if size > int64(cfg.MaxFileMB)*1024*1024 {
		return "", fmt.Errorf("audio attachment exceeds speech.max_file_mb (%d MB)", cfg.MaxFileMB)
	}

	fields := [][2]string{
		{"model_id", cfg.ElevenLabsModelID},
	}
	if cfg.Language != "auto" {
		fields = append(fields, [2]string{"language_code", cfg.Language})
	}
	fields = append(fields,
		[2]string{"tag_audio_events", "false"},
		[2]string{"diarize", "false"},
		[2]string{"timestamps_granularity", "none"},
	)

	callContext, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	upload := cloudUpload{
		fields:   fields,
		fileName: safeName(filepath.Base(request.Path)),
		file:     file,
		size:     size,
		key:      key,
	}
	text, err := e.attempt(callContext, upload)
	if err == nil {
		return text, nil
	}
	var rateErr *cloudAPIError
	if !errors.As(err, &rateErr) || !retryableCloudError(rateErr) {
		return "", err
	}
	wait := rateErr.retryAfter
	if wait < 0 {
		wait = 0
	}
	if err := sleepContext(callContext, wait); err != nil {
		return "", err
	}
	// Exactly one retry: the multipart body is rebuilt from the same validated
	// descriptor and bounded range.
	return e.attempt(callContext, upload)
}

// cloudUpload is one validated upload: the same opened descriptor and captured
// size are reused when the single retry rebuilds the multipart body.
type cloudUpload struct {
	fields   [][2]string
	fileName string
	file     *os.File
	size     int64
	key      string
}

func (e *ElevenLabs) attempt(ctx context.Context, upload cloudUpload) (string, error) {
	body, boundary := multipartUpload(upload.fields, upload.fileName, io.NewSectionReader(upload.file, 0, upload.size))
	defer body.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, body)
	if err != nil {
		return "", errors.New("cannot create the ElevenLabs transcription request")
	}
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	request.Header.Set("xi-api-key", upload.key)
	response, err := e.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("ElevenLabs transcription request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", cloudFailureFrom(response)
	}
	payload, err := readCloudBounded(response.Body, maxCloudResponseBytes)
	if err != nil {
		return "", err
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return "", errors.New("ElevenLabs returned an invalid transcription response")
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		return "", errors.New("ElevenLabs produced an empty transcription")
	}
	if len(text) > maxTranscriptBytes {
		return "", errors.New("generated transcription exceeds the 1 MB safety limit")
	}
	return text, nil
}

// multipartUpload streams one multipart/form-data body through an io.Pipe so
// the attachment is copied from the bounded file range on demand and is never
// buffered in memory.
func multipartUpload(fields [][2]string, fileName string, file io.Reader) (io.ReadCloser, string) {
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	go func() {
		var err error
		defer func() {
			if closeErr := form.Close(); err == nil {
				err = closeErr
			}
			writer.CloseWithError(err)
		}()
		for _, field := range fields {
			if err = form.WriteField(field[0], field[1]); err != nil {
				return
			}
		}
		var part io.Writer
		if part, err = form.CreateFormFile("file", fileName); err != nil {
			return
		}
		_, err = io.Copy(part, file)
	}()
	return reader, form.Boundary()
}

// readCloudBounded reads at most limit bytes and fails explicitly when the body
// is larger instead of truncating or buffering it fully.
func readCloudBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, errors.New("cannot read the ElevenLabs response")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("ElevenLabs response exceeds the 8 MB safety limit")
	}
	return data, nil
}

// cloudFailureFrom converts one non-success response into a sanitized
// cloudAPIError. The body is bounded to 64 KiB and only the documented error
// shapes are parsed.
func cloudFailureFrom(response *http.Response) error {
	failure := &cloudAPIError{status: response.StatusCode}
	failure.retryAfter, failure.retryAfterValid = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	data, err := io.ReadAll(io.LimitReader(response.Body, maxCloudErrorBytes+1))
	if err == nil && int64(len(data)) <= maxCloudErrorBytes {
		failure.code, failure.message = parseCloudErrorBody(data)
	}
	return failure
}

// parseCloudErrorBody understands the documented provider error shapes: the
// `{"detail":{"type","code","message",...}}` object, FastAPI-style validation
// arrays of `{"loc":...,"msg":...}` entries, and a plain `{"detail":"..."}`
// string. Everything else stays an unrecognized provider error.
func parseCloudErrorBody(data []byte) (code, message string) {
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Detail) == 0 {
		return "", "unrecognized provider error"
	}
	var object struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(envelope.Detail, &object); err == nil && (object.Type != "" || object.Code != "" || object.Message != "" || object.Msg != "") {
		code = object.Code
		if code == "" {
			code = object.Type
		}
		message = object.Message
		if message == "" {
			message = object.Msg
		}
		return sanitizeTranscriptionText(code, maxProviderCodeRunes), sanitizeTranscriptionText(message, maxProviderMessageRunes)
	}
	var entries []struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(envelope.Detail, &entries); err == nil && entries != nil {
		messages := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Msg != "" {
				messages = append(messages, entry.Msg)
			}
		}
		return "", sanitizeTranscriptionText(strings.Join(messages, "; "), maxProviderMessageRunes)
	}
	var text string
	if err := json.Unmarshal(envelope.Detail, &text); err == nil {
		return "", sanitizeTranscriptionText(text, maxProviderMessageRunes)
	}
	return "", "unrecognized provider error"
}

// parseRetryAfter accepts integer seconds or an HTTP-date. A past HTTP-date is
// valid and means retry immediately.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if deadline, err := http.ParseTime(value); err == nil {
		wait := deadline.Sub(now)
		if wait < 0 {
			wait = 0
		}
		return wait, true
	}
	return 0, false
}

// retryableCloudError reports the one narrow retry condition: a 429 whose
// parsed provider code is exactly rate_limit_exceeded with a valid Retry-After
// of at most 60 seconds. Concurrent limits, missing or malformed codes, other
// 4xx responses, timeouts, cancellations, and 5xx responses never retry.
func retryableCloudError(failure *cloudAPIError) bool {
	return failure.status == http.StatusTooManyRequests &&
		failure.code == "rate_limit_exceeded" &&
		failure.retryAfterValid &&
		failure.retryAfter <= maxRetryAfter
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
