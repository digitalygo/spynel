package markdown

import (
	"html"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// Telegram rich-text chunking limits.
//
// Telegram enforces 4096 characters per message. The complete reply budget
// used here is 32768 parsed-visible characters (8 messages). Visible means
// decoded Unicode code points in the rendered reply: HTML tags contribute
// nothing and entity spellings count as their single decoded code point.
// Newlines count as one visible code point each.
const (
	// TelegramMaxVisiblePerMessage is the maximum decoded Unicode code points
	// per Telegram message chunk.
	TelegramMaxVisiblePerMessage = 4096
	// TelegramMaxVisiblePerReply is the maximum decoded Unicode code points
	// for a complete reply, including any truncation marker.
	TelegramMaxVisiblePerReply = 32768
	// TelegramMaxChunks bounds the number of message chunks per reply.
	TelegramMaxChunks = 8
)

// TelegramTruncationMarker is the visible plain-text suffix appended when a
// rendered reply exceeds TelegramMaxVisiblePerReply. It fits inside the
// total reply budget. The full response remains durable elsewhere.
const TelegramTruncationMarker = "\n\n… (response truncated; full response available elsewhere)"

// TelegramChunks renders ordinary agent Markdown through the existing
// trusted TelegramHTML renderer exactly once, then splits the
// renderer-owned HTML into independently valid Telegram HTML chunks.
//
// Each chunk closes open formatting tags at its boundary and reopens them
// on the next chunk, preserving attributes on links and fenced-code
// language tags. Chunking never splits an HTML entity or a Unicode code
// point and preserves grapheme-cluster boundaries whenever the remaining
// tail still fits in the remaining allowed chunks. The hard
// TelegramMaxVisiblePerMessage limit takes precedence: when backtracking
// to a grapheme boundary would leave a tail exceeding remaining capacity,
// the cut stays at the exact rune boundary even inside a grapheme.
//
//   - Empty or whitespace-only input that renders to nothing returns nil,
//     preserving the current no-send behavior.
//   - Content at or below TelegramMaxVisiblePerReply keeps every visible
//     character: concatenated TelegramChunkPlainText output equals the
//     visible full reply, each chunk holds at most
//     TelegramMaxVisiblePerMessage visible code points, and a short reply
//     (at most TelegramMaxVisiblePerMessage visible) returns the exact
//     existing TelegramHTML output byte-for-byte.
//   - Content above TelegramMaxVisiblePerReply is truncated
//     deterministically with valid formatting preserved and
//     TelegramTruncationMarker appended inside the same total budget.
//     The result holds at most TelegramMaxChunks chunks.
//
// Only the fixed HTML subset emitted by TelegramHTML is accepted
// (b, i, s, code, pre, blockquote, a with href, code with language class).
// Impossible malformed renderer output is safely escaped, never passed
// through as arbitrary HTML.
func TelegramChunks(input string) []string {
	return chunkTelegramHTML(TelegramHTML(input))
}

// TelegramPlainChunks chunks literal plain text for Telegram without
// interpreting Markdown. The text is HTML-escaped so provider HTML parsing
// cannot treat content as markup, and the result is chunked and bounded by
// the exact same reply budget as TelegramChunks. Visible content is the
// literal input text; concatenated TelegramChunkPlainText output recovers it
// exactly up to the ordinary truncation boundary.
func TelegramPlainChunks(input string) []string {
	return chunkTelegramHTML(html.EscapeString(input))
}

// chunkTelegramHTML splits renderer-owned HTML (or HTML-escaped plain text)
// into independently valid Telegram HTML chunks.
func chunkTelegramHTML(rendered string) []string {
	if rendered == "" {
		return nil
	}
	// Bound parser allocations to the reply budget plus one sentinel rune.
	// The sentinel alone proves truncation is required without scanning or
	// allocating for the ignored tail. Short and in-budget replies parse in
	// full, preserving exact behavior; over-budget replies stop with possibly
	// open tags that truncation already closes.
	pieces, visible, normalized := parseTelegramHTMLLimited(rendered, TelegramMaxVisiblePerReply+1)
	if len(visible) == 0 {
		return nil
	}
	if len(visible) <= TelegramMaxVisiblePerMessage {
		if !normalized && isTelegramBalanced(pieces) {
			return []string{rendered}
		}
		return assembleTelegramChunks(pieces, nil)
	}
	if len(visible) <= TelegramMaxVisiblePerReply {
		bounds := telegramGraphemeBounds(visible)
		cuts := telegramCuts(len(visible), bounds)
		return assembleTelegramChunks(pieces, cuts)
	}
	return truncateTelegramPieces(pieces, visible)
}

// TelegramChunkPlainText converts one produced HTML chunk to its exact
// visible plain text for transport fallback if Telegram rejects HTML
// parsing. It strips only renderer-owned tags, decodes entities, and never
// executes or interprets arbitrary content. Unknown tags and stray markup
// are preserved literally so nothing is silently accepted as formatting.
func TelegramChunkPlainText(chunk string) string {
	var out strings.Builder
	out.Grow(len(chunk))
	i := 0
	for i < len(chunk) {
		switch chunk[i] {
		case '<':
			end := strings.IndexByte(chunk[i:], '>')
			if end < 0 {
				out.WriteByte('<')
				i++
				continue
			}
			end += i
			if _, _, ok := allowedTelegramTag(chunk[i+1 : end]); ok {
				i = end + 1
				continue
			}
			out.WriteByte('<')
			i++
		case '&':
			if raw, r, ok := parseTelegramEntity(chunk, i); ok {
				out.WriteRune(r)
				i += len(raw)
			} else {
				out.WriteByte('&')
				i++
			}
		case '>':
			out.WriteByte('>')
			i++
		default:
			r, size := utf8.DecodeRuneInString(chunk[i:])
			if r == utf8.RuneError && size <= 1 {
				out.WriteRune('\uFFFD')
				i++
				continue
			}
			out.WriteString(chunk[i : i+size])
			i += size
		}
	}
	return out.String()
}

type telegramPiece struct {
	isTag bool
	tag   string
	isEnd bool
	raw   string
	vis   rune
}

type telegramOpen struct {
	name string
	raw  string
}

// parseTelegramHTMLLimited parses renderer-owned HTML exactly like
// parseTelegramHTML but stops immediately after collecting limit decoded
// visible runes. Tags encountered before the limit are retained; the stop
// may leave renderer-owned tags open because truncation already closes
// them. Bytes beyond the stop point are never scanned and never allocate
// pieces or visible entries, bounding growth to the reply budget plus one
// sentinel. A non-positive limit parses without bound. When the input holds
// at most limit visible runes the result is identical to parseTelegramHTML.
func parseTelegramHTMLLimited(s string, limit int) (pieces []telegramPiece, visible []rune, normalized bool) {
	if limit <= 0 {
		return parseTelegramHTML(s)
	}
	pieces = make([]telegramPiece, 0, min(len(s), limit+16))
	visible = make([]rune, 0, limit)
	i := 0
	for i < len(s) {
		switch s[i] {
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				pieces = append(pieces, telegramPiece{raw: "&lt;", vis: '<'})
				visible = append(visible, '<')
				normalized = true
				i++
				if len(visible) >= limit {
					return pieces, visible, normalized
				}
				continue
			}
			end += i
			inner := s[i+1 : end]
			if name, isEnd, ok := allowedTelegramTag(inner); ok {
				pieces = append(pieces, telegramPiece{isTag: true, tag: name, isEnd: isEnd, raw: s[i : end+1]})
				i = end + 1
				continue
			}
			pieces = append(pieces, telegramPiece{raw: "&lt;", vis: '<'})
			visible = append(visible, '<')
			normalized = true
			i++
			if len(visible) >= limit {
				return pieces, visible, normalized
			}
		case '&':
			if raw, r, ok := parseTelegramEntity(s, i); ok {
				pieces = append(pieces, telegramPiece{raw: raw, vis: r})
				visible = append(visible, r)
				i += len(raw)
				if len(visible) >= limit {
					return pieces, visible, normalized
				}
			} else {
				pieces = append(pieces, telegramPiece{raw: "&amp;", vis: '&'})
				visible = append(visible, '&')
				normalized = true
				i++
				if len(visible) >= limit {
					return pieces, visible, normalized
				}
			}
		case '>':
			pieces = append(pieces, telegramPiece{raw: "&gt;", vis: '>'})
			visible = append(visible, '>')
			normalized = true
			i++
			if len(visible) >= limit {
				return pieces, visible, normalized
			}
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size <= 1 {
				pieces = append(pieces, telegramPiece{raw: "�", vis: '�'})
				visible = append(visible, '�')
				normalized = true
				i++
				if len(visible) >= limit {
					return pieces, visible, normalized
				}
				continue
			}
			if r == '\x00' || r == '\x02' || r == '\x03' || r == '\x04' || r == '\x05' {
				normalized = true
				i += size
				continue
			}
			pieces = append(pieces, telegramPiece{raw: s[i : i+size], vis: r})
			visible = append(visible, r)
			i += size
			if len(visible) >= limit {
				return pieces, visible, normalized
			}
		}
	}
	return pieces, visible, normalized
}

