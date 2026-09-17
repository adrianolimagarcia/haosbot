package compat

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bpe"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// This file is deliberately self-contained: its own dumper
// (compat/python/dump_bpe.py) and its own loader. It shares no declaration with
// differential_test.go beyond repoRoot, because dump_reference.py and
// differential_test.go are edited concurrently by several agents and a
// read-modify-write race there would destroy work.
//
// It compares internal/bpe against the REAL tiktoken 0.14.0 running in the
// project venv, on the FULL token ID sequence — not on token counts, which
// cannot distinguish a different split from a different merge.
//
// The reference's pre-tokenizer is compiled inside tiktoken's Rust core, so
// this is the only way to establish what it does. Three of the properties
// pinned here were established this way and are easy to get wrong by reading:
//
//   - the pattern's leading apostrophe (`'(?i:[sdmt]|ll|ve|re)|...`), without
//     which "don't" is ['d', 'on', "'t"] instead of ['don', "'t"];
//   - `\p{L}`/`\p{N}` from the Rust regex crate's tables, which are a strict
//     SUPERSET of Go 1.23.5's Unicode 15.0.0 tables (141_028 vs 136_104);
//   - `(?i:s)` matching U+017F, so "\u017fs" is two pieces.

// ---------------------------------------------------------------------------
// dumper document
// ---------------------------------------------------------------------------

type bpeCase struct {
	Text        string  `json:"text"`
	Ordinary    []int64 `json:"ordinary"`
	Encode      []int64 `json:"encode"`
	EncodeError *string `json:"encode_error"`
}

type bpeDecodeCase struct {
	Tokens []int64 `json:"tokens"`
	Text   string  `json:"text"`
}

type bpeByteDecodeCase struct {
	BytesB64 string  `json:"bytes_b64"`
	Tokens   []int64 `json:"tokens"`
	Text     string  `json:"text"`
}

type bpeClassCase struct {
	CP    int64 `json:"cp"`
	Space bool  `json:"space"`
	L     bool  `json:"L"`
	N     bool  `json:"N"`
}

type bpeSweep struct {
	Name         string `json:"name"`
	InputSHA256  string `json:"input_sha256"`
	InputBytes   int    `json:"input_bytes"`
	NTokens      int    `json:"n_tokens"`
	TokensSHA256 string `json:"tokens_sha256"`
}

type bpeTruncateCase struct {
	Text      string `json:"text"`
	MaxTokens int    `json:"max_tokens"`
	Out       string `json:"out"`
}

type bpeEstimateCase struct {
	Messages      []json.RawMessage `json:"messages"`
	Tools         json.RawMessage   `json:"tools"`
	Tokens        int               `json:"tokens"`
	Source        string            `json:"source"`
	MessageTokens []int             `json:"message_tokens"`
}

type bpeDoc struct {
	UpstreamCommit  string              `json:"upstream_commit"`
	TiktokenVersion string              `json:"tiktoken_version"`
	PatStr          string              `json:"pat_str"`
	NVocab          int                 `json:"n_vocab"`
	NMergeableRanks int                 `json:"n_mergeable_ranks"`
	FuzzSeed        int64               `json:"fuzz_seed"`
	NCorpusTexts    int                 `json:"n_corpus_texts"`
	Cases           []bpeCase           `json:"cases"`
	DecodeCases     []bpeDecodeCase     `json:"decode_cases"`
	ByteDecodeCases []bpeByteDecodeCase `json:"byte_decode_cases"`
	ClassCases      []bpeClassCase      `json:"class_cases"`
	Sweeps          []bpeSweep          `json:"sweeps"`
	TruncateCases   []bpeTruncateCase   `json:"truncate_cases"`
	EstimateCases   []bpeEstimateCase   `json:"estimate_cases"`
}

var (
	bpeDocOnce sync.Once
	bpeDocVal  *bpeDoc
	bpeDocSkip string
	bpeDocErr  error
)

