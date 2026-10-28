"""The engine of check-ci-filters.sh, whose header says what it proves and why. ci_job_reads.py,
beside it, is the data it reads: each ci.yaml gate's go commands and static reads, and the files
tests read outside their packages.

check-ci-filters.sh runs it from the repository root with this directory on PYTHONPATH, through
`import ci_filter_coverage` rather than as a script, so ci_job_reads.py's `from ci_filter_coverage
import ...` gets these very classes.
"""

from __future__ import annotations

import atexit
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

import yaml
from workflow_paths import PathsFilter, events, glob_regex, unread_filter_patterns

WORKFLOW = ".github/workflows/ci.yaml"
REPO = Path.cwd().resolve()
SHOWN = 20  # uncovered files printed per gate


@dataclass(frozen=True)
class Read:
    """A file or glob a gate reads, and why."""

    glob: str
    why: str


@dataclass(frozen=True)
class Run:
    """One go command. `mode` "test" runs the tests (closure with -test, testdata, and RULES
    applied to the files compiled in), "vet" compiles them (closure with -test, nothing runs),
    "build" compiles the packages only. A gate's own runs are pinned to the text that starts them:
    `step`, a step of the gate's job whose run: holds `text`, or `source`, a file that holds it."""

    workdir: str
    pkgs: tuple
    why: str
    tags: str = ""
    mode: str = "test"
    env: tuple = ()  # (("GOOS", "linux"), ...)
    overlay: tuple = ()  # ((replaced file, its replacement), ...), repository-relative
    step: str = ""
    source: str = ""
    text: str = ""


def run(workdir, pkgs, why, *, tags="", mode="test", env=None, overlay=None, step="", source="", text=""):
    if mode not in ("test", "vet", "build"):
        raise ValueError(f"run mode {mode!r}")
    return Run(workdir, tuple(pkgs), why, tags, mode, tuple(sorted((env or {}).items())),
               tuple(sorted((overlay or {}).items())), step, source, text)


@dataclass(frozen=True)
class Gate:
    """A dorny/paths-filter filter of the changes job and what the job (or, with `step`, the one
    step) it gates reads."""

    filter: str
    job: str
    runs: tuple = ()
    reads: tuple = ()
    step: str = ""


def gate(filter, job, runs=(), reads=(), step=""):
    return Gate(filter, job, tuple(runs), tuple(reads), step)


# Every ci.yaml step a why cites through at(), checked to exist so a renamed step fails here.
CITED: list = []


def at(job: str, step: str) -> str:
    """A citation of a ci.yaml step by its name, its `uses:` (without the @ref) or its whole run:
    text."""
    CITED.append((job, step))
    return f'ci.yaml {job} "{step}"'


# ---------------------------------------------------------------- go list
GO_COMMAND = re.compile(r"\bgo\s+(?:-C\s+\S+\s+)?(?:test|vet|build|run|list|install|generate)\b")
FIELDS = ("Dir,ImportPath,ForTest,Standard,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,EmbedPatterns,"
          "TestEmbedPatterns,XTestEmbedPatterns,Error")


