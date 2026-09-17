package telegram

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/pairing"
)

// These tests are white-box: they reach the unexported helpers the differential
// harness can only observe through a public entry point. They are NOT a
// substitute for compat/telegram_differential_test.go — every expectation here
// is a property of the reference that the corpus already pins, restated so a
// failure points at one function instead of at a 600-case table.

func TestIndexRuneFromIsRelativeToTheWholeSlice(t *testing.T) {
	runes := []rune("a\nb\nc")

	// Python: "a\nb\nc".find("\n", 2) == 3. Returning the offset within the
	// sub-slice would give 1, which is the bug this test exists for.
	if got := indexRuneFrom(runes, '\n', 2); got != 3 {
		t.Fatalf("indexRuneFrom(from=2) = %d, want 3", got)
	}
	if got := indexRuneFrom(runes, '\n', 0); got != 1 {
		t.Fatalf("indexRuneFrom(from=0) = %d, want 1", got)
	}
	if got := indexRuneFrom(runes, 'z', 0); got != -1 {
		t.Fatalf("indexRuneFrom(missing) = %d, want -1", got)
	}
	// Python: "abc".find("x", 99) == -1, not an exception.
	if got := indexRuneFrom(runes, '\n', 99); got != -1 {
		t.Fatalf("indexRuneFrom(from past end) = %d, want -1", got)
	}
	if got := indexRuneFrom(runes, '\n', -5); got != 1 {
		t.Fatalf("indexRuneFrom(negative from) = %d, want 1 (clamped to 0)", got)
	}
}

func TestLastIndexRuneFromMatchesPythonRfind(t *testing.T) {
	runes := []rune("a\nb\nc")
	if got := lastIndexRuneFrom(runes, '\n', 0); got != 3 {
		t.Fatalf("lastIndexRuneFrom(from=0) = %d, want 3", got)
	}
	if got := lastIndexRuneFrom(runes, '\n', 4); got != -1 {
		t.Fatalf("lastIndexRuneFrom(from=4) = %d, want -1", got)
	}
	// A start beyond the slice must not panic.
	if got := lastIndexRuneFrom(runes, '\n', 99); got != -1 {
		t.Fatalf("lastIndexRuneFrom(from past end) = %d, want -1", got)
	}
}

func TestSplitMarkdownCountsCharactersNotBytes(t *testing.T) {
	// Five emoji are 5 Python characters but 20 UTF-8 bytes. A byte-indexed
	// port would return the whole string as one chunk.
	content := "🎉🎉🎉🎉🎉"
	chunks := SplitMarkdown(content, 2)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks %q, want 3", len(chunks), chunks)
	}
	for i, chunk := range chunks {
		if n := utf8.RuneCountInString(chunk); n > 2 {
			t.Fatalf("chunk[%d] = %q has %d characters, budget is 2", i, chunk, n)
		}
	}
	if joined := strings.Join(chunks, ""); joined != content {
		t.Fatalf("joined = %q, want %q", joined, content)
	}
}

func TestSplitMarkdownWhitespaceOnlyInputIsEmpty(t *testing.T) {
	for _, input := range []string{"", " ", "\t", "\n", "\u001c", "\u00a0", "\u3000", "\u200b"} {
		got := SplitMarkdown(input, 10)
		if input == "\u200b" {
			// ZERO WIDTH SPACE is NOT Python whitespace, so it survives and
			// yields one chunk. Getting this backwards is the classic
			// strings.TrimSpace bug.
			if len(got) != 1 {
				t.Fatalf("SplitMarkdown(%q) = %q, want one chunk", input, got)
			}
			continue
		}
		if len(got) != 0 {
			t.Fatalf("SplitMarkdown(%q) = %q, want no chunks", input, got)
		}
	}
}

func TestSplitMarkdownNonPositiveBudgetDoesNotHang(t *testing.T) {
	// The reference loops forever for max_len <= 0. The port returns the whole
	// input instead; this test is the guard against someone "fixing" it back.
	got := SplitMarkdown("abcdef", 0)
	if len(got) != 1 || got[0] != "abcdef" {
		t.Fatalf("SplitMarkdown(budget 0) = %q, want one chunk", got)
	}
}