// loadBPEDoc runs compat/python/dump_bpe.py once per test binary.
//
// The dumper takes several seconds (it hashes the token IDs of four strings
// that between them contain every Unicode scalar value), so every BPE
// differential test shares one run.
func loadBPEDoc(t *testing.T) *bpeDoc {
	t.Helper()
	bpeDocOnce.Do(func() {
		root := repoRoot(t)
		python := filepath.Join(root, ".tools", "venv", "bin", "python")
		script := filepath.Join(root, "compat", "python", "dump_bpe.py")
		if _, err := os.Stat(python); err != nil {
			bpeDocSkip = fmt.Sprintf("SKIP: reference venv not present at %s — differential check not run", python)
			return
		}
		if _, err := os.Stat(script); err != nil {
			bpeDocSkip = fmt.Sprintf("SKIP: dumper missing at %s", script)
			return
		}
		cmd := exec.Command(python, script)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			bpeDocErr = fmt.Errorf("reference BPE dumper failed: %w\n%s", err, stderr.String())
			return
		}
		var doc bpeDoc
		if err := json.Unmarshal(out, &doc); err != nil {
			bpeDocErr = fmt.Errorf("parse BPE reference output: %w", err)
			return
		}
		bpeDocVal = &doc
	})
	if bpeDocSkip != "" {
		t.Skip(bpeDocSkip)
	}
	if bpeDocErr != nil {
		t.Fatal(bpeDocErr)
	}
	return bpeDocVal
}

func toUint32(in []int64) []uint32 {
	out := make([]uint32, len(in))
	for i, v := range in {
		out[i] = uint32(v)
	}
	return out
}

// ---------------------------------------------------------------------------
// Case-count guard
// ---------------------------------------------------------------------------

