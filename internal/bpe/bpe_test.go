package bpe

import (
	"math"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundtrip(t *testing.T) {
	texts := []string{
		"Hello, world!",
		"Go 1.25 runtime and BPE tokenization",
		"Special token: <|im_end|>", // Note: handled or rejected?
		"你好，世界！这是一个测试。",
		"Line 1\nLine 2\r\nLine 3\tTabbed",
		"",
	}

	for _, text := range texts {
		tokens, err := Encode(text)
		if err != nil {
			// Special token like <|im_end|> might return error or encode
			continue
		}
		decoded := Decode(tokens)
		if decoded != text {
			t.Errorf("Decode(Encode(%q)) = %q, want %q", text, decoded, text)
		}
	}
}

func TestEncodeOrdinary(t *testing.T) {
	text := "Hello world <|im_start|> not a real token <|im_end|>"
	tokens, err := EncodeOrdinary(text)
	if err != nil {
		t.Fatalf("EncodeOrdinary error: %v", err)
	}
	if len(tokens) == 0 {
		t.Fatalf("EncodeOrdinary returned 0 tokens")
	}
	decoded := Decode(tokens)
	if decoded != text {
		t.Errorf("Decode(EncodeOrdinary(%q)) = %q, want %q", text, decoded, text)
	}
}

func TestTokenCount(t *testing.T) {
	text := "The quick brown fox jumps over the lazy dog."
	count, err := TokenCount(text)
	if err != nil {
		t.Fatalf("TokenCount error: %v", err)
	}
	tokens, err := Encode(text)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	if count != len(tokens) {
		t.Errorf("TokenCount(%q) = %d, len(Encode()) = %d", text, count, len(tokens))
	}
}

func TestTruncateTextToTokens(t *testing.T) {
	text := "Alpha beta gamma delta epsilon zeta eta theta iota kappa"
	maxTokens := 4
	truncated := TruncateTextToTokens(text, maxTokens)

	tokens, err := Encode(truncated)
	if err != nil {
		t.Fatalf("Encode(truncated) error: %v", err)
	}
	if len(tokens) > maxTokens {
		t.Errorf("TruncateTextToTokens len = %d, want <= %d", len(tokens), maxTokens)
	}
	if !strings.HasPrefix(text, truncated) {
		t.Errorf("TruncateTextToTokens output %q is not prefix of %q", truncated, text)
	}

	// Empty text or zero/negative maxTokens returns text untouched
	if TruncateTextToTokens("", 10) != "" {
		t.Errorf("TruncateTextToTokens empty text failed")
	}
	if TruncateTextToTokens("text", 0) != "text" {
		t.Errorf("TruncateTextToTokens zero maxTokens failed")
	}
	if TruncateTextToTokens("text", -1) != "text" {
		t.Errorf("TruncateTextToTokens negative maxTokens failed")
	}
	// Very large maxTokens
	if TruncateTextToTokens(text, 1000) != text {
		t.Errorf("TruncateTextToTokens large maxTokens modified text")
	}
}

func TestAvailableAndNumRanks(t *testing.T) {
	if !Available() {
		t.Fatalf("BPE Available() = false, expected true")
	}
	ranks, err := NumRanks()
	if err != nil {
		t.Fatalf("NumRanks error: %v", err)
	}
	if ranks <= 100000 {
		t.Errorf("NumRanks = %d, expected > 100,000 for cl100k_base", ranks)
	}
}

func TestSpecialTokenError(t *testing.T) {
	err := &SpecialTokenError{Token: "<|im_end|>"}
	if !strings.Contains(err.Error(), "<|im_end|>") {
		t.Errorf("SpecialTokenError message missing token: %q", err.Error())
	}
}

func TestTruncateEdgeCases(t *testing.T) {
	// Unicode multibyte character preservation across boundaries
	chinese := "你好世界测试字符串"
	trunc := TruncateTextToTokens(chinese, 2)
	if len(trunc) == 0 {
		t.Errorf("TruncateTextToTokens on multibyte returned empty")
	}
	// MaxTokens negative returns original string
	if TruncateTextToTokens(chinese, -1) != chinese {
		t.Errorf("TruncateTextToTokens with negative tokens should return original string")
	}
	// Very large maxTokens > maxInt
	if TruncateTextToTokens(chinese, math.MaxInt) != chinese {
		t.Errorf("TruncateTextToTokens with MaxInt failed")
	}
}
