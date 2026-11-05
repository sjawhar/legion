#!/usr/bin/env python3
"""Run a command and say how much memory the largest process of its tree used, killing that tree
and failing the moment the peak passes a limit. It exists for envoy-go-race-docs
(dispatch://INFRA-1085), the job that race-tests the document service alone: the runner killed it
three times for memory, and a job the kernel's out-of-memory killer takes leaves no number behind
to argue with.

The sampler runs beside the command rather than `/usr/bin/time -v` around it because a killed
run must still say something. `go test` buffers a package's output until the package ends, and
`/usr/bin/time -v` prints its peak only after the command returns, so a run the runner kills has
neither: the log stops mid-step with no output at all, which is how this job failed. This script
writes a line every 5 s as the run goes, naming the tests running at that moment and the largest
process of the tree, so the last lines before a kill name what was running and how big it was.

The number is one process, the largest of the tree, and not a sum over it: one race binary is
what grows, and a total over `go test`, the compiler's children and that binary would hide which
of them it was. The peak is that process's `VmHWM`, the kernel's own high-water mark of resident
memory, because it sees what happens between two samples; the largest sampled `VmRSS` is the
fallback for a kernel that reports no `VmHWM` (gVisor is one), and it can miss a peak that rose
and fell inside one 5-second gap. choose_peak() says which of the two a run used, and so does the
last line of the log. Either number is read from a process that was alive when the walk read it,
so what a test binary grows to in its last seconds is never seen: the next walk finds it gone.

The first walk whose peak passes the limit ends the run there and then: the log names that walk's
tests and the second it was taken, the command's process group is sent SIGTERM and then SIGKILL,
and the script exits 1, so the step goes red while the runner still has room rather than after an
hour of running on. A command that had already ended of its own accord keeps its own status.

Usage: race_memory.py <command> [args...], for example

    .github/scripts/race_memory.py go test -json -race -count=1 -timeout 60m ./internal/dispatch/docs/

It has no options of its own: everything after the script name is the command. RACE_MEM_LIMIT_KB
sets the limit in kB, 8388608 (8 GiB) when it is unset or empty. The command's stdout is read
line by line, so a `go test -json` stream is unwrapped back into the test log it carries and the
running tests are known; the command's stderr is not touched and goes straight to this script's.
"""

import json
import os
import signal
import subprocess
import sys
import threading
import time
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path

# Half of a 16 GiB runner, which leaves the race binary room to be killed by this script with a
# log rather than by the kernel without one.
DEFAULT_LIMIT_KB = 8388608

SAMPLE_SECONDS = 5.0

# How long the command's process group has between SIGTERM and SIGKILL. SIGTERM is what lets
# `go test` and the test binary under it write out what they are holding - the -json lines of the
# tests still running, the race detector's own report - and a 4-vCPU runner swapping nothing needs
# a moment to schedule all of that. One sampling interval is long enough for it and short enough
# that a step the limit has already failed does not sit there.
TERM_GRACE_SECONDS = SAMPLE_SECONDS

# The -json actions that end a test. "cont" and "pause" leave it running; "output" carries the
# text and changes nothing.
DONE_ACTIONS = frozenset({"pass", "fail", "skip"})


def status_field(text: str, field: str) -> str | None:
    """The value of one `/proc/<pid>/status` field, or None when the kernel gives no such line."""
    head = field + ":"
    for line in text.splitlines():
        if line.startswith(head):
            return line[len(head) :].strip()
    return None


def status_kb(text: str, field: str) -> int | None:
    """One `/proc/<pid>/status` size in kB, or None when the kernel gives no such line."""
    value = status_field(text, field)
    if value is None:
        return None
    return int(value.split()[0])


def choose_peak(hwm_kb: int | None, rss_kb: int) -> tuple[int, str]:
    """The peak of a run or of one walk, in kB, with the name of the number it comes from.

    It is the `VmHWM` where there is one, because that is the kernel's own high-water mark: it
    holds a peak reached between two samples. gVisor reports no `VmHWM`, so where there is none
    the answer is the `VmRSS` instead, which is only the highest moment this script happened to
    look at and can miss a higher one between samples. A run that saw neither peaks at 0 kB.
    """
    if hwm_kb:
        return hwm_kb, "VmHWM"
    return rss_kb, "VmRSS"