func parseTelegramHTML(s string) (pieces []telegramPiece, visible []rune, normalized bool) {
	i := 0
	for i < len(s) {
		switch s[i] {
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				pieces = append(pieces, telegramPiece{raw: "&lt;", vis: '<'})
				visible = append(visible, '<')
				normalized = true
				i++
				continue
			}
			end += i
			inner := s[i+1 : end]
			if name, isEnd, ok := allowedTelegramTag(inner); ok {
				pieces = append(pieces, telegramPiece{isTag: true, tag: name, isEnd: isEnd, raw: s[i : end+1]})
				i = end + 1
				continue
			}
			pieces = append(pieces, telegramPiece{raw: "&lt;", vis: '<'})
			visible = append(visible, '<')
			normalized = true
			i++
		case '&':
			if raw, r, ok := parseTelegramEntity(s, i); ok {
				pieces = append(pieces, telegramPiece{raw: raw, vis: r})
				visible = append(visible, r)
				i += len(raw)
			} else {
				pieces = append(pieces, telegramPiece{raw: "&amp;", vis: '&'})
				visible = append(visible, '&')
				normalized = true
				i++
			}
		case '>':
			pieces = append(pieces, telegramPiece{raw: "&gt;", vis: '>'})
			visible = append(visible, '>')
			normalized = true
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size <= 1 {
				pieces = append(pieces, telegramPiece{raw: "�", vis: '�'})
				visible = append(visible, '�')
				normalized = true
				i++
				continue
			}
			if r == '\x00' || r == '\x02' || r == '\x03' || r == '\x04' || r == '\x05' {
				normalized = true
				i += size
				continue
			}
			pieces = append(pieces, telegramPiece{raw: s[i : i+size], vis: r})
			visible = append(visible, r)
			i += size
		}
	}
	return pieces, visible, normalized
}

