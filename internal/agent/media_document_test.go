package agent

import (
	"strings"
	"testing"
)

func TestExtractPDFTextBoundedPlainStream(t *testing.T) {
	pdf := []byte("%PDF-1.4\n1 0 obj<<>>stream\nBT (Hello PDF) Tj (second line) Tj ET\nendstream\nendobj")
	got := extractPDFTextBounded(pdf, 1024)
	if !strings.Contains(got, "Hello PDF") || !strings.Contains(got, "second line") { t.Fatalf("extracted %q", got) }
}

func TestExtractPDFTextBoundedEscapesAndLimit(t *testing.T) {
	pdf := []byte("%PDF-1.4\nstream\n(hello\\nworld) (abcdefghijklmnopqrstuvwxyz)\nendstream")
	got := extractPDFTextBounded(pdf, 12)
	if len(got) > 12 { t.Fatalf("limit exceeded: %d %q", len(got), got) }
	if !strings.Contains(got, "hello") { t.Fatalf("missing text: %q", got) }
}

func TestExtractPDFTextBoundedIgnoresNonText(t *testing.T) {
	pdf := []byte("%PDF-1.4\nstream\n<deadbeef> 12 0 R\nendstream")
	if got := extractPDFTextBounded(pdf, 1024); got != "" { t.Fatalf("unexpected extraction %q", got) }
}
