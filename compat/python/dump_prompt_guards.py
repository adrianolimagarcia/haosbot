#!/usr/bin/env python3
"""Dump reference values for the ContextBuilder's bundled-template guards.

This is a third, independent dumper alongside dump_reference.py. It exists
because dump_reference.py is a shared file that several agents edit
concurrently, and a read-modify-write race there would destroy work. Keeping
this section in its own file and its own Go test file removes the race
entirely.

What it covers
--------------
The reference guards three prompt inclusions with _is_template_content
(nanobot/agent/context.py:224), which compares a workspace file against the
BUNDLED template and treats "unchanged from the shipped default" as "the user
has not customised this":

  * context.py:128-130  the "# Memory" section
  * context.py:208-212  SOUL.md is swapped for legacy/SOUL.md's successor
  * context.py:213-217  unmodified AGENTS.md / USER.md are skipped

On a fresh install every workspace file is byte-identical to its bundled
template (sync_workspace_templates copies them), so all three guards fire and
the reference deliberately withholds three sections the naive port emits.

Usage
-----
    .tools/venv/bin/python compat/python/dump_prompt_guards.py [base_dir]

base_dir defaults to a PID-suffixed scratch directory under .tools/. It is
created if absent; one workspace per case is created inside it. The caller owns
cleanup.

Emits exactly one JSON document on stdout.

Covers HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9.
"""
from __future__ import annotations

import hashlib
import json
import os
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "upstream" / "nanobot"))

# loguru writes diagnostics to stderr; this harness must emit exactly one JSON
# document on stdout so the Go side can parse it. sync_workspace_templates
# initialises a GitStore inside a try/except that logs on failure, so the sink
# is removed rather than merely quieted.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:  # pragma: no cover - loguru is a hard dependency upstream
    pass

from nanobot.agent.context import ContextBuilder  # noqa: E402
from nanobot.utils.helpers import (  # noqa: E402
    load_bundled_template,
    sync_workspace_templates,
)

# The five templates the guards actually consult, plus the two that
# sync_workspace_templates copies but the prompt builder never reads. All seven
# are digested so the Go side can prove its embed matches byte for byte.
TEMPLATE_NAMES = [
    "AGENTS.md",
    "SOUL.md",
    "USER.md",
    "legacy/SOUL.md",
    "memory/MEMORY.md",
    "HEARTBEAT.md",
    "prompts/README.md",
]

# Sections whose presence the Go test must agree on. "## Format Hint" is
# included as a control: it is NOT guarded by _is_template_content, so it must
# agree in every case. Without it, a harness that accidentally differed in its
# arguments to the two sides would look like a clean pass.
SECTION_MARKERS = ["## AGENTS.md", "## USER.md", "## SOUL.md", "# Memory", "## Format Hint"]

BOOTSTRAP_FILES = ["AGENTS.md", "SOUL.md", "USER.md"]


# --------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------


def _sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def _template(name: str) -> str:
    tpl = load_bundled_template(name)
    if tpl is None:
        raise SystemExit(f"reference template {name!r} is not bundled")
    return tpl


def _sections(prompt: str) -> dict:
    return {marker: (marker in prompt) for marker in SECTION_MARKERS}


# --------------------------------------------------------------------------
# cases
# --------------------------------------------------------------------------
#
# Each case is (name, channel, include_memory, build) where build(base) returns
# (agent_workspace, project_workspace). The workspace is ALWAYS created by the
# reference's own sync_workspace_templates first, so "fresh install" means
# exactly what a real first run produces — including HEARTBEAT.md,
# prompts/README.md, an empty memory/history.jsonl and a .git store. Hand-built
# fixtures would not catch a divergence that depends on a file the sync creates.


def _synced(base: Path, name: str = "agent") -> Path:
    ws = (base / name).resolve()
    if ws.exists():
        shutil.rmtree(ws)
    ws.mkdir(parents=True)
    sync_workspace_templates(ws, silent=True)
    return ws


def _case_fresh_install(base: Path):
    ws = _synced(base)
    return ws, ws


def _case_memory_customised(base: Path):
    ws = _synced(base)
    _write(ws / "memory" / "MEMORY.md", _template("memory/MEMORY.md") + "\n- Likes tea.\n")
    return ws, ws


