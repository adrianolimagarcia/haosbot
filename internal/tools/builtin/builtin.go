package builtin

import "github.com/adrianolimagarcia/nanobot-go/internal/tools"

// Config bundles the policies of the built-in tool set.
type Config struct {
	// Files is the path policy shared by read_file, write_file, edit_file and
	// list_dir.
	Files PathPolicy
	// Exec configures the exec tool.
	Exec ExecOptions
	// SSRFWhitelist is the explicit outbound-network exception list shared by
	// tools that accept model/user-controlled URLs.
	SSRFWhitelist []string

	// EnforceCapabilities makes the Enable* switches authoritative. It defaults
	// to false so package-level compatibility tests and legacy direct callers
	// keep the historical complete catalog. Production composition roots should
	// always set it to true.
	EnforceCapabilities bool
	EnableFiles         bool
	EnableExec          bool
	EnableNetwork       bool
}

func capabilityEnabled(enforce, enabled bool) bool { return !enforce || enabled }

// Register adds the core built-in tools to r in a deterministic order.
func Register(r *tools.Registry, cfg Config) {
	if capabilityEnabled(cfg.EnforceCapabilities, cfg.EnableFiles) {
		r.Register(NewApplyPatch(cfg.Files))
		r.Register(NewReadFile(cfg.Files))
		r.Register(NewWriteFile(cfg.Files))
		r.Register(NewEditFile(cfg.Files))
		r.Register(NewListDir(cfg.Files))
	}
	if capabilityEnabled(cfg.EnforceCapabilities, cfg.EnableExec) {
		r.Register(NewExec(cfg.Exec))
		// python_exec is also arbitrary process/code execution and follows the
		// same exec capability boundary rather than bypassing tools.exec.enable.
		r.Register(NewPythonExec(cfg.Files.Workspace))
	}
	if capabilityEnabled(cfg.EnforceCapabilities, cfg.EnableNetwork) {
		r.Register(NewA2ACall(cfg.SSRFWhitelist))
	}
}

// Tools returns the core built-in tools in the same order as Register.
func Tools(cfg Config) []tools.Tool {
	out := make([]tools.Tool, 0, 8)
	if capabilityEnabled(cfg.EnforceCapabilities, cfg.EnableFiles) {
		out = append(out,
			NewApplyPatch(cfg.Files),
			NewReadFile(cfg.Files),
			NewWriteFile(cfg.Files),
			NewEditFile(cfg.Files),
			NewListDir(cfg.Files),
		)
	}
	if capabilityEnabled(cfg.EnforceCapabilities, cfg.EnableExec) {
		out = append(out, NewExec(cfg.Exec), NewPythonExec(cfg.Files.Workspace))
	}
	if capabilityEnabled(cfg.EnforceCapabilities, cfg.EnableNetwork) {
		out = append(out, NewA2ACall(cfg.SSRFWhitelist))
	}
	return out
}
