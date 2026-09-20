package markdown

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func telegramVisible(chunk string) int {
	return len([]rune(TelegramChunkPlainText(chunk)))
}

func telegramConcatPlain(chunks []string) string {
	var out strings.Builder
	for _, c := range chunks {
		out.WriteString(TelegramChunkPlainText(c))
	}
	return out.String()
}

func assertTelegramBalanced(t *testing.T, chunk string) {
	t.Helper()
	pieces, _, normalized := parseTelegramHTML(chunk)
	if normalized {
		t.Fatalf("chunk required escaping, not independently valid: %q", chunk)
	}
	if !isTelegramBalanced(pieces) {
		t.Fatalf("chunk is not balanced: %q", chunk)
	}
}

func assertTelegramChunkLimits(t *testing.T, chunks []string) {
	t.Helper()
	if len(chunks) > TelegramMaxChunks {
		t.Fatalf("chunk count = %d, want at most %d", len(chunks), TelegramMaxChunks)
	}
	for i, c := range chunks {
		if got := telegramVisible(c); got > TelegramMaxVisiblePerMessage {
			t.Fatalf("chunk %d visible = %d, want at most %d", i, got, TelegramMaxVisiblePerMessage)
		}
		assertTelegramBalanced(t, c)
	}
}

func TestTelegramChunksShortPreservesExactOutput(t *testing.T) {
	inputs := []string{
		"hello",
		"# Release\n\nUse **bold**, *italic*, ~~removed~~, and `code`.",
		"Link [docs](https://example.com) and auto https://example.com",
		"Entity & < > \" ' test",
		"界 CJK test",
		"e\u0301 combining test",
		"astral \U0001F600 test",
		sample,
	}
	for _, input := range inputs {
		want := TelegramHTML(input)
		if want == "" {
			continue
		}
		got := TelegramChunks(input)
		if len(got) != 1 {
			t.Fatalf("short input %q chunk count = %d, want 1", input, len(got))
		}
		if got[0] != want {
			t.Fatalf("short output changed:\n got %q\nwant %q", got[0], want)
		}
	}
}

func TestTelegramChunksEmptyAndWhitespaceNoSend(t *testing.T) {
	for _, input := range []string{"", "   ", "\n", "  \n  \n ", "\t  \n"} {
		if got := TelegramChunks(input); len(got) != 0 {
			t.Fatalf("input %q chunks = %#v, want no-send (nil/empty)", input, got)
		}
		if TelegramHTML(input) != "" {
			t.Fatalf("whitespace input %q rendered to %q, want empty", input, TelegramHTML(input))
		}
	}
}

func TestTelegramChunksRichFormattingAcrossBoundaries(t *testing.T) {
	var md strings.Builder
	md.WriteString("# Long report\n\n")
	for i := 0; i < 300; i++ {
		md.WriteString("Use **bold text** with *italic text* and ~~struck text~~ plus `inline code` and [link text](https://example.com/long/path) here.\n\n")
	}
	chunks := TelegramChunks(md.String())
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want multiple", len(chunks))
	}
	assertTelegramChunkLimits(t, chunks)
	full := TelegramHTML(md.String())
	if got, want := telegramConcatPlain(chunks), TelegramChunkPlainText(full); got != want {
		t.Fatalf("concatenated plain mismatch:\n got %.100q\nwant %.100q", got, want)
	}
	joined := strings.Join(chunks, "\n")
	for _, tag := range []string{"<b>", "</b>", "<i>", "</i>", "<s>", "</s>", "<code>", "</code>", `<a href="https://example.com/long/path">`, "</a>"} {
		if !strings.Contains(joined, tag) {
			t.Fatalf("chunked output missing %q", tag)
		}
	}
}

