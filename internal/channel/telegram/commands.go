package telegram

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/digitalygo/spynel/internal/core"
)

// telegramBotCommand is one Bot API command-menu entry. Command carries the
// slash-free root and Description the visible one-line help text.
type telegramBotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// telegramBotCommandScope selects the chats a command menu applies to. The
// registration path only ever uses all_private_chats, so group menus stay
// untouched and no group scope is ever registered.
type telegramBotCommandScope struct {
	Type string `json:"type"`
}

// setMyCommandsPayload is the Bot API setMyCommands request body.
type setMyCommandsPayload struct {
	Commands []telegramBotCommand    `json:"commands"`
	Scope    telegramBotCommandScope `json:"scope"`
}

// telegramCommandRootPattern matches the Bot API command-name grammar: 1 to
// 32 lowercase ASCII letters, digits, or underscores.
var telegramCommandRootPattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// SetCommands converts the shared slash commands through the command-menu
// validator and stores the validated copy used for provider registration.
func (b *Bot) SetCommands(commands []core.SlashCommand) {
	b.commandMenu = buildTelegramCommandMenu(commands)
}

// buildTelegramCommandMenu projects shared slash commands onto validated Bot
// API command-menu entries. Each root is the first whitespace-separated token
// of the value with exactly one leading slash removed; a token without that
// leading slash yields an empty root and is dropped. Roots must match the
// command-name grammar and trimmed descriptions must be valid UTF-8 of 1 to
// 256 Unicode code points. Invalid entries are dropped, the first valid entry
// of each duplicate root wins in source order, and the menu stops at 100
// valid unique commands.
func buildTelegramCommandMenu(commands []core.SlashCommand) []telegramBotCommand {
	menu := make([]telegramBotCommand, 0, len(commands))
	claimed := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		if len(menu) >= 100 {
			break
		}
		tokens := strings.Fields(command.Value)
		if len(tokens) == 0 {
			continue
		}
		root, slashed := strings.CutPrefix(tokens[0], "/")
		if !slashed {
			continue
		}
		if !telegramCommandRootPattern.MatchString(root) {
			continue
		}
		description := strings.TrimSpace(command.Description)
		if !utf8.ValidString(description) {
			continue
		}
		if count := utf8.RuneCountInString(description); count < 1 || count > 256 {
			continue
		}
		if _, duplicate := claimed[root]; duplicate {
			continue
		}
		claimed[root] = struct{}{}
		menu = append(menu, telegramBotCommand{Command: root, Description: description})
	}
	return menu
}

// registerCommands publishes the stored menu through exactly one setMyCommands
// request scoped to all private_chats. An empty or unset menu registers
// nothing and reports success. The request goes through the ordinary provider
// wrapper, so the global runtime authorization check and the single bounded
// rate-limit retry apply unchanged. Authorization loss and caller cancellation
// surface to the caller as startup failures; any other provider failure is
// logged without content and stays non-fatal so a rejected menu can never
// block channel startup.
func (b *Bot) registerCommands(ctx context.Context) error {
	if len(b.commandMenu) == 0 {
		return nil
	}
	payload := setMyCommandsPayload{
		Commands: b.commandMenu,
		Scope:    telegramBotCommandScope{Type: "all_private_chats"},
	}
	err := b.post(ctx, "setMyCommands", payload)
	if err == nil {
		return nil
	}
	if errors.Is(err, errTelegramRuntimeAuthorization) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if b.log != nil {
		_, _ = fmt.Fprintln(b.log, "telegram: command menu registration failed")
	}
	return nil
}
