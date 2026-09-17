package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type PythonExecTool struct {
	workspace string
	params    json.RawMessage
}

func NewPythonExec(workspace string) *PythonExecTool {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"code": {
				"type": "string",
				"description": "Python code snippet or script to execute (supports asyncio, sqlite3, json, http, etc.)."
			},
			"timeout": {
				"type": "integer",
				"description": "Optional timeout in seconds (default 30)."
			}
		},
		"required": ["code"]
	}`)
	return &PythonExecTool{workspace: workspace, params: raw}
}

func (t *PythonExecTool) Name() string { return "python_exec" }

func (t *PythonExecTool) Description() string {
	return "Execute Python scripts or code snippets using the minimal CPython runtime (with sqlite3, asyncio, json, and socket support). Useful for data processing and automation."
}

func (t *PythonExecTool) Parameters() json.RawMessage {
	return t.params
}

func (t *PythonExecTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var params struct {
		Code    string `json:\"code\"`
		Timeout int    `json:\"timeout\"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Code == "" {
		return tools.Result{}, fmt.Errorf("code argument is required")
	}

	timeout := 30 * time.Second
	if params.Timeout > 0 && params.Timeout <= 300 {
		timeout = time.Duration(params.Timeout) * time.Second
	}

	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Locate minimal python or fallback to system python3
	pyBin := filepath.Join(os.Getenv("HOME"), ".nanobot", "python-min", "bin", "python3")
	if _, err := os.Stat(pyBin); err != nil {
		pyBin = "python3"
	}

	cmd := exec.CommandContext(subCtx, pyBin, "-c", params.Code)
	if t.workspace != "" {
		cmd.Dir = t.workspace
	}

	out, err := cmd.CombinedOutput()
	result := string(out)
	if len(result) > 20000 {
		result = result[:20000] + "\n...[output truncated]"
	}

	if err != nil {
		if subCtx.Err() == context.DeadlineExceeded {
			return tools.OK(fmt.Sprintf("Python execution timed out after %v\n%s", timeout, result)), nil
		}
		return tools.OK(fmt.Sprintf("Python execution failed:\n%s\nOutput:\n%s", err, result)), nil
	}

	if result == "" {
		return tools.OK("Python script executed successfully with no output."), nil
	}
	return tools.OK(result), nil
}