func TestTelegramChunksLongFencedCodeSpansBoundaries(t *testing.T) {
	var md strings.Builder
	md.WriteString("before\n\n```go\n")
	for i := 0; i < 400; i++ {
		md.WriteString("fmt.Println(\"line with <angle> & ampersand and 界 emoji \U0001F600\")\n")
	}
	md.WriteString("```\n\nafter\n")
	chunks := TelegramChunks(md.String())
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want multiple", len(chunks))
	}
	assertTelegramChunkLimits(t, chunks)
	full := TelegramHTML(md.String())
	if got, want := telegramConcatPlain(chunks), TelegramChunkPlainText(full); got != want {
		t.Fatalf("code concatenated plain mismatch")
	}
	foundPre := false
	for _, c := range chunks {
		if strings.Contains(c, "<pre>") && strings.Contains(c, "</pre>") {
			foundPre = true
		}
		if strings.Contains(c, "<pre>") != strings.Contains(c, "</pre>") {
			t.Fatalf("code chunk has unbalanced pre: %.120q", c)
		}
		if strings.Contains(c, "<code") != strings.Contains(c, "</code>") {
			t.Fatalf("code chunk has unbalanced code: %.120q", c)
		}
	}
	if !foundPre {
		t.Fatalf("no chunk carries fenced code block")
	}
	if !strings.Contains(strings.Join(chunks, ""), `class="language-go"`) {
		t.Fatalf("code language attribute was not preserved across chunks")
	}
}

func TestTelegramChunksEntityCJKCombiningEmoji(t *testing.T) {
	pattern := "amp & lt < gt > quot \" 界 CJK e\u0301 combining \U0001F600 astral \U0001F469\u200D\U0001F4BB zwj "
	var md strings.Builder
	for len([]rune(TelegramChunkPlainText(TelegramHTML(md.String())))) <= TelegramMaxVisiblePerMessage+500 {
		md.WriteString(pattern + "\n")
	}
	input := md.String()
	chunks := TelegramChunks(input)
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want multiple", len(chunks))
	}
	assertTelegramChunkLimits(t, chunks)
	full := TelegramHTML(input)
	if got, want := telegramConcatPlain(chunks), TelegramChunkPlainText(full); got != want {
		t.Fatalf("entity/CJK concatenated plain mismatch")
	}
	for i, c := range chunks {
		if strings.HasSuffix(c, "&") || strings.HasSuffix(c, "&amp") || strings.HasSuffix(c, "&lt") || strings.HasSuffix(c, "&gt") {
			t.Fatalf("chunk %d ends inside an entity: %.40q", i, c[len(c)-20:])
		}
	}
	fullVisible := []rune(TelegramChunkPlainText(full))
	bounds := map[int]bool{0: true}
	g := uniseg.NewGraphemes(string(fullVisible))
	pos := 0
	for g.Next() {
		pos += len(g.Runes())
		bounds[pos] = true
	}
	offset := 0
	for i, c := range chunks {
		offset += telegramVisible(c)
		if !bounds[offset] && i < len(chunks)-1 {
			t.Fatalf("chunk %d boundary splits a grapheme cluster at visible %d", i, offset)
		}
	}
	if got := len([]rune("界")); got != 1 {
		t.Fatalf("test sanity: CJK rune count = %d", got)
	}
	if got := len([]rune("\U0001F600")); got != 1 {
		t.Fatalf("astral emoji must count as one code point, got %d", got)
	}
}

func TestTelegramChunksConcatenatedEqualityUnderLimit(t *testing.T) {
	inputs := []string{
		strings.Repeat("plain text line with 界 and \U0001F600\n", 200),
		strings.Repeat("**bold** *italic* `code` [x](https://example.com)\n", 300),
		strings.Repeat("# heading\n\n> quote with **bold**\n\n- item one\n- item two\n\n", 100),
	}
	for _, input := range inputs {
		full := TelegramHTML(input)
		fullVisible := telegramVisible(full)
		if fullVisible > TelegramMaxVisiblePerReply {
			t.Fatalf("fixture exceeds reply budget: %d", fullVisible)
		}
		chunks := TelegramChunks(input)
		assertTelegramChunkLimits(t, chunks)
		if got := telegramConcatPlain(chunks); got != TelegramChunkPlainText(full) {
			t.Fatalf("concatenated plain mismatch for fixture")
		}
	}
}