def tree_statuses(root_pid: int) -> list[str]:
    """The `/proc/<pid>/status` text of the command's own process and of every descendant of it.

    One pass over /proc reads each process's status once: the text carries the parent pid the tree
    is built from (`PPid:`) along with the sizes. A process that exits between the walk and the
    read is skipped, which is the common case under `go test`: the compiler's children come and go
    while the sampler walks.
    """
    texts: dict[int, str] = {}
    children: dict[int, list[int]] = {}
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            text = (entry / "status").read_text()
        except OSError:
            continue
        parent = status_field(text, "PPid")
        if parent is None:
            continue
        pid = int(entry.name)
        texts[pid] = text
        children.setdefault(int(parent), []).append(pid)

    found: list[str] = []
    seen: set[int] = set()
    stack = [root_pid]
    while stack:
        pid = stack.pop()
        if pid in seen:
            continue
        seen.add(pid)
        if pid in texts:
            found.append(texts[pid])
        stack.extend(children.get(pid, ()))
    return found


@dataclass(frozen=True)
class Sample:
    """One walk of the tree: when it was taken, the largest sizes it found, the command name of
    the process holding that largest `VmRSS`, and the tests running at that moment."""

    seconds: float
    rss_kb: int
    hwm_kb: int | None
    comm: str
    tests: str

    def line(self) -> str:
        sizes = f"VmRSS {self.rss_kb} kB in {self.comm}"
        if self.hwm_kb is not None:
            sizes += f", VmHWM {self.hwm_kb} kB"
        return f"race memory: at {self.seconds:.1f} s, {sizes}; tests running: {self.tests}"


def summarize(statuses: list[str], seconds: float, tests: str) -> Sample | None:
    """A Sample over one walk's texts, or None when the tree reported nothing: every process of it
    had exited by the time the sampler read their status."""
    biggest_rss = -1
    comm = "?"
    peak_hwm: int | None = None
    for text in statuses:
        rss = status_kb(text, "VmRSS")
        if rss is not None and rss > biggest_rss:
            biggest_rss = rss
            comm = status_field(text, "Name") or "?"
        hwm = status_kb(text, "VmHWM")
        if hwm is not None:
            peak_hwm = hwm if peak_hwm is None else max(peak_hwm, hwm)
    if biggest_rss < 0:
        return None
    return Sample(seconds=seconds, rss_kb=biggest_rss, hwm_kb=peak_hwm, comm=comm, tests=tests)