func allowedTelegramTag(inner string) (string, bool, bool) {
	if inner == "" {
		return "", false, false
	}
	if inner[0] == '/' {
		switch inner[1:] {
		case "b", "i", "s", "code", "pre", "blockquote", "a":
			return inner[1:], true, true
		default:
			return "", false, false
		}
	}
	switch inner {
	case "b", "i", "s", "code", "pre", "blockquote":
		return inner, false, true
	}
	const codePrefix = `code class="language-`
	if strings.HasPrefix(inner, codePrefix) && strings.HasSuffix(inner, `"`) && len(inner) > len(codePrefix)+1 {
		lang := inner[len(codePrefix) : len(inner)-1]
		if lang != "" && !strings.ContainsAny(lang, "<>\"") && !strings.Contains(lang, "\n") {
			return "code", false, true
		}
		return "", false, false
	}
	const aPrefix = `a href="`
	if strings.HasPrefix(inner, aPrefix) && strings.HasSuffix(inner, `"`) {
		value := inner[len(aPrefix) : len(inner)-1]
		if !strings.ContainsAny(value, "<>\"") && !strings.Contains(value, "\n") && !strings.Contains(value, "\x00") {
			return "a", false, true
		}
		return "", false, false
	}
	return "", false, false
}

func parseTelegramEntity(s string, i int) (string, rune, bool) {
	const maxEntityLen = 32
	limit := min(len(s), i+maxEntityLen)
	semi := -1
	for k := i + 1; k < limit; k++ {
		if s[k] == ';' {
			semi = k
			break
		}
		if s[k] == '&' || s[k] == '<' || s[k] == '>' || s[k] == '\n' {
			return "", 0, false
		}
	}
	if semi < i+2 {
		return "", 0, false
	}
	candidate := s[i : semi+1]
	decoded := html.UnescapeString(candidate)
	if decoded == candidate {
		return "", 0, false
	}
	runes := []rune(decoded)
	if len(runes) != 1 {
		return "", 0, false
	}
	return candidate, runes[0], true
}

func isTelegramBalanced(pieces []telegramPiece) bool {
	var stack []string
	for _, p := range pieces {
		if !p.isTag {
			continue
		}
		if !p.isEnd {
			stack = append(stack, p.tag)
			continue
		}
		if len(stack) == 0 || stack[len(stack)-1] != p.tag {
			return false
		}
		stack = stack[:len(stack)-1]
	}
	return len(stack) == 0
}

func telegramGraphemeBounds(visible []rune) []bool {
	n := len(visible)
	bounds := make([]bool, n+1)
	bounds[0] = true
	bounds[n] = true
	if n == 0 {
		return bounds
	}
	g := uniseg.NewGraphemes(string(visible))
	pos := 0
	for g.Next() {
		pos += len(g.Runes())
		if pos >= 0 && pos <= n {
			bounds[pos] = true
		}
	}
	return bounds
}

