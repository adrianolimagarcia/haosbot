package bpe

import (
	"strings"
	"testing"
)

func benchText(kind string, n int) string {
	var s string
	switch kind {
	case "prose":
		s = "The quick brown fox jumps over the lazy dog. It was the best of times, it was the worst of times. "
	case "code":
		s = "func main() {\n\tif err := run(); err != nil {\n\t\treturn fmt.Errorf(\"run: %w\", err)\n\t}\n}\n"
	case "cjk":
		s = "你好世界这是一个测试"
	case "json":
		s = `{"key": "value", "n": 12345, "ok": true},`
	default:
		panic("?")
	}
	return strings.Repeat(s, n/len(s)+1)[:n]
}

func BenchmarkEncode(b *testing.B) {
	for _, kind := range []string{"prose", "code", "cjk", "json"} {
		text := benchText(kind, 100000)
		b.Run(kind, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for i := 0; i < b.N; i++ {
				if _, err := EncodeOrdinary(text); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
