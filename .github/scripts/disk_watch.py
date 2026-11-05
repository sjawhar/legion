#!/usr/bin/env python3
"""Measure the disk a CI step uses and the disk it had when it started, and print the margin
between the two. It exists for envoy-smoke (dispatch://INFRA-1085), whose live tests build
container images on the runner's disk: nothing in that job's log says how close the step came to
filling it, so a run that fails for space says only that it ran out, never how much it was short
by or how much room the step ever had.

Two steps of the job run it, around the step being measured:

    .github/scripts/disk_watch.py start --path /tmp --state "$RUNNER_TEMP/disk-watch.json"
    ... the step whose disk is measured ...
    .github/scripts/disk_watch.py report --state "$RUNNER_TEMP/disk-watch.json"

`start` prints what each file system the job writes to holds and has free - the watched path's,
the Go caches', Docker's data root's - writes the watched file system's free kB into the state
file as the run's "before" number, and leaves a detached sampler behind that re-reads that number
every second and keeps the least it has seen. `report` stops the sampler and prints one line:

    disk: before <N> kB, least <M> kB, used <N-M> kB, margin <ok|short>

where `ok` means the step ended with at least as much free disk as it used, that is: it started
with at least twice the disk it went on to need.

The state crosses the two steps in a file rather than in $GITHUB_ENV because a step of this job
that writes $GITHUB_ENV or $GITHUB_PATH makes ci_filter_coverage.py's job_reason non-empty for
the whole job, which unreduces every go command of envoy-smoke and breaks the envoy_smoke gate
in .github/scripts/ci_job_reads.py. The job's docker-plugin step crosses steps through
$GITHUB_OUTPUT and a step env: for the same reason.

The report prints and never fails the job: it exits 0 for `margin short` as it does for
`margin ok`, and a missing or unreadable state file prints a line saying so and exits 0 too.
This change is the measurement, nothing else, so no run may go red because of it.

`sample` is the detached sampler's own mode, started by `start` and ended by `report`. It is not
for a workflow to call.
"""

import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import time

# How often the sampler re-reads the free space. The steps it watches run for minutes, so a
# second is often enough to catch the trough and rare enough to cost nothing: a sample that
# finds nothing changed writes nothing at all.
SAMPLE_SECONDS = 1.0

# The sampler gives up after this long on its own, so a job that is cancelled between the two
# steps - which leaves no `report` to stop it - leaves nothing running behind it either. The
# envoy-smoke job's own timeout is 20 minutes.
SAMPLER_MAX_SECONDS = 3600.0

# How long `report` waits for the sampler to go after SIGTERM before printing anyway. The sampler
# holds no lock and writes atomically, so a straggler can only lower a number that is already
# written.
STOP_SECONDS = 2.0

# How long `go env` or `docker info` may take before start gives up on it and measures what it
# can. Neither is the measurement, only a way of naming one more directory.
TOOL_SECONDS = 20.0


def total_free_kb(directory: str) -> tuple[int, int]:
    """The total and the free kB of the file system holding `directory`, read from the kernel
    (shutil.disk_usage, which is os.statvfs here), not parsed out of df. Free is what an
    unprivileged process may still write: df's "Available", which is what a CI step actually
    has, and which is the number disk_usage reports as free."""
    usage = shutil.disk_usage(directory)
    return usage.total // 1024, usage.free // 1024


def free_kb(directory: str) -> int:
    return total_free_kb(directory)[1]


def mount_point(directory: str) -> str:
    """Where the file system holding `directory` is mounted: the highest directory above it that
    is still on the same device."""
    here = os.path.realpath(directory)
    while not os.path.ismount(here):
        here = os.path.dirname(here)
    return here


def tool_says(argv: list[str]) -> str | None:
    """One line of output from a tool that may not be there: None when the program is not on
    PATH, when it fails, when it takes too long or when it says nothing, with a line saying
    which. A runner without Go, or with no Docker daemon answering, is not a failure here: the
    directories that tool would have named go unmeasured and the rest are measured."""
    spelled = " ".join(argv)
    if shutil.which(argv[0]) is None:
        print(f"disk: no {argv[0]} on PATH, so `{spelled}` names no directory")
        return None
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=TOOL_SECONDS)
    except (OSError, subprocess.SubprocessError) as problem:
        print(f"disk: `{spelled}` did not answer ({problem}), so it names no directory")
        return None
    answer = done.stdout.strip()
    if done.returncode != 0 or not answer:
        print(f"disk: `{spelled}` said nothing (status {done.returncode}), so it names no directory")
        return None
    return answer


def tool_directories() -> list[str]:
    """The directories the job's other big writers use, as their own tools name them: the Go
    build and module caches, and Docker's data root, where the images the live tests build go."""
    found = []
    for name in ("GOCACHE", "GOMODCACHE"):
        said = tool_says(["go", "env", name])
        if said:
            found.append(said)
    said = tool_says(["docker", "info", "--format", "{{.DockerRootDir}}"])
    if said:
        found.append(said)
    return found


