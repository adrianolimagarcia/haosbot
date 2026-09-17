package api

import (
	"strings"
	"testing"
	"time"
)

func TestRenderSafeMarkdownEscapesRawHTML(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "script tag",
			input: `<script>alert(1)</script>`,
			want:  "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>\n",
		},
		{
			name:  "img onerror",
			input: `<img src=x onerror=alert(1)>`,
			want:  "<p>&lt;img src=x onerror=alert(1)&gt;</p>\n",
		},
		{
			name:  "iframe",
			input: `<iframe src="https://evil.example/"></iframe>`,
			want:  "<p>&lt;iframe src=&quot;https://evil.example/&quot;&gt;&lt;/iframe&gt;</p>\n",
		},
		{
			name:  "attribute breakout",
			input: `"><script>alert(1)</script>`,
			want:  "<p>&quot;&gt;&lt;script&gt;alert(1)&lt;/script&gt;</p>\n",
		},
		{
			name:  "ampersand and quotes",
			input: `a & b " c ' d < e > f`,
			want:  "<p>a &amp; b &quot; c &#39; d &lt; e &gt; f</p>\n",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
		{
			name:  "blank lines only",
			input: "\n\n  \n",
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderSafeMarkdown(tc.input); got != tc.want {
				t.Errorf("renderSafeMarkdown(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRenderSafeMarkdownDropsUnsafeLinkTargets(t *testing.T) {
	unsafe := []string{
		`[click](javascript:alert(1))`,
		`[click](JaVaScRiPt:alert(1))`,
		"[click](java\tscript:alert(1))",
		"[click](java\nscript:alert(1))",
		"[click]( javascript:alert(1))",
		`[click](vbscript:msgbox(1))`,
		`[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)`,
		`[click](file:///etc/passwd)`,
		`[click](blob:https://evil.example/abc)`,
		`![pixel](javascript:alert(1))`,
	}

	for _, input := range unsafe {
		t.Run(input, func(t *testing.T) {
			got := renderSafeMarkdown(input)
			if strings.Contains(got, "<a ") || strings.Contains(got, "<img") {
				t.Errorf("renderSafeMarkdown(%q) produced a link/image: %s", input, got)
			}
			assertInertMarkup(t, input, got)
		})
	}
}

// Images are rendered as links: a remote <img> would let attacker-influenced
// output make the operator's browser call an arbitrary host, and the WebUI CSP
// blocks remote images anyway.
func TestRenderSafeMarkdownRendersImagesAsLinks(t *testing.T) {
	got := renderSafeMarkdown(`![pixel](https://evil.example/pixel.png)`)
	if strings.Contains(got, "<img") {
		t.Errorf("image syntax produced an <img>: %s", got)
	}
	if !strings.Contains(got, `<a href="https://evil.example/pixel.png"`) {
		t.Errorf("image syntax should degrade to a link: %s", got)
	}
	assertInertMarkup(t, "image", got)
}

func TestRenderSafeMarkdownKeepsSafeLinks(t *testing.T) {
	cases := map[string]string{
		`[docs](https://example.com/a?b=1&c=2)`: `<a href="https://example.com/a?b=1&amp;c=2" rel="noopener noreferrer nofollow">docs</a>`,
		`[mail](mailto:ops@example.com)`:        `<a href="mailto:ops@example.com" rel="noopener noreferrer nofollow">mail</a>`,
		`[local](/status)`:                      `<a href="/status" rel="noopener noreferrer nofollow">local</a>`,
	}
	for input, want := range cases {
		if got := renderSafeMarkdown(input); !strings.Contains(got, want) {
			t.Errorf("renderSafeMarkdown(%q) = %q, want it to contain %q", input, got, want)
		}
	}
}

func TestRenderSafeMarkdownRendersMarkdown(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"heading", "## Title", "<h2>Title</h2>"},
		{"h6", "###### deep", "<h6>deep</h6>"},
		{"not a heading", "#hashtag", "<p>#hashtag</p>"},
		{"bold", "**bold**", "<strong>bold</strong>"},
		{"italic", "*italic*", "<em>italic</em>"},
		{"inline code", "use `ls -la` now", "<code>ls -la</code>"},
		{"fenced code", "```go\nif a < b {}\n```", "<pre><code>if a &lt; b {}</code></pre>"},
		{"tilde fence", "~~~\nx\n~~~", "<pre><code>x</code></pre>"},
		{"unordered list", "- one\n- two", "<ul>\n<li>one</li>\n<li>two</li>\n</ul>"},
		{"ordered list", "1. one\n2. two", "<ol>\n<li>one</li>\n<li>two</li>\n</ol>"},
		{"blockquote", "> quoted", "<blockquote>quoted</blockquote>"},
		{"horizontal rule", "---", "<hr>"},
		{"paragraph lines", "a\nb", "<p>a<br>b</p>"},
		{"crlf", "a\r\nb", "<p>a<br>b</p>"},
		{"code span keeps markup inert", "`<b>x</b>`", "<code>&lt;b&gt;x&lt;/b&gt;</code>"},
		{"fence keeps markup inert", "```\n<script>x</script>\n```", "<pre><code>&lt;script&gt;x&lt;/script&gt;</code></pre>"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderSafeMarkdown(tc.input)
			if !strings.Contains(got, tc.want) {
				t.Errorf("renderSafeMarkdown(%q) = %q, want it to contain %q", tc.input, got, tc.want)
			}
			assertInertMarkup(t, tc.name, got)
		})
	}
}