@dataclass
class Session:
    """The go commands' environment and results. GOPROXY=off over an empty module cache: no
    download, no rewrite of a go.work.sum, and the same answer on a warm machine as on a cold
    runner. A third-party package then fails to load, which loses nothing first-party
    (check-ci-filters.sh's header says why)."""

    problems: list = field(default_factory=list)
    cache: dict = field(default_factory=dict)
    tmp: str = ""

    def __post_init__(self):
        self.tmp = tempfile.mkdtemp(prefix="check-ci-filters-")
        atexit.register(shutil.rmtree, self.tmp, True)
        os.mkdir(f"{self.tmp}/modcache")

    def go(self, run_: Run, args: list) -> str:
        env = dict(os.environ, GOPROXY="off", GOMODCACHE=f"{self.tmp}/modcache", GOFLAGS="", GOWORK="")
        env.update(dict(run_.env))
        result = subprocess.run(["go", *args], cwd=REPO / run_.workdir, env=env, capture_output=True, text=True)
        if result.returncode != 0:
            self.problems.append(f"::error::in {run_.workdir}, go {' '.join(args)} failed, so the closure of "
                                 f"{run_.why} is unknown: {' '.join(result.stderr.split())[:500]}")
        return result.stdout

    def overlay_args(self, run_: Run) -> list:
        if not run_.overlay:
            return []
        path = Path(self.tmp) / f"overlay-{len(self.cache)}.json"
        path.write_text(json.dumps({"Replace": {str(REPO / a): str(REPO / b) for a, b in run_.overlay}}))
        return ["-overlay", str(path)]

    def listing(self, run_: Run, deps: bool) -> list:
        key = ("list", run_.workdir, run_.pkgs, run_.tags, run_.mode, run_.env, run_.overlay, deps)
        if key not in self.cache:
            args = ["list", "-e", f"-json={FIELDS}"]
            if deps:
                args.append("-deps")
                if run_.mode in ("test", "vet"):
                    args.append("-test")
            if run_.tags:
                args += ["-tags", run_.tags]
            args += self.overlay_args(run_)
            self.cache[key] = decode_stream(self.go(run_, args + list(run_.pkgs)))
        return self.cache[key]

    def module_files(self, run_: Run) -> list:
        """go.work, go.work.sum and each bound module's go.mod and go.sum, or the one module's."""
        key = ("mod", run_.workdir, run_.env)
        if key not in self.cache:
            info = json.loads(self.go(run_, ["env", "-json", "GOWORK", "GOMOD"]) or "{}")
            reads = []
            gowork = info.get("GOWORK", "")
            if gowork and gowork != "off":
                why = f"the workspace of go commands in {run_.workdir} (go env GOWORK)"
                reads += [Read(rel(gowork), why), Read(rel(gowork + ".sum"), why)]
                work = json.loads(self.go(run_, ["work", "edit", "-json", gowork]) or "{}")
                for use in work.get("Use") or []:
                    moddir = (Path(gowork).parent / use["DiskPath"]).resolve()
                    reads += [Read(rel(str(moddir / name)), f"{why}, use {use['DiskPath']}") for name in ("go.mod", "go.sum")]
            elif info.get("GOMOD") and info["GOMOD"] != os.devnull:
                why = f"the module of go commands in {run_.workdir} (go env GOMOD)"
                reads += [Read(rel(info["GOMOD"]), why), Read(rel(str(Path(info["GOMOD"]).parent / "go.sum")), why)]
            self.cache[key] = [r for r in reads if r.glob]
        return self.cache[key]


def decode_stream(text: str) -> list:
    decoder, index, out = json.JSONDecoder(), 0, []
    while True:
        while index < len(text) and text[index].isspace():
            index += 1
        if index >= len(text):
            return out
        obj, index = decoder.raw_decode(text, index)
        out.append(obj)


def rel(path: str) -> str | None:
    try:
        return Path(path).resolve().relative_to(REPO).as_posix()
    except ValueError:
        return None


def embed_reads(dir_rel: str, patterns, why: str) -> list:
    reads = []
    for pattern in patterns or []:
        pattern = pattern.removeprefix("all:")
        if any(c in pattern for c in "*?["):
            parent = os.path.dirname(pattern)
            reads.append(Read(f"{dir_rel}/{parent}/*" if parent else f"{dir_rel}/*", f"{why} (embed {pattern})"))
        elif (REPO / dir_rel / pattern).is_dir():
            reads.append(Read(f"{dir_rel}/{pattern}/**", f"{why} (embed {pattern})"))
        else:
            reads.append(Read(f"{dir_rel}/{pattern}", f"{why} (embed {pattern})"))
    return reads


def pattern_dirs(run_: Run) -> list:
    """A `<root>/...` pattern reads every directory under root that holds .go files, to evaluate
    their build constraints, the packages it then excludes included; testdata, `_` and `.`
    directories, node_modules and nested modules are skipped, as the go command skips them."""
    reads = []
    for pattern in run_.pkgs:
        if not pattern.endswith("..."):
            continue
        root = (REPO / run_.workdir / pattern.removesuffix("...").rstrip("/")).resolve()
        for current, subdirs, names in os.walk(root):
            here = Path(current)
            subdirs[:] = [s for s in subdirs if not (s == "testdata" or s.startswith((".", "_")) or s == "node_modules"
                                                     or (here / s / "go.mod").is_file())]
            if any(n.endswith(".go") for n in names):
                reads.append(Read(f"{rel(current)}/*", f"{run_.why} (the pattern {pattern} reads the directory's .go files)"))
    return reads


