package agentdocs

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCatalogReferencesAreStableAndResolvable(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{"integration", "commands", "notifications", "jobs", "logs", "configuration", "channels", "harnesses", "instances-primary", "workspace-state", "security", "troubleshooting", "architecture"}
	for _, id := range want {
		if _, ok := topicByID(id); !ok {
			t.Errorf("missing required topic %q", id)
		}
	}
}

func TestNoRetiredWorkflowTopicsOrCommands(t *testing.T) {
	for _, retired := range []string{"tasks", "goals", "reviews", "persistent-instructions", "workflows"} {
		document, err := Lookup(Request{Topic: retired})
		if err != nil {
			t.Fatal(err)
		}
		if document.Error == nil || document.Error.Code != "unknown_topic" {
			t.Errorf("retired topic %q = %#v", retired, document)
		}
	}
	commands := strings.Join(DocumentedSlashCommands(), " ")
	for _, retired := range []string{"/tasks", "/goals", "/task", "/goal", "/trigger"} {
		if strings.Contains(commands, retired) {
			t.Errorf("retired slash command %q is still documented", retired)
		}
	}
	for _, topic := range topics {
		var content strings.Builder
		for _, section := range topic.Sections {
			content.WriteString(section.Content)
			content.WriteString("\n")
		}
		output := strings.ToLower(content.String())
		for _, retired := range []string{
			"/tasks", "/goals", "/task ", "/goal ", "/trigger",
			"spynel tasks", "spynel goals", "spynel task ", "spynel goal ", "spynel trigger",
			"semantic heartbeat", "notification agent", "unresponded",
			"markdown task management", "agent prefix", "harness.reviews", "orchestrator",
		} {
			if strings.Contains(output, retired) {
				t.Errorf("topic %q still documents retired behavior %q", topic.ID, retired)
			}
		}
	}
}

