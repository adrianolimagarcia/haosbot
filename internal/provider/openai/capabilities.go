package openai

import "strings"

// ToolCallFormat selects the parser for text-encoded tool calls. Native
// provider tool_calls are always parsed independently. Explicit selection keeps
// model/provider capability policy out of response-shape heuristics.
type ToolCallFormat string

const (
	ToolCallFormatAuto   ToolCallFormat = "auto"
	ToolCallFormatNative ToolCallFormat = "native"
	ToolCallFormatJSON   ToolCallFormat = "json"
	ToolCallFormatXML    ToolCallFormat = "xml"
)

func normalizeToolCallFormat(format ToolCallFormat) ToolCallFormat {
	switch ToolCallFormat(strings.ToLower(strings.TrimSpace(string(format)))) {
	case ToolCallFormatNative:
		return ToolCallFormatNative
	case ToolCallFormatJSON:
		return ToolCallFormatJSON
	case ToolCallFormatXML:
		return ToolCallFormatXML
	default:
		return ToolCallFormatAuto
	}
}