def _case_agents_customised(base: Path):
    ws = _synced(base)
    _write(ws / "AGENTS.md", "# Agent Instructions\n\n- Custom project rule.\n")
    return ws, ws


def _case_user_customised(base: Path):
    ws = _synced(base)
    _write(ws / "USER.md", "User prefers Chinese.\n")
    return ws, ws


def _case_soul_customised(base: Path):
    ws = _synced(base)
    _write(ws / "SOUL.md", "# Soul\n\nCustom soul body.\n")
    return ws, ws


def _case_all_customised(base: Path):
    ws = _synced(base)
    _write(ws / "AGENTS.md", "# Agent Instructions\n\n- Custom project rule.\n")
    _write(ws / "USER.md", "User prefers Chinese.\n")
    _write(ws / "SOUL.md", "# Soul\n\nCustom soul body.\n")
    _write(ws / "memory" / "MEMORY.md", "# Long-term Memory\n\n- Likes tea.\n")
    return ws, ws


def _case_whitespace_only(base: Path):
    # Every bootstrap file and the memory file hold only ASCII whitespace.
    # AGENTS.md/USER.md/SOUL.md are blank and skipped; MEMORY.md is still
    # INJECTED, because context.py:129 tests `if memory` on the RAW string.
    ws = _synced(base)
    _write(ws / "AGENTS.md", "   \n\t\n")
    _write(ws / "USER.md", "\t \n")
    _write(ws / "SOUL.md", "  \n")
    _write(ws / "memory" / "MEMORY.md", " \n ")
    return ws, ws


def _case_unit_separator_only(base: Path):
    # U+001C..U+001F are whitespace to Python's str.strip() (bidi class S) and
    # NOT to Go's unicode.IsSpace. This is the exact boundary where a port that
    # used strings.TrimSpace diverges: Python treats all four files as blank.
    ws = _synced(base)
    for name in BOOTSTRAP_FILES:
        _write(ws / name, "\x1c")
    _write(ws / "memory" / "MEMORY.md", "\x1c")
    return ws, ws


def _case_template_plus_padding(base: Path):
    # Both sides of _is_template_content are .strip()ed, so padding must NOT
    # defeat the guard — and a padded LEGACY SOUL.md must still be upgraded.
    ws = _synced(base)
    _write(ws / "AGENTS.md", "\n\n  " + _template("AGENTS.md") + " \n")
    _write(ws / "USER.md", _template("USER.md") + "\n")
    _write(ws / "memory" / "MEMORY.md", "\n" + _template("memory/MEMORY.md") + "\n\n")
    _write(ws / "SOUL.md", "\t" + _template("legacy/SOUL.md") + "\n")
    return ws, ws


def _case_legacy_soul_exact(base: Path):
    ws = _synced(base)
    _write(ws / "SOUL.md", _template("legacy/SOUL.md"))
    return ws, ws


def _case_missing_files(base: Path):
    ws = _synced(base)
    for name in BOOTSTRAP_FILES:
        (ws / name).unlink()
    (ws / "memory" / "MEMORY.md").unlink()
    return ws, ws


def _case_include_memory_false(base: Path):
    ws = _synced(base)
    _write(ws / "memory" / "MEMORY.md", "# Long-term Memory\n\n- Likes tea.\n")
    return ws, ws


def _case_project_workspace_differs(base: Path):
    # AGENTS.md comes from the PROJECT root while SOUL.md/USER.md come from the
    # AGENT workspace. Passing the wrong root to either side is a classic way to
    # fake agreement, so this case makes the two roots distinguishable.
    ws = _synced(base)
    project = (base / "project").resolve()
    if project.exists():
        shutil.rmtree(project)
    project.mkdir(parents=True)
    _write(project / "AGENTS.md", "# Agent Instructions\n\n- Project-only rule.\n")
    return ws, project


CASES = [
    ("fresh_install", "cli", True, _case_fresh_install),
    ("memory_customised", "cli", True, _case_memory_customised),
    ("agents_customised", "cli", True, _case_agents_customised),
    ("user_customised", "cli", True, _case_user_customised),
    ("soul_customised", "cli", True, _case_soul_customised),
    ("all_customised", "cli", True, _case_all_customised),
    ("whitespace_only", "cli", True, _case_whitespace_only),
    ("unit_separator_only", "cli", True, _case_unit_separator_only),
    ("template_plus_padding", "cli", True, _case_template_plus_padding),
    ("legacy_soul_exact", "cli", True, _case_legacy_soul_exact),
    ("missing_files", "cli", True, _case_missing_files),
    ("include_memory_false", "cli", False, _case_include_memory_false),
    ("project_workspace_differs", "telegram", True, _case_project_workspace_differs),
]