def closure(session: Session, rules: dict, run_: Run, seen: set) -> list:
    """Every Read the go command reads, with the closures of the go commands its tests start."""
    if run_ in seen:
        return []
    seen.add(run_)
    reads = list(session.module_files(run_)) + pattern_dirs(run_)
    reads += [Read(b, f"{run_.why} (the overlay's replacement for {a})") for a, b in run_.overlay]
    dirs = set()
    for pkg in session.listing(run_, deps=True):
        if pkg.get("Standard") or not pkg.get("Dir"):
            continue
        d = rel(pkg["Dir"])
        if d is None:
            continue  # the module cache: third-party code
        error = (pkg.get("Error") or {}).get("Err", "")
        if error and "build constraints exclude" not in error:
            session.problems.append(f"::error::{d}: go list cannot load {pkg.get('ImportPath')} for {run_.why}, so its "
                                    f"imports are unknown: {' '.join(error.split())[:300]}")
        if (REPO / d).is_dir() and d not in dirs:
            dirs.add(d)
            reads.append(Read(f"{d}/*", f"{run_.why} (go list -deps)"))
            if run_.mode == "test" and (REPO / d / "testdata").is_dir():
                reads.append(Read(f"{d}/testdata/**", f"{run_.why} (the testdata of a package it tests or compiles)"))
        reads += embed_reads(d, pkg.get("EmbedPatterns"), run_.why)
        if run_.mode == "test" and not pkg.get("ForTest"):
            for name in (pkg.get("GoFiles") or []) + (pkg.get("CgoFiles") or []):
                reads += apply_rules(session, rules, f"{d}/{name}", seen)
    if run_.mode in ("test", "vet"):
        for pkg in session.listing(run_, deps=False):
            d = rel(pkg["Dir"]) if pkg.get("Dir") else None
            if d is None:
                continue
            embeds = (pkg.get("TestEmbedPatterns") or []) + (pkg.get("XTestEmbedPatterns") or [])
            reads += embed_reads(d, embeds, f"{run_.why} (tests)")
            if run_.mode == "test":
                for name in (pkg.get("TestGoFiles") or []) + (pkg.get("XTestGoFiles") or []):
                    reads += apply_rules(session, rules, f"{d}/{name}", seen)
    return reads


def apply_rules(session: Session, rules: dict, source: str, seen: set) -> list:
    reads = []
    for action in rules.get(source, ()):
        reads += closure(session, rules, action, seen) if isinstance(action, Run) else [action]
    return reads


def module_reads(session: Session, files: list, run_: Run) -> tuple:
    """What a claimed command this check does not reduce counts as reading: every Go file of its
    module (the nearest directory at or above the Run's workdir holding a go.mod, any nested
    module's subtree aside), that module's go.mod and go.sum, and its workspace's module files.
    Returns (the module's directory, those paths)."""
    root = (REPO / run_.workdir).resolve()
    while root != REPO and not (root / "go.mod").is_file():
        root = root.parent
    prefix = "" if root == REPO else f"{rel(str(root))}/"
    nested = [f.removesuffix("go.mod") for f in files
              if os.path.basename(f) == "go.mod" and f.startswith(prefix) and f != f"{prefix}go.mod"]
    paths = [f for f in files if f.endswith(".go") and f.startswith(prefix) and not f.startswith(tuple(nested))]
    paths += [f"{prefix}go.mod", f"{prefix}go.sum"] + [read.glob for read in session.module_files(run_)]
    return prefix.rstrip("/") or ".", paths


# ---------------------------------------------------------------- files and filters
def repo_files() -> list:
    """Tracked files and untracked ones git does not ignore, so an uncommitted new file counts
    and a build output, which no diff ever carries, does not."""
    out = []
    for extra in ([], ["--others", "--exclude-standard"]):
        result = subprocess.run(["git", "ls-files", "-z", *extra], cwd=REPO, capture_output=True, text=True)
        if result.returncode != 0:
            raise SystemExit(f"::error::git ls-files failed in {REPO}: {result.stderr.strip()}")
        out += result.stdout.split("\0")
    return sorted({f for f in out if f and (REPO / f).is_file()})


class Expander:
    def __init__(self, files: list):
        self.files = files
        self.known = set(files)
        self.by_parent = defaultdict(list)
        for f in files:
            self.by_parent[os.path.dirname(f)].append(f)

    def expand(self, glob: str) -> list:
        if glob.endswith("/**") and not any(c in glob[:-3] for c in "*?["):
            return [f for f in self.files if f.startswith(glob[:-2])]
        if glob.endswith("/*") and not any(c in glob[:-2] for c in "*?["):
            return list(self.by_parent.get(glob[:-2], []))
        if not any(c in glob for c in "*?["):
            return [glob] if glob in self.known else []
        regex = glob_regex(glob)
        return [f for f in self.files if regex.fullmatch(f)]