func TestCountFenceIsNonOverlapping(t *testing.T) {
	cases := map[string]int{
		"":         0,
		"```":      1,
		"``````":   2,
		"````":     1,
		"``":       0,
		"```a```":  2,
		"```````":  2, // seven backticks: two non-overlapping runs, one left over
		"a```b``c": 1,
	}
	for input, want := range cases {
		if got := countFence([]rune(input)); got != want {
			t.Errorf("countFence(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestSplitMarkdownFenceMarkersAreDuplicated(t *testing.T) {
	// The reference re-emits the opening and closing fence so every chunk is a
	// self-contained Markdown document, which means join(chunks) != content.
	// The differential corpus records that flag; this asserts it locally so the
	// surprising behaviour is visible in a unit test too.
	content := "```\n" + strings.Repeat("x", 40) + "\n```"
	chunks := SplitMarkdown(content, 20)
	if len(chunks) < 2 {
		t.Fatalf("expected the fenced block to be split, got %q", chunks)
	}
	if joined := strings.Join(chunks, ""); joined == content {
		t.Fatal("chunks rejoined to the input; the reference duplicates fences and drops cut whitespace")
	}
	for i, chunk := range chunks {
		if got := countFence([]rune(chunk)); got%2 != 0 {
			t.Fatalf("chunk[%d] = %q has %d fence markers: the block is unbalanced", i, chunk, got)
		}
	}
}

func TestNormalizeCommandKeepsArgumentTextByCharacter(t *testing.T) {
	// The reference slices with content[len(alias):], a CHARACTER index, so a
	// multi-byte argument must survive intact.
	got := NormalizeCommand("/dream_log 🎉 café")
	if got != "/dream-log 🎉 café" {
		t.Fatalf("NormalizeCommand = %q", got)
	}
	if got := NormalizeCommand("/dream_logx"); got != "/dream_logx" {
		t.Fatalf("a longer word must not be rewritten, got %q", got)
	}
	if got := NormalizeCommand("/DREAM_LOG"); got != "/DREAM_LOG" {
		t.Fatalf("matching is case-sensitive, got %q", got)
	}
	if got := NormalizeCommand("/dream_log\tx"); got != "/dream_log\tx" {
		t.Fatalf("only a SPACE is an argument separator, got %q", got)
	}
}

func TestReplaceDisplayCommandsBoundaries(t *testing.T) {
	cases := map[string]string{
		"/dream-log":            "/dream_log",
		" /dream-log":           " /dream_log",
		"x/dream-log":           "x/dream-log",
		"/dream-logx":           "/dream-logx",
		"/dream-log-x":          "/dream-log-x",
		"/dream-log.x":          "/dream-log.x",
		"/dream-log_":           "/dream-log_",
		"/dream-log,/dream-log": "/dream_log,/dream_log",
		"é/dream-log":           "é/dream-log",
		"é /dream-log":          "é /dream_log",
		"/dream-log é":          "/dream_log é",
		"中/dream-log":           "中/dream-log",
		"/dream-log 中":          "/dream_log 中",
		"\u200b/dream-log":      "\u200b/dream_log", // ZWSP is not in [\w/.-]
		"\u0301/dream-log":      "\u0301/dream_log", // combining acute is not a word char
	}
	for input, want := range cases {
		if got := ReplaceDisplayCommands(input); got != want {
			t.Errorf("ReplaceDisplayCommands(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPySplitLinesKeepEndsBoundaries(t *testing.T) {
	// The boundary set was enumerated from the reference: LF, VT, FF, CR, CRLF,
	// U+001C..U+001E, U+0085, U+2028, U+2029. U+001F and U+200B are NOT
	// boundaries, and strings.Split(s, "\n") would be wrong for all of them.
	cases := map[string][]string{
		"":         {},
		"abc":      {"abc"},
		"abc\n":    {"abc\n"},
		"a\r\nb":   {"a\r\n", "b"},
		"a\rb":     {"a\r", "b"},
		"a\x0bb":   {"a\x0b", "b"},
		"a\x0cb":   {"a\x0c", "b"},
		"a\x1cb":   {"a\x1c", "b"},
		"a\x1db":   {"a\x1d", "b"},
		"a\x1eb":   {"a\x1e", "b"},
		"a\u0085b": {"a\u0085", "b"},
		"a\u2028b": {"a\u2028", "b"},
		"a\u2029b": {"a\u2029", "b"},
		"a\x1fb":   {"a\x1fb"},
		"a\u200bb": {"a\u200bb"},
		"a\n\nb":   {"a\n", "\n", "b"},
	}
	for input, want := range cases {
		got := pySplitLinesKeepEnds(input)
		if len(got) != len(want) {
			t.Errorf("pySplitLinesKeepEnds(%q) = %q, want %q", input, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("pySplitLinesKeepEnds(%q)[%d] = %q, want %q", input, i, got[i], want[i])
			}
		}
	}
}

func TestDisplayCommandTextVerticalTabEndsALine(t *testing.T) {
	// VT is a Python line boundary, so a fence can be opened on a line that a
	// strings.Split(s, "\n") implementation would never see.
	input := "before\x0b```\x0b/dream-log\x0b```\x0bafter /dream-log"
	got := DisplayCommandText(input)
	if !strings.Contains(got, "\x0b/dream-log\x0b") {
		t.Fatalf("a command inside a VT-delimited fence was rewritten: %q", got)
	}
	if !strings.Contains(got, "after /dream_log") {
		t.Fatalf("the command after the closing fence was not rewritten: %q", got)
	}
}

func TestEscapeHTMLDoesNotDoubleEscape(t *testing.T) {
	// The reference replaces & first, then < and >, and never touches the
	// entities it just produced.
	if got := EscapeHTML("&<>"); got != "&amp;&lt;&gt;" {
		t.Fatalf("EscapeHTML = %q", got)
	}
	if got := EscapeHTML("&amp;"); got != "&amp;amp;" {
		t.Fatalf("EscapeHTML of an entity = %q", got)
	}
}

func TestResolveEnvRefsIsAllOrNothing(t *testing.T) {
	// envLookup defaults to os.LookupEnv, so t.Setenv drives the real thing and
	// the process environment is restored automatically.
	t.Setenv("NB_TEST_SET", "value")

	if got := ResolveEnvRefs("${NB_TEST_SET}"); got != "value" {
		t.Fatalf("resolved = %q, want %q", got, "value")
	}
	// One unset reference collapses the WHOLE value to "", which is what makes
	// a half-configured token report token_env instead of being sent upstream.
	if got := ResolveEnvRefs("a${NB_TEST_SET}b${NB_TEST_UNSET}"); got != "" {
		t.Fatalf("partial resolution = %q, want empty", got)
	}
	if got := ResolveEnvRefs("plain"); got != "plain" {
		t.Fatalf("no-reference value = %q", got)
	}
	// ${1BAD} does not match the pattern, so it is left alone rather than
	// treated as an unset variable.
	if got := ResolveEnvRefs("${1BAD}"); got != "${1BAD}" {
		t.Fatalf("non-identifier reference = %q", got)
	}
}

func TestConfigParseRejectsAllowlistGroupPolicy(t *testing.T) {
	// GROUP_POLICIES advertises "allowlist" but TelegramConfig only accepts
	// "open" and "mention". A config written for another channel must be
	// rejected here, not silently downgraded to "mention".
	_, err := ParseConfig(map[string]any{"groupPolicy": "allowlist"})
	if err == nil {
		t.Fatal("groupPolicy=allowlist was accepted")
	}
	verr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error is %T, want *ValidationError", err)
	}
	if len(verr.Errors) != 1 || verr.Errors[0].Type != "literal_error" {
		t.Fatalf("errors = %+v", verr.Errors)
	}
	if !strings.Contains(verr.Errors[0].Msg, "'mention'") {
		t.Fatalf("message does not list the accepted choices: %q", verr.Errors[0].Msg)
	}
}

func TestConfigParseAliasBeatsFieldName(t *testing.T) {
	// pydantic with an alias generator and populate_by_name looks the alias up
	// FIRST, so the camelCase key wins whichever order the keys appear in.
	for _, values := range []map[string]any{
		{"allowFrom": []any{"alice"}, "allow_from": []any{"bob"}},
		{"allow_from": []any{"bob"}, "allowFrom": []any{"alice"}},
	} {
		cfg, err := ParseConfig(values)
		if err != nil {
			t.Fatalf("ParseConfig(%v): %v", values, err)
		}
		if len(cfg.AllowFrom) != 1 || cfg.AllowFrom[0] != "alice" {
			t.Fatalf("ParseConfig(%v).AllowFrom = %q, want [alice]", values, cfg.AllowFrom)
		}
	}
}

func TestConfigParseReportsEveryListElementError(t *testing.T) {
	_, err := ParseConfig(map[string]any{"allowFrom": []any{"a", 1, nil, true}})
	if err == nil {
		t.Fatal("expected an error")
	}
	verr := err.(*ValidationError)
	if len(verr.Errors) != 3 {
		t.Fatalf("got %d errors %+v, want 3", len(verr.Errors), verr.Errors)
	}
	for i, want := range []string{"1", "2", "3"} {
		if len(verr.Errors[i].Loc) != 2 || verr.Errors[i].Loc[1] != want {
			t.Fatalf("error[%d].Loc = %v, want index %s", i, verr.Errors[i].Loc, want)
		}
	}
}

func TestConfigParseErrorsFollowFieldOrder(t *testing.T) {
	// pydantic aggregates errors in field-DECLARATION order, not in the order
	// the offending keys appear in the input.
	_, err := ParseConfig(map[string]any{
		"webhookListenPort": 0,
		"mode":              "nope",
		"enabled":           "nope",
	})
	verr := err.(*ValidationError)
	got := make([]string, 0, len(verr.Errors))
	for _, e := range verr.Errors {
		got = append(got, e.Loc[0])
	}
	want := []string{"enabled", "mode", "webhookListenPort"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("error order = %v, want %v", got, want)
	}
}

func TestConfigWebhookPathValidatorUsesPythonStrip(t *testing.T) {
	// "\u001c" IS Python whitespace and "\u200b" is NOT, so the first is
	// stripped and the second makes the path fail the leading-slash check.
	cfg, err := ParseConfig(map[string]any{"webhookPath": "\u001c/hook"})
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.WebhookPath != "/hook" {
		t.Fatalf("WebhookPath = %q, want /hook", cfg.WebhookPath)
	}

	_, err = ParseConfig(map[string]any{"webhookPath": "\u200b/hook"})
	if err == nil {
		t.Fatal("\u200b/hook was accepted; the validator is stripping too much")
	}

	// An empty or whitespace-only path falls back to the default.
	cfg, err = ParseConfig(map[string]any{"webhookPath": "   "})
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.WebhookPath != "/telegram" {
		t.Fatalf("WebhookPath = %q, want /telegram", cfg.WebhookPath)
	}
}

func TestConfigWebhookURLValidatorMessages(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]any
		want   string
	}{
		{"missing url", map[string]any{"mode": "webhook", "webhookSecretToken": "abc"},
			"Value error, webhook_url is required when Telegram mode is webhook"},
		{"not https", map[string]any{"mode": "webhook", "webhookUrl": "http://example.com/h", "webhookSecretToken": "abc"},
			"Value error, webhook_url must be a public HTTPS URL"},
		{"missing secret", map[string]any{"mode": "webhook", "webhookUrl": "https://example.com/h"},
			"Value error, webhook_secret_token is required when Telegram mode is webhook"},
		{"bad secret", map[string]any{"mode": "webhook", "webhookUrl": "https://example.com/h", "webhookSecretToken": "bad token!"},
			"Value error, webhook_secret_token must be 1-256 characters using only A-Z, a-z, 0-9, _ and -"},
		// urlsplit raises on these and validate_webhook_config does not catch
		// it, so urllib's own message text reaches the caller.
		{"unclosed bracket", map[string]any{"mode": "webhook", "webhookUrl": "https://[::1", "webhookSecretToken": "abc"},
			"Value error, Invalid IPv6 URL"},
		{"nfkc netloc", map[string]any{"mode": "webhook", "webhookUrl": "https://\u2100.example/h", "webhookSecretToken": "abc"},
			"Value error, netloc '\u2100.example' contains invalid characters under NFKC normalization"},
	}
	for _, tc := range cases {
		_, err := ParseConfig(tc.values)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		verr, ok := err.(*ValidationError)
		if !ok {
			t.Errorf("%s: error is %T", tc.name, err)
			continue
		}
		if len(verr.Errors) != 1 || verr.Errors[0].Msg != tc.want {
			t.Errorf("%s: msg = %q, want %q", tc.name, verr.Errors[0].Msg, tc.want)
		}
	}
}

func TestConfigAcceptsHTTPSWithUnsafeBytesRemoved(t *testing.T) {
	// urlsplit deletes TAB, CR and LF from anywhere in the URL BEFORE the
	// scheme/netloc split, so a tab between the colon and the slashes still
	// yields a netloc. Without that step this is not a public HTTPS URL.
	for _, url := range []string{"https:\t//example.com/hook", "https:\r//example.com/hook", "https:\n//example.com/hook"} {
		cfg, err := ParseConfig(map[string]any{
			"mode":               "webhook",
			"webhookUrl":         url,
			"webhookSecretToken": "abc",
		})
		if err != nil {
			t.Errorf("ParseConfig(%q): %v", url, err)
			continue
		}
		if cfg.WebhookURL != url {
			t.Errorf("WebhookURL = %q, want the original %q (the URL is not rewritten)", cfg.WebhookURL, url)
		}
	}
}

func TestProxyURLIsValidTrimsOnlyLeadingControlAndSpace(t *testing.T) {
	// _urlsplit lstrips U+0000..U+0020 but deliberately keeps a TRAILING space,
	// which then lands in the port and makes it unparseable.
	if !ProxyURLIsValid("\x00http://proxy.local:8080") {
		t.Error("a leading NUL should be stripped")
	}
	if ProxyURLIsValid("http://proxy.local:8080 ") {
		t.Error("a trailing space stays in the port and must make the URL invalid")
	}
	if !ProxyURLIsValid("http://\u3000proxy:8080") {
		t.Error("U+3000 is outside the lstrip set and is a valid host character")
	}
	if !ProxyURLIsValid("http://[fe80::1%25eth0]:80") {
		t.Error("a scoped IPv6 literal is accepted by the reference")
	}
	if ProxyURLIsValid("http://[1.2.3.4]:80") {
		t.Error("an IPv4 address in brackets is rejected")
	}
}

func TestSenderPolicyLegacyBranchNeedsAnAttribute(t *testing.T) {
	// `getattr(self.config, "allow_from", [])` finds nothing on a DICT, so the
	// legacy "id|username" form is unreachable for a map section even though
	// the allowlist contains the id.
	// A temp-dir store keeps the test off ~/.nanobot/pairing.json, whose
	// contents would otherwise decide the result.
	store := pairing.NewStore(filepath.Join(t.TempDir(), "pairing.json"))
	section := channels.NewMapSection(map[string]any{"allow_from": []any{"123"}})
	policy := NewSenderPolicy(section, store)
	if policy.IsAllowed("123|alice") {
		t.Fatal("a dict section must not reach the legacy branch")
	}

	// The same allowlist as an object DOES match.
	obj := NewSenderPolicyForConfig(Config{AllowFrom: []string{"123"}}, store)
	if !obj.IsAllowed("123|alice") {
		t.Fatal("an object section with allow_from=[123] must accept 123|alice")
	}
}

func TestSenderPolicyLegacyBranchRequiresExactlyOnePipe(t *testing.T) {
	store := pairing.NewStore(filepath.Join(t.TempDir(), "pairing.json"))
	policy := NewSenderPolicyForConfig(Config{AllowFrom: []string{"123", "alice"}}, store)
	// "123" alone is deliberately NOT in this list: it is in the allowlist, so
	// the BASE check accepts it before the legacy branch is ever reached.
	for _, sender := range []string{"999", "999|alice|bob", "|alice", "123|"} {
		if policy.IsAllowed(sender) {
			t.Errorf("IsAllowed(%q) = true, want false", sender)
		}
	}
	if !policy.IsAllowed("123|alice") {
		t.Error("IsAllowed(123|alice) = false, want true")
	}
	// str.isdigit accepts superscripts, so "²|alice" is a legacy-shaped id.
	sup := NewSenderPolicyForConfig(Config{AllowFrom: []string{"\u00b2"}}, store)
	if !sup.IsAllowed("\u00b2|alice") {
		t.Error("a superscript-two id must count as digits, as str.isdigit does")
	}
}

func TestSenderPolicyWildcardDisablesLegacyBranch(t *testing.T) {
	// The base check already accepts everything when "*" is present, and the
	// legacy branch bails out on "*" too.
	store := pairing.NewStore(filepath.Join(t.TempDir(), "pairing.json"))
	policy := NewSenderPolicyForConfig(Config{AllowFrom: []string{"*"}}, store)
	if !policy.IsAllowed("anyone") {
		t.Fatal("a wildcard allowlist must accept any sender")
	}
}

func TestPyTruthyMatchesPythonBool(t *testing.T) {
	truthy := []any{true, "x", "0", 1, -1, 0.5, json.Number("1"), []any{1}, map[string]any{"a": 1}}
	for _, v := range truthy {
		if !pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) = false, want true", v)
		}
	}
	falsy := []any{nil, false, "", 0, 0.0, json.Number("0"), []any{}, []string{}, map[string]any{}}
	for _, v := range falsy {
		if pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) = true, want false", v)
		}
	}
}

