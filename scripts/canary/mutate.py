#!/usr/bin/env python3
"""Inject a controlled change into a repository clone, for the regression canary.

The canary's question is not "does vouch run", it is "does vouch catch a
regression it is supposed to catch, and stay quiet about changes it should not
flag". Answering that needs a change whose correct verdict is known in advance,
so each arm below has exactly one expected outcome:

    baseline    no change              -> VERIFIED (nothing to break)
    regression  a test that must fail  -> BROKEN
    harmless    a comment line         -> not BROKEN
    removed     delete a passing test  -> VERIFIED, with a removed-tests warning

Usage:
    mutate.py --arm regression --repo /path/to/clone

Exit codes:
    0  the change was applied
    3  this repo's layout is not supported (caller records SKIP, does not count
       it as a failure -- an injector that cannot find a test file says so
       instead of silently testing nothing)
    2  bad arguments / refused for safety
"""

from __future__ import annotations

import argparse
import os
import re
import sys
from pathlib import Path

SKIP = 3

# Marker used in every injected construct so an accidental leftover is greppable
# and can never be mistaken for the project's own code.
MARK = "vouch_canary_injected"


def fail(msg: str, code: int = SKIP) -> None:
    print(f"mutate: {msg}", file=sys.stderr)
    sys.exit(code)


def guard(repo: Path) -> None:
    """Refuse to touch anything but a throwaway clone.

    The injector rewrites source files, so pointing it at a working tree would
    destroy uncommitted work. Two independent conditions must hold: the target
    is a git repo, and it is not this project.
    """
    if not (repo / ".git").exists():
        fail(f"{repo} is not a git repository", 2)
    here = Path(__file__).resolve().parents[2]
    if repo.resolve() == here:
        fail("refusing to mutate the vouch working tree itself", 2)
    if (repo / "go.mod").exists():
        mod = (repo / "go.mod").read_text(errors="replace")
        if "junqingyongyuanbusi/vouch" in mod:
            fail("refusing to mutate the vouch repository", 2)


# --- ecosystem detection ----------------------------------------------------

SKIP_DIRS = {
    ".git", "vendor", "node_modules", ".venv", "venv", "dist", "build",
    "testdata", "third_party", ".tox", "__pycache__", "target",
}


def walk(repo: Path, match) -> list[Path]:
    found = []
    for root, dirs, files in os.walk(repo):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS and not d.startswith(".")]
        for name in files:
            if match(name):
                found.append(Path(root) / name)
    # Deterministic order: the same repo must produce the same injection every
    # run, or a rate table cannot be compared across runs.
    return sorted(found)


def detect(repo: Path) -> str:
    if (repo / "go.mod").exists():
        return "go"
    if (repo / "package.json").exists():
        return "js"
    if (repo / "pyproject.toml").exists() or (repo / "setup.py").exists() or (repo / "setup.cfg").exists():
        return "python"
    fail("no go.mod / package.json / pyproject.toml -- unsupported layout")
    raise AssertionError("unreachable")


def test_files(repo: Path, eco: str) -> list[Path]:
    if eco == "go":
        files = walk(repo, lambda n: n.endswith("_test.go"))
        # Multi-module repos (golang/example is one) have nested go.mod files,
        # and the root module's `go test ./...` never reaches them. Injecting
        # there produces a failing test nothing runs, which would be scored as
        # a missed regression when in fact nothing was ever asked to look.
        nested = {p.parent for p in repo.glob("*/**/go.mod")} | {p.parent for p in repo.glob("*/go.mod")}
        if nested:
            files = [f for f in files
                     if not any(f.is_relative_to(n) for n in nested)]
        return files
    if eco == "python":
        return walk(repo, lambda n: n.startswith("test_") and n.endswith(".py"))
    # Two JS conventions: co-located `*.test.js` / `*.spec.js`, and a `test/`
    # directory of plain `.js` files (mocha's default, which express and much of
    # the older ecosystem still use). Missing the second one makes the injector
    # skip repos it could perfectly well measure.
    files = walk(repo, lambda n: re.search(r"\.(test|spec)\.[jt]sx?$", n) is not None)
    if files:
        return files
    test_dir = repo / "test"
    if test_dir.is_dir():
        return sorted(p for p in test_dir.glob("*.js") if p.is_file())
    return []


def source_file(repo: Path, eco: str):
    if eco == "go":
        cands = walk(repo, lambda n: n.endswith(".go") and not n.endswith("_test.go"))
    elif eco == "python":
        cands = walk(repo, lambda n: n.endswith(".py") and not n.startswith("test_"))
    else:
        cands = walk(repo, lambda n: re.search(r"\.[jt]sx?$", n) is not None
                     and not re.search(r"\.(test|spec)\.", n))
    return cands[0] if cands else None


