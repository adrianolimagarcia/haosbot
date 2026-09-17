package pyjson

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestFormatFloatEdgeCases(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{1.0, "1.0"},
		{-1.5, "-1.5"},
		{0.1, "0.1"},
		{100.0, "100.0"},
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1e17, "1e+17"},
		{1e20, "1e+20"},
		{1e-4, "0.0001"},
		{1e-5, "1e-05"},
		{1e-7, "1e-07"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.NaN(), "NaN"},
		{5e-324, "5e-324"},
	}

	for _, tc := range cases {
		got := FormatFloat(tc.in)
		if got != tc.want {
			t.Errorf("FormatFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFloatMarshalJSON(t *testing.T) {
	cases := []struct {
		in   Float
		want string
	}{
		{Float(1.0), "1.0"},
		{Float(0.0), "0.0"},
		{Float(math.Copysign(0, -1)), "-0.0"},
		{Float(1e16), "1e+16"},
		{Float(0.5), "0.5"},
		{Float(math.Inf(1)), "Infinity"},
	}

	for _, tc := range cases {
		gotBytes, err := tc.in.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%v) unexpected error: %v", tc.in, err)
		}
		if string(gotBytes) != tc.want {
			t.Errorf("Float(%v).MarshalJSON() = %q, want %q", tc.in, string(gotBytes), tc.want)
		}
		if tc.in.Float64() != float64(tc.in) {
			t.Errorf("Float64() = %v, want %v", tc.in.Float64(), float64(tc.in))
		}
	}

	// Verify in struct json.Marshal
	type testStruct struct {
		Val Float `json:"val"`
	}
	s := testStruct{Val: Float(1.0)}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(testStruct) error: %v", err)
	}
	want := `{"val":1.0}`
	if string(b) != want {
		t.Errorf("struct marshal = %q, want %q", string(b), want)
	}
}

func TestUnescapeLineSeparators(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no escape", "plain text", "plain text"},
		{"u2028 escape", `hello\u2028world`, "hello\u2028world"},
		{"u2029 escape", `hello\u2029world`, "hello\u2029world"},
		{"both escapes", `a\u2028b\u2029c`, "a\u2028b\u2029c"},
		{"escaped backslash before u2028", `hello\\u2028world`, `hello\\u2028world`},
		{"three backslashes", `hello\\\u2028world`, `hello\\` + "\u2028world"},
		{"four backslashes", `hello\\\\u2028world`, `hello\\\\u2028world`},
		{"near match 2027", `hello\u2027world`, `hello\u2027world`},
		{"truncated sequence", `hello\u202`, `hello\u202`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnescapeLineSeparators([]byte(tc.in))
			if !bytes.Equal(got, []byte(tc.want)) {
				t.Errorf("UnescapeLineSeparators(%q) = %q, want %q", tc.in, string(got), tc.want)
			}
		})
	}
}