// TestBPEDifferentialCaseCount fails when the corpus is empty.
//
// A differential suite that compares nothing passes silently, which is worse
// than not having it. Every other test in this file is gated on a non-zero
// count here.
func TestBPEDifferentialCaseCount(t *testing.T) {
	doc := loadBPEDoc(t)

	type part struct {
		name string
		n    int
	}
	parts := []part{
		{"cases (curated + seeded fuzz)", len(doc.Cases)},
		{"decode_cases", len(doc.DecodeCases)},
		{"byte_decode_cases", len(doc.ByteDecodeCases)},
		{"class_cases", len(doc.ClassCases)},
		{"sweeps", len(doc.Sweeps)},
		{"truncate_cases", len(doc.TruncateCases)},
		{"estimate_cases", len(doc.EstimateCases)},
	}
	tokenIDs := 0
	for _, c := range doc.Cases {
		tokenIDs += len(c.Ordinary)
	}
	sweepTokens := 0
	for _, s := range doc.Sweeps {
		sweepTokens += s.NTokens
	}

	for _, p := range parts {
		if p.n == 0 {
			t.Fatalf("reference corpus section %q is EMPTY — the suite would pass vacuously", p.name)
		}
		t.Logf("reference %-32s %6d", p.name, p.n)
	}
	if tokenIDs == 0 {
		t.Fatal("reference corpus carries zero token IDs — the suite would pass vacuously")
	}
	if sweepTokens == 0 {
		t.Fatal("reference sweeps carry zero token IDs — the suite would pass vacuously")
	}
	t.Logf("reference %-32s %6d", "token IDs compared", tokenIDs)
	t.Logf("reference %-32s %6d", "token IDs hashed in sweeps", sweepTokens)
	t.Logf("reference tiktoken %s, n_vocab=%d, mergeable ranks=%d",
		doc.TiktokenVersion, doc.NVocab, doc.NMergeableRanks)

	if doc.PatStr != bpe.Pattern {
		t.Fatalf("pre-tokenizer pattern drifted:\n reference %q\n port      %q", doc.PatStr, bpe.Pattern)
	}
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

// TestBPEDifferentialEncode compares the FULL token ID sequence for every
// corpus text, for both encode_ordinary and the default encode.
func TestBPEDifferentialEncode(t *testing.T) {
	doc := loadBPEDoc(t)
	if len(doc.Cases) == 0 {
		t.Fatal("no reference cases")
	}

	ordinaryChecked, encodeChecked, errorsChecked := 0, 0, 0
	failures := 0
	for i, c := range doc.Cases {
		got, err := bpe.EncodeOrdinary(c.Text)
		if err != nil {
			t.Fatalf("case %d (%q): EncodeOrdinary: %v", i, c.Text, err)
		}
		if !equalIDs(got, c.Ordinary) {
			failures++
			if failures <= 10 {
				t.Errorf("case %d EncodeOrdinary(%q):\n got  %v\n want %v\n pieces(got)  %s",
					i, c.Text, got, c.Ordinary, describePieces(c.Text))
			}
		}
		ordinaryChecked++

		gotEnc, err := bpe.Encode(c.Text)
		switch {
		case c.EncodeError != nil:
			var se *bpe.SpecialTokenError
			if !errors.As(err, &se) {
				failures++
				if failures <= 10 {
					t.Errorf("case %d Encode(%q): reference raised %q, port returned err=%v",
						i, c.Text, *c.EncodeError, err)
				}
			} else if !strings.Contains(*c.EncodeError, se.Token) {
				t.Errorf("case %d Encode(%q): reference error %q does not mention port token %q",
					i, c.Text, *c.EncodeError, se.Token)
			}
			errorsChecked++
		default:
			if err != nil {
				failures++
				if failures <= 10 {
					t.Errorf("case %d Encode(%q): unexpected error %v", i, c.Text, err)
				}
			} else if !equalIDs(gotEnc, c.Encode) {
				failures++
				if failures <= 10 {
					t.Errorf("case %d Encode(%q):\n got  %v\n want %v", i, c.Text, gotEnc, c.Encode)
				}
			}
			encodeChecked++
		}
	}
	if ordinaryChecked == 0 || encodeChecked == 0 || errorsChecked == 0 {
		t.Fatalf("vacuous: ordinary=%d encode=%d error=%d", ordinaryChecked, encodeChecked, errorsChecked)
	}
	t.Logf("encode_ordinary compared: %d texts (%d token IDs)", ordinaryChecked, countIDs(doc.Cases))
	t.Logf("encode compared:          %d texts, %d raising SpecialTokenError", encodeChecked, errorsChecked)
}

// TestBPEDifferentialDecode compares Encoding.decode on token prefixes.
//
// Prefixes are the interesting input: a token boundary is not a character
// boundary, so the prefix is frequently not valid UTF-8 and Python's
// errors="replace" rule decides the result.
func TestBPEDifferentialDecode(t *testing.T) {
	doc := loadBPEDoc(t)
	if len(doc.DecodeCases) == 0 {
		t.Fatal("no reference decode cases")
	}
	for i, c := range doc.DecodeCases {
		got := bpe.Decode(toUint32(c.Tokens))
		if got != c.Text {
			t.Errorf("decode case %d %v:\n got  %q\n want %q", i, c.Tokens, got, c.Text)
		}
	}
	t.Logf("decode compared: %d token prefixes", len(doc.DecodeCases))
}

// TestBPEDifferentialByteDecode pins the utf-8 errors="replace" rule on byte
// strings that no token prefix would produce, including every invalid shape:
// truncated sequences, lone continuations, overlong leads, surrogates and
// out-of-range leads.
func TestBPEDifferentialByteDecode(t *testing.T) {
	doc := loadBPEDoc(t)
	if len(doc.ByteDecodeCases) == 0 {
		t.Fatal("no reference byte-decode cases")
	}
	for i, c := range doc.ByteDecodeCases {
		raw, err := base64.StdEncoding.DecodeString(c.BytesB64)
		if err != nil {
			t.Fatalf("case %d: bad base64: %v", i, err)
		}
		got := bpe.Decode(toUint32(c.Tokens))
		if got != c.Text {
			t.Errorf("byte-decode case %d %q:\n got  %q (% x)\n want %q",
				i, raw, got, []byte(got), c.Text)
		}
		// The same bytes must survive a bytes-level round trip through the
		// encoder, which is the property Decode's lossy UTF-8 step cannot give.
		enc, err := bpe.EncodeOrdinary(string(raw))
		if err != nil {
			t.Fatalf("case %d: EncodeOrdinary: %v", i, err)
		}
		if !bytes.Equal(bpe.DecodeBytes(enc), raw) {
			t.Errorf("byte-decode case %d: DecodeBytes(EncodeOrdinary(%q)) = %q, want the input",
				i, raw, bpe.DecodeBytes(enc))
		}
	}
	t.Logf("byte-decode compared: %d byte strings", len(doc.ByteDecodeCases))
}

// TestBPEDifferentialCharClasses checks \s, \p{L} and \p{N} per code point.
//
// The sweeps below cover the same ground through real encoding; this test is
// what turns a sweep hash mismatch into a named code point.
func TestBPEDifferentialCharClasses(t *testing.T) {
	doc := loadBPEDoc(t)
	if len(doc.ClassCases) == 0 {
		t.Fatal("no reference class cases")
	}
	for _, c := range doc.ClassCases {
		space, letter, number := bpe.Classify(rune(c.CP))
		if space != c.Space || letter != c.L || number != c.N {
			t.Errorf("U+%04X: port(space=%v L=%v N=%v) reference(space=%v L=%v N=%v)",
				c.CP, space, letter, number, c.Space, c.L, c.N)
		}
	}
	t.Logf("character classes compared: %d code points", len(doc.ClassCases))
}

// ---------------------------------------------------------------------------
// Whole-code-point sweeps
// ---------------------------------------------------------------------------

// TestBPEDifferentialSweeps compares a sha256 over the token IDs of four
// strings that between them contain EVERY Unicode scalar value.
//
// This is the test that proves the embedded \p{L}/\p{N} tables are complete,
// not merely right on a sample: a single misclassified code point anywhere in
// U+0000..U+10FFFF changes the hash. The input strings are rebuilt in Go from
// the same rule the dumper uses, and their sha256 is checked against the
// dumper's before the token hash is compared, so a corpus drift cannot be
// mistaken for an encoder drift.
func TestBPEDifferentialSweeps(t *testing.T) {
	doc := loadBPEDoc(t)
	if len(doc.Sweeps) == 0 {
		t.Fatal("no reference sweeps")
	}
	for _, s := range doc.Sweeps {
		input := sweepInput(s.Name)
		sum := sha256.Sum256([]byte(input))
		if got := hex.EncodeToString(sum[:]); got != s.InputSHA256 {
			t.Fatalf("sweep %q: input string drifted\n got  %s (%d bytes)\n want %s (%d bytes)",
				s.Name, got, len(input), s.InputSHA256, s.InputBytes)
		}
		if len(input) != s.InputBytes {
			t.Fatalf("sweep %q: input length %d bytes, reference %d", s.Name, len(input), s.InputBytes)
		}
		tokens, err := bpe.EncodeOrdinary(input)
		if err != nil {
			t.Fatalf("sweep %q: EncodeOrdinary: %v", s.Name, err)
		}
		if len(tokens) != s.NTokens {
			t.Errorf("sweep %q: %d tokens, reference %d", s.Name, len(tokens), s.NTokens)
		}
		h := sha256.New()
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(tokens)))
		h.Write(buf[:])
		var b4 [4]byte
		for _, tok := range tokens {
			binary.LittleEndian.PutUint32(b4[:], tok)
			h.Write(b4[:])
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != s.TokensSHA256 {
			t.Errorf("sweep %q: token digest differs\n got  %s (%d tokens)\n want %s (%d tokens)",
				s.Name, got, len(tokens), s.TokensSHA256, s.NTokens)
		}
		t.Logf("sweep %-16s %8d bytes -> %8d tokens, digest %s", s.Name, len(input), len(tokens), s.TokensSHA256[:16])
	}
}

