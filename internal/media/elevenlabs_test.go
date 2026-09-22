package media

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitalygo/spynel/internal/config"
)

const testElevenLabsKey = "test-key-123"

func elevenLabsFixture(t *testing.T, mutate func(*config.Speech)) *ElevenLabs {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg.Speech)
	}
	worker := NewElevenLabs(config.NewStore(cfg))
	worker.endpoint = "http://127.0.0.1:0/invalid"
	return worker
}

func elevenLabsFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertNoSecretLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), testElevenLabsKey) {
		t.Fatalf("error leaks the API key: %v", err)
	}
}

// rawServer answers every request with one fixed status, headers, and body.
func rawServer(status int, headers map[string]string, body string) (*httptest.Server, *atomic.Int32) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		for key, value := range headers {
			writer.Header().Set(key, value)
		}
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}))
	return server, &requests
}

func TestElevenLabsSendsExactMultipartFieldsAndAPIKey(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if got := request.Header.Get("xi-api-key"); got != testElevenLabsKey {
			t.Errorf("xi-api-key header = %q", got)
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(writer, "bad multipart", http.StatusBadRequest)
			return
		}
		values := request.MultipartForm.Value
		if got := values["model_id"]; len(got) != 1 || got[0] != "scribe_v2" {
			t.Errorf("model_id = %#v", got)
		}
		if got := values["language_code"]; len(got) != 0 {
			t.Errorf("language_code must be omitted for auto, got %#v", got)
		}
		for key, want := range map[string]string{
			"tag_audio_events":       "false",
			"diarize":                "false",
			"timestamps_granularity": "none",
		} {
			if got := values[key]; len(got) != 1 || got[0] != want {
				t.Errorf("%s = %#v, want %q", key, got, want)
			}
		}
		if len(values) != 4 {
			t.Errorf("unexpected form fields: %#v", values)
		}
		file, header, err := request.FormFile("file")
		if err != nil {
			t.Errorf("file part: %v", err)
		} else {
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "audio bytes" {
				t.Errorf("file body = %q", data)
			}
			if header.Filename != "voice-note.ogg" {
				t.Errorf("file name = %q", header.Filename)
			}
			if got := header.Header.Get("Content-Type"); got != "application/octet-stream" {
				t.Errorf("file content type = %q", got)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"  hello from the cloud  ","language_code":"en"}`))
	}))
	defer server.Close()

	worker := elevenLabsFixture(t, func(speech *config.Speech) { speech.Language = "auto" })
	worker.endpoint = server.URL
	text, err := worker.Transcribe(context.Background(), TranscriptionRequest{
		Path:            elevenLabsFile(t, "voice-note.ogg", []byte("audio bytes")),
		DurationSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello from the cloud" {
		t.Fatalf("transcript = %q", text)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestElevenLabsSendsConfiguredModelAndLanguageCode(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	language := make(chan string, 1)
	model := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		language <- request.FormValue("language_code")
		model <- request.FormValue("model_id")
		_, _ = writer.Write([]byte(`{"text":"ok"}`))
	}))
	defer server.Close()

	worker := elevenLabsFixture(t, func(speech *config.Speech) {
		speech.Language = "de"
		speech.ElevenLabsModelID = "scribe_v1"
	})
	worker.endpoint = server.URL
	if _, err := worker.Transcribe(context.Background(), TranscriptionRequest{
		Path:            elevenLabsFile(t, "note.ogg", []byte("audio bytes")),
		DurationSeconds: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-language; got != "de" {
		t.Fatalf("language_code = %q", got)
	}
	if got := <-model; got != "scribe_v1" {
		t.Fatalf("model_id = %q", got)
	}
}

func TestElevenLabsNeverFollowsRedirectsWithKeyOrAudio(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	var forwarded atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		forwarded.Add(1)
		_, _ = writer.Write([]byte(`{"text":"stolen"}`))
	}))
	defer victim.Close()
	var sourceRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sourceRequests.Add(1)
		if got := request.Header.Get("xi-api-key"); got != testElevenLabsKey {
			t.Errorf("xi-api-key header = %q", got)
		}
		writer.Header().Set("Location", victim.URL+"/steal")
		writer.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	worker := elevenLabsFixture(t, nil)
	worker.endpoint = server.URL
	_, err := worker.Transcribe(context.Background(), TranscriptionRequest{
		Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
		DurationSeconds: 5,
	})
	if err == nil {
		t.Fatal("redirect response was accepted")
	}
	assertNoSecretLeak(t, err)
	if !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect error = %v", err)
	}
	if forwarded.Load() != 0 {
		t.Fatalf("redirect target received %d requests with the key or audio", forwarded.Load())
	}
	if sourceRequests.Load() != 1 {
		t.Fatalf("source requests = %d, want 1", sourceRequests.Load())
	}
}

func TestElevenLabsEnforcesDescriptorSizeAndDurationBeforeUpload(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	server, requests := rawServer(http.StatusOK, nil, `{"text":"unexpected"}`)
	defer server.Close()

	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.ogg")
	regular := elevenLabsFile(t, "voice.ogg", []byte("audio bytes"))
	oversized := elevenLabsFile(t, "oversized.ogg", make([]byte, 2*1024*1024))

	for _, test := range []struct {
		name    string
		mutate  func(*config.Speech)
		request TranscriptionRequest
		want    string
	}{
		{
			name:    "missing declared duration",
			request: TranscriptionRequest{Path: regular, DurationSeconds: 0},
			want:    "declared audio duration",
		},
		{
			name:    "negative declared duration",
			request: TranscriptionRequest{Path: regular, DurationSeconds: -3},
			want:    "declared audio duration",
		},
		{
			name:    "duration above the limit",
			mutate:  func(speech *config.Speech) { speech.MaxDurationSec = 10 },
			request: TranscriptionRequest{Path: regular, DurationSeconds: 11},
			want:    "max_duration_seconds",
		},
		{
			name:    "file above the size limit",
			mutate:  func(speech *config.Speech) { speech.MaxFileMB = 1 },
			request: TranscriptionRequest{Path: oversized, DurationSeconds: 5},
			want:    "max_file_mb",
		},
		{
			name:    "directory instead of a regular file",
			request: TranscriptionRequest{Path: directory, DurationSeconds: 5},
			want:    "not a regular file",
		},
		{
			name:    "missing file",
			request: TranscriptionRequest{Path: missing, DurationSeconds: 5},
			want:    "cannot open",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker := elevenLabsFixture(t, test.mutate)
			worker.endpoint = server.URL
			before := requests.Load()
			_, err := worker.Transcribe(context.Background(), test.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			assertNoSecretLeak(t, err)
			if strings.Contains(err.Error(), directory) {
				t.Fatalf("error leaks the local path: %v", err)
			}
			if got := requests.Load(); got != before {
				t.Fatalf("network requests = %d, want %d (limits must fail before upload)", got, before)
			}
		})
	}
}

func TestElevenLabsMissingOrBlankEnvironmentKeyFailsWithoutRequest(t *testing.T) {
	server, requests := rawServer(http.StatusOK, nil, `{"text":"unexpected"}`)
	defer server.Close()
	path := elevenLabsFile(t, "voice.ogg", []byte("audio bytes"))

	t.Run("blank", func(t *testing.T) {
		t.Setenv(config.DefaultElevenLabsAPIKeyEnv, "   ")
		worker := elevenLabsFixture(t, nil)
		worker.endpoint = server.URL
		_, err := worker.Transcribe(context.Background(), TranscriptionRequest{Path: path, DurationSeconds: 5})
		if err == nil || !strings.Contains(err.Error(), "is not set") {
			t.Fatalf("blank key error = %v", err)
		}
		assertNoSecretLeak(t, err)
	})

	t.Run("missing", func(t *testing.T) {
		worker := elevenLabsFixture(t, func(speech *config.Speech) {
			speech.ElevenLabsAPIKeyEnv = "ELEVENLABS_TEST_KEY_DEFINITELY_ABSENT"
		})
		worker.endpoint = server.URL
		_, err := worker.Transcribe(context.Background(), TranscriptionRequest{Path: path, DurationSeconds: 5})
		if err == nil || !strings.Contains(err.Error(), "ELEVENLABS_TEST_KEY_DEFINITELY_ABSENT") {
			t.Fatalf("missing key error = %v", err)
		}
		assertNoSecretLeak(t, err)
	})

	if requests.Load() != 0 {
		t.Fatalf("network requests = %d, want 0", requests.Load())
	}
}

func TestElevenLabsStreamsAttachmentWithoutBuffering(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	fixture := make([]byte, 4*1024*1024)
	for index := range fixture {
		fixture[index] = byte(index * 31)
	}
	wantHash := sha256.Sum256(fixture)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// A buffered upload would carry a fixed Content-Length; the streaming
		// pipe body is chunked instead.
		if request.ContentLength != -1 {
			t.Errorf("content length = %d, want chunked (-1)", request.ContentLength)
		}
		if len(request.TransferEncoding) == 0 || request.TransferEncoding[0] != "chunked" {
			t.Errorf("transfer encoding = %#v, want chunked", request.TransferEncoding)
		}
		reader, err := request.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			http.Error(writer, "bad multipart", http.StatusBadRequest)
			return
		}
		hash := sha256.New()
		var size int64
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("next part: %v", err)
				break
			}
			if part.FormName() == "file" {
				size, _ = io.Copy(hash, part)
			}
		}
		if size != int64(len(fixture)) {
			t.Errorf("streamed size = %d, want %d", size, len(fixture))
		}
		if got := hash.Sum(nil); string(got) != string(wantHash[:]) {
			t.Errorf("streamed content hash mismatch")
		}
		_, _ = writer.Write([]byte(`{"text":"streamed"}`))
	}))
	defer server.Close()

	worker := elevenLabsFixture(t, nil)
	worker.endpoint = server.URL
	text, err := worker.Transcribe(context.Background(), TranscriptionRequest{
		Path:            elevenLabsFile(t, "large.ogg", fixture),
		DurationSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "streamed" {
		t.Fatalf("transcript = %q", text)
	}
}

// countingReader records how many source bytes the multipart body actually
// consumed, which exposes eager buffering.
type countingReader struct {
	reader io.Reader
	count  atomic.Int64
}

func (c *countingReader) Read(buffer []byte) (int, error) {
	count, err := c.reader.Read(buffer)
	c.count.Add(int64(count))
	return count, err
}

func TestMultipartUploadConsumesSourceLazily(t *testing.T) {
	source := &countingReader{reader: strings.NewReader(strings.Repeat("a", 1<<20))}
	body, _ := multipartUpload([][2]string{{"model_id", "scribe_v2"}}, "voice.ogg", source)
	// Reading only the preamble must not drain the attachment: the pipe
	// writer blocks instead of materializing the body in memory.
	head := make([]byte, 512)
	if _, err := io.ReadFull(body, head); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if consumed := source.count.Load(); consumed >= 1<<20 {
		t.Fatalf("multipart body buffered the source: %d bytes consumed for 512 bytes read", consumed)
	}
}

func TestElevenLabsParsesAndBoundsResponses(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "empty text", body: `{"text":"   "}`, want: "empty transcription"},
		{name: "oversized transcript", body: `{"text":"` + strings.Repeat("a", 1024*1024+16) + `"}`, want: "1 MB safety limit"},
		{name: "oversized body", body: `{"text":"` + strings.Repeat("a", 8*1024*1024) + `"}`, want: "8 MB safety limit"},
		{name: "invalid json", body: "not json at all", want: "invalid transcription response"},
		{name: "missing text field", body: `{"language_code":"en"}`, want: "empty transcription"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := rawServer(http.StatusOK, map[string]string{"Content-Type": "application/json"}, test.body)
			defer server.Close()
			worker := elevenLabsFixture(t, nil)
			worker.endpoint = server.URL
			_, err := worker.Transcribe(context.Background(), TranscriptionRequest{
				Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
				DurationSeconds: 5,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			assertNoSecretLeak(t, err)
		})
	}
}

func TestElevenLabsErrorParsingAndSanitization(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	long := strings.Repeat("x", 500)
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantParts []string
		absent    string
	}{
		{
			name:      "object shape with sanitized message",
			status:    http.StatusBadRequest,
			body:      `{"detail":{"type":"validation_error","code":"bad_code","message":"first\nsecond \u0000\u001b[31m [evil](x) ` + long + `"}}`,
			wantParts: []string{"HTTP 400", "(bad_code)", "first second", "(evil)(x)", "…"},
		},
		{
			name:      "object shape falls back to type code",
			status:    http.StatusTooManyRequests,
			body:      `{"detail":{"type":"rate_limit_exceeded","message":"slow down"}}`,
			wantParts: []string{"HTTP 429", "(rate_limit_exceeded)", "slow down"},
		},
		{
			name:      "fastapi validation array",
			status:    http.StatusUnprocessableEntity,
			body:      `{"detail":[{"loc":["body","file"],"msg":"field required","type":"value_error.missing"},{"loc":["body"],"msg":"not a valid file","type":"value_error"}]}`,
			wantParts: []string{"HTTP 422", "field required; not a valid file"},
		},
		{
			name:      "plain detail string",
			status:    http.StatusForbidden,
			body:      `{"detail":"quota [exceeded]"}`,
			wantParts: []string{"HTTP 403", "quota (exceeded)"},
		},
		{
			name:      "unrecognized json",
			status:    http.StatusBadGateway,
			body:      `{"error":"proxy noise"}`,
			wantParts: []string{"HTTP 502", "unrecognized provider error"},
			absent:    "proxy noise",
		},
		{
			name:      "unrecognized body",
			status:    http.StatusServiceUnavailable,
			body:      "<html>upstream exploded</html>",
			wantParts: []string{"HTTP 503", "unrecognized provider error"},
			absent:    "upstream exploded",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := rawServer(test.status, nil, test.body)
			defer server.Close()
			worker := elevenLabsFixture(t, nil)
			worker.endpoint = server.URL
			_, err := worker.Transcribe(context.Background(), TranscriptionRequest{
				Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
				DurationSeconds: 5,
			})
			if err == nil {
				t.Fatal("provider failure was accepted")
			}
			assertNoSecretLeak(t, err)
			text := err.Error()
			for _, part := range test.wantParts {
				if !strings.Contains(text, part) {
					t.Fatalf("error = %q, missing %q", text, part)
				}
			}
			if test.absent != "" && strings.Contains(text, test.absent) {
				t.Fatalf("error leaked raw body content: %q", text)
			}
			if strings.ContainsAny(text, "\x00\x1b\n") {
				t.Fatalf("error is not one sanitized line: %q", text)
			}
			if strings.Contains(text, "[") || strings.Contains(text, "]") {
				t.Fatalf("error retained square brackets: %q", text)
			}
			if utf8.RuneCountInString(text) > maxProviderMessageRunes+80 {
				t.Fatalf("error exceeds the bounded length: %d runes", utf8.RuneCountInString(text))
			}
		})
	}
}

func TestElevenLabsBoundedErrorBodyOversizeFailsWithoutParsing(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	tail := strings.Repeat("A", 100*1024) + "TAIL-MARKER-SHOULD-NEVER-APPEAR"
	server, _ := rawServer(http.StatusBadRequest, nil, `{"detail":{"code":"boom","message":"`+tail+`"}}`)
	defer server.Close()
	worker := elevenLabsFixture(t, nil)
	worker.endpoint = server.URL
	_, err := worker.Transcribe(context.Background(), TranscriptionRequest{
		Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
		DurationSeconds: 5,
	})
	if err == nil {
		t.Fatal("oversized error body was accepted")
	}
	if strings.Contains(err.Error(), "TAIL-MARKER") || strings.Contains(err.Error(), "boom") {
		t.Fatalf("oversized error body leaked content: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("error = %v", err)
	}
}

type scriptedResponse struct {
	status  int
	headers map[string]string
	body    string
}

func scriptedServer(t *testing.T, script []scriptedResponse) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		index := int(requests.Add(1)) - 1
		if index >= len(script) {
			index = len(script) - 1
		}
		step := script[index]
		for key, value := range step.headers {
			writer.Header().Set(key, value)
		}
		writer.WriteHeader(step.status)
		_, _ = io.WriteString(writer, step.body)
	}))
	return server, &requests
}

func TestElevenLabsRetrySemantics(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	rateLimit := `{"detail":{"code":"rate_limit_exceeded","message":"slow down"}}`
	ok := `{"text":"recovered"}`

	for _, test := range []struct {
		name        string
		script      []scriptedResponse
		wantText    string
		wantError   string
		wantRequest int
	}{
		{
			name: "retries once for rate limit with seconds",
			script: []scriptedResponse{
				{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "0"}, body: rateLimit},
				{status: http.StatusOK, body: ok},
			},
			wantText:    "recovered",
			wantRequest: 2,
		},
		{
			name: "retries once for rate limit with http date",
			script: []scriptedResponse{
				{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": time.Now().Add(-time.Second).UTC().Format(http.TimeFormat)}, body: rateLimit},
				{status: http.StatusOK, body: ok},
			},
			wantText:    "recovered",
			wantRequest: 2,
		},
		{
			name: "never retries twice",
			script: []scriptedResponse{
				{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "0"}, body: rateLimit},
				{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "0"}, body: rateLimit},
			},
			wantError:   "HTTP 429",
			wantRequest: 2,
		},
		{
			name:        "no retry above the 60 second cap",
			script:      []scriptedResponse{{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "61"}, body: rateLimit}},
			wantError:   "HTTP 429",
			wantRequest: 1,
		},
		{
			name:        "no retry for malformed Retry-After",
			script:      []scriptedResponse{{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "soon"}, body: rateLimit}},
			wantError:   "HTTP 429",
			wantRequest: 1,
		},
		{
			name:        "no retry without Retry-After",
			script:      []scriptedResponse{{status: http.StatusTooManyRequests, body: rateLimit}},
			wantError:   "HTTP 429",
			wantRequest: 1,
		},
		{
			name:        "no retry for concurrent limit",
			script:      []scriptedResponse{{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "0"}, body: `{"detail":{"code":"concurrent_limit_exceeded","message":"busy"}}`}},
			wantError:   "concurrent_limit_exceeded",
			wantRequest: 1,
		},
		{
			name:        "no retry for malformed 429 code",
			script:      []scriptedResponse{{status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "0"}, body: `{"detail":{"message":"no code here"}}`}},
			wantError:   "no code here",
			wantRequest: 1,
		},
		{
			name:        "no retry for bad request",
			script:      []scriptedResponse{{status: http.StatusBadRequest, body: `{"detail":{"code":"bad_request","message":"bad"}}`}},
			wantError:   "HTTP 400",
			wantRequest: 1,
		},
		{
			name:        "no retry for unauthorized",
			script:      []scriptedResponse{{status: http.StatusUnauthorized, body: `{"detail":{"code":"unauthorized","message":"no"}}`}},
			wantError:   "HTTP 401",
			wantRequest: 1,
		},
		{
			name:        "no retry for forbidden",
			script:      []scriptedResponse{{status: http.StatusForbidden, body: `{"detail":{"code":"forbidden","message":"no"}}`}},
			wantError:   "HTTP 403",
			wantRequest: 1,
		},
		{
			name:        "no retry for validation errors",
			script:      []scriptedResponse{{status: http.StatusUnprocessableEntity, body: `{"detail":[{"msg":"field required"}]}`}},
			wantError:   "HTTP 422",
			wantRequest: 1,
		},
		{
			name:        "no retry for server errors",
			script:      []scriptedResponse{{status: http.StatusInternalServerError, body: `{"detail":{"code":"boom","message":"oops"}}`}},
			wantError:   "HTTP 500",
			wantRequest: 1,
		},
		{
			name:        "no retry for unavailable service",
			script:      []scriptedResponse{{status: http.StatusServiceUnavailable, body: `maintenance`}},
			wantError:   "HTTP 503",
			wantRequest: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, requests := scriptedServer(t, test.script)
			defer server.Close()
			worker := elevenLabsFixture(t, nil)
			worker.endpoint = server.URL
			text, err := worker.Transcribe(context.Background(), TranscriptionRequest{
				Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
				DurationSeconds: 5,
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				assertNoSecretLeak(t, err)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if text != test.wantText {
					t.Fatalf("transcript = %q, want %q", text, test.wantText)
				}
			}
			if requests.Load() != int32(test.wantRequest) {
				t.Fatalf("requests = %d, want %d", requests.Load(), test.wantRequest)
			}
		})
	}

	t.Run("no retry for timeout", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			time.Sleep(300 * time.Millisecond)
			_, _ = writer.Write([]byte(`{"text":"late"}`))
		}))
		defer server.Close()
		worker := elevenLabsFixture(t, nil)
		worker.endpoint = server.URL
		worker.timeout = 30 * time.Millisecond
		_, err := worker.Transcribe(context.Background(), TranscriptionRequest{
			Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
			DurationSeconds: 5,
		})
		if err == nil {
			t.Fatal("timeout was accepted")
		}
		assertNoSecretLeak(t, err)
		time.Sleep(350 * time.Millisecond)
		if requests.Load() != 1 {
			t.Fatalf("requests = %d, want 1 (timeouts never retry)", requests.Load())
		}
	})

	t.Run("no retry for cancellation", func(t *testing.T) {
		var requests atomic.Int32
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			<-release
			_, _ = writer.Write([]byte(`{"text":"late"}`))
		}))
		defer server.Close()
		worker := elevenLabsFixture(t, nil)
		worker.endpoint = server.URL
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		_, err := worker.Transcribe(ctx, TranscriptionRequest{
			Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
			DurationSeconds: 5,
		})
		if err == nil {
			t.Fatal("cancellation was accepted")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
		close(release)
		time.Sleep(50 * time.Millisecond)
		if requests.Load() != 1 {
			t.Fatalf("requests = %d, want 1 (cancellations never retry)", requests.Load())
		}
	})
}

func TestElevenLabsSerializesConcurrentTranscriptions(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)
	var active atomic.Int32
	var maxActive atomic.Int32
	var served atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		count := active.Add(1)
		if count > maxActive.Load() {
			maxActive.Store(count)
		}
		if served.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		active.Add(-1)
		_, _ = writer.Write([]byte(`{"text":"serialized"}`))
	}))
	defer server.Close()
	worker := elevenLabsFixture(t, nil)
	worker.endpoint = server.URL
	path := elevenLabsFile(t, "voice.ogg", []byte("audio bytes"))

	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := worker.Transcribe(context.Background(), TranscriptionRequest{Path: path, DurationSeconds: 5})
			done <- err
		}()
	}
	<-entered
	time.Sleep(50 * time.Millisecond)
	if served.Load() != 1 {
		close(release)
		t.Fatalf("second transcription reached the network while one was active: %d requests", served.Load())
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if maxActive.Load() != 1 {
		t.Fatalf("concurrent cloud transcriptions = %d, want 1", maxActive.Load())
	}
}

func TestElevenLabsTimeoutCoversUploadProcessingAndRetryWait(t *testing.T) {
	t.Setenv(config.DefaultElevenLabsAPIKeyEnv, testElevenLabsKey)

	t.Run("upload and processing", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			time.Sleep(300 * time.Millisecond)
			_, _ = writer.Write([]byte(`{"text":"late"}`))
		}))
		defer server.Close()
		worker := elevenLabsFixture(t, nil)
		worker.endpoint = server.URL
		worker.timeout = 40 * time.Millisecond
		start := time.Now()
		_, err := worker.Transcribe(context.Background(), TranscriptionRequest{
			Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
			DurationSeconds: 5,
		})
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
			t.Fatalf("timeout fired after %v, want the configured 40ms bound", elapsed)
		}
	})

	t.Run("optional retry wait", func(t *testing.T) {
		server, requests := scriptedServer(t, []scriptedResponse{{
			status:  http.StatusTooManyRequests,
			headers: map[string]string{"Retry-After": "30"},
			body:    `{"detail":{"code":"rate_limit_exceeded","message":"slow down"}}`,
		}})
		defer server.Close()
		worker := elevenLabsFixture(t, nil)
		worker.endpoint = server.URL
		worker.timeout = 100 * time.Millisecond
		start := time.Now()
		_, err := worker.Transcribe(context.Background(), TranscriptionRequest{
			Path:            elevenLabsFile(t, "voice.ogg", []byte("audio bytes")),
			DurationSeconds: 5,
		})
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("retry wait ignored the overall timeout: %v", elapsed)
		}
		if requests.Load() != 1 {
			t.Fatalf("requests = %d, want 1 (the retry never left before the timeout)", requests.Load())
		}
	})
}