def flatten(value) -> list:
    """A filter's patterns, an anchor's nested list flattened into it as dorny/paths-filter does."""
    if isinstance(value, str):
        return [value]
    if isinstance(value, list):
        return [p for item in value for p in flatten(item)]
    if isinstance(value, dict):  # the change-type form: {added|modified|...: pattern(s)}
        return [p for item in value.values() for p in flatten(item)]
    return []


def changes_filters(document: dict, problems: list) -> tuple:
    filters, quantifiers = {}, {}
    steps = ((document.get("jobs") or {}).get("changes") or {}).get("steps") or []
    for step in steps:
        if not str(step.get("uses", "")).startswith("dorny/paths-filter"):
            continue
        options = step.get("with") or {}
        quantifier = str(options.get("predicate-quantifier", "some"))
        if quantifier not in PathsFilter.QUANTIFIERS:
            problems.append(f"::error file={WORKFLOW}::the changes job's paths-filter step {step.get('id')} has "
                            f"predicate-quantifier {quantifier}, which this check does not model")
            continue
        for name, value in (yaml.safe_load(str(options.get("filters", ""))) or {}).items():
            if name in filters:
                problems.append(f"::error file={WORKFLOW}::filter {name} is defined in two paths-filter steps")
            filters[name], quantifiers[name] = flatten(value), quantifier
    if not filters:
        problems.append(f"::error file={WORKFLOW}::no changes job with a dorny/paths-filter step")
    return filters, quantifiers


GATE_REF = re.compile(r"needs\.changes\.outputs\.([A-Za-z0-9_-]+)")


def step_label(step: dict) -> str:
    return str(step.get("name") or step.get("id") or step.get("uses") or str(step.get("run", "")).strip())


def step_matches(step: dict, ref: str) -> bool:
    return ref in (step.get("name"), step.get("id"), str(step.get("uses", "")).split("@")[0],
                   str(step.get("run", "")).strip())


def squash(text: str) -> str:
    return " ".join(text.split())


def go_lines(text: str) -> list:
    """The physical lines of a run: script that hold a go command, comments aside, each with
    whether it is part of a backslash continuation (it ends in one or follows a line that does)."""
    lines = text.splitlines()
    return [(line.strip(), continued(lines, i)) for i, line in enumerate(lines)
            if not line.strip().startswith("#") and GO_COMMAND.search(line)]


def continued(lines: list, i: int) -> bool:
    return lines[i].rstrip().endswith("\\") or (i > 0 and lines[i - 1].rstrip().endswith("\\"))


# The one form of a go command this check reduces: `go test|vet|build`, then relative package paths
# and only these flags. Any other claimed command counts as reading every Go file of its module.
NOT_SIMPLE = "$'\"`|&;()<>*?\\"
PACKAGE = re.compile(r"\.\.?(/[A-Za-z0-9_./-]*)?")
FLAG = re.compile(r"-v|-race|-count=[0-9]+|-timeout=[0-9]+[smh]|-buildvcs=false")
DURATION = re.compile(r"[0-9]+[smh]")
TAGS = re.compile(r"[A-Za-z0-9_,]+")


def reduce_line(line: str) -> tuple:
    """A go line in the one form this check reduces as (packages, tags, mode, ""), any other as
    (None, None, None, the first reason it is not that form)."""
    unreduced = (None, None, None)
    special = next((c for c in line if c in NOT_SIMPLE), None)
    if special:
        return *unreduced, f"it has `{special}`"
    words = line.split()
    if words[0] != "go":
        return *unreduced, f"it starts with `{words[0]}`, not go"
    if len(words) < 2 or words[1] not in ("test", "vet", "build"):
        return *unreduced, f"`{' '.join(words[:2])}` is not go test, vet or build"
    pkgs, tags, index = set(), None, 2
    while index < len(words):
        word, value = words[index], words[index + 1] if index + 1 < len(words) else ""
        if PACKAGE.fullmatch(word):
            pkgs.add(word)
        elif word == "-timeout" and DURATION.fullmatch(value):
            index += 1
        elif word == "-tags" or word.startswith("-tags="):
            given = value if word == "-tags" else word.removeprefix("-tags=")
            if tags is not None or not TAGS.fullmatch(given):
                return *unreduced, f"`-tags {given}` is not its one plain -tags"
            tags, index = given, index + (word == "-tags")
        elif not FLAG.fullmatch(word):
            return *unreduced, f"`{word}` is neither a relative package path nor a flag this check reduces"
        index += 1
    return pkgs, tag_set(tags or ""), words[1], ""