# --- arms -------------------------------------------------------------------

INJECT = {
    "go": (
        "\n\nfunc Test_{mark}(t *testing.T) {{\n"
        "\tt.Fatal(\"{mark}: this failure is injected by the vouch canary\")\n"
        "}}\n"
    ),
    "python": (
        "\n\ndef test_{mark}():\n"
        "    assert False, \"{mark}: this failure is injected by the vouch canary\"\n"
    ),
    # Two JS shapes: files that pull the globals in explicitly (vitest's default
    # config, and any project with `globals: false`) would not compile against a
    # bare `test(...)`, and the arm would then measure "does vouch catch a type
    # error" instead of "does vouch catch a failing test".
    "js": (
        "\n\n{fn}('{mark}', () => {{\n"
        "  throw new Error('{mark}: this failure is injected by the vouch canary');\n"
        "}});\n"
    ),
}

COMMENT = {"go": "// {mark}: harmless comment\n",
           "python": "# {mark}: harmless comment\n",
           "js": "// {mark}: harmless comment\n"}

# Matches the start of the last test declaration, so `removed` deletes a whole
# test rather than leaving a syntactically broken fragment behind.
TEST_START = {
    "go": re.compile(r"^func\s+Test\w*\s*\(", re.M),
    "python": re.compile(r"^def\s+test_\w+\s*\(", re.M),
    "js": re.compile(r"^\s*(it|test)\s*\(", re.M),
}


def js_test_fn(body: str) -> str:
    """Pick a test function the file can actually call.

    A file that imports its test helpers explicitly has no `test` global, so
    appending a bare `test(...)` would be a type error rather than a failing
    test -- the arm would then measure the wrong thing. Prefer a name the file
    already imports or uses.
    """
    m = re.search(r"^import\s*\{([^}]*)\}\s*from\s*['\"]vitest['\"]", body, re.M)
    if m:
        imported = {n.strip() for n in m.group(1).split(",")}
        for name in ("it", "test"):
            if name in imported:
                return name
        fail("vitest import exposes neither `it` nor `test`")
    # No explicit import: globals are in play, so follow whatever the file uses.
    return "it" if re.search(r"^\s*it\s*\(", body, re.M) else "test"


def arm_regression(repo: Path, eco: str) -> str:
    files = test_files(repo, eco)
    if not files:
        fail(f"no {eco} test files found")
    # Pick a file the injection can actually compile into. For Go the first
    # file alphabetically is often `example_test.go`, which holds only
    # ExampleXxx functions and may not import testing -- appending a TestXxx
    # there would be a build error, not a failing test.
    target = None
    for cand in files:
        body = cand.read_text(errors="replace")
        if eco != "go" or re.search(r"^func\s+Test\w*\s*\(", body, re.M):
            target = cand
            break
    if target is None:
        fail(f"no {eco} test file with a runnable test function")
    body = target.read_text(errors="replace")
    fn = js_test_fn(body) if eco == "js" else ""
    target.write_text(body + INJECT[eco].format(mark=MARK, fn=fn))
    return f"appended a failing test to {target.relative_to(repo)}"


def arm_harmless(repo: Path, eco: str) -> str:
    target = source_file(repo, eco)
    if target is None:
        fail(f"no {eco} source file found")
    target.write_text(target.read_text(errors="replace") + "\n" + COMMENT[eco].format(mark=MARK))
    return f"appended a comment to {target.relative_to(repo)}"


def arm_removed(repo: Path, eco: str) -> str:
    """Delete the last test in the first test file that has more than one.

    Removing the only test in a file would leave an empty (and for Go, invalid)
    file, which is a compile error rather than the coverage drop this arm is
    meant to represent.
    """
    for target in test_files(repo, eco):
        body = target.read_text(errors="replace")
        starts = [m.start() for m in TEST_START[eco].finditer(body)]
        if len(starts) < 2:
            continue
        target.write_text(body[: starts[-1]].rstrip() + "\n")
        return f"removed the last test from {target.relative_to(repo)}"
    fail(f"no {eco} test file with more than one test")
    raise AssertionError("unreachable")


ARMS = {
    "baseline": lambda repo, eco: "no change",
    "regression": arm_regression,
    "harmless": arm_harmless,
    "removed": arm_removed,
}


def main() -> None:
    ap = argparse.ArgumentParser(description="inject a canary change into a repo clone")
    ap.add_argument("--arm", required=True, choices=sorted(ARMS))
    ap.add_argument("--repo", required=True, type=Path)
    args = ap.parse_args()

    repo = args.repo
    if not repo.is_dir():
        fail(f"{repo} is not a directory", 2)
    guard(repo)

    eco = detect(repo)
    print(f"mutate: {args.arm} [{eco}] {ARMS[args.arm](repo, eco)}")


if __name__ == "__main__":
    main()