// sweepInput rebuilds the dumper's sweep strings. See sweep_inputs() in
// compat/python/dump_bpe.py — the two must stay in lockstep, and the input
// sha256 check in the caller is what enforces it.
func sweepInput(name string) string {
	var sb strings.Builder
	sb.Grow(0x110000 * 3)
	for c := rune(0); c <= 0x10FFFF; c++ {
		if c >= 0xD800 && c <= 0xDFFF {
			continue
		}
		switch name {
		case "bare":
			sb.WriteRune(c)
		case "space-prefixed":
			sb.WriteByte(' ')
			sb.WriteRune(c)
		case "letter-prefixed":
			sb.WriteByte('a')
			sb.WriteRune(c)
		case "digit-suffixed":
			sb.WriteRune(c)
			sb.WriteByte('1')
		default:
			panic("unknown sweep " + name)
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// nanobot helpers
// ---------------------------------------------------------------------------

// TestBPEDifferentialTruncate compares truncate_text_to_tokens.
func TestBPEDifferentialTruncate(t *testing.T) {
	doc := loadBPEDoc(t)
	if len(doc.TruncateCases) == 0 {
		t.Fatal("no reference truncate cases")
	}
	for i, c := range doc.TruncateCases {
		got := bpe.TruncateTextToTokens(c.Text, c.MaxTokens)
		if got != c.Out {
			t.Errorf("truncate case %d (text %d chars, max_tokens=%d):\n got  %q\n want %q",
				i, len(c.Text), c.MaxTokens, got, c.Out)
		}
	}
	t.Logf("truncate_text_to_tokens compared: %d cases", len(doc.TruncateCases))
}

// TestBPEDifferentialEstimate compares the prompt-token estimate and its
// reported source for reference-shaped message lists.
//
// The "<|endoftext|>" case is the important one: tiktoken's default
// disallowed_special="all" raises, and the reference's `except Exception` then
// reports the byte heuristic. A port that silently encoded it would report
// source "tiktoken" with a different number.
func TestBPEDifferentialEstimate(t *testing.T) {
	doc := loadBPEDoc(t)
	if len(doc.EstimateCases) == 0 {
		t.Fatal("no reference estimate cases")
	}
	sawHeuristic := false
	sawTiktoken := false
	for i, c := range doc.EstimateCases {
		messages := make([]core.Message, 0, len(c.Messages))
		for _, raw := range c.Messages {
			var m core.Message
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("estimate case %d: unmarshal message %s: %v", i, raw, err)
			}
			messages = append(messages, m)
		}
		got, source := agent.EstimatePromptTokens(messages, nil)
		if got != c.Tokens || source != c.Source {
			t.Errorf("estimate case %d:\n got  (%d, %q)\n want (%d, %q)",
				i, got, source, c.Tokens, c.Source)
		}
		switch source {
		case "heuristic":
			sawHeuristic = true
		case "tiktoken":
			sawTiktoken = true
		}
		if len(c.MessageTokens) != len(messages) {
			t.Fatalf("estimate case %d: %d message_tokens for %d messages", i, len(c.MessageTokens), len(messages))
		}
		for j, want := range c.MessageTokens {
			if got := agent.EstimateMessageTokens(messages[j]); got != want {
				t.Errorf("estimate case %d message %d: EstimateMessageTokens = %d, want %d", i, j, got, want)
			}
		}
	}
	if !sawHeuristic || !sawTiktoken {
		t.Fatalf("estimate corpus exercised only one source (heuristic=%v tiktoken=%v) — "+
			"the fallback or the tiktoken path is untested", sawHeuristic, sawTiktoken)
	}
	t.Logf("prompt/message estimates compared: %d cases (both sources exercised)", len(doc.EstimateCases))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func equalIDs(got []uint32, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if int64(got[i]) != want[i] {
			return false
		}
	}
	return true
}

func countIDs(cases []bpeCase) int {
	n := 0
	for _, c := range cases {
		n += len(c.Ordinary)
	}
	return n
}

// describePieces renders the port's pre-tokenizer split, so a failing case
// points at the branch that is wrong rather than only at the token list.
func describePieces(text string) string {
	var sb strings.Builder
	for _, p := range bpe.ScanPieces(text) {
		fmt.Fprintf(&sb, "%q ", text[p.Start:p.End])
	}
	return sb.String()
}