func TestRenderSafeMarkdownUnterminatedFenceIsInert(t *testing.T) {
	got := renderSafeMarkdown("```\n<script>alert(1)</script>\nstill code")
	want := "<pre><code>&lt;script&gt;alert(1)&lt;/script&gt;\nstill code</code></pre>\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Adversarial inputs must not panic, must not emit markup, and must stay
// bounded in time (the renderer is linear in the input size).
func TestRenderSafeMarkdownSurvivesAdversarialInput(t *testing.T) {
	cases := map[string]string{
		"emphasis bomb":   strings.Repeat("**a", 20000) + strings.Repeat("**", 20000),
		"bracket bomb":    strings.Repeat("[", 1<<20),
		"backtick bomb":   strings.Repeat("`", 1<<20),
		"star bomb":       strings.Repeat("*", 1<<20),
		"underscore bomb": strings.Repeat("_", 1<<20),
		"link bomb":       strings.Repeat("[a](b)", 100000),
		"fence bomb":      strings.Repeat("```\n", 100000),
		"heading bomb":    strings.Repeat("#", 1<<20),
		"nul and control": strings.Repeat("\x00\x01\x02<a>", 50000),
		"html soup":       strings.Repeat(`<img src=x onerror=alert(1)><script>alert(1)</script>`, 20000),
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			got := renderSafeMarkdown(input)
			elapsed := time.Since(start)
			if elapsed > 20*time.Second {
				t.Errorf("renderSafeMarkdown took %s for %d bytes; the renderer must stay linear", elapsed, len(input))
			}
			assertInertMarkup(t, name, got)
			for _, forbidden := range []string{"<script", "<img", "<iframe", "<style"} {
				if strings.Contains(strings.ToLower(got), forbidden) {
					t.Fatalf("%s: output contains %s", name, forbidden)
				}
			}
		})
	}
}

func TestSafeLinkURL(t *testing.T) {
	safe := []string{
		"https://example.com/x",
		"http://example.com/x",
		"mailto:a@b.c",
		"/relative/path",
		"#fragment",
		"relative/path",
		"//example.com/protocol-relative",
	}
	for _, in := range safe {
		if _, ok := safeLinkURL(in); !ok {
			t.Errorf("safeLinkURL(%q) rejected a safe URL", in)
		}
	}

	unsafe := []string{
		"javascript:alert(1)",
		"JAVASCRIPT:alert(1)",
		" javascript:alert(1)",
		"java\tscript:alert(1)",
		"java\nscript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"blob:https://evil.example/x",
		"",
		"   ",
	}
	for _, in := range unsafe {
		if got, ok := safeLinkURL(in); ok {
			t.Errorf("safeLinkURL(%q) accepted an unsafe URL as %q", in, got)
		}
	}
}
