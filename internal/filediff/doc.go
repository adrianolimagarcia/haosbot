// Package filediff ports the file-edit diff machinery of the Python reference:
// nanobot/utils/file_edit_events.py at upstream 1bb712d3.
//
// Ported symbols:
//
//	IndelOpcodes              Indel.opcodes            (rapidfuzz, via file_edit_events.py:62)
//	FileDiff                  FileDiff                 (file_edit_events.py:49-132)
//	FileDiffFromText          FileDiff.from_text       (file_edit_events.py:58-69)
//	FileDiff.Matches          FileDiff.matches         (file_edit_events.py:71-76)
//	FileDiff.Groups           FileDiff._groups         (file_edit_events.py:78-106)
//	FileDiff.UnifiedLines     FileDiff.unified_lines   (file_edit_events.py:108-132)
//	LineDiffStats             line_diff_stats          (file_edit_events.py:182-187)
//	BuildUnifiedDiffPayload   build_unified_diff_payload (file_edit_events.py:190-222)
//	UnifiedDiffOptions        its keyword arguments    (file_edit_events.py:193-199)
//	IsFileEditTool            is_file_edit_tool        (file_edit_events.py:150-151)
//	TrackedFileEditTools      TRACKED_FILE_EDIT_TOOLS  (file_edit_events.py:13)
//	DisplayFileEditPath       display_file_edit_path   (file_edit_events.py:154-160)
//
// The module-private helpers _limit_unified_diff_lines,
// _limit_unified_diff_line_chars and _rewrite_hunk_header_for_body
// (file_edit_events.py:225-333) are ported as limitUnifiedDiffLines,
// limitUnifiedDiffLineChars and rewriteHunkHeaderForBody; they are only
// reachable through BuildUnifiedDiffPayload, exactly as upstream.
//
// # What is deliberately NOT ported here
//
// FileSnapshot, build_file_edit_events and the file_edit_activity hook
// (file_edit_events.py:20-47 and :340-566, agent/hooks/file_edit_activity.py)
// are not ported: they are the producer half of the WebUI file-edit progress
// stream, and they need the hook infrastructure and the per-turn snapshot
// bookkeeping that this port does not have. internal/events already carries the
// consumer half (FileEditEvent, ProgressEvent.FileEditEvents) and
// internal/channels already forwards it.
//
// A consequence worth stating plainly: LineDiffStats, BuildUnifiedDiffPayload,
// IsFileEditTool and TrackedFileEditTools currently have NO caller in this port.
// They are ported now because they are pure, self-contained and cheap to verify
// against the reference, and because they are the building blocks the missing
// producer half needs. They are covered by
// compat/apply_patch_differential_test.go rather than by a live call path.
//
// # Why this is its own package
//
// The reference keeps this code in nanobot/utils/, which both the tools
// (agent/tools/apply_patch.py, agent/tools/filesystem.py) and the agent
// runner's file-edit tracker import. Mirroring that placement here would mean
// putting it in a package that internal/tools/builtin imports; putting it in
// internal/tools itself would make the tools package depend on a diff
// algorithm it does not own, and putting it in internal/events would invert the
// dependency (the event layer consumes diffs, it does not produce them).
//
// internal/filediff therefore depends on nothing but internal/textutil, and is
// importable by internal/tools (for tools.Result.FileDiffs),
// internal/tools/builtin (for the edit summaries) and any future file-edit
// tracker without a cycle.
//
// # Indel.opcodes is not difflib
//
// FileDiff.from_text does NOT use difflib.SequenceMatcher. It calls
// rapidfuzz.distance.Indel.opcodes, which is a bit-parallel LCS alignment
// (Hyyrö, "A Note on Bit-Parallel Alignment Computation", Stringology 2004).
// Two consequences are load-bearing:
//
//   - Indel emits only "equal", "insert" and "delete". There is no "replace"
//     tag, so a changed line is an insert followed by a delete, and the
//     added/deleted counts differ from what difflib would report.
//   - The alignment is an optimal indel-distance (LCS) alignment, not
//     difflib's heuristic matching-block algorithm, and its tie-breaking is
//     specified by the bit-parallel recovery walk rather than by an LCS DP.
//
// internal/gitstore/pydifflib.go is therefore NOT usable here; see indel.go for
// the port and its evidence.
//
// # Resource characteristic
//
// IndelOpcodes retains one bit vector per line of the "after" text, each
// ceil(len(before)/64) words wide, because the recovery walk needs the whole
// matrix. Peak memory is O(linesAfter * linesBefore / 8) bytes, which is the
// same order as the reference (rapidfuzz's C++ lcs_matrix stores the identical
// matrix, and its pure-Python fallback stores one arbitrary-precision int per
// row). No cap is imposed here because the reference imposes none, and a cap
// would change the observable (+N/-M) summary text. Callers that need a bound
// must apply it before calling, as internal/tools/builtin does for edit_file.
package filediff
