#!/usr/bin/env python3
"""Dump reference values for the SkillsLoader subsystem and the prompt's skills sections.

This is a fourth, independent dumper alongside dump_reference.py,
dump_prompt_guards.py and the others. It exists because dump_reference.py is a
shared file that several agents edit concurrently, and a read-modify-write race
there would destroy work. Keeping this section in its own file and its own Go
test file removes the race entirely.

What it covers
--------------
nanobot/agent/skills.py:SkillsLoader (372 lines) in full, plus the two places
ContextBuilder consults it (agent/context.py:132-143):

  * list_skills(filter_unavailable=True/False) — names, sources, PATHS and ORDER
  * build_skills_summary — the "# Skills" body, byte for byte
  * get_always_skills / load_skills_for_context — the "# Active Skills" section
  * get_skill_description / get_skill_availability / get_skill_requirements
  * load_skill
  * _strip_frontmatter / parse_skill_metadata / _parse_nanobot_metadata /
    valid_skill_metadata on an adversarial frontmatter corpus
  * the FULL rendered system prompt

Usage
-----
    .tools/venv/bin/python compat/python/dump_skills.py [base_dir]

base_dir defaults to a PID-suffixed scratch directory under .tools/. It is
created if absent. The caller owns cleanup.

The dumper sets HOME to <base_dir>/home BEFORE touching nanobot, because the
Agent Plugin activation marker and the CLI Apps alias registry both live under
the config directory ($HOME/.nanobot). Pinning HOME is what makes those two
fixtures reproducible: the Go side sets HOME to the same value, so both
implementations resolve the same marker file.

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

# HOME must be redirected before nanobot.config is imported anywhere, because
# every path helper derives from Path.home() at call time.
BASE_DEFAULT = ROOT / ".tools" / f"skills-ref-{os.getpid()}"
BASE = Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else BASE_DEFAULT
BASE.mkdir(parents=True, exist_ok=True)
HOME = BASE / "home"
HOME.mkdir(parents=True, exist_ok=True)
os.environ["HOME"] = str(HOME)

sys.path.insert(0, str(ROOT / "upstream" / "nanobot"))

# loguru writes diagnostics to stderr; this harness must emit exactly one JSON
# document on stdout. Several code paths here log warnings (invalid plugin
# manifests, invalid skill metadata), so the sink is removed rather than merely
# quieted.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:  # pragma: no cover - loguru is a hard dependency upstream
    pass

import nanobot.agent.skills as skills_module  # noqa: E402
from nanobot.agent.context import ContextBuilder  # noqa: E402
from nanobot.agent.skills import (  # noqa: E402
    SkillsLoader,
    parse_skill_metadata,
    valid_skill_metadata,
)

# The reference's own built-in skills directory. Every case pins the loader to
# this exact path (via builtin_skills_dir for SkillsLoader, and by rebinding the
# module global for ContextBuilder) so the Go side can be pointed at the same
# directory and the ABSOLUTE display roots in build_skills_summary can be
# compared byte for byte.
BUILTIN_DIR = (ROOT / "upstream" / "nanobot" / "nanobot" / "skills").resolve()

# Environment variables the fixtures rely on. The Go test sets the same values.
PROBE_ENV_SET = "NANOBOT_SKILLS_TEST_SET"
PROBE_ENV_EMPTY = "NANOBOT_SKILLS_TEST_EMPTY"
os.environ[PROBE_ENV_SET] = "1"
os.environ[PROBE_ENV_EMPTY] = ""

# Commands the fixtures require. Recorded so the Go side can report the same
# availability verdicts and so a reader can tell which ones this machine has.
PROBE_BINS = ["gh", "tmux", "summarize", "curl", "sh", "definitely-not-a-real-cli"]


# --------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------


def _sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _canon(value):
    """A canonical rendering shared with the Go side.

    json.dumps cannot be used directly: Python distinguishes int from float and
    Go's encoding/json does not, so `1` and `1.0` would collapse. The port's
    YAML subset parser produces int64 and float64 separately, so the
    distinction is worth preserving.
    """
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return repr(value)
    if isinstance(value, list):
        return "[" + ", ".join(_canon(item) for item in value) + "]"
    if isinstance(value, dict):
        return (
            "{"
            + ", ".join(
                json.dumps(str(key), ensure_ascii=False) + ": " + _canon(value[key])
                for key in sorted(value, key=str)
            )
            + "}"
        )
    return "<" + type(value).__name__ + ">"


def _write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def _write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def _skill(name: str, body: str, frontmatter: str = "", *, with_file: bool = True) -> str:
    """Build a SKILL.md document. frontmatter is the YAML body without fences."""
    if frontmatter:
        return f"---\n{frontmatter}\n---\n\n{body}"
    return body if with_file else body


def _fresh(base: Path, name: str) -> Path:
    ws = (base / name).resolve()
    if ws.exists():
        shutil.rmtree(ws)
    ws.mkdir(parents=True)
    return ws


def _normalize_runtime(prompt: str) -> str:
    """Replace the runtime line so the full prompts can be compared byte for byte.

    The reference reports "<OS> <machine>, Python <version>" and the port
    reports the Go toolchain instead (an intentional, documented divergence in
    internal/prompt/context.go). Everything else must match exactly, so the line
    is replaced with a fixed placeholder on both sides.
    """
    marker = "## Runtime\n"
    index = prompt.find(marker)
    if index < 0:
        return prompt
    start = index + len(marker)
    end = prompt.find("\n", start)
    if end < 0:
        return prompt
    return prompt[:start] + "<runtime>" + prompt[end:]


def _exception(fn, *args, **kwargs):
    """Run fn and return (result, error_string)."""
    try:
        return fn(*args, **kwargs), None
    except Exception as exc:  # noqa: BLE001 - the harness must record anything
        return None, f"{type(exc).__name__}: {exc}"


# --------------------------------------------------------------------------
# workspace cases
# --------------------------------------------------------------------------


def _case_fresh_builtin_only(base: Path):
    """No skills/ directory at all: the built-in group is the only one."""
    ws = _fresh(base, "fresh")
    return ws, ws, [], {}


def _case_workspace_skills_creation_order(base: Path):
    """Skill directories created in NON-alphabetical order.

    _skill_entries_from_dir uses base.iterdir() and never sorts, so the prompt
    lists them in directory order. A port that used os.ReadDir (sorted) or
    sort.Strings produces zeta/alpha/mid here instead of mid/zeta/alpha.
    """
    ws = _fresh(base, "creation_order")
    for name in ["mid", "zeta", "alpha"]:
        _write(ws / "skills" / name / "SKILL.md",
               _skill(name, f"# {name}\n", f"name: {name}\ndescription: The {name} skill."))
    return ws, ws, [], {}


def _case_project_workspace_differs(base: Path):
    """project != agent -> use_relative_roots is False -> ABSOLUTE display roots."""
    ws = _fresh(base, "project_differs")
    _write(ws / "skills" / "zeta" / "SKILL.md",
           _skill("zeta", "# zeta\n", "name: zeta\ndescription: Zeta skill."))
    project = _fresh(base, "project_differs_project")
    return ws, project, [], {}


def _case_plugins(base: Path):
    """An ENABLED Agent Plugin contributing one skill.

    enabled_agent_plugin_skills only returns skills from plugins whose
    activation marker validates, and the marker is written by the reference
    itself here (in the legacy bare-root form, which the reference upgrades in
    place) so the Go side has to reproduce the package fingerprint exactly to
    agree.
    """
    ws = _fresh(base, "plugins")
    plugin = ws / "plugins" / "demo"
    _write(
        plugin / "plugin.json",
        json.dumps(
            {
                "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
                "name": "demo",
                "description": "A demo Agent Plugin.",
            }
        ),
    )
    _write(
        plugin / "skills" / "demo-skill" / "SKILL.md",
        _skill("demo-skill", "# Demo\n", "name: demo-skill\ndescription: A plugin skill."),
    )
    # A second skill directory with a name that does not match its frontmatter:
    # _discover_plugin_skills rejects it.
    _write(
        plugin / "skills" / "mismatched" / "SKILL.md",
        _skill("mismatched", "# Nope\n", "name: something-else\ndescription: Mismatched."),
    )
    _write(ws / "skills" / "local" / "SKILL.md",
           _skill("local", "# local\n", "name: local\ndescription: Workspace skill."))
    return ws, ws, [], {}


def _enable_plugin(workspace: Path, name: str) -> None:
    """Write the LEGACY activation marker (the bare package root).

    _enabled_package_fingerprint treats that form as a valid marker for the
    current package and rewrites it in the JSON form, so this exercises the
    migration branch as well as the enabled path.
    """
    from nanobot.agent.plugins import _plugin_data_dir

    root = (workspace / "plugins" / name).resolve()
    data_dir = _plugin_data_dir(workspace, name, create=True)
    marker = data_dir / "enabled"
    marker.write_text(str(root), encoding="utf-8")
    marker.chmod(0o600)


def _case_always_top_level(base: Path):
    ws = _fresh(base, "always_top")
    _write(ws / "skills" / "always-top" / "SKILL.md",
           _skill("always-top", "ALWAYS TOP BODY\n",
                  "name: always-top\ndescription: Always top.\nalways: true"))
    _write(ws / "skills" / "never" / "SKILL.md",
           _skill("never", "NEVER BODY\n", "name: never\ndescription: Not always.\nalways: false"))
    return ws, ws, [], {}


def _case_always_nested(base: Path):
    """metadata.always via BOTH the flow form and the nested block form."""
    ws = _fresh(base, "always_nested")
    _write(ws / "skills" / "flow-always" / "SKILL.md",
           _skill("flow-always", "FLOW BODY\n",
                  'name: flow-always\ndescription: Flow nested always.\n'
                  'metadata: {"nanobot":{"always":true}}'))
    _write(ws / "skills" / "block-always" / "SKILL.md",
           _skill("block-always", "BLOCK BODY\n",
                  "name: block-always\ndescription: Block nested always.\n"
                  "metadata:\n  nanobot:\n    always: true"))
    _write(ws / "skills" / "openclaw-always" / "SKILL.md",
           _skill("openclaw-always", "OPENCLAW BODY\n",
                  "name: openclaw-always\ndescription: openclaw fallback.\n"
                  'metadata: {"openclaw":{"always":true}}'))
    _write(ws / "skills" / "json-string-always" / "SKILL.md",
           _skill("json-string-always", "JSONSTRING BODY\n",
                  "name: json-string-always\ndescription: JSON string metadata.\n"
                  "metadata: '{\"nanobot\":{\"always\":true}}'"))
    return ws, ws, [], {}


def _case_always_truthiness(base: Path):
    """The truthiness edge cases of `always`."""
    ws = _fresh(base, "always_truthiness")
    variants = {
        "always-string-false": '"false"',   # a non-empty STRING: truthy -> active
        "always-zero": "0",                 # falsy -> inactive
        "always-one": "1",                  # truthy -> active
        "always-empty": '""',               # empty string -> falsy -> inactive
        "always-null": "null",              # falsy -> inactive
        "always-yes": "yes",                # YAML bool true -> active
        "always-off": "off",                # YAML bool false -> inactive
        "always-empty-list": "[]",          # falsy -> inactive
    }
    for name, value in variants.items():
        _write(ws / "skills" / name / "SKILL.md",
               _skill(name, f"# {name}\n", f"name: {name}\ndescription: {name}.\nalways: {value}"))
    return ws, ws, [], {}


def _case_always_unavailable(base: Path):
    """always: true but an unmet requirement: list_skills(True) filters it out
    BEFORE the always test, so it is NOT active."""
    ws = _fresh(base, "always_unavailable")
    _write(ws / "skills" / "always-missing-bin" / "SKILL.md",
           _skill("always-missing-bin", "MISSING BIN BODY\n",
                  "name: always-missing-bin\ndescription: Always but no CLI.\n"
                  "always: true\n"
                  'metadata: {"nanobot":{"requires":{"bins":["definitely-not-a-real-cli"]}}}'))
    _write(ws / "skills" / "always-present-bin" / "SKILL.md",
           _skill("always-present-bin", "PRESENT BIN BODY\n",
                  "name: always-present-bin\ndescription: Always and CLI present.\n"
                  "always: true\n"
                  'metadata: {"nanobot":{"requires":{"bins":["gh"]}}}'))
    return ws, ws, [], {}


def _case_requirements(base: Path):
    """Every requirement shape: missing bin, missing env, empty env, wrong types."""
    ws = _fresh(base, "requirements")
    cases = {
        "req-missing-bin": 'metadata: {"nanobot":{"requires":{"bins":["definitely-not-a-real-cli"]}}}',
        "req-present-bin": 'metadata: {"nanobot":{"requires":{"bins":["gh"]}}}',
        "req-missing-env": 'metadata: {"nanobot":{"requires":{"env":["NANOBOT_SKILLS_TEST_UNSET"]}}}',
        "req-present-env": f'metadata: {{"nanobot":{{"requires":{{"env":["{PROBE_ENV_SET}"]}}}}}}',
        "req-empty-env": f'metadata: {{"nanobot":{{"requires":{{"env":["{PROBE_ENV_EMPTY}"]}}}}}}',
        "req-both-missing": ('metadata: {"nanobot":{"requires":{"bins":["definitely-not-a-real-cli"],'
                             '"env":["NANOBOT_SKILLS_TEST_UNSET"]}}}'),
        "req-bins-not-list": 'metadata: {"nanobot":{"requires":{"bins":"gh"}}}',
        "req-bins-mixed": 'metadata: {"nanobot":{"requires":{"bins":["gh","","   ",5,null,"tmux"]}}}',
        "req-requires-not-dict": 'metadata: {"nanobot":{"requires":["gh"]}}',
        "req-requires-empty": 'metadata: {"nanobot":{"requires":{}}}',
        "req-requires-null": 'metadata: {"nanobot":{"requires":null}}',
        "req-no-metadata": "description: No metadata key.",
        "req-metadata-list": 'metadata: ["gh"]',
        "req-metadata-bad-json": 'metadata: "not json"',
        "req-metadata-json-true": 'metadata: "true"',
        "req-metadata-nanobot-null": 'metadata: {"nanobot":null}',
    }
    for name, extra in cases.items():
        frontmatter = f"name: {name}\ndescription: {name}."
        if extra.startswith("description:"):
            frontmatter = f"name: {name}\n{extra}"
        else:
            frontmatter = f"{frontmatter}\n{extra}"
        _write(ws / "skills" / name / "SKILL.md", _skill(name, f"# {name}\n", frontmatter))
    return ws, ws, [], {}


def _case_malformed_frontmatter(base: Path):
    ws = _fresh(base, "malformed")
    documents = {
        "no-frontmatter": "# Just a body\n",
        "unclosed-frontmatter": "---\nname: unclosed\ndescription: d\n",
        "empty-frontmatter": "---\n---\n\n# Body\n",
        "frontmatter-sequence": "---\n- a\n- b\n---\n\n# Body\n",
        "frontmatter-scalar": "---\njust a string\n---\n\n# Body\n",
        "frontmatter-null": "---\nnull\n---\n\n# Body\n",
        "frontmatter-tab-indent": "---\nname: tabbed\n\tmetadata:\n\t  nanobot:\n\t    always: true\n---\n\n# Body\n",
        "frontmatter-duplicate-keys": "---\nname: dup\ndescription: first\ndescription: second\n---\n\n# Body\n",
        "frontmatter-comments": "---\n# a comment\nname: commented  # trailing\ndescription: With a # inside\n---\n\n# Body\n",
        "frontmatter-doc-end": "---\nname: docend\ndescription: d\n...\n\n# Body\n",
        "four-dashes": "----\nname: four\n---\n\n# Body\n",
        "crlf-frontmatter": "---\r\nname: crlf\r\ndescription: CRLF description\r\n---\r\n\r\n# Body\r\n",
        "empty-file": "",
        "only-dashes": "---",
        "only-dashes-newline": "---\n",
        "unit-separator-padding": "\x1c---\nname: us\n---\n",
        "nbsp-after-dashes": "---\u00a0\nname: nbsp\n---\n",
        "trailing-space-after-dashes": "---   \nname: trail\n---\n",
    }
    for name, content in documents.items():
        _write(ws / "skills" / name / "SKILL.md", content)
    return ws, ws, [], {}


def _case_disabled(base: Path):
    ws = _fresh(base, "disabled")
    for name in ["keep-me", "drop-me", "cli-app-my-app"]:
        _write(ws / "skills" / name / "SKILL.md",
               _skill(name, f"# {name}\n", f"name: {name}\ndescription: The {name} skill."))
    return ws, ws, ["drop-me", "cli-app-my_app"], {}


def _case_symlinked_skills_dir(base: Path):
    """workspace/skills is a SYMLINK to a real directory elsewhere.

    Path.exists()/is_dir() follow symlinks, and pyResolve resolves them, so the
    absolute display root in the differing-project case is the link TARGET.
    """
    ws = _fresh(base, "symlinked")
    real = _fresh(base, "symlinked_real")
    _write(real / "linked" / "SKILL.md",
           _skill("linked", "# linked\n", "name: linked\ndescription: Behind a symlink."))
    os.symlink(real, ws / "skills")
    return ws, ws, [], {}


def _case_symlinked_skills_dir_project_differs(base: Path):
    ws = _fresh(base, "symlinked_abs")
    real = _fresh(base, "symlinked_abs_real")
    _write(real / "linked" / "SKILL.md",
           _skill("linked", "# linked\n", "name: linked\ndescription: Behind a symlink."))
    os.symlink(real, ws / "skills")
    project = _fresh(base, "symlinked_abs_project")
    return ws, project, [], {}


def _case_misc_entries(base: Path):
    """Entries that must be skipped or tolerated, none of which crashes the
    reference: a skill dir with no SKILL.md, a plain file in skills/, a dangling
    symlink, an EMPTY SKILL.md, a CRLF SKILL.md and one with no frontmatter."""
    ws = _fresh(base, "misc")
    _write(ws / "skills" / "has-skill" / "SKILL.md",
           _skill("has-skill", "# has-skill\n", "name: has-skill\ndescription: Has a SKILL.md."))
    (ws / "skills" / "no-skillmd").mkdir(parents=True)
    _write(ws / "skills" / "no-skillmd" / "README.md", "not a skill\n")
    _write(ws / "skills" / "empty-skillmd" / "SKILL.md", "")
    _write(ws / "skills" / "plain-file.txt", "not a directory\n")
    os.symlink(ws / "skills" / "nowhere", ws / "skills" / "dangling")
    _write(ws / "skills" / "crlf-skill" / "SKILL.md",
           "---\r\nname: crlf-skill\r\ndescription: CRLF skill.\r\n---\r\n\r\n# CRLF\r\nBody line.\r\n")
    _write(ws / "skills" / "no-frontmatter-skill" / "SKILL.md", "# no frontmatter\n\nbody\n")
    return ws, ws, [], {}


def _case_skillmd_is_dir(base: Path):
    """skills/<name>/SKILL.md is a DIRECTORY.

    Path.exists() is True for a directory, so _skill_entries_from_dir KEEPS the
    entry; read_text then raises IsADirectoryError out of load_skill and the
    reference's build_skills_summary / build_system_prompt blow up. Isolated in
    its own case so it cannot mask the byte-exact comparison of the others.
    """
    ws = _fresh(base, "skillmd_is_dir")
    _write(ws / "skills" / "ok" / "SKILL.md",
           _skill("ok", "# ok\n", "name: ok\ndescription: Fine."))
    (ws / "skills" / "broken" / "SKILL.md").mkdir(parents=True)
    return ws, ws, [], {}


def _case_invalid_utf8(base: Path):
    """A SKILL.md that is not valid UTF-8: read_text raises UnicodeDecodeError."""
    ws = _fresh(base, "invalid_utf8")
    _write(ws / "skills" / "ok" / "SKILL.md",
           _skill("ok", "# ok\n", "name: ok\ndescription: Fine."))
    _write_bytes(
        ws / "skills" / "bad-bytes" / "SKILL.md",
        b"---\nname: bad-bytes\ndescription: Bad \xff\xfe bytes.\n---\n\nBody.\n",
    )
    return ws, ws, [], {}


def _case_symlinked_workspace_with_plugin(base: Path):
    """An agent workspace reached THROUGH A SYMLINK, with an enabled plugin.

    Plugin skill paths are resolved by _contained (so they carry the link
    TARGET) while the plugin group root is `self.workspace / "plugins"` — the
    unresolved, symlinked spelling. Path.relative_to is purely lexical, so the
    two disagree and the reference raises ValueError out of build_skills_summary.
    The harness records the exception; the Go side reports
    skills.RelativeToError instead of crashing.
    """
    real = _fresh(base, "symlinked_ws_real")
    link = base / "symlinked_ws_link"
    if link.exists() or link.is_symlink():
        link.unlink()
    os.symlink(real, link)

    plugin = real / "plugins" / "demo"
    _write(
        plugin / "plugin.json",
        json.dumps(
            {
                "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
                "name": "demo",
            }
        ),
    )
    _write(plugin / "skills" / "demo-skill" / "SKILL.md",
           _skill("demo-skill", "# Demo\n", "name: demo-skill\ndescription: A plugin skill."))
    _enable_plugin(real, "demo")
    return link, link, [], {"symlinked_workspace": True}


def _case_duplicate_plugin_names(base: Path):
    """Two plugin directories declaring the same identity: both are ignored."""
    ws = _fresh(base, "duplicate_plugins")
    for dirname in ["aaa", "bbb"]:
        plugin = ws / "plugins" / dirname
        _write(
            plugin / "plugin.json",
            json.dumps(
                {
                    "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
                    "name": "same-name",
                }
            ),
        )
        _write(plugin / "skills" / f"skill-{dirname}" / "SKILL.md",
               _skill(f"skill-{dirname}", "# x\n",
                      f"name: skill-{dirname}\ndescription: From {dirname}."))
    return ws, ws, [], {}


def _case_invalid_plugin_manifest(base: Path):
    """Manifests that must be rejected: wrong schema, bad name, no manifest,
    unreadable JSON, and a skill whose frontmatter name disagrees with its dir."""
    ws = _fresh(base, "invalid_plugins")
    specs = {
        "wrong-schema": {"$schema": "https://example.com/other.json", "name": "wrong-schema"},
        "bad-name": {"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
                     "name": "Bad Name"},
        "double-dash": {"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
                        "name": "bad--name"},
    }
    for dirname, payload in specs.items():
        plugin = ws / "plugins" / dirname
        _write(plugin / "plugin.json", json.dumps(payload))
        _write(plugin / "skills" / "s" / "SKILL.md",
               _skill("s", "# s\n", "name: s\ndescription: Skill s."))
    _write(ws / "plugins" / "no-manifest" / "skills" / "s" / "SKILL.md",
           _skill("s", "# s\n", "name: s\ndescription: Skill s."))
    _write(ws / "plugins" / "bad-json" / "plugin.json", "{not json")
    _write(ws / "plugins" / "bad-json" / "skills" / "s" / "SKILL.md",
           _skill("s", "# s\n", "name: s\ndescription: Skill s."))
    return ws, ws, [], {}


def _case_name_shadowing(base: Path):
    """A workspace skill and a built-in skill with the SAME name.

    seen_names is populated from the workspace group and the built-in group
    skips those names, so the built-in `cron` must NOT appear twice.
    """
    ws = _fresh(base, "shadowing")
    _write(ws / "skills" / "cron" / "SKILL.md",
           _skill("cron", "# local cron\n", "name: cron\ndescription: Local cron override."))
    return ws, ws, [], {}


CASES = [
    ("fresh_builtin_only", "cli", True, _case_fresh_builtin_only),
    ("workspace_skills_creation_order", "cli", True, _case_workspace_skills_creation_order),
    ("project_workspace_differs", "telegram", True, _case_project_workspace_differs),
    ("plugins", "cli", True, _case_plugins),
    ("always_top_level", "cli", True, _case_always_top_level),
    ("always_nested", "cli", True, _case_always_nested),
    ("always_truthiness", "cli", True, _case_always_truthiness),
    ("always_unavailable", "cli", True, _case_always_unavailable),
    ("requirements", "cli", True, _case_requirements),
    ("malformed_frontmatter", "cli", True, _case_malformed_frontmatter),
    ("disabled", "cli", True, _case_disabled),
    ("symlinked_skills_dir", "cli", True, _case_symlinked_skills_dir),
    ("symlinked_skills_dir_project_differs", "cli", True, _case_symlinked_skills_dir_project_differs),
    ("misc_entries", "cli", True, _case_misc_entries),
    ("skillmd_is_dir", "cli", True, _case_skillmd_is_dir),
    ("invalid_utf8", "cli", True, _case_invalid_utf8),
    ("symlinked_workspace_with_plugin", "cli", True, _case_symlinked_workspace_with_plugin),
    ("duplicate_plugin_names", "cli", True, _case_duplicate_plugin_names),
    ("invalid_plugin_manifest", "cli", True, _case_invalid_plugin_manifest),
    ("name_shadowing", "cli", True, _case_name_shadowing),
    ("no_memory_section", "email", False, _case_workspace_skills_creation_order),
]

# Explicit-invocation probes, run against every case. They exercise
# get_explicitly_invoked_skills / build_explicit_skill_runtime_context.
EXPLICIT_TEXTS = [
    "$cron please",
    "use $weather and $cron",
    "$cron $cron",
    "a$cron",
    "$cli-app-my_app",
    "$cli-app-my-app",
    "$unknown-skill",
    "$",
    "",
    "price is $5 and $cron",
    "$my-åäö",
]


# --------------------------------------------------------------------------
# frontmatter corpus
# --------------------------------------------------------------------------

FRONTMATTER_DOCS = {
    "basic": "---\nname: basic\ndescription: A basic skill.\n---\n\nBody.\n",
    "crlf": "---\r\nname: crlf\r\ndescription: CRLF.\r\n---\r\n\r\nBody.\r\n",
    "no-frontmatter": "# No frontmatter\n",
    "unclosed": "---\nname: unclosed\ndescription: d\n",
    "empty-body": "---\n---\n\nBody.\n",
    "sequence-root": "---\n- a\n- b\n---\n\nBody.\n",
    "scalar-root": "---\njust a string\n---\n\nBody.\n",
    "null-root": "---\nnull\n---\n\nBody.\n",
    "only-dashes": "---",
    "four-dashes": "----\nname: four\n---\n",
    "unit-separator-padding": "\x1c---\nname: us\ndescription: d\n---\n",
    "nbsp-after-opening": "---\u00a0\nname: nbsp\ndescription: d\n---\n",
    "ideographic-space-after-opening": "---\u3000\nname: ideo\ndescription: d\n---\n",
    "trailing-spaces-after-opening": "---   \nname: trail\ndescription: d\n---\n",
    "trailing-spaces-after-closing": "---\nname: trail2\ndescription: d\n---   \nbody\n",
    "no-newline-after-closing": "---\nname: nonl\ndescription: d\n---",
    "quoted-double": '---\nname: qd\ndescription: "quoted: with colon"\n---\n',
    "quoted-single": "---\nname: qs\ndescription: 'single ''quoted'' value'\n---\n",
    "quoted-escapes": '---\nname: qe\ndescription: "tab\\there\\nnewline \\u00e9"\n---\n',
    "quoted-unicode-escape": '---\nname: qu\ndescription: "\\U0001F600 emoji"\n---\n',
    "plain-with-colon-url": "---\nname: pc\nhomepage: https://example.com/:path\ndescription: d\n---\n",
    "plain-with-hash": "---\nname: ph\ndescription: text # not a comment in quotes?\n---\n",
    "comment-lines": "---\n# leading comment\nname: cl  # trailing comment\ndescription: d\n---\n",
    "only-comments": "---\n# nothing else\n---\n",
    "nested-block": "---\nname: nb\ndescription: d\nmetadata:\n  nanobot:\n    always: true\n---\n",
    "nested-block-deep": "---\nname: nbd\ndescription: d\nmetadata:\n  nanobot:\n    requires:\n      bins:\n        - gh\n        - tmux\n      env:\n        - HOME\n---\n",
    "flow-nested": '---\nname: fn\ndescription: d\nmetadata: {"nanobot":{"requires":{"bins":["gh"]}}}\n---\n',
    "flow-multiline": '---\nname: fm\ndescription: d\nmetadata: {\n  "nanobot": {\n    "always": true\n  }\n}\n---\n',
    "flow-sequence-root": "---\n- 1\n- two\n- [3, 4]\n---\n",
    "flow-nested-seq": "---\nname: fns\ndescription: d\nmetadata: {\"nanobot\":{\"requires\":{\"bins\":[\"gh\",\"tmux\"]}}}\n---\n",
    "flow-empty-collections": "---\nname: fec\ndescription: d\nmetadata: {}\nalways: []\n---\n",
    "block-scalar-literal": "---\nname: bsl\ndescription: |\n  line one\n  line two\n---\n",
    "block-scalar-literal-strip": "---\nname: bsls\ndescription: |-\n  line one\n  line two\n---\n",
    "block-scalar-folded": "---\nname: bsf\ndescription: >\n  folded line one\n  folded line two\n---\n",
    "block-scalar-folded-strip": "---\nname: bsfs\ndescription: >-\n  folded one\n  folded two\n---\n",
    "duplicate-keys": "---\nname: dup\ndescription: first\ndescription: second\n---\n",
    "tab-indentation": "---\nname: tabbed\n\tmetadata: x\n---\n",
    "tab-in-value": "---\nname: tabval\ndescription:\tTabbed value\n---\n",
    "int-values": "---\nname: iv\ndescription: d\ncount: 42\nnegative: -7\nzero: 0\n---\n",
    "float-values": "---\nname: fv\ndescription: d\nratio: 1.5\nexp: 1.0e+3\nplainint: 1\n---\n",
    "bool-values": "---\nname: bv\ndescription: d\nyes_val: yes\nno_val: no\non_val: on\noff_val: off\ntrue_val: True\nfalse_val: FALSE\n---\n",
    "null-values": "---\nname: nv\ndescription: d\ntilde: ~\nnull_val: null\nempty:\n---\n",
    "numeric-keys": "---\nname: nk\ndescription: d\n1: one\ntrue: yes\n---\n",
    "sexagesimal": "---\nname: sx\ndescription: d\ntime: 1:30\n---\n",
    "octal-and-hex": "---\nname: oh\ndescription: d\noctal: 0755\nhex: 0x1f\nbinary: 0b101\n---\n",
    "anchor": "---\nname: an\ndescription: d\nbase: &b value\ncopy: *b\n---\n",
    "tag": "---\nname: tg\ndescription: !!str d\n---\n",
    "merge-key": "---\nname: mk\ndescription: d\n<<: {a: 1}\n---\n",
    "complex-key": "---\nname: ck\ndescription: d\n? [a, b]\n: value\n---\n",
    "multi-line-plain": "---\nname: mlp\ndescription: first part\n  second part\n---\n",
    "sequence-same-indent": "---\nname: ssi\ndescription: d\nbins:\n- gh\n- tmux\n---\n",
    "sequence-compact-mapping": "---\nname: scm\ndescription: d\nitems:\n  - id: a\n    label: A\n  - id: b\n    label: B\n---\n",
    "unicode-description": "---\nname: ud\ndescription: 设置更新 — ünïcödé 🎉\n---\n",
    "bom-prefix": "\ufeff---\nname: bom\ndescription: d\n---\n",
    "doc-end-marker": "---\nname: dem\ndescription: d\n...\n",
    "cr-only-newlines": "---\rname: cr\ndescription: d\r---\r",
    "empty-string": "",
    "whitespace-only": "   \n\t\n",
    "description-not-string": "---\nname: dns\ndescription: 123\n---\n",
    "name-not-string": "---\nname: 123\ndescription: d\n---\n",
    "description-empty": '---\nname: de\ndescription: ""\n---\n',
    "description-space-only": '---\nname: dso\ndescription: "   "\n---\n',
    "name-with-double-dash": "---\nname: bad--name\ndescription: d\n---\n",
    "name-uppercase": "---\nname: BadName\ndescription: d\n---\n",
    "name-with-underscore": "---\nname: bad_name\ndescription: d\n---\n",
    "name-trailing-dash": "---\nname: bad-\ndescription: d\n---\n",
    "name-single-char": "---\nname: a\ndescription: d\n---\n",
    "name-64-chars": "---\nname: " + ("a" * 64) + "\ndescription: d\n---\n",
    "name-65-chars": "---\nname: " + ("a" * 65) + "\ndescription: d\n---\n",
    "description-1024": '---\nname: d1024\ndescription: "' + ("d" * 1024) + '"\n---\n',
    "description-1025": '---\nname: d1025\ndescription: "' + ("d" * 1025) + '"\n---\n',
    "description-multibyte-1024": '---\nname: dm1024\ndescription: "' + ("é" * 1024) + '"\n---\n',
    "description-multibyte-1025": '---\nname: dm1025\ndescription: "' + ("é" * 1025) + '"\n---\n',
    "metadata-json-string": "---\nname: mjs\ndescription: d\nmetadata: '{\"nanobot\":{\"always\":true}}'\n---\n",
    "metadata-json-openclaw": '---\nname: mjo\ndescription: d\nmetadata: {"openclaw":{"always":true}}\n---\n',
    "metadata-nanobot-null": '---\nname: mnn\ndescription: d\nmetadata: {"nanobot":null}\n---\n',
    "metadata-both-keys": '---\nname: mbk\ndescription: d\nmetadata: {"nanobot":{"always":true},"openclaw":{"always":false}}\n---\n',
    "metadata-list": '---\nname: ml\ndescription: d\nmetadata: [1, 2]\n---\n',
    "metadata-bad-json": '---\nname: mbj\ndescription: d\nmetadata: "not json"\n---\n',
    "metadata-json-true": '---\nname: mjt\ndescription: d\nmetadata: "true"\n---\n',
    "metadata-json-empty-string": '---\nname: mjes\ndescription: d\nmetadata: ""\n---\n',
    "requires-wrong-shapes": '---\nname: rws\ndescription: d\nmetadata: {"nanobot":{"requires":{"bins":"gh","env":"HOME"}}}\n---\n',
    "requires-mixed-list": '---\nname: rml\ndescription: d\nmetadata: {"nanobot":{"requires":{"bins":["gh","","   ",5,null,"tmux"]}}}\n---\n',
    "requires-unit-separator-entry": '---\nname: ruse\ndescription: d\nmetadata: {"nanobot":{"requires":{"bins":["\\u001c"]}}}\n---\n',
}

# valid_skill_metadata probes: (document, name-to-check)
VALID_NAME_PROBES = [
    ("basic", "basic"),
    ("basic", "other"),
    ("name-not-string", "123"),
    ("name-with-double-dash", "bad--name"),
    ("name-uppercase", "BadName"),
    ("name-single-char", "a"),
    ("name-64-chars", "a" * 64),
    ("name-65-chars", "a" * 65),
    ("description-not-string", "dns"),
    ("description-empty", "de"),
    ("description-space-only", "dso"),
    ("description-1024", "d1024"),
    ("description-1025", "d1025"),
    ("description-multibyte-1024", "dm1024"),
    ("description-multibyte-1025", "dm1025"),
    ("unicode-description", "ud"),
    ("no-frontmatter", "no-frontmatter"),
]


# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------


def _skill_report(loader: SkillsLoader, names: list[str]) -> dict:
    report = {}
    for name in names:
        content, error = _exception(loader.load_skill, name)
        description, description_error = _exception(loader.get_skill_description, name)
        # get_skill_availability returns a TUPLE (available, missing); it is
        # unpacked here because a JSON array would be compared against two
        # separate Go fields.
        availability, availability_error = _exception(loader.get_skill_availability, name)
        available, missing = None, None
        if availability is not None:
            available, missing = availability
        requirements, req_error = _exception(loader.get_skill_requirements, name)
        meta, meta_error = _exception(loader.get_skill_metadata, name)
        report[name] = {
            "description": description,
            "description_error": description_error,
            "available": available,
            "availability_error": availability_error,
            "missing": missing,
            "requirements": requirements,
            "load_sha256": None if content is None else _sha256_text(content),
            "load_len": None if content is None else len(content),
            "load_error": error,
            "metadata_canon": None if meta is None else _canon(meta),
            "metadata_error": meta_error,
            "requirements_error": req_error,
        }
    return report


def main() -> int:
    # CLI Apps registry: two installed apps, one whose legacy and canonical
    # skill names differ (the underscore) and one whose names coincide.
    _write(
        HOME / ".nanobot" / "cli-apps" / "installed.json",
        json.dumps({"schema_version": 1, "apps": {"my_app": {}, "plain": {}}}),
    )

    bundled_files = {}
    for path in sorted(BUILTIN_DIR.rglob("*")):
        if path.is_file():
            bundled_files[path.relative_to(BUILTIN_DIR).as_posix()] = hashlib.sha256(
                path.read_bytes()
            ).hexdigest()
    bundled_order = [entry.name for entry in BUILTIN_DIR.iterdir()]

    out_cases = []
    for name, channel, include_memory, build in CASES:
        case_dir = BASE / "cases" / name
        case_dir.mkdir(parents=True, exist_ok=True)
        agent_ws, project_ws, disabled, options = build(case_dir)
        if name in {"plugins", "symlinked_workspace_with_plugin"}:
            _enable_plugin(Path(agent_ws).resolve(), "demo")

        loader = SkillsLoader(
            agent_ws,
            builtin_skills_dir=BUILTIN_DIR,
            disabled_skills=set(disabled) if disabled else None,
        )

        list_all, list_all_error = _exception(loader.list_skills, False)
        list_available, list_available_error = _exception(loader.list_skills, True)
        always, always_error = _exception(loader.get_always_skills)

        # The prompt path. ContextBuilder builds its own SkillsLoader from the
        # module global, so the global is rebound to BUILTIN_DIR first.
        original_builtin = skills_module.BUILTIN_SKILLS_DIR
        skills_module.BUILTIN_SKILLS_DIR = BUILTIN_DIR
        try:
            builder = ContextBuilder(agent_ws, disabled_skills=list(disabled) or None)
            summary, summary_error = _exception(
                builder.skills.build_skills_summary,
                exclude=set(always or []),
                workspace=project_ws,
            )
            active_content, active_error = _exception(
                builder.skills.load_skills_for_context, list(always or [])
            )
            prompt, prompt_error = _exception(
                builder.build_system_prompt,
                channel=channel,
                session_summary=None,
                workspace=project_ws,
                include_memory=include_memory,
            )
        finally:
            skills_module.BUILTIN_SKILLS_DIR = original_builtin

        names = sorted({entry["name"] for entry in (list_all or [])})

        explicit = {}
        explicit_context = {}
        for text in EXPLICIT_TEXTS:
            invoked, invoked_error = _exception(loader.get_explicitly_invoked_skills, text)
            block, block_error = _exception(loader.build_explicit_skill_runtime_context, text)
            explicit[text] = {"invoked": invoked, "error": invoked_error}
            explicit_context[text] = {
                "source": None if block is None else block.source,
                "content": None if block is None else block.content,
                "error": block_error,
            }

        out_cases.append(
            {
                "name": name,
                "channel": channel,
                "include_memory": include_memory,
                "agent_workspace": str(agent_ws),
                "project_workspace": str(project_ws),
                "builtin_skills_dir": str(BUILTIN_DIR),
                "disabled_skills": sorted(disabled),
                "symlinked_workspace": bool(options.get("symlinked_workspace")),
                "list_all": None if list_all is None else [
                    [entry["name"], entry["source"], entry["path"]] for entry in list_all
                ],
                "list_all_error": list_all_error,
                "list_available": None if list_available is None else [
                    [entry["name"], entry["source"], entry["path"]] for entry in list_available
                ],
                "list_available_error": list_available_error,
                "summary": summary,
                "summary_error": summary_error,
                "always": always,
                "always_error": always_error,
                "active_content": active_content,
                "active_error": active_error,
                "skills": _skill_report(loader, names),
                "explicit": explicit,
                "explicit_context": explicit_context,
                "prompt_len": None if prompt is None else len(_normalize_runtime(prompt)),
                "prompt": None if prompt is None else _normalize_runtime(prompt),
                "prompt_error": prompt_error,
            }
        )

    frontmatter = []
    for name, content in FRONTMATTER_DOCS.items():
        meta, error = _exception(parse_skill_metadata, content)
        stripped, strip_error = _exception(
            SkillsLoader(Path("/nonexistent"))._strip_frontmatter, content
        )
        frontmatter.append(
            {
                "name": name,
                "content": content,
                "strip": stripped,
                "strip_error": strip_error,
                "metadata": None if meta is None else _canon(meta),
                "metadata_is_none": meta is None,
                "error": error,
            }
        )

    valid_probes = []
    for doc_name, skill_name in VALID_NAME_PROBES:
        content = FRONTMATTER_DOCS[doc_name]
        meta = parse_skill_metadata(content)
        result = None if meta is None else bool(valid_skill_metadata(meta, skill_name))
        valid_probes.append(
            {"document": doc_name, "skill_name": skill_name, "result": result}
        )

    nanobot_metadata = []
    for name, content in FRONTMATTER_DOCS.items():
        meta = parse_skill_metadata(content)
        if meta is None:
            continue
        raw = meta.get("metadata")
        payload = SkillsLoader(Path("/nonexistent"))._parse_nanobot_metadata(raw)
        nanobot_metadata.append(
            {"document": name, "raw_canon": _canon(raw), "result": _canon(payload)}
        )

    doc = {
        "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
        "python_version": sys.version.split()[0],
        "base_dir": str(BASE),
        "home": str(HOME),
        "builtin_skills_dir": str(BUILTIN_DIR),
        "bundled_files": bundled_files,
        "bundled_order": bundled_order,
        "probe_env": {PROBE_ENV_SET: os.environ[PROBE_ENV_SET], PROBE_ENV_EMPTY: ""},
        "probe_bins": {name: bool(shutil.which(name)) for name in PROBE_BINS},
        "path": os.environ.get("PATH", ""),
        "frontmatter": frontmatter,
        "valid_metadata": valid_probes,
        "nanobot_metadata": nanobot_metadata,
        "explicit_texts": EXPLICIT_TEXTS,
        "cases": out_cases,
    }
    json.dump(doc, sys.stdout, ensure_ascii=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
