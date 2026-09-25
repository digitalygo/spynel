package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFindReportsUninitializedDirectory(t *testing.T) {
	_, err := Find(t.TempDir())
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Find error = %v, want ErrNotInitialized", err)
	}
}

func TestFindDiscoversCanonicalConfigFromChildDirectory(t *testing.T) {
	root := t.TempDir()
	path := writeTestConfig(t, root, []byte("version: 1\n"))
	child := filepath.Join(root, "nested", "project")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	found, err := Find(child)
	if err != nil {
		t.Fatal(err)
	}
	if found != path {
		t.Fatalf("Find() = %q, want %q", found, path)
	}
}

func writeTestConfig(t *testing.T, root string, data []byte) string {
	t.Helper()
	path := PathForRoot(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Name != "" {
		t.Fatalf("default coding harness should await detection, got %q", cfg.Harness.Name)
	}
	if cfg.Harness.Sandbox != "danger-full-access" {
		t.Fatalf("default coding harness should be unrestricted, got %q", cfg.Harness.Sandbox)
	}
	if !cfg.Speech.Enabled || cfg.Speech.Language != "en" || cfg.Speech.NumThreads != 2 {
		t.Fatalf("unexpected speech defaults: %#v", cfg.Speech)
	}
	if cfg.Channels.TUI.Theme != "spynel" {
		t.Fatalf("unexpected default TUI theme: %#v", cfg.Channels.TUI)
	}
	if cfg.Workspace.CleanupRetentionDays != 30 {
		t.Fatalf("cleanup retention default = %d, want 30", cfg.Workspace.CleanupRetentionDays)
	}
}

func TestCleanupRetentionValidationRequiresBoundedPositiveWholeDays(t *testing.T) {
	for _, invalid := range []int{-1, 0, 36501} {
		cfg := Default()
		cfg.Workspace.CleanupRetentionDays = invalid
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cleanup_retention_days") {
			t.Fatalf("invalid cleanup retention %d produced %v", invalid, err)
		}
	}
}

func TestSpeechLanguagesContainEveryParakeetLanguage(t *testing.T) {
	want := []string{"auto", "bg", "hr", "cs", "da", "nl", "en", "et", "fi", "fr", "de", "el", "hu", "it", "lv", "lt", "mt", "pl", "pt", "ro", "sk", "sl", "es", "sv", "ru", "uk"}
	for _, language := range want {
		if !IsSpeechLanguage(language) {
			t.Fatalf("supported language %q is missing", language)
		}
	}
	if IsSpeechLanguage("ja") {
		t.Fatal("unsupported language was accepted")
	}
}

func TestTelegramWhitelistIsRequiredWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.Channels.Telegram.Enabled = true
	for _, invalid := range [][]string{nil, {}, {"  "}, {"@"}, {"..."}, {"-7"}, {"bad user"}, {strings.Repeat("a", 33)}} {
		cfg.Channels.Telegram.AllowedUsers = invalid
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_users requires at least one user") {
			t.Fatalf("enabled Telegram accepted invalid whitelist %#v: %v", invalid, err)
		}
	}
	cfg.Channels.Telegram.AllowedUsers = []string{"123456789"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled Telegram rejected a configured whitelist: %v", err)
	}
}

func TestTelegramWebhookRequiresSecretWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Mode = "webhook"
	cfg.Channels.Telegram.WebhookURL = "https://public.example"
	cfg.Channels.Telegram.AllowedUsers = []string{"123456789"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webhook_secret is required") {
		t.Fatalf("enabled Telegram webhook accepted an empty secret: %v", err)
	}
	cfg.Channels.Telegram.WebhookSecret = "verification-secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled Telegram webhook rejected a configured secret: %v", err)
	}
}

func TestWhatsAppWhitelistIsRequiredWhenEnabled(t *testing.T) {
	cfg := Default()
	cfg.Channels.WhatsApp.Enabled = true
	for _, invalid := range [][]string{nil, {}, {"  "}, {" + "}, {"phone"}, {"12x34"}, {"1234567890123456"}} {
		cfg.Channels.WhatsApp.AllowedNumbers = invalid
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_numbers requires at least one number") {
			t.Fatalf("enabled WhatsApp accepted invalid whitelist %#v: %v", invalid, err)
		}
	}
	cfg.Channels.WhatsApp.AllowedNumbers = []string{"+1 (555) 123-4567"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled WhatsApp rejected a configured whitelist: %v", err)
	}
}