def env_reason(document: dict, job: dict, steps: list) -> str:
    """The first env: block of the workflow, the job or one of the steps that can change what a go
    command compiles (a GO* or CGO_* variable, or an expression), or ''."""
    blocks = [("the workflow's", document.get("env")), ("the job's", job.get("env"))]
    blocks += [(f'step "{step_label(s)}"\'s', s.get("env")) for s in steps]
    for where, env in blocks:
        if env is not None and not isinstance(env, dict):
            return f"{where} env is an expression"
        for name in env or {}:
            if str(name).startswith(("GO", "CGO_")):
                return f"{where} env sets {name}"
    return ""


def tag_set(tags: str) -> frozenset:
    return frozenset(t for t in tags.split(",") if t)


# ---------------------------------------------------------------- the check
def check_table(document: dict, data, filters: dict, files: set, problems: list) -> dict:
    """The table must say what ci.yaml says: every gate a real filter on a real job, every go
    command a job's run: steps start listed, every pin and citation still found. Returns, per
    filter, each claimed command it does not reduce: (Run, the command as the job runs it, why)."""
    jobs = document.get("jobs") or {}
    gates = {g.filter: g for g in data.GATES}
    excluded = dict(data.EXCLUDED)
    for name in sorted(set(gates) & set(excluded)):
        problems.append(f"::error file=.github/scripts/ci_job_reads.py::{name} is both a gate and EXCLUDED")
    gated = defaultdict(set)  # job -> filters its job-level if: names
    for job_id, job in jobs.items():
        refs = [(None, str(job.get("if", "")))] + [(s, str(s.get("if", ""))) for s in job.get("steps") or []]
        for step, condition in refs:
            for name in GATE_REF.findall(condition):
                if step is None:
                    gated[job_id].add(name)
                if name not in gates and name not in excluded:
                    where = f"job {job_id}" + (f' step "{step_label(step)}"' if step else "")
                    problems.append(f"::error file={WORKFLOW}::{where} is gated on filter {name}, which "
                                    f"ci_job_reads.py neither lists in GATES nor EXCLUDES; add the gate with its reads")
    for g in data.GATES:
        if g.filter not in filters:
            problems.append(f"::error file={WORKFLOW}::{g.job} has no filter {g.filter} in the changes job")
        job = jobs.get(g.job)
        if job is None:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::gate {g.filter} names job {g.job}, "
                            f"which {WORKFLOW} does not have")
            continue
        if g.step:
            steps = [s for s in job.get("steps") or [] if step_matches(s, g.step)]
            if len(steps) != 1 or g.filter not in GATE_REF.findall(str(steps[0].get("if", ""))):
                problems.append(f"::error file={WORKFLOW}::gate {g.filter} names step \"{g.step}\" of {g.job}, but "
                                f"no single step of that name has an if: on needs.changes.outputs.{g.filter}")
        elif g.filter not in gated[g.job]:
            problems.append(f"::error file={WORKFLOW}::job {g.job}'s if: does not test needs.changes.outputs.{g.filter}")
    for name in excluded:
        if name not in filters:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::EXCLUDED names filter {name}, which "
                            f"the changes job does not have")

    # Every go command line of a run: step must be claimed by a pinned Run of a gate on that job. A
    # claimed line is reduced only in its one simple form, whose packages, -tags and mode must then
    # be exactly the Run's; any other claimed command is unreduced and falls to its whole module.
    skipped = {j for j, names in gated.items() if names and names <= set(excluded)}
    claims = defaultdict(list)  # (job, step label) -> pinned texts
    owner = {}  # id(Run) -> the gate's filter
    unreduced = defaultdict(list)  # filter -> (Run, the command as the job runs it, why it is not reduced)
    for g in data.GATES:
        for r in g.runs:
            if not (r.step or r.source) or not r.text:
                problems.append(f"::error file=.github/scripts/ci_job_reads.py::gate {g.filter}'s run "
                                f"{r.workdir} {' '.join(r.pkgs)} is pinned to no step or source text")
            if r.step:
                claims[(g.job, r.step)].append(r)
                owner[id(r)] = g.filter
    for job_id, job in jobs.items():
        if job_id in skipped:
            continue
        for step in job.get("steps") or []:
            if "run" not in step:
                continue
            mine = [r for (j, ref), rs in claims.items() if j == job_id and step_matches(step, ref) for r in rs]
            for line, joined in go_lines(str(step["run"])):
                where = f"job {job_id} step \"{step_label(step)}\" runs `{line}`"
                pinned = [r for r in mine if squash(r.text) in squash(line)]
                if not pinned:
                    problems.append(f"::error file={WORKFLOW}::{where}, which no Run in ci_job_reads.py lists; add it "
                                    f"to the job's gate so its closure is checked")
                    continue
                reason = env_reason(document, job, [step]) or ("it is part of a backslash continuation" if joined else "")
                named, tags, mode, reason = reduce_line(line) if not reason else (None, None, None, reason)
                for r in pinned:
                    if reason:
                        unreduced[owner[id(r)]].append((r, where, reason))
                        continue
                    parts = []
                    extra = sorted(named - set(r.pkgs))
                    missing = sorted(set(r.pkgs) - named)
                    if extra:
                        parts.append(f"it also names {' '.join(extra)}")
                    if missing:
                        parts.append(f"it no longer names {' '.join(missing)}")
                    if tags != tag_set(r.tags):
                        parts.append(f"it builds with -tags {','.join(sorted(tags)) or '(none)'}, the Run with "
                                     f"{','.join(sorted(tag_set(r.tags))) or '(none)'}")
                    if mode != r.mode:
                        parts.append(f"its go command is a {mode} run, the Run a {r.mode} run")
                    if parts:
                        problems.append(f"::error file={WORKFLOW}::{where}, which is not the Run's "
                                        f"({' '.join(r.pkgs)}): {'; '.join(parts)}; make the Run in ci_job_reads.py "
                                        f"name exactly the packages, tags and mode the line does, so the closure "
                                        f"checked against the job's filter is the one it compiles")
    for (job_id, ref), rs in claims.items():
        steps = [s for s in (jobs.get(job_id) or {}).get("steps") or [] if step_matches(s, ref)]
        text = squash(" ".join(str(s.get("run", "")) for s in steps))
        for r in rs:
            if not steps or squash(r.text) not in text:
                problems.append(f"::error file=.github/scripts/ci_job_reads.py::job {job_id} step \"{ref}\" no longer "
                                f"runs `{r.text}`; update or remove the Run")
    # A script's go command is held exact rather than reduced: the line holding the pinned text is
    # that text alone, uncontinued, and no env: of the job or any of its steps (the check does not
    # know which starts the script) names a variable go reads. Its variables are the Run author's.
    for g in data.GATES:
        for r in g.runs:
            if not r.source:
                continue
            if not (r.source in files and squash(r.text) in squash((REPO / r.source).read_text())):
                problems.append(f"::error file={r.source}::{r.source} no longer holds `{r.text}`, the go command "
                                f"gate {g.filter} lists; update or remove the Run")
                continue
            job = jobs.get(g.job) or {}
            lines = (REPO / r.source).read_text().splitlines()
            held = [i for i, line in enumerate(lines) if r.text in line]
            reason = env_reason(document, job, job.get("steps") or []) or ("" if held else "its text spans lines")
            for i in held:
                if not reason and lines[i].strip() != r.text:
                    reason = f"its line is `{lines[i].strip()}`, not the text alone"
                elif not reason and continued(lines, i):
                    reason = "it is part of a backslash continuation"
            if reason:
                unreduced[g.filter].append((r, f"{r.source}, which job {g.job} runs, runs `{r.text}`", reason))
    for source in data.RULES:
        if source not in files:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::RULES names {source}, which is not "
                            f"in the repository; update or remove the rule")
    for job_id, ref in CITED:
        if not any(step_matches(s, ref) for s in (jobs.get(job_id) or {}).get("steps") or []):
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::a why cites step \"{ref}\" of job "
                            f"{job_id}, which {WORKFLOW} does not have")
    return unreduced

