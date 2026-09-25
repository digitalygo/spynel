package agentdocs_test

import (
	"strings"
	"testing"

	"github.com/digitalygo/spynel/internal/agentdocs"
	"github.com/digitalygo/spynel/internal/app"
)

// TestDocumentedCommandsMatchCanonicalSlashCatalog keeps the static command
// topic and the canonical application slash catalog in lockstep in both
// directions: no retired or unknown command is documented, and no current
// catalog command is missing from the documented list.
func TestDocumentedCommandsMatchCanonicalSlashCatalog(t *testing.T) {
	catalog := map[string]bool{}
	for _, command := range app.SlashCommands() {
		fields := strings.Fields(command.Value)
		if len(fields) == 0 {
			continue
		}
		catalog[fields[0]] = true
	}
	documented := map[string]bool{}
	for _, command := range agentdocs.DocumentedSlashCommands() {
		documented[command] = true
		if !catalog[command] {
			t.Errorf("documented slash command %q is not in the canonical catalog", command)
		}
	}
	for command := range catalog {
		if !documented[command] {
			t.Errorf("canonical slash command %q is missing from the documented command list", command)
		}
	}
}
