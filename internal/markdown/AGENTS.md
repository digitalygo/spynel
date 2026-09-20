# Markdown Rendering DOX

## Purpose

- Own GitHub-flavored Markdown conversion for ANSI terminals, Telegram HTML, and WhatsApp-native formatting.

## Local Contracts

- Sanitize terminal control behavior while preserving supported formatting, compact code-block boundaries, and explicit absolute-file or URI hyperlinks.
- Do not invent bases for relative links or let one theme's syntax palette leak into another render.
- `TerminalLayout` retains logical Markdown text, source-indexed rows, and grapheme cells separately from ANSI display. Render without reflow, then wrap once; soft wraps never introduce logical newlines. Generated inline-code padding has zero-length source ranges. Preserve genuine code indentation, trailing whitespace, and leading/trailing blank source lines while excluding renderer margins from semantic text. Trim document margins before removing code-boundary markers, including adjacent code blocks. Ordinary noninteractive `TerminalWithTheme` rendering remains available to form/help callers.
- Keep transport escaping and length behavior deterministic; rendering must not mutate persisted Markdown source.
- `TelegramChunks` renders ordinary agent Markdown through the trusted `TelegramHTML` conversion exactly once, then splits it into independently valid Telegram HTML chunks within the parsed-visible limits: at most 4096 Unicode code points per chunk, 32768 per reply, and eight chunks per reply. Each chunk balances open formatting tags, including link attributes and fenced-code language classes, never splits an HTML entity or Unicode code point, and keeps grapheme boundaries whenever the remaining tail still fits the remaining chunks. Over-budget replies truncate deterministically with `TelegramTruncationMarker` inside the same budget; `TelegramChunkPlainText` derives exact visible plain text for a single transport fallback; empty or whitespace-only input that renders to nothing stays a no-send.

## Child DOX Index

No child DOX files.
