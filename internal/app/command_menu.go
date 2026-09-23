package app

import "github.com/digitalygo/spynel/internal/core"

// telegramCommands is the ordered command menu registered by the Telegram
// adapter. Every entry is a bare root handled by framework code in
// handleCommand and available in canonical private Telegram conversations.
// Entries Telegram itself refuses, TUI-only surfaces, values that dispatch a
// harness prompt, and commands used only during bot setup are excluded.
var telegramCommands = []core.SlashCommand{
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

// TelegramCommands returns the Telegram command-menu catalog in registration
// order. The returned slice is safe for the caller to modify.
func TelegramCommands() []core.SlashCommand {
	return append([]core.SlashCommand(nil), telegramCommands...)
}