func TestTelegramChunksTruncationOverReplyBudget(t *testing.T) {
	big := strings.Repeat("content line with **bold** and 界 \U0001F600 text to fill the reply budget deterministically\n", 800)
	full := TelegramHTML(big)
	if telegramVisible(full) <= TelegramMaxVisiblePerReply {
		t.Fatalf("fixture visible = %d, want above %d", telegramVisible(full), TelegramMaxVisiblePerReply)
	}
	first := TelegramChunks(big)
	second := TelegramChunks(big)
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("truncated chunks are empty")
	}
	if strings.Join(first, "\x00") != strings.Join(second, "\x00") {
		t.Fatal("truncation is not deterministic")
	}
	assertTelegramChunkLimits(t, first)
	if len(first) > TelegramMaxChunks {
		t.Fatalf("truncated chunk count = %d", len(first))
	}
	total := 0
	for _, c := range first {
		total += telegramVisible(c)
	}
	if total != TelegramMaxVisiblePerReply {
		t.Fatalf("truncated total visible = %d, want exactly %d", total, TelegramMaxVisiblePerReply)
	}
	plain := telegramConcatPlain(first)
	if !strings.HasSuffix(plain, TelegramTruncationMarker) {
		t.Fatalf("truncated plain missing marker suffix: %.120q", plain[len(plain)-120:])
	}
	if strings.Contains(plain, TelegramTruncationMarker[:len(TelegramTruncationMarker)-5]) && !strings.HasSuffix(plain, TelegramTruncationMarker) {
		t.Fatalf("marker appears in the middle, want single suffix")
	}
}

func TestTelegramChunksFullBudgetGraphemeDense(t *testing.T) {
	womanTechnologist := "\U0001F469\u200D\U0001F4BB"
	base := strings.Repeat(womanTechnologist, 10923)
	runes := []rune(base)
	if len(runes) < TelegramMaxVisiblePerReply {
		t.Fatalf("test sanity: repeated ZWJ runes = %d", len(runes))
	}
	input := string(runes[:TelegramMaxVisiblePerReply])
	full := TelegramHTML(input)
	fullPlain := TelegramChunkPlainText(full)
	if len([]rune(fullPlain)) != TelegramMaxVisiblePerReply {
		t.Fatalf("fixture visible = %d, want exactly %d", len([]rune(fullPlain)), TelegramMaxVisiblePerReply)
	}
	chunks := TelegramChunks(input)
	assertTelegramChunkLimits(t, chunks)
	if len(chunks) > TelegramMaxChunks {
		t.Fatalf("chunk count = %d, want at most %d", len(chunks), TelegramMaxChunks)
	}
	for i, c := range chunks {
		if got := telegramVisible(c); got > TelegramMaxVisiblePerMessage {
			t.Fatalf("chunk %d visible = %d, want at most %d", i, got, TelegramMaxVisiblePerMessage)
		}
	}
	if got := telegramConcatPlain(chunks); got != fullPlain {
		t.Fatalf("concatenated plain mismatch for full-budget grapheme-dense reply")
	}
}

func TestTelegramChunksHardLimitBoundaries(t *testing.T) {
	womanTechnologist := "\U0001F469\u200D\U0001F4BB"
	denseRunes := []rune(strings.Repeat(womanTechnologist, 10923))
	cases := []int{4095, 4096, 4097, 8191, 8192, 8193, 32766, 32767, 32768}
	for _, n := range cases {
		inputs := map[string]string{
			"plain": strings.Repeat("a", n),
			"dense": string(denseRunes[:n]),
		}
		for name, input := range inputs {
			full := TelegramHTML(input)
			fullPlain := TelegramChunkPlainText(full)
			if len([]rune(fullPlain)) != n {
				t.Fatalf("%s n=%d fixture visible = %d", name, n, len([]rune(fullPlain)))
			}
			chunks := TelegramChunks(input)
			if len(chunks) > TelegramMaxChunks {
				t.Fatalf("%s n=%d chunk count = %d, want at most %d", name, n, len(chunks), TelegramMaxChunks)
			}
			for i, c := range chunks {
				if got := telegramVisible(c); got > TelegramMaxVisiblePerMessage {
					t.Fatalf("%s n=%d chunk %d visible = %d, want at most %d", name, n, i, got, TelegramMaxVisiblePerMessage)
				}
			}
			if got := telegramConcatPlain(chunks); got != fullPlain {
				t.Fatalf("%s n=%d concatenated plain mismatch", name, n)
			}
		}
	}
	// Small dense replies still preserve grapheme boundaries when capacity
	// allows: 4097 visible runes split as 4095 + 2, not a hard 4096 cut.
	smallDense := string(denseRunes[:4097])
	smallChunks := TelegramChunks(smallDense)
	if len(smallChunks) != 2 {
		t.Fatalf("dense 4097 chunk count = %d, want 2", len(smallChunks))
	}
	if got := telegramVisible(smallChunks[0]); got != 4095 {
		t.Fatalf("dense 4097 first chunk visible = %d, want 4095 (grapheme preserved)", got)
	}
}

