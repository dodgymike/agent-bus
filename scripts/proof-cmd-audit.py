#!/usr/bin/env python3
"""Read-only audit of mechanically broken Spec Server proof commands.

The auditor deliberately parses strings; it never executes a stored proof.
"""

import argparse
import json
import os
import re
import shlex
import stat
import subprocess
import sys
import unicodedata
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
MAX_INPUT = 16 * 1024 * 1024
MAX_TASKS = 20_000
MAX_COMMAND = 128 * 1024
MAX_FINDINGS = 100_000
MAX_OUTPUT = 16 * 1024 * 1024
MAX_SOURCE_BYTES = 64 * 1024 * 1024
RANK = {"both-success-branches": 0, "success-masking-pipeline": 1,
        "missing-go-test-target": 2}


def finding(task, kind, detail):
    return {"severity": "high" if RANK[kind] < 2 else "medium",
            "kind": kind, "task": task.get("key") or task.get("public_id") or "<unknown>",
            "public_id": task.get("public_id", ""), "detail": detail}


def safe_text(value):
    """Keep terminal output single-line and immune to control-sequence repainting."""
    return "".join("?" if unicodedata.category(c).startswith("C") else c for c in value)


def branch_always_succeeds(body):
    parts = [part.strip() for part in body.split(";") if part.strip()]
    if not parts or not re.fullmatch(r"(?:exit\s+0|true|:)", parts[-1]):
        return False
    return all(re.fullmatch(r"(?:echo|printf)(?:\s+[^;&|]*)?", part) for part in parts[:-1])


def strip_go_noncode(source):
    """Blank comments and string/rune literals while preserving line structure."""
    out = list(source)
    i, state = 0, "code"
    while i < len(source):
        char = source[i]
        nxt = source[i + 1] if i + 1 < len(source) else ""
        if state == "code":
            if char == "/" and nxt == "/":
                out[i] = out[i + 1] = " "; i += 2; state = "line"; continue
            if char == "/" and nxt == "*":
                out[i] = out[i + 1] = " "; i += 2; state = "block"; continue
            if char in ('"', "'", "`"):
                out[i] = " "; state = {"\"": "string", "'": "rune", "`": "raw"}[char]
            i += 1; continue
        if state == "line":
            if char == "\n": state = "code"
            else: out[i] = " "
            i += 1; continue
        if state == "block":
            if char == "*" and nxt == "/":
                out[i] = out[i + 1] = " "; i += 2; state = "code"; continue
            if char != "\n": out[i] = " "
            i += 1; continue
        if state in ("string", "rune") and char == "\\":
            out[i] = " "
            if i + 1 < len(source): out[i + 1] = " "
            i += 2; continue
        terminator = {"string": '"', "rune": "'", "raw": "`"}[state]
        if char == terminator:
            out[i] = " "; state = "code"
        elif char != "\n":
            out[i] = " "
        i += 1
    return "".join(out)


def literal_run_matches(pattern, name):
    anchored_start, anchored_end = pattern.startswith("^"), pattern.endswith("$")
    literal = pattern[anchored_start:len(pattern) - anchored_end if anchored_end else None]
    if not re.fullmatch(r"Test[A-Za-z0-9_]+", literal):
        return None
    if anchored_start and anchored_end: return name == literal
    if anchored_start: return name.startswith(literal)
    if anchored_end: return name.endswith(literal)
    return literal in name


class TestSourceCache:
    """Discover each canonical package once under one aggregate byte budget."""
    def __init__(self):
        self.names = {}
        self.source_bytes = 0

    def package_names(self, directory):
        canonical = directory.resolve()
        if canonical in self.names:
            return self.names[canonical]
        files = sorted(canonical.glob("*_test.go"))
        safe_files = []
        for test_file in files:
            info = test_file.lstat()
            if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
                self.names[canonical] = None
                return None
            self.source_bytes += info.st_size
            if self.source_bytes > MAX_SOURCE_BYTES:
                raise ValueError("test source scan exceeds aggregate byte limit")
            safe_files.append(test_file)
        names = []
        for test_file in safe_files:
            source = strip_go_noncode(test_file.read_text(encoding="utf-8", errors="replace"))
            names.extend(re.findall(r"^\s*func\s+(Test[A-Za-z0-9_]+)\s*\(", source,
                                    re.MULTILINE))
        self.names[canonical] = names
        return names