# The jobs ci-ok.sh holds to success on every run, so its SELECTED_BY maps neither.
ALWAYS_RUN = ("changes", "lint")
# ci-ok.sh's lists of SELECTED_BY jobs whose if: admits only some events: per list, the if: term
# that puts a job on it, what that term admits, and the events where ci-ok.sh accepts its skip.
EVENT_LISTS = {
    "PULL_REQUEST_ONLY": (re.compile(r"github\.event_name\s*==\s*'pull_request'"),
                          "admits only pull_request", "github.event_name == 'pull_request'", "every other event"),
    "NOT_ON_PUSH": (re.compile(r"github\.event_name\s*!=\s*'push'"),
                    "admits no push", "github.event_name != 'push'", "a push"),
}


def check_ci_ok(document: dict, problems: list) -> None:
    """ci-ok.sh fails a job its SELECTED_BY says `changes` selected unless the job succeeded, so
    SELECTED_BY must be exactly {job: the output its if: tests} for every job ci-ok needs but
    changes and lint, and each EVENT_LISTS list exactly the jobs whose if: holds its term."""
    jobs = document.get("jobs") or {}
    ci_ok = jobs.get("ci-ok")
    if ci_ok is None:
        problems.append(f"::error file={WORKFLOW}::no ci-ok job, whose SELECTED_BY this check holds to the jobs' if:s")
        return
    envs = [s["env"] for s in ci_ok.get("steps") or [] if "SELECTED_BY" in (s.get("env") or {})]
    if len(envs) != 1:
        problems.append(f"::error file={WORKFLOW}::ci-ok has {len(envs)} steps that set SELECTED_BY; ci-ok.sh reads "
                        f"it from exactly one")
        return
    try:
        selected_by = json.loads(str(envs[0]["SELECTED_BY"]))
        listed = {name: json.loads(str(envs[0].get(name, ""))) for name in EVENT_LISTS}
    except json.JSONDecodeError as error:
        problems.append(f"::error file={WORKFLOW}::ci-ok's SELECTED_BY, {' or '.join(EVENT_LISTS)} is missing or not "
                        f"JSON: {error}")
        return
    if not (isinstance(selected_by, dict) and all(isinstance(v, str) for v in selected_by.values())):
        problems.append(f"::error file={WORKFLOW}::ci-ok's SELECTED_BY is not a JSON object of job to output name")
        return
    for name, jobs_listed in listed.items():
        if not (isinstance(jobs_listed, list) and all(isinstance(j, str) for j in jobs_listed)):
            problems.append(f"::error file={WORKFLOW}::ci-ok's {name} is not a JSON array of job names")
            return
    needs = ci_ok.get("needs") or []
    needed = [j for j in ([needs] if isinstance(needs, str) else needs) if j not in ALWAYS_RUN]
    expected, list_expected = {}, {name: set() for name in EVENT_LISTS}
    for job_id in needed:
        condition = str((jobs.get(job_id) or {}).get("if", ""))
        outputs = sorted(set(GATE_REF.findall(condition)))
        if len(outputs) != 1:
            problems.append(f"::error file={WORKFLOW}::ci-ok needs {job_id}, whose if: tests "
                            f"{len(outputs)} outputs of changes ({', '.join(outputs) or 'none'}), so ci-ok's "
                            f"SELECTED_BY cannot say what selects it")
            continue
        expected[job_id] = outputs[0]
        for name, (term, _, _, _) in EVENT_LISTS.items():
            if term.search(condition):
                list_expected[name].add(job_id)
    for job_id in sorted(set(needed) | set(selected_by)):
        want, have = expected.get(job_id), selected_by.get(job_id)
        if job_id not in needed:
            problems.append(f"::error file={WORKFLOW}::ci-ok's SELECTED_BY maps {job_id}, which is not a job ci-ok "
                            f"needs other than {' and '.join(ALWAYS_RUN)}; drop it")
        elif want is None or want == have:
            continue
        elif have is None:
            problems.append(f"::error file={WORKFLOW}::ci-ok's SELECTED_BY does not map {job_id}, which ci-ok needs, "
                            f"so ci-ok.sh fails it; add \"{job_id}\": \"{want}\", the output its if: tests")
        else:
            problems.append(f"::error file={WORKFLOW}::ci-ok's SELECTED_BY maps {job_id} to {have}, but its if: tests "
                            f"needs.changes.outputs.{want}")
    for name, (_, admits, text, where) in EVENT_LISTS.items():
        for job_id in sorted(list_expected[name] - set(listed[name])):
            problems.append(f"::error file={WORKFLOW}::ci-ok's {name} lacks {job_id}, whose if: {admits}, so ci-ok.sh "
                            f"fails its skip on {where}")
        for job_id in sorted(set(listed[name]) - list_expected[name]):
            problems.append(f"::error file={WORKFLOW}::ci-ok's {name} lists {job_id}, whose if: has no {text} term, so "
                            f"ci-ok.sh accepts its skip on {where} even when changes selected it")


