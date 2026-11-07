#!/usr/bin/env python3
"""Rank the tests and packages of a `go test -json` stream, and compare two streams.

ci.yaml's envoy-go job writes one stream per `go test` step to $RUNNER_TEMP/go-test-json and
uploads the directory as `envoy-go-test-json-<run attempt>` (dispatch://INFRA-1075). This reads
those streams, from .json files, a directory of them, or the artifact's .zip, and prints:

  * every package's seconds, slowest first;
  * every leaf test's seconds, slowest first. A leaf is a test with no subtests of its own: no
    other test of its package is named `<this test>/<something>`. Its time is the Elapsed of its
    own pass, fail or skip event, which for a parallel test is wall time under contention, not
    CPU time;
  * with --bars, only the packages and leaf tests over a bar, which is what the pull request's
    exception list is read against.

  --compare <baseline> <head> prints what the two streams disagree about: a (package, test) that
  passed at the baseline and does not pass at the head, one that skips at the head and did not at
  the baseline, and the tests each stream alone holds. A test the head alone holds is another
  change's, which the comparison attributes rather than counting against this one.

It reports and nothing in CI runs it: a gate on test times is the next rung of STRATEGY.md's
enforcement ladder, and nothing has measured an instruction failing here yet.

  .github/scripts/go-test-times.py artifact.zip
  .github/scripts/go-test-times.py --bars "$RUNNER_TEMP/go-test-json"
  .github/scripts/go-test-times.py --compare baseline.zip head.zip
"""

import argparse
import json
import sys
import zipfile
from dataclasses import dataclass, field
from pathlib import Path

# go test -json's terminal actions for a test or a package.
TERMINAL = ("pass", "fail", "skip")


@dataclass
class Stream:
    """What one or more `go test -json` streams say about each package and each test."""

    # (package, test) -> its last terminal action
    status: dict = field(default_factory=dict)
    # (package, test) -> the Elapsed of that action, in seconds
    elapsed: dict = field(default_factory=dict)
    # package -> its last terminal action
    package_status: dict = field(default_factory=dict)
    # package -> the Elapsed of that action, in seconds
    package_elapsed: dict = field(default_factory=dict)

    def add(self, event: dict) -> None:
        action, package = event.get("Action"), event.get("Package")
        if action not in TERMINAL or not package:
            return
        test = event.get("Test")
        seconds = float(event.get("Elapsed") or 0.0)
        if test:
            self.status[(package, test)] = action
            self.elapsed[(package, test)] = seconds
        else:
            self.package_status[package] = action
            self.package_elapsed[package] = seconds

    def leaves(self) -> dict:
        """(package, test) -> seconds, for the tests with no subtests of their own."""
        parents = set()
        for package, test in self.status:
            parts = test.split("/")
            for depth in range(1, len(parts)):
                parents.add((package, "/".join(parts[:depth])))
        return {key: self.elapsed[key] for key in self.status if key not in parents}


def read_lines(path: Path):
    """Every line of one `go test -json` stream: a .json file, a directory of them, or a .zip."""
    if path.is_dir():
        for entry in sorted(path.rglob("*.json")):
            yield from read_lines(entry)
    elif path.suffix == ".zip":
        with zipfile.ZipFile(path) as archive:
            for name in sorted(archive.namelist()):
                if name.endswith(".json"):
                    with archive.open(name) as handle:
                        for line in handle:
                            yield line.decode("utf-8", "replace")
    else:
        with path.open(encoding="utf-8", errors="replace") as handle:
            yield from handle


def load(paths) -> Stream:
    stream = Stream()
    for path in paths:
        where = Path(path)
        if not where.exists():
            sys.exit(f"go-test-times.py: {path} does not exist")
        for line in read_lines(where):
            line = line.strip()
            if not line.startswith("{"):
                continue  # a go command's own output, which test2json passes through
            try:
                stream.add(json.loads(line))
            except json.JSONDecodeError:
                continue
    return stream


