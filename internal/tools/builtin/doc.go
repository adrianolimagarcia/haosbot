// Package builtin implements the core built-in agent tools.
//
// Ported from the reference tool implementations at upstream 1bb712d3:
//
//	apply_patch nanobot/agent/tools/apply_patch.py:20-254  (ApplyPatchTool)
//	read_file   nanobot/agent/tools/filesystem.py:251-415  (ReadFileTool)
//	write_file  nanobot/agent/tools/filesystem.py:533-571  (WriteFileTool)
//	edit_file   nanobot/agent/tools/filesystem.py:831-1040 (EditFileTool)
//	list_dir    nanobot/agent/tools/filesystem.py:1087-1169 (ListDirTool)
//	exec        nanobot/agent/tools/shell.py:118-356       (ExecTool)
//
// Parameter schemas are produced with the same shapes as
// tool_parameters_schema (nanobot/agent/tools/schema.py:217-235): a strict
// object root (additionalProperties=false) plus an optional required list.
// apply_patch's inner edit object is the one exception: the reference builds it
// from a nested ObjectSchema that leaves additional_properties unset, so it is
// deliberately non-strict there.
//
// # SCOPE NOTE — deliberate divergences from the reference
//
// These are listed explicitly so their absence is not mistaken for parity.
// Each is also flagged in the file that owns it.
//
//   - No multimodal results. tools.Result carries a string only, so the
//     reference's image content blocks, PDF extraction and Office document
//     extraction (filesystem.py:330-344, :417-525) are not implemented.
//     Reading such a file returns an error result instead.
//   - No file-state cache. FileStates (filesystem.py:347-350, :408-410) is not
//     ported, so read_file never returns "[File unchanged since last read: ...]"
//     and the force parameter is accepted and ignored. apply_patch therefore
//     omits the record_write bookkeeping at apply_patch.py:246-247, which only
//     feeds that cache.
//   - edit_file matches exactly. The progressively looser matchers
//     (filesystem.py:776-787: trim, quote-normalized, quote-normalized trim) are
//     NOT ported; a near-miss is reported as a not-found error. The
//     quote-style/re-indent fixups (filesystem.py:618-668, :1016-1017) are
//     identity transforms under exact matching and are therefore omitted.
//     Unlike the reference, an ambiguous old_text is an error result rather
//     than a plain "Warning: ..." string (see edit_file.go).
//   - No diff statistics. The "+N/-M" suffix of the edit summary comes from
//     FileDiff (utils/file_edit_events.py:50-65), which is backed by rapidfuzz;
//     this port emits "Patch applied:\n- <action> <path>" without the counts.
//   - apply_patch does not type-check its arguments the way a Python caller
//     would. Where the reference lets a wrong-typed value reach a str method and
//     reports the resulting AttributeError as text, this port reproduces that
//     text (see pyvalue.go) rather than raising a Go type error.
//   - OS error text is Go's, not Python's. Where the reference surfaces an errno
//     string (for example a rollback's FileExistsError), this port surfaces the
//     corresponding Go error. This is the same choice edit_file already made in
//     permissionOrGenericEdit.
//   - No image/PDF/Office/diff MIME plumbing, no "Did you mean" sibling
//     suggestions for a missing edit target (filesystem.py:1042-1053).
//   - exec: the background exec-session mode (yield_time_ms,
//     agent/tools/exec_session.py), the sandbox wrapper (agent/tools/sandbox.py),
//     the internal-URL guard (security/network.py), the allow-pattern exemption
//     and the absolute-path scan of _guard_command (shell.py:864-907) are not
//     ported. The ported guard is working_dir containment, the ".." traversal
//     check and the deny-pattern filter. Non-zero exits are reported with
//     IsError=true (the reference reports them as ordinary content).
//   - exec child environment: PATH is forwarded in addition to the reference's
//     HOME/LANG/TERM/PYTHONUNBUFFERED (shell.py:758-805), because this port has
//     no path_prepend/login-profile mechanism to compensate. Secrets are still
//     not forwarded.
//
// # File diffs
//
// The "(+N/-M)" suffix of the apply_patch and edit_file summaries comes from
// FileDiff (utils/file_edit_events.py), which is backed by rapidfuzz's Indel
// alignment. That machinery is ported in internal/filediff, and the resulting
// diffs are carried on tools.Result.FileDiffs and core.ToolResult.FileDiffs so
// that a caller can render them without recomputing the alignment.
//
// # Resource caps (this port, not in the reference)
//
// The reference reads whole files and buffers whole command output before
// applying its character limits. To keep a 10 GiB file or a runaway command
// from exhausting memory, this port adds internal byte caps that are applied
// while reading:
//
//	read_file  readMaxBytes   = 100 MiB (same bound as the reference's
//	                            _MAX_FILE_SIZE_BYTES, filesystem.py:275)
//	edit_file  maxEditFileSize = 64 MiB (reference allows 1 GiB,
//	                            filesystem.py:864; lowered because the whole
//	                            file must be held in memory for replacement)
//	exec       maxCaptureBytes = 1 MiB per stream (reference buffers without
//	                            limit and truncates afterwards)
//
// apply_patch imposes no size cap, matching the reference: it reads each target
// whole in order to compute the diff. The alignment itself costs
// O(lines_after x lines_before / 8) bytes for its bit matrix, which is the
// reference's own characteristic (see internal/filediff/doc.go).
//
// All caps are documented at their declaration site and reported to the model
// in the result rather than failing silently.
package builtin