# --------------------------------------------------------------------------
# _is_template_content matrix
# --------------------------------------------------------------------------


def _is_template_matrix() -> list:
    """Direct probes of the static _is_template_content, independent of any
    workspace. These isolate the two easy-to-get-wrong details: `.strip()` on
    BOTH sides, and `tpl is None -> False`."""
    agents = _template("AGENTS.md")
    soul = _template("SOUL.md")
    legacy = _template("legacy/SOUL.md")
    memory = _template("memory/MEMORY.md")

    probes = [
        ("identical", agents, "AGENTS.md"),
        ("identical_trailing_newline", agents + "\n", "AGENTS.md"),
        ("identical_surrounded_by_space", "  " + agents + "  ", "AGENTS.md"),
        ("identical_surrounded_by_unit_separator", "\x1c" + agents + "\x1f", "AGENTS.md"),
        ("identical_surrounded_by_nbsp", "\u00a0" + agents + "\u00a0", "AGENTS.md"),
        ("customised", "# my own rules\n", "AGENTS.md"),
        ("customised_with_padding", "\n  # my own rules  \n", "AGENTS.md"),
        ("empty_vs_nonempty_template", "", "AGENTS.md"),
        ("zero_byte_prefix", "\x00" + agents, "AGENTS.md"),
        ("bom_prefix", "\ufeff" + agents, "AGENTS.md"),
        ("missing_template", agents, "nonexistent/path.md"),
        ("memory_identical", memory, "memory/MEMORY.md"),
        ("memory_customised", memory + "- extra\n", "memory/MEMORY.md"),
        ("memory_only_unit_separator", "\x1c", "memory/MEMORY.md"),
        ("soul_identical", soul, "SOUL.md"),
        ("soul_vs_legacy_template", soul, "legacy/SOUL.md"),
        ("legacy_vs_soul_template", legacy, "legacy/SOUL.md"),
        ("legacy_vs_current_soul_template", legacy, "SOUL.md"),
        ("heartbeat_identical", _template("HEARTBEAT.md"), "HEARTBEAT.md"),
        ("prompts_readme_identical", _template("prompts/README.md"), "prompts/README.md"),
    ]
    return [
        {
            "name": name,
            "content": content,
            "template": template,
            "result": bool(ContextBuilder._is_template_content(content, template)),
        }
        for name, content, template in probes
    ]


# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------


def main() -> int:
    base = Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else (
        ROOT / ".tools" / f"prompt-guards-ref-{os.getpid()}"
    )
    base.mkdir(parents=True, exist_ok=True)

    templates = {}
    for name in TEMPLATE_NAMES:
        content = load_bundled_template(name)
        templates[name] = None if content is None else _sha256_text(content)

    out_cases = []
    for name, channel, include_memory, build in CASES:
        case_dir = base / name
        case_dir.mkdir(parents=True, exist_ok=True)
        agent_ws, project_ws = build(case_dir)

        builder = ContextBuilder(agent_ws)
        prompt = builder.build_system_prompt(
            channel=channel,
            session_summary=None,
            workspace=project_ws,
            include_memory=include_memory,
        )

        out_cases.append(
            {
                "name": name,
                "channel": channel,
                "include_memory": include_memory,
                "agent_workspace": str(agent_ws),
                "project_workspace": str(project_ws),
                "sections": _sections(prompt),
                # Byte-exact bootstrap block. _load_bootstrap_files is pure
                # string concatenation with no Jinja2 involvement, so unlike the
                # full prompt it CAN be compared byte for byte.
                "bootstrap_block": builder._load_bootstrap_files(project_ws),
                "bootstrap_sha256": _sha256_text(builder._load_bootstrap_files(project_ws)),
                "memory_raw": builder.memory.read_memory(),
                "prompt_len": len(prompt),
            }
        )

    doc = {
        "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
        "python_version": sys.version.split()[0],
        "base_dir": str(base),
        "templates": templates,
        "is_template_content": _is_template_matrix(),
        "cases": out_cases,
    }
    json.dump(doc, sys.stdout, ensure_ascii=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
