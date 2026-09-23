package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/digitalygo/spynel/internal/core"
)

func TestTelegramCommandsExactCatalog(t *testing.T) {
	want := []core.SlashCommand{
		{Value: "/status", Usage: "/status", Description: "Show work, runtime, channel, and orchestrator state"},
		{Value: "/tasks", Usage: "/tasks", Description: "List open tasks or select a semantic view"},
		{Value: "/goals", Usage: "/goals", Description: "List open goals or select a semantic view"},
		{Value: "/jobs", Usage: "/jobs", Description: "List running agent jobs"},
		{Value: "/job", Usage: "/job", Description: "Inspect or control an agent job by number"},
		{Value: "/log", Usage: "/log", Description: "Inspect, search, or clear captured runtime logs"},
		{Value: "/help", Usage: "/help", Description: "Show the help topic index"},
		{Value: "/config", Usage: "/config", Description: "Inspect or change Spynel configuration"},
		{Value: "/harness", Usage: "/harness", Description: "Show or select the coding harness"},
		{Value: "/model", Usage: "/model", Description: "Show or select the harness model"},
		{Value: "/effort", Usage: "/effort", Description: "Show, select, or reset reasoning effort"},
		{Value: "/speed", Usage: "/speed", Description: "Show, select, or reset a model service mode"},
		{Value: "/theme", Usage: "/theme", Description: "List or select the TUI color theme"},
		{Value: "/title", Usage: "/title", Description: "Rename and persist the TUI window title"},
		{Value: "/history", Usage: "/history", Description: "Show this chat's complete history file"},
		{Value: "/clear", Usage: "/clear", Description: "Clear this chat's history and harness thread"},
		{Value: "/stop", Usage: "/stop", Description: "Stop the active execution for this chat"},
		{Value: "/restart", Usage: "/restart", Description: "Restart Spynel and restore saved state"},
		{Value: "/update", Usage: "/update", Description: "Update and restart all instances of this installation"},
		{Value: "/trigger", Usage: "/trigger", Description: "List or start a triggerable background process"},
		{Value: "/cleanup", Usage: "/cleanup", Description: "Remove old conversations and job archives; archive old terminal tasks"},
		{Value: "/extension", Usage: "/extension", Description: "List or manage installed project extensions"},
		{Value: "/welcome", Usage: "/welcome", Description: "Show the Spynel welcome guide"},
		{Value: "/whatsapp", Usage: "/whatsapp", Description: "Inspect or change WhatsApp configuration"},
		{Value: "/pi", Usage: "/pi", Description: "Inspect or manage this private chat's Pi session"},
	}
	got := TelegramCommands()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TelegramCommands() = %#v, want %#v", got, want)
	}
}

func TestTelegramCommandsBareRoots(t *testing.T) {
	slashRoots := map[string]bool{}
	for _, command := range SlashCommands() {
		fields := strings.Fields(command.Value)
		if len(fields) == 0 {
			continue
		}
		slashRoots[fields[0]] = true
	}

	seen := map[string]bool{}
	for _, command := range TelegramCommands() {
		if !strings.HasPrefix(command.Value, "/") {
			t.Errorf("command %q does not start with a slash", command.Value)
		}
		if strings.ContainsAny(command.Value, " \t") {
			t.Errorf("command %q is not a bare root without arguments", command.Value)
		}
		if len(command.Value) < 2 {
			t.Errorf("command %q has an empty root", command.Value)
		}
		if command.Value != command.Usage {
			t.Errorf("command %q usage = %q, want the bare root", command.Value, command.Usage)
		}
		if !slashRoots[command.Value] {
			t.Errorf("command %q does not match any SlashCommands() root", command.Value)
		}
		if seen[command.Value] {
			t.Errorf("duplicate root %q", command.Value)
		}
		seen[command.Value] = true
	}
}

func TestTelegramCommandsExclusionsAndPresence(t *testing.T) {
	values := map[string]bool{}
	for _, command := range TelegramCommands() {
		values[command.Value] = true
	}
	for _, excluded := range []string{"/task", "/goal", "/telegram", "/quit", "/primary", "/new", "/resume"} {
		if values[excluded] {
			t.Errorf("command %q must not appear in the Telegram menu", excluded)
		}
	}
	for _, included := range []string{"/theme", "/title", "/pi"} {
		if !values[included] {
			t.Errorf("command %q must appear in the Telegram menu", included)
		}
	}
}

func TestTelegramCommandsReturnsDefensiveCopy(t *testing.T) {
	first := TelegramCommands()
	if len(first) == 0 {
		t.Fatal("TelegramCommands() returned an empty catalog")
	}
	first[0] = core.SlashCommand{Value: "/replaced", Usage: "/replaced", Description: "replaced"}
	first = append(first, core.SlashCommand{Value: "/extra", Usage: "/extra", Description: "extra"})

	second := TelegramCommands()
	if len(second) != len(telegramCommands) {
		t.Fatalf("TelegramCommands() length = %d after mutation, want %d", len(second), len(telegramCommands))
	}
	if second[0].Value != "/status" || second[0].Description != "Show work, runtime, channel, and orchestrator state" {
		t.Fatalf("first entry = %#v after mutation, want the canonical /status entry", second[0])
	}
	for _, command := range second {
		if command.Value == "/extra" || command.Value == "/replaced" {
			t.Fatalf("mutated command %q leaked into a later catalog call", command.Value)
		}
	}
}