func TestPyContainsMatchesPythonIn(t *testing.T) {
	if !pyContains([]any{"a", "b"}, "a") {
		t.Error("list membership")
	}
	// Python's == between a str and an int is False, so 1 does not match "1".
	if pyContains([]any{1, 2}, "1") {
		t.Error("a non-string list element must not match")
	}
	if !pyContains("abc", "b") {
		t.Error("a string operand is a SUBSTRING test, not membership")
	}
	if !pyContains(map[string]any{"k": 1}, "k") {
		t.Error("a dict operand is a KEY test")
	}
	if pyContains(map[string]any{"k": 1}, "1") {
		t.Error("a dict operand must not match a value")
	}
}

func TestHasMentionEntityClampsOutOfRangeOffsets(t *testing.T) {
	offset, length := 0, 999
	// Python slices with clamping; an unclamped Go slice would panic.
	if !HasMentionEntity("@nanobot", []MentionEntity{{Type: "mention", Offset: &offset, Length: &length}}, "nanobot", nil) {
		t.Fatal("an out-of-range length must clamp, not panic")
	}
	// A negative offset must clamp to 0 instead of panicking on runes[-5:].
	// The text deliberately does NOT contain the handle, so the substring
	// fallback cannot mask the result.
	neg := -5
	three := 3
	if HasMentionEntity("abc", []MentionEntity{{Type: "mention", Offset: &neg, Length: &three}}, "nanobot", nil) {
		t.Fatal("a negative offset must clamp to 0 and not match")
	}
}

