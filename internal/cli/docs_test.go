package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/digitalygo/spynel/internal/agentdocs"
)

func TestParseDocsArgsSupportsPortableFlagAndPageOrdering(t *testing.T) {
	for _, test := range []struct {
		args   []string
		topic  string
		search string
		page   int
		format string
	}{
		{args: nil, page: 1, format: "text"},
		{args: []string{"commands", "--format", "json"}, topic: "commands", page: 1, format: "json"},
		{args: []string{"--format=json", "search", "job output", "page", "2"}, search: "job output", page: 2, format: "json"},
		{args: []string{"page", "1"}, page: 1, format: "text"},
	} {
		request, err := parseDocsArgs(test.args)
		if err != nil {
			t.Fatalf("parse %v: %v", test.args, err)
		}
		if request.Topic != test.topic || request.Search != test.search || request.Page != test.page || request.Format != test.format {
			t.Fatalf("parse %v = %#v", test.args, request)
		}
	}
}

func TestDocsCommandIsOfflineStructuredAndRejectsBadInputs(t *testing.T) {
	var output bytes.Buffer
	if err := runDocsCommand([]string{"commands", "--format", "json"}, &output); err != nil {
		t.Fatal(err)
	}
	var document agentdocs.Document
	if err := json.Unmarshal(output.Bytes(), &document); err != nil || document.SchemaVersion != agentdocs.SchemaVersion || document.ID != "commands" {
		t.Fatalf("document = %#v, %v", document, err)
	}
	output.Reset()
	err := runDocsCommand([]string{"jobss"}, &output)
	var exit docsExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(output.String(), "spynel docs jobs") {
		t.Fatalf("unknown topic = %T %v, %q", err, err, output.String())
	}
	for _, args := range [][]string{{"page", "0"}, {"commands", "page", "x"}, {"search"}, {"--wat"}} {
		if _, err := parseDocsArgs(args); err == nil {
			t.Errorf("parseDocsArgs(%v) accepted malformed input", args)
		}
	}
}

func TestDocsRetiredTopicsAreUnavailable(t *testing.T) {
	for _, topic := range []string{"tasks", "goals", "reviews", "persistent-instructions"} {
		var output bytes.Buffer
		err := runDocsCommand([]string{topic, "--format", "json"}, &output)
		var exit docsExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("retired topic %q = %T %v, %q", topic, err, err, output.String())
		}
		var document agentdocs.Document
		if err := json.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatalf("retired topic %q output = %q: %v", topic, output.String(), err)
		}
		if document.Error == nil || document.Error.Code != "unknown_topic" {
			t.Fatalf("retired topic %q document = %#v", topic, document)
		}
	}
}

func TestDocsBroadSearchDoesNotCreateTinyPages(t *testing.T) {
	var output bytes.Buffer
	if err := runDocsCommand([]string{"search", "e"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Page 1/1;") {
		t.Fatalf("broad text search was unexpectedly paginated:\n%s", output.String())
	}

	output.Reset()
	if err := runDocsCommand([]string{"search", "e", "--format", "json"}, &output); err != nil {
		t.Fatal(err)
	}
	var document agentdocs.Document
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Page.Total != 1 || document.Page.TotalEntries < 20 || len(document.Topics) != document.Page.TotalEntries {
		t.Fatalf("broad JSON search page = %#v, results = %d", document.Page, len(document.Topics))
	}
}