def check_push_workflows(data, filters: dict, problems: list) -> list:
    """A PUSH_WORKFLOWS workflow does on a push to main what its filter's job does on a pull request
    and a merge group, so its on.push.paths must name exactly the filter's patterns: a pattern only
    the filter has lets a change pass the pull request's build and never reach main's, and one only
    the paths have runs main's for a change no pull request built. Returns the report lines."""
    report = []
    for name, path in data.PUSH_WORKFLOWS.items():
        if name not in filters:
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::PUSH_WORKFLOWS names filter {name}, which "
                            f"the changes job does not have")
            continue
        if not (REPO / path).is_file():
            problems.append(f"::error file=.github/scripts/ci_job_reads.py::PUSH_WORKFLOWS names {path} for filter "
                            f"{name}, which is not in the repository")
            continue
        push = events(yaml.safe_load((REPO / path).read_text()) or {}).get("push")
        paths = push.get("paths") if isinstance(push, dict) else None
        if not (isinstance(paths, list) and paths and all(isinstance(p, str) for p in paths)):
            problems.append(f"::error file={path}::{path} has no on.push.paths list, which must equal {WORKFLOW}'s "
                            f"filter {name}")
            continue
        missing = [p for p in dict.fromkeys(filters[name]) if p not in paths]
        extra = [p for p in dict.fromkeys(paths) if p not in filters[name]]
        for p in missing:
            problems.append(f"::error file={path}::{path}'s on.push.paths lacks {p}, which {WORKFLOW}'s filter {name} "
                            f"has, so a push to main that changes only a file it matches never runs {path}")
        for p in extra:
            problems.append(f"::error file={path}::{path}'s on.push.paths has {p}, which {WORKFLOW}'s filter {name} "
                            f"lacks; add it to the filter or drop it here")
        verdict = "differs from" if missing or extra else "equals"
        report.append(f"{path}: on.push.paths {verdict} filter {name} ({len(set(paths))} patterns)")
    return report