func TestIsGroupMessageForBotZeroBotIDIsFalsy(t *testing.T) {
	zero := int64(0)
	reply := int64(0)
	// `bool(bot_id and reply_user and reply_user.id == bot_id)` rejects a zero
	// bot id even when the reply matches, because 0 is falsy in Python.
	got := IsGroupMessageForBot(
		GroupMessageInput{ChatType: "group", ReplyToUserID: &reply},
		"mention", &zero, "")
	if got {
		t.Fatal("bot_id 0 must be falsy and disable the reply-to-bot rule")
	}
}

func TestSplitMarkdownHTMLChunksRejectsATokenOverTheLimit(t *testing.T) {
	// A single character cannot fit a budget of 0, so the reference raises
	// rather than looping.
	if _, err := SplitMarkdownHTMLChunks("abc", 0); err == nil {
		t.Fatal("expected an error for max_html_len 0")
	}
}

func TestSetupSpecFieldOrderAndSecrets(t *testing.T) {
	if len(SETUP_SPEC.Fields) != 19 {
		t.Fatalf("field count = %d, want 19", len(SETUP_SPEC.Fields))
	}
	if SETUP_SPEC.Fields[0].Name != "token" {
		t.Fatalf("first field = %q, want token", SETUP_SPEC.Fields[0].Name)
	}
	if got := SETUP_SPEC.Secrets(); len(got) != 2 || got[0] != "token" || got[1] != "webhookSecretToken" {
		t.Fatalf("Secrets() = %v", got)
	}
	if got := SETUP_SPEC.SimpleRequiredFields(); len(got) != 1 || got[0] != "token" {
		t.Fatalf("SimpleRequiredFields() = %v", got)
	}
	// The public dict omits default_value for a nil default rather than
	// writing null.
	public := SETUP_SPEC.ToPublicDict("telegram")
	fields := public["fields"].([]any)
	token := fields[0].(map[string]any)
	if _, present := token["default_value"]; present {
		t.Fatalf("token has no default, so default_value must be absent: %v", token)
	}
	if token["required"] != true {
		t.Fatalf("token must be marked required: %v", token)
	}
	reply := fields[5].(map[string]any)
	if reply["default_value"] != "false" {
		t.Fatalf("a bool default renders lower-case: %v", reply)
	}
}

func TestLookupSetupSpecRejectsOtherChannels(t *testing.T) {
	if LookupSetupSpec("telegram") == nil {
		t.Fatal("telegram must resolve")
	}
	if LookupSetupSpec("discord") != nil {
		t.Fatal("this package must not claim another channel's spec")
	}
}

func TestFieldLabelMatchesPython(t *testing.T) {
	cases := map[string]string{
		"token":        "Token",
		"webhookUrl":   "Webhook Url",
		"allow_from":   "Allow from",
		"channels.a.b": "Channels a b",
		"":             "",
		"aB":           "A B",
	}
	for input, want := range cases {
		if got := fieldLabel(input); got != want {
			t.Errorf("fieldLabel(%q) = %q, want %q", input, got, want)
		}
	}
}