def audit_command(task, source_cache=None):
    command = task.get("proof_cmd")
    if not isinstance(command, str) or not command.strip():
        return []
    out = []
    if source_cache is None:
        source_cache = TestSourceCache()
    # Only unconditional successes are named. General pipelines, grep predicates,
    # quoting, command substitutions, and dynamic shell are intentionally skipped.
    if re.search(r"(?:\||\|\|)\s*(?:true|:|echo(?:\s|$)|printf(?:\s|$))(?:\s*(?:[;&]|$))", command):
        out.append(finding(task, "success-masking-pipeline",
                           "terminal pipeline/fallback clause succeeds unconditionally"))
    for branch in re.finditer(r"\bif\b.*?\bthen\b(.*?)\belse\b(.*?)\bfi\b", command,
                              re.DOTALL):
        if all(branch_always_succeeds(part) for part in branch.groups()):
            out.append(finding(task, "both-success-branches",
                               "if and else branches both succeed unconditionally"))

    # Exact, unambiguous `-run TestName` with local, non-recursive static packages.
    # Anything regex-shaped, subtest-shaped, shell-expanded, or dynamic is skipped.
    for invocation in re.finditer(r"(?:^|[;&|]\s*)go\s+test\s+([^;&|]+)", command):
        try:
            words = shlex.split(invocation.group(1), posix=True)
        except ValueError:
            continue
        target = None
        for pos, word in enumerate(words):
            if word == "-run" and pos + 1 < len(words):
                target = words[pos + 1]
                break
            if word.startswith("-run="):
                target = word[5:]
                break
        if target is None:
            continue
        # Never feed backlog input to a regex engine. Only a literal Go test
        # identifier with optional anchors is modeled; all metacharacters skip.
        if literal_run_matches(target, "") is None:
            continue
        packages = [w for w in words if w.startswith("./") and "..." not in w
                    and not any(c in w for c in "*$?[]{}")]
        dirs = [(ROOT / p).resolve() for p in packages]
        if not dirs or not all(d.is_dir() and (d == ROOT or ROOT in d.parents) for d in dirs):
            continue
        names = []
        for directory in dirs:
            package_names = source_cache.package_names(directory)
            if package_names is None:
                names = None
                break
            names.extend(package_names)
        if names is None:
            continue
        exists = any(literal_run_matches(target, name) for name in names)
        if not exists:
            out.append(finding(task, "missing-go-test-target",
                               f"exact -run target {target} does not exist in validated package(s)"))
    return out


def load_export(path):
    if path:
        if os.path.getsize(path) > MAX_INPUT:
            raise ValueError("input exceeds byte limit")
        with open(path, encoding="utf-8") as source:
            return json.load(source)
    helper = os.environ.get("PROOF_CMD_AUDIT_SPEC_CLOUD", str(ROOT / "scripts/spec-cloud.sh"))
    proc = subprocess.Popen([helper, "-sf", "/api/v1/projects/agent-bus/export?format=json"],
                            cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    assert proc.stdout is not None
    raw = proc.stdout.read(MAX_INPUT + 1)
    if len(raw) > MAX_INPUT:
        proc.kill()
        proc.wait()
        raise ValueError("export exceeds byte limit")
    rc = proc.wait()
    if rc:
        raise RuntimeError("Spec export fetch failed")
    return json.loads(raw.decode("utf-8"))


def validate_export(export):
    if not isinstance(export, dict) or set(export).isdisjoint({"tasks"}):
        raise ValueError("input must be an export object containing tasks")
    tasks = export["tasks"]
    if not isinstance(tasks, list) or len(tasks) > MAX_TASKS:
        raise ValueError("tasks must be a list within the task limit")
    for index, task in enumerate(tasks):
        if not isinstance(task, dict):
            raise ValueError(f"task {index} is not an object")
        if task.get("key") is not None and not isinstance(task.get("key"), str):
            raise ValueError(f"task {index} has invalid key")
        for field in ("public_id", "status"):
            if not isinstance(task.get(field), str):
                raise ValueError(f"task {index} has invalid {field}")
        proof = task.get("proof_cmd")
        if proof is not None and not isinstance(proof, str):
            raise ValueError(f"task {index} has invalid proof_cmd")
        if isinstance(proof, str) and len(proof.encode("utf-8")) > MAX_COMMAND:
            raise ValueError(f"task {index} proof_cmd exceeds byte limit")
    return tasks


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", metavar="FILE", help="audit a JSON export fixture")
    parser.add_argument("--json", action="store_true", help="emit stable JSON")
    args = parser.parse_args()
    try:
        export = load_export(args.input)
        tasks = validate_export(export)
        source_cache = TestSourceCache()
        findings = [item for task in tasks for item in audit_command(task, source_cache)]
        if len(findings) > MAX_FINDINGS:
            raise ValueError("finding count exceeds limit")
        findings.sort(key=lambda x: (RANK[x["kind"]], x["task"], x["detail"]))
    except (OSError, ValueError, KeyError, json.JSONDecodeError, RuntimeError) as exc:
        print(f"proof-cmd-audit: error: {exc}", file=sys.stderr)
        return 2
    if args.json:
        rendered = json.dumps({"findings": findings, "finding_count": len(findings)},
                              sort_keys=True, separators=(",", ":"))
    elif findings:
        lines = ["\t".join(safe_text(item[field]) for field in
                           ("severity", "task", "kind", "detail")) for item in findings]
        lines.append(f"proof-cmd-audit: {len(findings)} finding(s)")
        rendered = "\n".join(lines)
    else:
        rendered = "proof-cmd-audit: no findings"
    if len(rendered.encode("utf-8")) > MAX_OUTPUT:
        print("proof-cmd-audit: error: output exceeds byte limit", file=sys.stderr)
        return 2
    print(rendered)
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