// telegramCuts plans chunk boundaries over n visible runes. Every chunk is
// at most TelegramMaxVisiblePerMessage runes and the result holds at most
// TelegramMaxChunks-1 cuts, so any n <= TelegramMaxVisiblePerReply fits in
// TelegramMaxChunks chunks. Grapheme boundaries are preserved whenever the
// remaining tail still fits in the remaining allowed chunks; otherwise the
// cut stays at the exact hard rune boundary even inside a grapheme because
// hard limits take precedence when both cannot be satisfied.
func telegramCuts(n int, bounds []bool) []int {
	var cuts []int
	prev := 0
	for prev+TelegramMaxVisiblePerMessage < n {
		target := prev + TelegramMaxVisiblePerMessage
		if target < len(bounds) && bounds[target] {
			cuts = append(cuts, target)
			prev = target
		} else {
			back := target - 1
			for back > prev && (back >= len(bounds) || !bounds[back]) {
				back--
			}
			if back <= prev {
				cuts = append(cuts, target)
				prev = target
			} else if remaining := TelegramMaxChunks - (len(cuts) + 1); remaining < 0 || n-back <= remaining*TelegramMaxVisiblePerMessage {
				cuts = append(cuts, back)
				prev = back
			} else {
				cuts = append(cuts, target)
				prev = target
			}
		}
		if len(cuts) >= TelegramMaxChunks-1 {
			break
		}
	}
	return cuts
}

func assembleTelegramChunks(pieces []telegramPiece, cuts []int) []string {
	var chunks []string
	var cur strings.Builder
	var stack []telegramOpen
	cutIdx := 0
	global := 0
	flushIntermediate := func() {
		for i := len(stack) - 1; i >= 0; i-- {
			cur.WriteString("</" + stack[i].name + ">")
		}
		chunks = append(chunks, cur.String())
		cur.Reset()
		for _, o := range stack {
			cur.WriteString(o.raw)
		}
	}
	for _, p := range pieces {
		if p.isTag {
			if !p.isEnd {
				stack = append(stack, telegramOpen{name: p.tag, raw: p.raw})
				cur.WriteString(p.raw)
			} else {
				if len(stack) > 0 && stack[len(stack)-1].name == p.tag {
					stack = stack[:len(stack)-1]
					cur.WriteString(p.raw)
				}
			}
			continue
		}
		cur.WriteString(p.raw)
		global++
		if cutIdx < len(cuts) && global == cuts[cutIdx] {
			flushIntermediate()
			cutIdx++
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		cur.WriteString("</" + stack[i].name + ">")
	}
	chunks = append(chunks, cur.String())
	return chunks
}

func truncateTelegramPieces(pieces []telegramPiece, visible []rune) []string {
	markerRunes := []rune(TelegramTruncationMarker)
	contentBudget := TelegramMaxVisiblePerReply - len(markerRunes)
	if contentBudget < 0 {
		contentBudget = 0
	}
	bounds := telegramGraphemeBounds(visible)
	truncPos := contentBudget
	if truncPos > len(visible) {
		truncPos = len(visible)
	}
	if truncPos < len(bounds) && !bounds[truncPos] {
		back := truncPos
		for back > 0 && !bounds[back] {
			back--
		}
		if back > 0 {
			truncPos = back
		}
	}
	var prefix []telegramPiece
	count := 0
	for _, p := range pieces {
		if count >= truncPos {
			break
		}
		if p.isTag {
			prefix = append(prefix, p)
			continue
		}
		prefix = append(prefix, p)
		count++
	}
	var stack []string
	for _, p := range prefix {
		if !p.isTag {
			continue
		}
		if !p.isEnd {
			stack = append(stack, p.tag)
			continue
		}
		if len(stack) > 0 && stack[len(stack)-1] == p.tag {
			stack = stack[:len(stack)-1]
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		prefix = append(prefix, telegramPiece{isTag: true, tag: stack[i], isEnd: true, raw: "</" + stack[i] + ">"})
	}
	for _, r := range markerRunes {
		prefix = append(prefix, telegramPiece{raw: html.EscapeString(string(r)), vis: r})
	}
	truncatedVisible := make([]rune, 0, truncPos+len(markerRunes))
	truncatedVisible = append(truncatedVisible, visible[:truncPos]...)
	truncatedVisible = append(truncatedVisible, markerRunes...)
	boundsTrunc := telegramGraphemeBounds(truncatedVisible)
	cuts := telegramCuts(len(truncatedVisible), boundsTrunc)
	return assembleTelegramChunks(prefix, cuts)
}