func TestNormalizeWhatsAppNumber(t *testing.T) {
	tests := map[string]string{
		"+420 123 456 789":  "420123456789",
		"00420-123-456-789": "420123456789",
		"(420) 123.456.789": "420123456789",
		"0123 456 789":      "0123456789",
		"phone":             "",
	}
	for input, want := range tests {
		if got := NormalizeWhatsAppNumber(input); got != want {
			t.Errorf("NormalizeWhatsAppNumber(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadMergesUserValuesWithDefaults(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\nworkspace:\n  history_char_limit: 321\nharness:\n  name: codex\nchannels:\n  whatsapp:\n    mode: dedicated\n")
	path := writeTestConfig(t, root, data)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.HistoryCharLimit != 321 || cfg.Workspace.HistoryMaxMessages != 50 {
		t.Fatalf("defaults were not retained: %#v", cfg.Workspace)
	}
	if cfg.Harness.Sandbox != "danger-full-access" {
		t.Fatalf("missing sandbox did not inherit the unrestricted default: %#v", cfg.Harness)
	}
	if cfg.Channels.WhatsApp.Mode != "dedicated" || cfg.Channels.Telegram.PollTimeoutSec != 30 {
		t.Fatalf("nested defaults were not retained: %#v", cfg.Channels)
	}
	if cfg.Channels.TUI.Theme != "spynel" {
		t.Fatalf("missing TUI theme did not inherit the default: %#v", cfg.Channels.TUI)
	}
	if cfg.Resolve("relative") != filepath.Join(root, "relative") {
		t.Fatalf("relative path did not resolve against config root")
	}
}

func TestReasoningEffortOmissionPreservesLegacyMediumAndExplicitInherit(t *testing.T) {
	root := t.TempDir()
	path := writeTestConfig(t, root, []byte("version: 1\nharness:\n  name: codex\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.ReasoningEffort != "medium" {
		t.Fatalf("omitted reasoning effort = %q, want legacy medium", cfg.Harness.ReasoningEffort)
	}
	if !cfg.Harness.UsesLegacyReasoningEffort() {
		t.Fatal("omitted reasoning effort lost its legacy provenance")
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "reasoning_effort:") {
		t.Fatalf("legacy omission was materialized during unrelated save:\n%s", saved)
	}
	cfg, err = Load(path)
	if err != nil || cfg.Harness.ReasoningEffort != "medium" {
		t.Fatalf("saved omitted reasoning effort = %q, %v", cfg.Harness.ReasoningEffort, err)
	}

	path = writeTestConfig(t, root, []byte("version: 1\nharness:\n  name: codex\n  reasoning_effort: inherit\n"))
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.ReasoningEffort != "" {
		t.Fatalf("explicit inherited reasoning effort = %q, want empty provider default", cfg.Harness.ReasoningEffort)
	}
	if cfg.Harness.UsesLegacyReasoningEffort() {
		t.Fatal("explicit inherit was marked as a legacy omitted effort")
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil || cfg.Harness.ReasoningEffort != "" {
		t.Fatalf("saved inherited reasoning effort = %q, %v", cfg.Harness.ReasoningEffort, err)
	}
}

func TestDirectConfigRejectsUnsupportedACPInferenceProperties(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name string
		yaml string
		want string
	}{
		{name: "reasoning", yaml: "reasoning_effort: high", want: "reasoning_effort is not supported for ACP"},
		{name: "reasoning medium", yaml: "reasoning_effort: medium", want: "reasoning_effort is not supported for ACP"},
		{name: "speed", yaml: "service_mode: fast", want: "service_mode is not supported for agent-zero"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeTestConfig(t, root, []byte("version: 1\nharness:\n  name: agent-zero\n  "+test.yaml+"\n"))
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsupported ACP property error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadIgnoresUnusedKeysAndSaveRemovesThem(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\nunused: {anything: true}\nspeech:\n  command: obsolete\nchannels:\n  tui:\n    enabled: false\n    title: Preserved\norchestrator:\n  routes: [{source: old-folder}]\n")
	path := writeTestConfig(t, root, data)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels.TUI.Title != "Preserved" {
		t.Fatal("known setting lost")
	}
	before, err := os.ReadFile(path)
	if err != nil || string(before) != string(data) {
		t.Fatal("load rewrote config")
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"unused:", "command:", "routes:", "old-folder", "obsolete"} {
		if strings.Contains(string(after), key) {
			t.Fatalf("unused key survived save: %s", key)
		}
	}
	reloaded, err := Load(path)
	if err != nil || reloaded.Channels.TUI.Title != "Preserved" {
		t.Fatalf("saved settings: %#v, %v", reloaded, err)
	}
	for _, invalid := range []string{"speech: {enabled: invalid}", "version: 1\nversion: 2"} {
		if _, err := decode([]byte(invalid), path); err == nil {
			t.Fatalf("invalid current setting accepted: %s", invalid)
		}
	}
}

func TestRetiredWorkflowKeysAreIgnoredOnLoadAndOmittedOnSave(t *testing.T) {
	legacy := []byte("version: 1\n" +
		"orchestrator:\n" +
		"  enabled: true\n" +
		"  interval_seconds: 5\n" +
		"  retrigger_unresponded_messages: false\n" +
		"  semantic_heartbeat_minutes: 15\n" +
		"  task_notifications: always\n" +
		"  max_parallel: 8\n" +
		"harness:\n" +
		"  name: codex\n" +
		"  reviews: never\n" +
		"  chat_agent_prefix: /ultrathink\n" +
		"  developer_agent_prefix: /dev\n" +
		"  reviewer_agent_prefix: /review\n" +
		"  heartbeat_agent_prefix: /audit\n" +
		"workspace:\n" +
		"  history_max_messages: 25\n" +
		"channels:\n" +
		"  telegram:\n" +
		"    token: 123:legacy-token\n" +
		"    allowed_users: [\"123456789\"]\n" +
		"speech:\n" +
		"  elevenlabs_api_key: legacy-stored-key\n")
	current := []byte("version: 1\n" +
		"harness:\n" +
		"  name: codex\n" +
		"workspace:\n" +
		"  history_max_messages: 25\n" +
		"channels:\n" +
		"  telegram:\n" +
		"    token: 123:legacy-token\n" +
		"    allowed_users: [\"123456789\"]\n" +
		"speech:\n" +
		"  elevenlabs_api_key: legacy-stored-key\n")
	root := t.TempDir()
	path := writeTestConfig(t, root, legacy)

	legacyCfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(legacy) {
		t.Fatal("load rewrote a legacy configuration containing retired keys")
	}

	// The retired keys must not influence the decoded configuration at all:
	// removing them from the same file yields an identical config.
	canonicalCfg, err := Load(writeTestConfig(t, root, current))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacyCfg, canonicalCfg) {
		t.Fatalf("retired keys changed the decoded configuration:\nlegacy %#v\ncurrent %#v", legacyCfg, canonicalCfg)
	}
	if legacyCfg.Channels.Telegram.Token != "123:legacy-token" || len(legacyCfg.Channels.Telegram.AllowedUsers) != 1 {
		t.Fatalf("channel settings were not preserved: %#v", legacyCfg.Channels.Telegram)
	}
	if legacyCfg.Speech.ElevenLabsAPIKey != "legacy-stored-key" {
		t.Fatalf("stored secret was not preserved: %q", legacyCfg.Speech.ElevenLabsAPIKey)
	}
	if legacyCfg.Workspace.HistoryMaxMessages != 25 || legacyCfg.Harness.Name != "codex" {
		t.Fatalf("retained settings were not preserved: %#v", legacyCfg)
	}

	// An explicit canonical save drops every retired key while keeping the
	// current settings and secrets.
	path = writeTestConfig(t, root, legacy)
	legacyCfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(legacyCfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{
		"orchestrator:", "retrigger_unresponded_messages", "semantic_heartbeat_minutes",
		"task_notifications", "max_parallel", "reviews:", "chat_agent_prefix:", "developer_agent_prefix:",
		"reviewer_agent_prefix:", "heartbeat_agent_prefix:", "/ultrathink", "/dev", "/review", "/audit", "never",
	} {
		if strings.Contains(string(saved), retired) {
			t.Fatalf("retired key or value %q survived the canonical save:\n%s", retired, saved)
		}
	}
	for _, preserved := range []string{
		"name: codex", "history_max_messages: 25", "token: 123:legacy-token", "allowed_users:", "elevenlabs_api_key: legacy-stored-key",
	} {
		if !strings.Contains(string(saved), preserved) {
			t.Fatalf("canonical save lost %q:\n%s", preserved, saved)
		}
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// yaml marshals a nil allow-list as [] and decodes [] as an empty slice,
	// so the canonical round trip is compared with normalized empties.
	if !reflect.DeepEqual(normalizeEmptyConfigSlices(reloaded), normalizeEmptyConfigSlices(legacyCfg)) {
		t.Fatalf("reloading the saved configuration changed it:\nreloaded %#v\nbefore %#v", reloaded, legacyCfg)
	}
}

// normalizeEmptyConfigSlices collapses empty allow-list and argument slices
// to nil so canonical save/load round trips compare by content.
func normalizeEmptyConfigSlices(cfg Config) Config {
	if len(cfg.Channels.Telegram.AllowedUsers) == 0 {
		cfg.Channels.Telegram.AllowedUsers = nil
	}
	if len(cfg.Channels.WhatsApp.AllowedNumbers) == 0 {
		cfg.Channels.WhatsApp.AllowedNumbers = nil
	}
	if len(cfg.Harness.ACPArgs) == 0 {
		cfg.Harness.ACPArgs = nil
	}
	return cfg
}

func TestHarnessSandboxValidationAcceptsCanonicalModes(t *testing.T) {
	for _, mode := range []string{"read-only", "workspace-write", "danger-full-access"} {
		cfg := Default()
		cfg.Harness.Sandbox = mode
		if err := cfg.Validate(); err != nil {
			t.Fatalf("sandbox %q: %v", mode, err)
		}
	}
	cfg := Default()
	cfg.Harness.Sandbox = "unknown"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "harness.sandbox") {
		t.Fatalf("invalid sandbox validation = %v", err)
	}
}

func TestCustomACPRequiresCommandAndPreservesShellFreeArguments(t *testing.T) {
	cfg := Default()
	cfg.Harness.Name = "acp"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "harness.acp_command") {
		t.Fatalf("custom ACP without command = %v", err)
	}
	cfg.Harness.ACPCommand = "/tools/custom agent"
	cfg.Harness.ACPArgs = []string{"--stdio", "value with spaces"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid custom ACP rejected: %v", err)
	}
	arguments := cfg.HarnessArgs()
	if len(arguments) != 2 || arguments[1] != "value with spaces" {
		t.Fatalf("custom ACP args = %#v", arguments)
	}
	arguments[0] = "changed"
	if cfg.Harness.ACPArgs[0] != "--stdio" {
		t.Fatal("custom ACP arguments were returned by reference")
	}
	cfg.Harness.ACPArgs = []string{"bad\x00argument"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("custom ACP NUL argument = %v", err)
	}
}

func TestLoadRejectsUnknownHarnessName(t *testing.T) {
	root := t.TempDir()
	data := []byte("version: 1\nharness:\n  name: custom-acp\n  sandbox: danger-full-access\n  acp_command: fixture\n")
	path := writeTestConfig(t, root, data)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "harness.name") {
		t.Fatalf("unknown harness error = %v", err)
	}
}

func TestStorePersistsValidatedUpdatesAndPublishesSnapshot(t *testing.T) {
	root := t.TempDir()
	path := PathForRoot(root)
	cfg := Default()
	cfg.Path = path
	cfg.Root = root
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	store := NewStore(cfg)
	updated, err := store.Update(func(next *Config) error {
		next.Workspace.HistoryMaxMessages = 25
		next.Workspace.HistoryCharLimit = 9000
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Workspace.HistoryMaxMessages != 25 || store.Snapshot().Workspace.HistoryCharLimit != 9000 {
		t.Fatalf("unexpected stored config: %#v", store.Snapshot().Workspace)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Workspace.HistoryMaxMessages != 25 || reloaded.Workspace.HistoryCharLimit != 9000 {
		t.Fatalf("update was not persisted: %#v", reloaded.Workspace)
	}
	select {
	case event := <-store.Updates():
		if event.Workspace.HistoryMaxMessages != 25 {
			t.Fatalf("unexpected update event: %#v", event.Workspace)
		}
	default:
		t.Fatal("configuration update was not published")
	}
	if _, err := store.Update(func(next *Config) error {
		next.Workspace.HistoryMaxMessages = -1
		return nil
	}); err == nil {
		t.Fatal("invalid configuration update succeeded")
	}
	if store.Snapshot().Workspace.HistoryMaxMessages != 25 {
		t.Fatal("invalid update changed the in-memory snapshot")
	}
}

func TestStoreUpdateSavesAndReloadsSharedSnapshot(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	cfg.Path = PathForRoot(root)
	cfg.Root = root
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	store := NewStore(cfg)
	updated, err := store.Update(func(next *Config) error {
		next.Channels.Telegram.Name = "reloaded"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(cfg.Path)
	if err != nil || reloaded.Channels.Telegram.Name != "reloaded" {
		t.Fatalf("saved configuration = %q, %v", reloaded.Channels.Telegram.Name, err)
	}
	if updated.Channels.Telegram.Name != "reloaded" || store.Snapshot().Channels.Telegram.Name != "reloaded" {
		t.Fatalf("shared snapshot was not refreshed: update=%q snapshot=%q", updated.Channels.Telegram.Name, store.Snapshot().Channels.Telegram.Name)
	}
}

func TestTranscriptEchoDefaultsOnAndDecodesExplicitly(t *testing.T) {
	root := t.TempDir()
	omitted, err := Load(writeTestConfig(t, root, []byte("version: 1\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !omitted.Speech.TranscriptEcho {
		t.Fatal("omitted speech.transcript_echo should default on")
	}
	loaded, err := Load(writeTestConfig(t, root, []byte("version: 1\nspeech:\n  transcript_echo: false\n")))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Speech.TranscriptEcho {
		t.Fatal("explicitly disabled speech.transcript_echo decoded as enabled")
	}
}

func TestSpeechProviderDefaultsAndNormalization(t *testing.T) {
	cfg := Default()
	if cfg.Speech.Provider != SpeechProviderElevenLabs {
		t.Fatalf("default speech provider = %q, want %q", cfg.Speech.Provider, SpeechProviderElevenLabs)
	}
	if cfg.Speech.ElevenLabsAPIKeyEnv != DefaultElevenLabsAPIKeyEnv {
		t.Fatalf("default API key env = %q, want %q", cfg.Speech.ElevenLabsAPIKeyEnv, DefaultElevenLabsAPIKeyEnv)
	}
	if cfg.Speech.ElevenLabsModelID != ElevenLabsModelScribeV2 {
		t.Fatalf("default ElevenLabs model = %q, want %q", cfg.Speech.ElevenLabsModelID, ElevenLabsModelScribeV2)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	// An existing workspace that never wrote the provider key keeps working: it
	// decodes to the current default, whose missing API key falls back to the
	// local Parakeet backend at transcription time.
	omitted, err := Load(writeTestConfig(t, root, []byte("version: 1\n")))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Speech.Provider != SpeechProviderElevenLabs {
		t.Fatalf("omitted provider = %q, want %q", omitted.Speech.Provider, SpeechProviderElevenLabs)
	}
	if err := omitted.Validate(); err != nil {
		t.Fatal(err)
	}

	path := writeTestConfig(t, root, []byte("version: 1\nspeech:\n  provider: '  PARAKEET '\n  elevenlabs_model_id: ' Scribe_V1 '\n  elevenlabs_api_key_env: ' MY_KEY_1 '\n"))
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Speech.Provider != "parakeet" {
		t.Fatalf("normalized provider = %q", loaded.Speech.Provider)
	}
	if loaded.Speech.ElevenLabsModelID != "scribe_v1" {
		t.Fatalf("normalized model = %q", loaded.Speech.ElevenLabsModelID)
	}
	if loaded.Speech.ElevenLabsAPIKeyEnv != "MY_KEY_1" {
		t.Fatalf("normalized API key env = %q", loaded.Speech.ElevenLabsAPIKeyEnv)
	}
}

func TestSpeechProviderAndElevenLabsValidation(t *testing.T) {
	root := t.TempDir()
	for _, invalid := range []string{
		"version: 1\nspeech: {provider: local}\n",
		"version: 1\nspeech: {provider: ''}\n",
		"version: 1\nspeech: {elevenlabs_model_id: scribe_v3}\n",
		"version: 1\nspeech: {elevenlabs_model_id: ''}\n",
		"version: 1\nspeech: {elevenlabs_api_key_env: ''}\n",
		"version: 1\nspeech: {elevenlabs_api_key_env: '1BAD'}\n",
		"version: 1\nspeech: {elevenlabs_api_key_env: 'A-B'}\n",
		"version: 1\nspeech: {elevenlabs_api_key_env: 'has space'}\n",
		"version: 1\nspeech: {elevenlabs_api_key_env: '" + strings.Repeat("a", 129) + "'}\n",
	} {
		path := writeTestConfig(t, root, []byte(invalid))
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid speech setting accepted: %s", invalid)
		}
	}
}

func TestElevenLabsAPIKeyResolutionPrecedenceAndNormalization(t *testing.T) {
	const stored = "stored-secret-key"
	const inherited = "inherited-env-key"
	cfg := Default()
	t.Setenv(cfg.Speech.ElevenLabsAPIKeyEnv, inherited)

	if got := cfg.ElevenLabsAPIKey(); got != inherited {
		t.Fatalf("environment fallback = %q, want %q", got, inherited)
	}
	cfg.Speech.ElevenLabsAPIKey = "  " + stored + "\t"
	if got := cfg.ElevenLabsAPIKey(); got != stored {
		t.Fatalf("stored key must win over the environment: %q", got)
	}
	cfg.Speech.ElevenLabsAPIKey = "   "
	if got := cfg.ElevenLabsAPIKey(); got != inherited {
		t.Fatalf("blank stored key must fall through to the environment: %q", got)
	}
	cfg.Speech.ElevenLabsAPIKey = ""
	cfg.Speech.ElevenLabsAPIKeyEnv = "ELEVENLABS_TEST_KEY_DEFINITELY_ABSENT"
	if got := cfg.ElevenLabsAPIKey(); got != "" {
		t.Fatalf("missing environment key = %q, want empty", got)
	}

	// A decoded stored value is trimmed like the Telegram token, and the
	// environment value is trimmed at resolution time.
	root := t.TempDir()
	path := writeTestConfig(t, root, []byte("version: 1\nspeech:\n  elevenlabs_api_key: '  "+stored+"  '\n"))
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Speech.ElevenLabsAPIKey != stored {
		t.Fatalf("decoded stored key = %q, want %q", loaded.Speech.ElevenLabsAPIKey, stored)
	}
	t.Setenv("ELEVENLABS_TEST_KEY_WITH_SPACES", "  env-trimmed  ")
	loaded.Speech.ElevenLabsAPIKey = ""
	loaded.Speech.ElevenLabsAPIKeyEnv = "ELEVENLABS_TEST_KEY_WITH_SPACES"
	if got := loaded.ElevenLabsAPIKey(); got != "env-trimmed" {
		t.Fatalf("trimmed environment key = %q", got)
	}
}

func TestElevenLabsAPIKeyCanonicalSaveOmitsEmptyStoredKey(t *testing.T) {
	root := t.TempDir()
	path := writeTestConfig(t, root, []byte("version: 1\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "elevenlabs_api_key:") {
		t.Fatalf("fresh canonical save contains a stored API key:\n%s", data)
	}
	cfg.Speech.ElevenLabsAPIKey = "stored-secret-key"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "elevenlabs_api_key: stored-secret-key") {
		t.Fatalf("stored API key was not persisted:\n%s", data)
	}
}

func TestValidEnvironmentVariableName(t *testing.T) {
	for value, want := range map[string]bool{
		"ELEVENLABS_API_KEY":     true,
		"_x9":                    true,
		"a":                      true,
		"":                       false,
		"1BAD":                   false,
		"A-B":                    false,
		"has space":              false,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
	} {
		if got := ValidEnvironmentVariableName(value); got != want {
			t.Fatalf("ValidEnvironmentVariableName(%q) = %t, want %t", value, got, want)
		}
	}
}