func TestTelegramChunkPlainTextExactness(t *testing.T) {
	tests := []struct{ html, want string }{
		{html: "<b>bold</b>", want: "bold"},
		{html: "<b>hi &amp; &lt;界&gt;</b>", want: "hi & <界>"},
		{html: `<a href="https://example.com">docs</a>`, want: "docs"},
		{html: "<code>a &amp; b</code>", want: "a & b"},
		{html: "<blockquote>quoted</blockquote>", want: "quoted"},
		{html: `<pre><code class="language-go">fmt.Println(&quot;ok&quot;)</code></pre>`, want: `fmt.Println("ok")`},
		{html: "a\n\nb", want: "a\n\nb"},
	}
	for _, test := range tests {
		if got := TelegramChunkPlainText(test.html); got != test.want {
			t.Fatalf("plain(%q) = %q, want %q", test.html, got, test.want)
		}
	}
}

func TestTelegramChunkPlainTextMalformedFailSafe(t *testing.T) {
	unknown := TelegramChunkPlainText("<div>hi</div>")
	if unknown != "<div>hi</div>" {
		t.Fatalf("unknown tags must be preserved literally, got %q", unknown)
	}
	script := TelegramChunkPlainText(`<script>alert("x")</script>`)
	if strings.Contains(script, "<script>") == false {
		t.Fatalf("script tag must not be stripped as formatting: %q", script)
	}
	if got := TelegramChunkPlainText("<b>unclosed"); got != "unclosed" {
		t.Fatalf("stray formatting start must be stripped, got %q", got)
	}
	if got := TelegramChunkPlainText("a & b"); got != "a & b" {
		t.Fatalf("bare ampersand must survive literally, got %q", got)
	}
	if got := TelegramChunkPlainText("a < b"); got != "a < b" {
		t.Fatalf("bare angle bracket must survive literally, got %q", got)
	}
	if got := TelegramChunkPlainText("a &notanentity; b"); got != "a &notanentity; b" {
		t.Fatalf("unknown entity must survive literally, got %q", got)
	}
}

func TestTelegramChunksRejectMalformedRendererOutput(t *testing.T) {
	pieces, _, normalized := parseTelegramHTML("<div>hi</div>")
	if !normalized {
		t.Fatal("unknown div tag must be flagged as malformed")
	}
	chunks := assembleTelegramChunks(pieces, nil)
	joined := strings.Join(chunks, "")
	if strings.Contains(joined, "<div>") || strings.Contains(joined, "</div>") {
		t.Fatalf("malformed tag passed through as HTML: %q", joined)
	}
	if !strings.Contains(joined, "&lt;div&gt;") {
		t.Fatalf("malformed tag was not safely escaped: %q", joined)
	}
	if got := TelegramChunkPlainText(joined); got != "<div>hi</div>" {
		t.Fatalf("escaped malformed plain = %q", got)
	}
}

func TestTelegramParserLimitedBoundsVeryLargeRenderedInput(t *testing.T) {
	limit := TelegramMaxVisiblePerReply + 1
	large := strings.Repeat("a", 1000000)
	pieces, visible, normalized := parseTelegramHTMLLimited(large, limit)
	if normalized {
		t.Fatal("plain input must not be flagged as malformed")
	}
	if len(visible) != limit {
		t.Fatalf("limited visible = %d, want exactly %d", len(visible), limit)
	}
	if len(pieces) != limit {
		t.Fatalf("limited pieces = %d, want exactly %d", len(pieces), limit)
	}
	if len(large) <= limit*10 {
		t.Fatalf("test sanity: large input %d bytes must dwarf the %d budget", len(large), limit)
	}
	// The ignored tail must never allocate: a distinctive tail marker stays
	// outside the bounded prefix except for the single sentinel rune.
	tailed := strings.Repeat("a", limit-1) + "Z" + strings.Repeat("Q", 1000000)
	_, tailVisible, _ := parseTelegramHTMLLimited(tailed, limit)
	if len(tailVisible) != limit {
		t.Fatalf("tailed visible = %d, want exactly %d", len(tailVisible), limit)
	}
	if tailVisible[limit-1] != 'Z' {
		t.Fatalf("sentinel visible = %q, want Z (tail must be ignored after the budget)", string(tailVisible[limit-1]))
	}
	for _, r := range tailVisible {
		if r == 'Q' {
			t.Fatal("ignored tail leaked into bounded visible prefix")
		}
	}
}

