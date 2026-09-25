// Spynel loads this extension additively through one --extension argument
// for chat:telegram: Pi RPC processes only. Pi keeps discovering the user's
// global and trusted-project APPEND_SYSTEM.md and loads ordinary global
// extensions, skills, prompt templates, themes, context files, and model
// settings unchanged. On every before_agent_start it adds exactly one
// namespaced system prompt section so the agent treats each Telegram reply
// as final and asks any clarification or approval question as one final
// question ending the turn. It never returns a system prompt, forces the
// full prompt, prepends user messages, registers commands or tools, or
// touches project trust or resources. This is guidance, not an enforcement
// boundary: a user extension returning a forced prompt or starting its own
// runs may override or bypass it.
export default function spynelTelegramNote(pi) {
  pi.on("before_agent_start", (event) => {
    event.systemPromptOptions.sections.spynel_telegram =
      "The user chats over Telegram and sees only your final replies, never intermediate progress. If you need clarification, approval, or support, ask in one final reply and end your turn; the user's next message continues this same conversation session.";
  });
}