def main() -> int:
    problems: list = []
    document = yaml.safe_load((REPO / WORKFLOW).read_text())
    import ci_job_reads as data  # imported here so its at() citations land in this module's CITED

    filters, quantifiers = changes_filters(document, problems)
    files = repo_files()
    unreduced = check_table(document, data, filters, set(files), problems)
    check_ci_ok(document, problems)
    expander = Expander(files)
    session = Session()
    report = []
    for g in data.GATES:
        if g.filter not in filters:
            continue
        patterns = filters[g.filter]
        negated = [p for p in patterns if p.startswith("!")]
        if negated and quantifiers[g.filter] == "some":
            problems.append(f"::error file={WORKFLOW}::filter {g.filter} has {negated[0]}, a `!` pattern under "
                            f"predicate-quantifier some, where it matches every file outside it")
        if unread_filter_patterns(patterns):
            problems.append(f"::error file={WORKFLOW}::filter {g.filter} has {unread_filter_patterns(patterns)[0]}, "
                            f"a brace picomatch expands and this check does not read")
        whys = defaultdict(list)
        seen: set = set()
        for r in g.runs:
            for read in closure(session, data.RULES, r, seen):
                whys[read.glob].append(read.why)
        for read in g.reads:
            whys[read.glob].append(read.why)
        # An unreduced command counts as reading every Go file of its module, and fails naming the
        # command when its filter misses one.
        held = []
        for r, command, why_not in unreduced.get(g.filter, []):
            module, paths = module_reads(session, files, r)
            why = (f"{command}, which this check does not reduce ({why_not}), so it counts as reading every Go "
                   f"file of module {module}")
            for path in paths:
                whys[path].append(why)
            held.append((f"::error file={WORKFLOW}::{why}; filter {g.filter} must select it on any Go change", paths))
        reason = {}
        for glob, why in whys.items():
            for f in expander.expand(glob):
                reason.setdefault(f, why[0])
        matcher = PathsFilter(patterns, quantifiers[g.filter])
        uncovered = sorted(f for f in reason if not matcher.covers(f))
        where = g.job + (f' step "{g.step}"' if g.step else "")
        report.append(f"{where}: {len(reason)} files, {len(uncovered)} uncovered (filter {g.filter})")
        for f in uncovered[:SHOWN]:
            problems.append(f"::error file={f}::{where} reads {f}, which its filter {g.filter} does not match, so a "
                            f"change to it skips the job: {reason[f]}")
        if len(uncovered) > SHOWN:
            problems.append(f"::error file={WORKFLOW}::{where} reads {len(uncovered) - SHOWN} more files its filter "
                            f"{g.filter} does not match")
        problems += [message for message, paths in held if not set(paths).isdisjoint(uncovered)]
    problems += session.problems
    report += check_push_workflows(data, filters, problems)
    for line in report:
        print(line)
    problems = list(dict.fromkeys(problems))
    for problem in problems:
        print(problem)
    if problems:
        print(f"::error::{len(problems)} CI filter problem(s)")
        return 1
    print(f"every ci.yaml filter matches every file its job reads ({len(data.GATES)} gates), and every "
          f"PUSH_WORKFLOWS workflow's on.push.paths equals its filter ({len(data.PUSH_WORKFLOWS)})")
    return 0
