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
}

// Register adds the core built-in tools to r in a deterministic order.
//
// apply_patch is registered first because that is where it falls in the
// reference's advertisement order: the reference registry sorts built-ins by
// name before advertising them (nanobot/agent/tools/registry.py:86-108), so
// apply_patch sorts ahead of edit_file, exec, list_dir, read_file and
// write_file. The remaining five keep the order this port already used.
// This repository's Registry advertises in registration order
// (internal/tools/tool.go:174-191), so the caller controls the final ordering.
func Register(r *tools.Registry, cfg Config) {
	r.Register(NewApplyPatch(cfg.Files))
	r.Register(NewReadFile(cfg.Files))
	r.Register(NewWriteFile(cfg.Files))
	r.Register(NewEditFile(cfg.Files))
	r.Register(NewListDir(cfg.Files))
	r.Register(NewExec(cfg.Exec))
	r.Register(NewPythonExec(cfg.Files.Workspace))
	r.Register(NewA2ACall(cfg.SSRFWhitelist))
}

// Tools returns the core built-in tools in the same order as Register.
func Tools(cfg Config) []tools.Tool {
	return []tools.Tool{
		NewApplyPatch(cfg.Files),
		NewReadFile(cfg.Files),
		NewWriteFile(cfg.Files),
		NewEditFile(cfg.Files),
		NewListDir(cfg.Files),
		NewExec(cfg.Exec),
		NewPythonExec(cfg.Files.Workspace),
		NewA2ACall(cfg.SSRFWhitelist),
	}
}