func TestDocumentedSlashCommandsMatchRetainedCatalog(t *testing.T) {
	want := []string{"/help", "/status", "/primary", "/welcome", "/config", "/harness", "/model", "/effort", "/speed", "/theme", "/telegram", "/whatsapp", "/title", "/new", "/stop", "/pi", "/restart", "/update", "/history", "/resume", "/log", "/jobs", "/job", "/clear", "/cleanup", "/extension", "/quit"}
	got := DocumentedSlashCommands()
	if len(got) != len(want) {
		t.Fatalf("documented commands = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("documented command %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestHelpTopicsAreExactlySharedHelpIDs(t *testing.T) {
	want := map[string]string{"about": "workspace-state", "commands": "commands", "config": "configuration", "channels": "channels", "extensions": "architecture"}
	help := HelpTopics()
	if len(help) != len(want) {
		t.Fatalf("help topics = %#v", help)
	}
	for _, topic := range help {
		source, ok := want[topic.ID]
		if !ok {
			t.Errorf("unexpected help topic %q", topic.ID)
			continue
		}
		if topic.Title == "" || topic.Summary == "" || topic.Kind == "" {
			t.Errorf("help topic %q lacks metadata: %#v", topic.ID, topic)
		}
		if stored, ok := topicByID(source); !ok || stored.HelpID != topic.ID {
			t.Errorf("help topic %q is not backed by topic %q", topic.ID, source)
		}
	}
}

func TestRetainedSectionIDsAreStable(t *testing.T) {
	want := map[string][]string{
		"integration":       {"contract", "events", "transport"},
		"commands":          {"shell", "slash", "updates"},
		"notifications":     {"recent-routing", "delivery"},
		"jobs":              {"live", "states", "control", "archive"},
		"logs":              {"query", "job-output", "safety"},
		"configuration":     {"file", "changes", "secrets"},
		"channels":          {"conversations", "tui-editing", "delivery", "remote-access"},
		"harnesses":         {"selection", "sessions", "slash-resources", "permissions"},
		"instances-primary": {"election", "cross-environment", "handoff", "conversation"},
		"workspace-state":   {"product-model", "pillars", "layout", "startup-discovery", "ownership", "static-vs-live"},
		"security":          {"secrets", "execution", "precedence"},
		"troubleshooting":   {"offline", "runtime", "foreign-primary"},
		"architecture":      {"boundaries", "flow", "extensions"},
	}
	for id, sections := range want {
		topic, ok := topicByID(id)
		if !ok {
			t.Errorf("missing topic %q", id)
			continue
		}
		if len(topic.Sections) != len(sections) {
			t.Errorf("topic %q sections = %#v", id, topic.Sections)
			continue
		}
		for index, section := range topic.Sections {
			if section.ID != sections[index] {
				t.Errorf("topic %q section %d = %q, want %q", id, index, section.ID, sections[index])
			}
		}
	}
}

func TestCommandsTopicDocumentsRetainedSurfaces(t *testing.T) {
	output, err := Render(Request{Topic: "commands"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/jobs", "/cleanup [days]", "/pi session", "/primary", "Unrecognized slash input", "native skill", "prompt-template", "`notify`", "`spynel docs`"} {
		if !strings.Contains(output, want) {
			t.Errorf("commands documentation missing %q:\n%s", want, output)
		}
	}
}

func TestNotificationsTopicDocumentsExplicitNotify(t *testing.T) {
	output, err := Render(Request{Topic: "notifications"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--recent-authorized", "--workdir", "--origin", "--config", "outbox", "at least once", "mutually exclusive"} {
		if !strings.Contains(output, want) {
			t.Errorf("notifications documentation missing %q:\n%s", want, output)
		}
	}
}

func TestChannelsTopicDocumentsTelegramTopicsAndRichText(t *testing.T) {
	output, err := Render(Request{Topic: "channels"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"TG-<user-id>-topic-<thread-id>",
		"TG-group-<chat-id>-topic-<thread-id>",
		"General topic",
		"independent durable history and harness state",
		"last non-continuing final response or terminal error",
		"4096 parsed-visible code points",
		"32768 parsed-visible code point reply budget",
		"truncation marker",
		"sendMessage",
		"rate-limit rejection",
		"--extension",
		"before_agent_start",
		"spynel_telegram",
		"APPEND_SYSTEM.md",
		"guidance, not a guarantee",
		"one final question that ends the turn",
		"all_private_chats",
		"native command menu",
		"surface-invalid",
		"never replaces inbound allow-list checks",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("channels documentation missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "append-system-prompt") {
		t.Errorf("channels documentation still claims the stale --append-system-prompt mechanism:\n%s", output)
	}
}

func TestHarnessTopicDocumentsPiACPAndQueueBatching(t *testing.T) {
	output, err := Render(Request{Topic: "harnesses"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`pi`", "ACP aliases", "harness.acp_command", "stdio", "not an endpoint URL", "dispatched together", "not an operating-system sandbox", "native skill", "prompt-template", "--extension", "before_agent_start", "spynel_telegram", "APPEND_SYSTEM.md", "guidance, not a guarantee"} {
		if !strings.Contains(output, want) {
			t.Errorf("harness documentation missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "append-system-prompt") {
		t.Errorf("harness documentation still claims the stale --append-system-prompt mechanism:\n%s", output)
	}
}

func TestAboutTopicStatesProductBoundaryAndPillars(t *testing.T) {
	output, err := Render(Request{Topic: "workspace-state"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Simplicity at scale", "classic, non-AI program", "One human → one agent → infinite agents", "assistant-facing relationship", "Three pillars", "communication interface", "Durable conversation history and runtime oversight", "Agentic loops", "harness improvements strengthen rather than displace", "Simplicity. Leverage. Quality."} {
		if !strings.Contains(output, want) {
			t.Errorf("about documentation missing %q:\n%s", want, output)
		}
	}
}

func TestIndexTopicAndSearchPagination(t *testing.T) {
	index, err := Lookup(Request{})
	if err != nil {
		t.Fatal(err)
	}
	if index.Kind != "index" || index.Page.Number != 1 || index.Page.Total != 1 || len(index.Topics) != len(topics) {
		t.Fatalf("index = %#v", index)
	}
	topic, _ := Lookup(Request{Topic: "jobs"})
	if topic.Kind != "topic" || topic.ID != "jobs" || len(topic.Sections) == 0 || topic.Sections[0].ID == "" {
		t.Fatalf("topic = %#v", topic)
	}
	search, _ := Lookup(Request{Search: "history"})
	if search.Kind != "search" || search.Query != "history" || search.Page.TotalEntries < 2 {
		t.Fatalf("search = %#v", search)
	}
	badPage, _ := Lookup(Request{Topic: "jobs", Page: 99})
	if badPage.Error == nil || badPage.Error.Code != "page_out_of_range" || !strings.Contains(badPage.Error.Suggestion, "jobs page 1") {
		t.Fatalf("bad page = %#v", badPage)
	}
}

func TestPaginationUsesConservativeTokenBudgetAndRecordBoundaries(t *testing.T) {
	weights := []pageWeight{{tokens: 4_000}, {tokens: 4_000}, {tokens: 4_000}, {tokens: 100}}
	start, end, first, ok := paginate(weights, 1)
	if !ok || start != 0 || end != 2 || first.Total != 2 || first.TokenBudget != DefaultTokenBudget || first.EstimatedTokens != 8_000 {
		t.Fatalf("first page = %d:%d %#v, %t", start, end, first, ok)
	}
	start, end, second, ok := paginate(weights, 2)
	if !ok || start != 2 || end != 4 || second.EstimatedTokens != 4_100 {
		t.Fatalf("second page = %d:%d %#v, %t", start, end, second, ok)
	}
	if got := estimateTokens(strings.Repeat("x", 30_000)); got != DefaultTokenBudget {
		t.Fatalf("token estimate = %d", got)
	}
	byteWeights := []pageWeight{{tokens: 1, bytes: 35_000, runes: 20_000}, {tokens: 1, bytes: 35_000, runes: 20_000}}
	_, end, metadata, ok := paginate(byteWeights, 1)
	if !ok || end != 1 || metadata.Total != 2 {
		t.Fatalf("byte-bounded page = end %d, %#v, %t", end, metadata, ok)
	}
	escaped := weigh("<plain>")
	if escaped.bytes != len(`\u003cplain\u003e`)+256 || escaped.runes != len(`\u003cplain\u003e`)+256 {
		t.Fatalf("representation-aware weight = %#v", escaped)
	}
}

func TestJSONSchemaErrorsSuggestionsAndOutputBounds(t *testing.T) {
	output, err := Render(Request{Topic: "channnels", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	var document Document
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatal(err)
	}
	if document.SchemaVersion != SchemaVersion || document.Kind != "error" || document.Error == nil || document.Error.Code != "unknown_topic" || document.Error.Suggestion != "spynel docs channels" {
		t.Fatalf("error document = %#v", document)
	}
	for _, request := range []Request{{}, {Topic: "jobs"}, {Search: "state"}, {Topic: strings.Repeat("x", 129)}} {
		for _, format := range []string{"text", "json"} {
			request.Format = format
			output, err := Render(request)
			if err != nil {
				t.Fatalf("Render(%#v): %v", request, err)
			}
			if len(output) > MaxBytes || utf8.RuneCountInString(output) > MaxRunes {
				t.Fatalf("unbounded output: %d bytes, %d runes", len(output), utf8.RuneCountInString(output))
			}
		}
	}
	section, _ := Lookup(Request{Topic: "jobs#states"})
	if section.ID != "jobs#states" || len(section.Sections) != 1 || section.Sections[0].ID != "states" {
		t.Fatalf("section reference = %#v", section)
	}
	badSection, _ := Lookup(Request{Topic: "jobs#statez"})
	if badSection.Error == nil || badSection.Error.Code != "unknown_section" || badSection.Error.Suggestion != "spynel docs jobs#states" {
		t.Fatalf("section suggestion = %#v", badSection)
	}
	malformed, _ := Lookup(Request{Topic: "jobs#"})
	if malformed.Error == nil || malformed.Error.Code != "invalid_reference" {
		t.Fatalf("malformed section reference = %#v", malformed)
	}
	jsonOutput, err := Render(Request{Topic: "jobs", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	var sized Document
	if err := json.Unmarshal([]byte(jsonOutput), &sized); err != nil || sized.Page.Bytes != len(jsonOutput) || sized.Page.Runes != utf8.RuneCountInString(jsonOutput) {
		t.Fatalf("JSON size metadata = %#v, actual %d/%d, %v", sized.Page, len(jsonOutput), utf8.RuneCountInString(jsonOutput), err)
	}
}

func TestPlainOutputHasNoTerminalControls(t *testing.T) {
	output, err := Render(Request{Topic: "commands"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "\x1b") || strings.Contains(output, "\r") {
		t.Fatalf("plain output contains terminal controls: %q", output)
	}
	control, err := Render(Request{Search: "history\x1b[31m"})
	if err != nil || strings.Contains(control, "\x1b") || !strings.Contains(control, "invalid_input") {
		t.Fatalf("control query response = %q, %v", control, err)
	}
}
