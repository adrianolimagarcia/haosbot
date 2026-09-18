package prompt

import "strings"

// portGuidanceSection is the only content this port adds to the system prompt
// that the reference does not produce. BuildSystemPrompt appends it LAST, so
// NormalizePortGuidance can discount it as a suffix and leave every other byte
// of the prompt comparable with the frozen reference.
//
// WHY IT EXISTS - both clauses below are measured, not assumed.
//
// Latency. On a recorded live session (webui:17dc0f08c770d2d09633a2f2abd371e9
// on dedirock) one investigation took 94.2 s and issued 16 sequential messages
// carrying one exec call each. Fitting every turn of that session gives
// duration ~= 3.9 s + 5.46 s x (number of tool calls): the cost is the NUMBER
// of model round trips, not the cost of the commands. The same work expressed
// as a few batched commands costs a few round trips. Parallel tool execution
// cannot rescue this case even though it is enabled by default
// (internal/agent/loop.go sets ConcurrentTools), because exec declares itself
// Exclusive (internal/tools/builtin/exec.go) and therefore always runs as a
// singleton batch.
//
// Measurement. Asked how long that turn had taken, the agent answered 25 to 35
// seconds - a fabricated figure. It had searched journalctl for the unit, which
// holds only systemd start/stop lines, and never consulted the gateway timing
// counters that do exist: /metrics reports turn_total_avg_ms,
// provider_total_avg_ms and tools_avg_ms. On the deployment measured here the
// systemd unit redirects stdout to logs/gateway.log under the data directory
// (StandardOutput=append), so the journal held only start/stop lines. The real
// figure was 94.2 s.
//
// FIDELITY. The reference has no equivalent section, so this is a deliberate,
// documented divergence of the same kind as brandingRewrites and the Go
// toolchain line in runtimeDescription. NormalizePortGuidance removes exactly
// this section, so compat/skills_differential_test.go keeps comparing every
// OTHER byte of the prompt against the reference, and
// TestPortGuidanceIsPresentInSystemPrompt fails if the section ever silently
// disappears.
const portGuidanceSection = `# Execution Efficiency

- Wall-clock latency is dominated by the number of model round trips, not by how
  long any single command takes. Every extra tool call costs one more full round
  trip, so fewer and larger calls answer sooner.
- Combine independent shell steps into ONE exec call (chain them with && or ;, or
  run a single script) instead of issuing one call per step. Prefer one batched
  command over many small exploratory ones. For example, instead of three round
  trips (exec ls, then exec cat config.json, then exec df -h), issue ONE exec
  running: ls; cat config.json; df -h
- When independent tool calls are genuinely separate, emit them together in the
  same assistant message instead of one per message.
- Do not spend exec calls on work that read_file, list_dir, grep, or find_files
  already answers directly.

# Measurement

- Never state a duration, count, size, or rate you did not actually observe. If
  the question is about timing or resource use, measure it first and name the
  source you measured.
- Authoritative sources for this service: the gateway /metrics endpoint (turn,
  provider, and tool timing averages) and the gateway log. Locate where that log
  is actually written before concluding that no timing data exists: when the
  service runs under systemd with its output redirected to a file, journalctl
  holds only start/stop lines and the per-request timing is in that file.`

// PortGuidance returns the port-specific guidance section verbatim.
//
// It is exported so tests can assert that the section is still present in the
// built prompt and that the differential discount removes exactly it.
func PortGuidance() string { return portGuidanceSection }

// NormalizePortGuidance removes the port-specific guidance section, together
// with the section separator that introduced it, so the remainder can be
// compared against the reference prompt byte for byte.
//
// The section is always the final part of a prompt built by BuildSystemPrompt,
// so the removal is a suffix operation. A prompt that does not carry the section
// is returned unchanged, and a prompt that carries it without the separator is
// still cleaned, so callers cannot accidentally compare a prompt that still
// contains port-only text.
func NormalizePortGuidance(text string) string {
	if portGuidanceSection == "" {
		return text
	}
	if idx := strings.Index(text, sectionSeparator+portGuidanceSection); idx >= 0 {
		return text[:idx] + text[idx+len(sectionSeparator)+len(portGuidanceSection):]
	}
	return strings.ReplaceAll(text, portGuidanceSection, "")
}