def print_space(directories: list[str]) -> None:
    """What df -k would print for the file systems holding `directories`, one line per distinct
    file system, naming the directories on it. A directory that cannot be reached - a cache its
    tool names before anything has written it, say - is named as such and skipped."""
    systems: dict[int, dict] = {}
    for directory in directories:
        try:
            device = os.stat(directory).st_dev
            total, free = total_free_kb(directory)
            mount = mount_point(directory)
        except OSError as problem:
            print(f"disk: {directory} cannot be read ({problem.strerror}), skipped")
            continue
        found = systems.get(device)
        if found is None:
            systems[device] = {"dirs": [directory], "mount": mount, "total": total, "free": free}
        else:
            found["dirs"].append(directory)
    for system in systems.values():
        print(
            f"disk: {', '.join(system['dirs'])} on {system['mount']}: "
            f"total {system['total']} kB, free {system['free']} kB"
        )


def read_state(state_path: str) -> dict | None:
    """The state the sampler and the report share, or None when it is not there or not JSON."""
    try:
        with open(state_path, encoding="utf-8") as handle:
            state = json.load(handle)
    except (OSError, ValueError):
        return None
    return state if isinstance(state, dict) else None


def write_state(state_path: str, state: dict) -> None:
    """Write the state whole: into a temporary file beside it, then rename over it, so a reader
    between two samples sees the state before or the state after, never half of either."""
    temporary = f"{state_path}.{os.getpid()}.tmp"
    with open(temporary, "w", encoding="utf-8") as handle:
        json.dump(state, handle)
        handle.write("\n")
    os.replace(temporary, state_path)


def start(path: str, state_path: str) -> int:
    """Print what the job's file systems hold, write the "before" number, and leave a sampler
    watching `path`."""
    try:
        before = free_kb(path)
    except OSError as problem:
        print(f"disk: {path} cannot be read ({problem.strerror}), nothing to watch")
        return 0
    print_space([path] + tool_directories())
    # The sampler is started before the state is written: it sleeps a second before its first
    # read and skips a state file that is not there yet, so one write, with its pid in it, is
    # all the file ever needs.
    sampler = subprocess.Popen(
        [sys.executable, os.path.abspath(__file__), "sample", "--path", path, "--state", state_path],
        # The sampler outlives this step, so it must hold none of the step's pipes: a child still
        # holding stdout keeps the runner waiting for an end of file that never comes.
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    write_state(
        state_path,
        {"path": path, "before_kb": before, "least_kb": before, "sampler_pid": sampler.pid},
    )
    print(f"disk: watching {path}, free {before} kB, sampler pid {sampler.pid}")
    return 0


def sample(path: str, state_path: str) -> int:
    """The detached sampler: keep the least free space `path`'s file system has had, until the
    report stops this process or the deadline above passes."""
    deadline = time.monotonic() + SAMPLER_MAX_SECONDS
    while time.monotonic() < deadline:
        time.sleep(SAMPLE_SECONDS)
        state = read_state(state_path)
        if state is None:
            continue
        try:
            free = free_kb(path)
        except OSError:
            continue
        try:
            least = min(int(state["least_kb"]), free)
        except (KeyError, TypeError, ValueError):
            least = free
        # Nearly every sample finds the file already saying this, and the file is on the very
        # file system being measured, so a sample that would rewrite the same bytes writes
        # nothing.
        if state.get("least_kb") == least:
            continue
        state["least_kb"] = least
        write_state(state_path, state)
    return 0


def stop_sampler(pid) -> None:
    """Stop the sampler named in the state file. A pid that is already gone, or a state file
    naming none, is no failure: the report measures, it does not police."""
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 0:
        return
    try:
        os.kill(pid, signal.SIGTERM)
    except OSError:
        return
    deadline = time.monotonic() + STOP_SECONDS
    while time.monotonic() < deadline:
        try:
            os.kill(pid, 0)
        except OSError:
            return
        time.sleep(0.05)


def report(state_path: str) -> int:
    """Stop the sampler and print the one line this whole script exists for. Always exits 0."""
    state = read_state(state_path)
    if state is None:
        print(f"disk: no state file to read at {state_path}, nothing measured")
        return 0
    stop_sampler(state.get("sampler_pid"))
    final = read_state(state_path) or state
    try:
        before = int(final["before_kb"])
        least = int(final["least_kb"])
    except (KeyError, TypeError, ValueError):
        print(f"disk: the state at {state_path} carries no numbers, nothing measured")
        return 0
    used = before - least
    margin = "ok" if least >= used else "short"
    print(f"disk: before {before} kB, least {least} kB, used {used} kB, margin {margin}")
    return 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="disk_watch.py",
        description="Measure the disk a CI step uses against the disk it started with.",
    )
    # `sample` is in the parser but out of the help: `start` starts it, and nothing else should.
    modes = parser.add_subparsers(dest="mode", required=True, metavar="{start,report}")
    starting = modes.add_parser("start", help="record the free space and start the sampler")
    starting.add_argument("--path", required=True, help="the directory whose file system is watched")
    starting.add_argument("--state", required=True, help="the file the two steps share")
    reporting = modes.add_parser("report", help="stop the sampler and print the margin")
    reporting.add_argument("--state", required=True, help="the file `start` wrote")
    sampling = modes.add_parser("sample")
    sampling.add_argument("--path", required=True)
    sampling.add_argument("--state", required=True)

    args = parser.parse_args(argv)
    if args.mode == "start":
        return start(args.path, args.state)
    if args.mode == "sample":
        return sample(args.path, args.state)
    return report(args.state)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