def report(stream: Stream, leaf_bar: float, package_bar: float, top: int, bars_only: bool) -> int:
    packages = sorted(stream.package_elapsed.items(), key=lambda row: -row[1])
    leaf_seconds = stream.leaves()
    leaves = sorted(leaf_seconds.items(), key=lambda row: -row[1])
    if not packages and not leaves:
        sys.exit("go-test-times.py: the stream holds no test or package result")

    over_packages = [row for row in packages if row[1] > package_bar]
    over_leaves = [row for row in leaves if row[1] > leaf_bar]

    print(f"packages: {len(packages)}, summing to {sum(stream.package_elapsed.values()):.1f} s")
    print(f"leaf tests: {len(leaves)}, summing to {sum(leaf_seconds.values()):.1f} s")
    print(f"over the {package_bar:g} s package bar: {len(over_packages)}")
    print(f"over the {leaf_bar:g} s leaf-test bar: {len(over_leaves)}")

    shown_packages = over_packages if bars_only else packages[:top]
    print(f"\n{'seconds':>9}  package")
    for package, seconds in shown_packages:
        print(f"{seconds:9.1f}  {package}  [{stream.package_status[package]}]")

    shown_leaves = over_leaves if bars_only else leaves[:top]
    print(f"\n{'seconds':>9}  leaf test")
    for (package, test), seconds in shown_leaves:
        print(f"{seconds:9.2f}  {package} {test}  [{stream.status[(package, test)]}]")
    return 0


def compare(baseline: Stream, head: Stream) -> int:
    regressed = sorted(
        key for key, action in baseline.status.items()
        if action == "pass" and head.status.get(key) != "pass"
    )
    newly_skipped = sorted(
        key for key, action in head.status.items()
        if action == "skip" and baseline.status.get(key) not in (None, "skip")
    )
    only_head = sorted(key for key in head.status if key not in baseline.status)
    only_baseline = sorted(key for key in baseline.status if key not in head.status)

    print(f"baseline: {len(baseline.status)} tests in {len(baseline.package_status)} packages")
    print(f"head:     {len(head.status)} tests in {len(head.package_status)} packages")

    print(f"\npassed at the baseline and does not pass at the head: {len(regressed)}")
    for package, test in regressed:
        print(f"  {package} {test}  [{head.status.get((package, test), 'absent')}]")

    print(f"\nskips at the head and did not at the baseline: {len(newly_skipped)}")
    for package, test in newly_skipped:
        print(f"  {package} {test}")

    print(f"\nat the head only (another change's, or renamed): {len(only_head)}")
    for package, test in only_head:
        print(f"  {package} {test}")

    print(f"\nat the baseline only (removed, renamed, or never ran): {len(only_baseline)}")
    for package, test in only_baseline:
        print(f"  {package} {test}")
    return 1 if regressed or newly_skipped else 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("paths", nargs="*", metavar="PATH",
                        help="a go test -json file, a directory of them, or the artifact's .zip")
    parser.add_argument("--compare", nargs=2, metavar=("BASELINE", "HEAD"),
                        help="print what the two streams disagree about; exits 1 when they do")
    parser.add_argument("--leaf-bar", type=float, default=1.0,
                        help="the leaf-test bar in seconds (default 1)")
    parser.add_argument("--package-bar", type=float, default=60.0,
                        help="the package bar in seconds (default 60)")
    parser.add_argument("--top", type=int, default=30,
                        help="how many rows each table shows without --bars (default 30)")
    parser.add_argument("--bars", action="store_true",
                        help="show only the packages and leaf tests over a bar")
    args = parser.parse_args(argv)

    if args.compare:
        if args.paths:
            parser.error("--compare takes its two streams itself; pass no other path")
        return compare(load([args.compare[0]]), load([args.compare[1]]))
    if not args.paths:
        parser.error("pass a go test -json file, a directory of them, or an artifact .zip")
    return report(load(args.paths), args.leaf_bar, args.package_bar, args.top, args.bars)


if __name__ == "__main__":
    sys.exit(main())
