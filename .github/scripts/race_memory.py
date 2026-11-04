#!/usr/bin/env python3
"""Run a command and say how much memory its process tree used, failing when the peak passes a
limit. It exists for envoy-go-race-docs (dispatch://INFRA-1085), the job that race-tests the
document service alone: the runner killed it three times for memory, and a job the kernel's
out-of-memory killer takes leaves no number behind to argue with.

The sampler runs beside the command rather than `/usr/bin/time -v` around it because a killed
run must still say something. `go test` buffers a package's output until the package ends, and
`/usr/bin/time -v` prints its peak only after the command returns, so a run the runner kills has
neither: the log stops mid-step with no output at all, which is how this job failed. This script
writes a line every 5 s as the run goes, naming the tests running at that moment and the largest
process of the tree, so the last lines before a kill name what was running and how big it was.

The peak is `VmHWM`, the kernel's own high-water mark of resident memory, because it sees what
happens between two samples; the largest sampled `VmRSS` is the fallback for a kernel that
reports no `VmHWM` (gVisor is one), and it can miss a peak that rose and fell inside one 5-second
gap. peak_memory_kb() says which of the two a run used, and so does the last line of the log.

Usage: race_memory.py <command> [args...], for example

    .github/scripts/race_memory.py go test -json -race -count=1 -timeout 60m ./internal/dispatch/docs/

It has no options of its own: everything after the script name is the command. RACE_MEM_LIMIT_KB
sets the limit in kB, 8388608 (8 GiB) when it is unset or empty. The command's stdout is read
line by line, so a `go test -json` stream is unwrapped back into the test log it carries and the
running tests are known; the command's stderr is not touched and goes straight to this script's.
"""

import json
import os
import subprocess
import sys
import threading
import time
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path

# Half of a 16 GiB runner, which leaves the race binary room to be killed by this script with a
# log rather than by the kernel without one.
DEFAULT_LIMIT_KB = 8388608

SAMPLE_SECONDS = 5.0

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


def peak_memory_kb(statuses: Iterable[str]) -> tuple[int, str]:
    """The peak of the `/proc/<pid>/status` texts sampled over a run, in kB, with the name of the
    number it comes from.

    It is the largest `VmHWM` of any sampled text that has one, because that is the kernel's own
    high-water mark: it holds a peak reached between two samples. gVisor reports no `VmHWM`, so
    where none of the texts has that line the answer is the largest `VmRSS` instead, which is only
    the highest moment this script happened to look at and can miss a higher one between samples.
    An empty run peaks at 0 kB.
    """
    peak_hwm = 0
    peak_rss = 0
    for text in statuses:
        hwm = status_kb(text, "VmHWM")
        if hwm is not None:
            peak_hwm = max(peak_hwm, hwm)
        rss = status_kb(text, "VmRSS")
        if rss is not None:
            peak_rss = max(peak_rss, rss)
    if peak_hwm:
        return peak_hwm, "VmHWM"
    return peak_rss, "VmRSS"


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


def limit_from_env(environ: dict[str, str]) -> tuple[int, str]:
    """The limit in kB and where it came from, for the first line of the log."""
    raw = environ.get("RACE_MEM_LIMIT_KB", "").strip()
    if not raw:
        return DEFAULT_LIMIT_KB, "the default; RACE_MEM_LIMIT_KB is unset"
    return int(raw), "RACE_MEM_LIMIT_KB"


def main(argv: list[str]) -> int:
    if not argv:
        print(
            "race memory: give a command to run, for example: race_memory.py go test -race ./...",
            file=sys.stderr,
        )
        return 2
    try:
        limit_kb, source = limit_from_env(dict(os.environ))
    except ValueError:
        print(
            f"race memory: RACE_MEM_LIMIT_KB is {os.environ['RACE_MEM_LIMIT_KB']!r}, not a number of kB",
            file=sys.stderr,
        )
        return 2

    log = Log()
    log.line(f"race memory: the limit is {limit_kb} kB ({source})")

    try:
        process = subprocess.Popen(argv, stdout=subprocess.PIPE, text=True, bufsize=1)
    except OSError as error:
        print(f"race memory: cannot run {argv[0]}: {error}", file=sys.stderr)
        return 127

    tests = RunningTests()
    reader = threading.Thread(target=pump, args=(process.stdout, tests, log), daemon=True)
    reader.start()

    samples: list[Sample] = []
    # The peak is folded in walk by walk rather than kept as every text sampled: a forty-minute
    # package at one walk every five seconds is hundreds of walks of a tree that is itself
    # hundreds of processes while the race binary compiles, and this script runs inside the
    # memory it measures.
    peak_hwm_kb = 0
    peak_rss_kb = 0
    started = time.monotonic()
    while True:
        walked = tree_statuses(process.pid)
        walk_kb, walk_field = peak_memory_kb(walked)
        if walk_field == "VmHWM":
            peak_hwm_kb = max(peak_hwm_kb, walk_kb)
        else:
            peak_rss_kb = max(peak_rss_kb, walk_kb)
        sample = summarize(walked, time.monotonic() - started, tests.names())
        if sample is not None:
            samples.append(sample)
            log.line(sample.line())
        try:
            process.wait(timeout=SAMPLE_SECONDS)
        except subprocess.TimeoutExpired:
            continue
        break

    reader.join()
    if process.stdout is not None:
        process.stdout.close()
    status = process.wait()

    peak_kb, field = (peak_hwm_kb, "VmHWM") if peak_hwm_kb else (peak_rss_kb, "VmRSS")
    log.line(f"race memory: the peak was {peak_kb} kB ({field}), the limit {limit_kb} kB")

    if peak_kb > limit_kb:
        highest = max(samples, key=lambda sample: sample.rss_kb, default=None)
        where = (
            f"the highest sampled VmRSS, {highest.rss_kb} kB at {highest.seconds:.1f} s, "
            f"had these tests running: {highest.tests}"
            if highest is not None
            else "no sample saw the process tree"
        )
        log.line(
            f"race memory: the peak of {peak_kb} kB passed the limit of {limit_kb} kB; {where}. "
            "VmHWM can be reached between two samples, so the peak may belong to another moment."
        )

    if status != 0:
        # A signalled command has a negative returncode; report it as a shell does.
        return status if status > 0 else 128 - status
    if peak_kb > limit_kb:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
