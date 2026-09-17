package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractXMLLikeTextToolCall(t *testing.T) {
	input := "before\n" +
		"<tool_call>\n" +
		"<function=exec>\n" +
		"<parameter=command>\n" +
		"printf '&lt;ok&gt;'\n" +
		"</parameter>\n" +
		"</function>\n" +
		"</tool_call>\n" +
		"after"

	visible, calls := extractTextToolCalls(input)
	if visible != "before\n\nafter" {
		t.Fatalf("visible content = %q, want %q", visible, "before\n\nafter")
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one call", calls)
	}
	if calls[0].Name != "exec" {
		t.Fatalf("tool name = %q, want exec", calls[0].Name)
	}
	if len(calls[0].ID) != 9 {
		t.Fatalf("generated id = %q, want nine characters", calls[0].ID)
	}

	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments are not JSON: %v (%s)", err, calls[0].Arguments)
	}
	if got := args["command"]; got != "printf '<ok>'" {
		t.Errorf("command = %q, want decoded command", got)
	}
}

func TestExtractXMLLikeTextToolCallsPreservesOrder(t *testing.T) {
	input := "<tool_call><function=read_file><parameter=path>a.txt</parameter></function></tool_call>\n" +
		"middle\n" +
		"<tool_call><function=exec><parameter=command>echo done</parameter></function></tool_call>"

	visible, calls := extractTextToolCalls(input)
	if visible != "middle" {
		t.Fatalf("visible content = %q, want middle", visible)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want two calls", calls)
	}
	if calls[0].Name != "read_file" || calls[1].Name != "exec" {
		t.Fatalf("call order = %q, %q", calls[0].Name, calls[1].Name)
	}
}

func TestExtractXMLLikeTextToolCallRejectsMalformedBlock(t *testing.T) {
	input := "keep <tool_call><function=exec><parameter=command>echo hi</function></tool_call>"
	visible, calls := extractTextToolCalls(input)
	if visible != input {
		t.Fatalf("malformed block was removed: %q", visible)
	}
	if len(calls) != 0 {
		t.Fatalf("malformed block produced calls: %+v", calls)
	}
}

func TestParseResponseLiftsXMLLikeToolCallFromBareContent(t *testing.T) {
	payload := `{"content":"<tool_call><function=exec><parameter=command>echo hi</parameter></function></tool_call>"}`

	response, err := parseResponse([]byte(payload))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if response.Content != "" || response.HasContent {
		t.Fatalf("visible content = %q, has_content=%v", response.Content, response.HasContent)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "exec" {
		t.Fatalf("tool calls = %+v", response.ToolCalls)
	}
	if string(response.ToolCalls[0].Arguments) != `{"command":"echo hi"}` {
		t.Fatalf("arguments = %s", response.ToolCalls[0].Arguments)
	}
	if !response.ShouldExecuteTools() {
		t.Fatalf("response should execute tools: %+v", response)
	}
}

func TestExtractXMLLikeTextToolCallRejectsDuplicateParameters(t *testing.T) {
	input := "<tool_call><function=exec>" +
		"<parameter=command>echo one</parameter>" +
		"<parameter=command>echo two</parameter>" +
		"</function></tool_call>"

	visible, calls := extractTextToolCalls(input)
	if visible != strings.TrimSpace(input) {
		t.Fatalf("duplicate-parameter block was removed: %q", visible)
	}
	if len(calls) != 0 {
		t.Fatalf("duplicate-parameter block produced calls: %+v", calls)
	}
}

func TestTextToolCallParserCapabilityModes(t *testing.T) {
	jsonBlock := `before<tool_call>{"name":"exec","arguments":{"command":"json"}}</tool_call>after`
	xmlBlock := `before<tool_call><function=exec><parameter=command>xml</parameter></function></tool_call>after`

	if _, calls := extractTextToolCallsWithFormat(jsonBlock, ToolCallFormatNative); len(calls) != 0 {
		t.Fatal("native mode must not heuristically parse text")
	}
	if _, calls := extractTextToolCallsWithFormat(jsonBlock, ToolCallFormatXML); len(calls) != 0 {
		t.Fatal("XML mode must not parse JSON text calls")
	}
	if visible, calls := extractTextToolCallsWithFormat(jsonBlock, ToolCallFormatJSON); len(calls) != 1 || visible != "beforeafter" {
		t.Fatalf("JSON mode: visible=%q calls=%d", visible, len(calls))
	}
	if _, calls := extractTextToolCallsWithFormat(xmlBlock, ToolCallFormatJSON); len(calls) != 0 {
		t.Fatal("JSON mode must not parse XML text calls")
	}
	if visible, calls := extractTextToolCallsWithFormat(xmlBlock, ToolCallFormatXML); len(calls) != 1 || visible != "beforeafter" {
		t.Fatalf("XML mode: visible=%q calls=%d", visible, len(calls))
	}
}