class RunningTests:
    """The tests the `go test -json` stream says are running, kept by the thread reading that
    stream and read by the thread sampling memory."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._running: set[str] = set()

    def record(self, event: dict) -> None:
        """Fold one -json event in. A package-level event carries no `Test` and changes nothing."""
        name = event.get("Test")
        if not name:
            return
        action = event.get("Action")
        with self._lock:
            if action == "run":
                self._running.add(name)
            elif action in DONE_ACTIONS:
                self._running.discard(name)

    def names(self) -> str:
        """The running tests, sorted, or `none`."""
        with self._lock:
            running = sorted(self._running)
        return ", ".join(running) if running else "none"


class Log:
    """stdout, written by the reader thread and the sampler both, one whole write at a time."""

    def __init__(self) -> None:
        self._lock = threading.Lock()

    def write(self, text: str) -> None:
        with self._lock:
            sys.stdout.write(text)
            sys.stdout.flush()

    def line(self, text: str) -> None:
        self.write(text + "\n")


def pump(stream: Iterable[str], tests: RunningTests, log: Log) -> None:
    """Pass the command's stdout through, unwrapping a `go test -json` stream as it goes.

    A line that parses as a JSON object with an `Action` key is a -json event: its `Output`, when
    it has one, is written as it stands (it carries its own newline) so the test log reads as it
    would without -json, and the event itself tells the sampler which tests are running. Any other
    line is the command's own and is written whole.
    """
    for line in stream:
        event = None
        try:
            parsed = json.loads(line)
        except ValueError:
            parsed = None
        if isinstance(parsed, dict) and "Action" in parsed:
            event = parsed
        if event is None:
            log.write(line)
            continue
        tests.record(event)
        output = event.get("Output")
        if output:
            log.write(output)


def limit_from_env(environ: Mapping[str, str]) -> tuple[int, str]:
    """The limit in kB and where it came from, for the first line of the log.

    A value that is not a number of kB raises ValueError naming that value, which is the message
    the caller prints.
    """
    raw = environ.get("RACE_MEM_LIMIT_KB", "")
    if not raw.strip():
        return DEFAULT_LIMIT_KB, "the default; RACE_MEM_LIMIT_KB is unset"
    try:
        return int(raw), "RACE_MEM_LIMIT_KB"
    except ValueError:
        raise ValueError(f"RACE_MEM_LIMIT_KB is {raw!r}, not a number of kB") from None


def stop_command(process: subprocess.Popen, log: Log) -> None:
    """Stop the command and everything it started: SIGTERM to its process group, then SIGKILL to
    whatever is still there after the grace above.

    The command has a session of its own (`start_new_session=True` in main), so the group holds it
    and its descendants and not this script, which lives on to write the lines that follow. The
    SIGKILL goes out whether or not the command itself went, because a descendant still holding
    the command's stdout would otherwise keep the reader thread waiting for an end of file that
    never comes.
    """
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except OSError:
        # The whole group has gone already: it ended between the walk and this signal.
        return
    try:
        process.wait(timeout=TERM_GRACE_SECONDS)
    except subprocess.TimeoutExpired:
        log.line(
            f"race memory: the command was still running {TERM_GRACE_SECONDS:.0f} s after "
            "SIGTERM, so its process group is being killed"
        )
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except OSError:
        pass


def main(argv: list[str]) -> int:
    if not argv:
        print(
            "race memory: give a command to run, for example: race_memory.py go test -race ./...",
            file=sys.stderr,
        )
        return 2
    try:
        limit_kb, source = limit_from_env(os.environ)
    except ValueError as problem:
        print(f"race memory: {problem}", file=sys.stderr)
        return 2

    log = Log()
    log.line(f"race memory: the limit is {limit_kb} kB ({source})")

    try:
        process = subprocess.Popen(
            # A session of its own, so that a peak over the limit can be answered by killing the
            # command and every process it started, and this script is not in that group.
            argv,
            stdout=subprocess.PIPE,
            text=True,
            bufsize=1,
            start_new_session=True,
        )
    except OSError as error:
        print(f"race memory: cannot run {argv[0]}: {error}", file=sys.stderr)
        return 127

    tests = RunningTests()
    reader = threading.Thread(target=pump, args=(process.stdout, tests, log), daemon=True)
    reader.start()

    # The peak and the highest sample are folded in walk by walk rather than kept as every text
    # sampled: a forty-minute package at one walk every five seconds is hundreds of walks of a
    # tree that is itself hundreds of processes while the race binary compiles, and this script
    # runs inside the memory it measures. The Sample already carries both of the walk's numbers,
    # so nothing is read twice.
    peak_hwm_kb = 0
    highest: Sample | None = None
    over: Sample | None = None
    started = time.monotonic()
    while True:
        walked = tree_statuses(process.pid)
        sample = summarize(walked, time.monotonic() - started, tests.names())
        if sample is not None:
            peak_hwm_kb = max(peak_hwm_kb, sample.hwm_kb or 0)
            # Strictly greater, so the first of equally high samples is the one kept.
            if highest is None or sample.rss_kb > highest.rss_kb:
                highest = sample
            log.line(sample.line())
            walk_kb, walk_field = choose_peak(sample.hwm_kb, sample.rss_kb)
            if walk_kb > limit_kb:
                over = sample
                log.line(
                    f"race memory: at {sample.seconds:.1f} s the peak of {walk_kb} kB "
                    f"({walk_field}) passed the limit of {limit_kb} kB; these tests were "
                    f"running: {sample.tests}. Stopping the command. VmHWM can be reached "
                    "between two samples, so the peak itself may belong to an earlier moment."
                )
                stop_command(process, log)
                break
        try:
            process.wait(timeout=SAMPLE_SECONDS)
        except subprocess.TimeoutExpired:
            continue
        break

    reader.join()
    if process.stdout is not None:
        process.stdout.close()
    status = process.wait()

    peak_kb, field = choose_peak(peak_hwm_kb, highest.rss_kb if highest is not None else 0)
    log.line(f"race memory: the peak was {peak_kb} kB ({field}), the limit {limit_kb} kB")

    if status > 0:
        # The command ended of its own accord, and its own status says more than the limit does:
        # a test that failed has named itself in the log above.
        return status
    if over is not None:
        return 1
    if status < 0:
        # Signalled by something other than this script - the kernel's out-of-memory killer, say.
        # A signalled command has a negative returncode; report it as a shell does.
        return 128 - status
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