func TestTelegramLimitedTruncationMatchesUnlimited(t *testing.T) {
	input := strings.Repeat("content line with **bold** and 界 \U0001F600 text to fill the reply budget deterministically\n", 800)
	rendered := TelegramHTML(input)
	if rendered == "" {
		t.Fatal("fixture rendered empty")
	}
	uPieces, uVisible, _ := parseTelegramHTML(rendered)
	if len(uVisible) <= TelegramMaxVisiblePerReply {
		t.Fatalf("fixture visible = %d, want above %d", len(uVisible), TelegramMaxVisiblePerReply)
	}
	limit := TelegramMaxVisiblePerReply + 1
	lPieces, lVisible, _ := parseTelegramHTMLLimited(rendered, limit)
	if len(lVisible) != limit {
		t.Fatalf("limited visible = %d, want exactly %d", len(lVisible), limit)
	}
	for i := range lVisible {
		if lVisible[i] != uVisible[i] {
			t.Fatalf("limited visible differs at %d: %q vs %q", i, string(lVisible[i]), string(uVisible[i]))
		}
	}
	want := truncateTelegramPieces(uPieces, uVisible)
	gotLimited := truncateTelegramPieces(lPieces, lVisible)
	if strings.Join(want, "\x00") != strings.Join(gotLimited, "\x00") {
		t.Fatal("limited truncation differs from unlimited truncation")
	}
	first := TelegramChunks(input)
	second := TelegramChunks(input)
	if strings.Join(first, "\x00") != strings.Join(second, "\x00") {
		t.Fatal("TelegramChunks truncation is not deterministic")
	}
	if strings.Join(first, "\x00") != strings.Join(want, "\x00") {
		t.Fatal("TelegramChunks output differs from unlimited truncation baseline")
	}
	assertTelegramChunkLimits(t, first)
	total := 0
	for _, c := range first {
		total += telegramVisible(c)
	}
	if total != TelegramMaxVisiblePerReply {
		t.Fatalf("truncated total visible = %d, want exactly %d", total, TelegramMaxVisiblePerReply)
	}
	if plain := telegramConcatPlain(first); !strings.HasSuffix(plain, TelegramTruncationMarker) {
		t.Fatalf("truncated plain missing marker suffix")
	}
}

func TestTelegramParserLimitedMatchesUnlimitedShortInput(t *testing.T) {
	inputs := []string{
		"hello",
		"# Release\n\nUse **bold**, *italic*, ~~removed~~, and `code`.",
		"Entity & < > \" ' test with [link](https://example.com)",
		strings.Repeat("plain text line with 界 and \U0001F600\n", 200),
	}
	limit := TelegramMaxVisiblePerReply + 1
	for _, input := range inputs {
		rendered := TelegramHTML(input)
		if rendered == "" {
			continue
		}
		uPieces, uVisible, uNormalized := parseTelegramHTML(rendered)
		lPieces, lVisible, lNormalized := parseTelegramHTMLLimited(rendered, limit)
		if len(uVisible) > TelegramMaxVisiblePerReply {
			t.Fatalf("fixture exceeds reply budget")
		}
		if len(lVisible) != len(uVisible) || len(lPieces) != len(uPieces) || lNormalized != uNormalized {
			t.Fatalf("limited short parse differs for %q", input)
		}
		for i := range uVisible {
			if lVisible[i] != uVisible[i] {
				t.Fatalf("limited visible differs at %d for %q", i, input)
			}
		}
	}
}
