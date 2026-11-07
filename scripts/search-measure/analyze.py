#!/usr/bin/env python3
"""Check a run's rows against the floors this tree measured, and print the table a person reads.

usage: analyze.py <out.jsonl> [--serial <out-serial.jsonl>]

Recall at 1, 3, 10 and 20, MRR and "absent from the page" per case kind, each beside the
baseline the hybrid route measured on the same cases (BASELINE below); the kind-1 breakdown of
how often the issue's own document comes back while the issue does not; and the latency of the
run, the server's own `took_ms` and the client's wall clock.

Then the check: every floor (FLOORS, ABSENT_CEILING, DEFECT_DEPTHS) with the n it was computed
over, and an exit status. A run that misses a floor exits non-zero naming each floor it missed
and by how much; a run that measured too few cases of a kind to say anything (MINIMUM_N) is void
and exits non-zero saying so, so a handful of cases can never read as a pass.
"""

import argparse
import json
import statistics
import sys
from collections import Counter

KIND_NAME = {
    1: "1 a sentence of the issue's own spec -> that issue",
    3: "3 an answered ask, in its own words -> that ask",
}

# BASELINE is the hybrid route's own numbers on these cases, measured against production on
# 2026-11-05 with meaning search live: recall at 1, 3, 10 and 20, MRR, absent from the top 50,
# and how many cases each was measured over. A number the baseline run did not report is None.
BASELINE = {
    1: {"n": 281, "r1": 0.025, "r3": 0.427, "r10": None, "r20": 0.669, "mrr": None, "absent": 0.263},
    3: {"n": 435, "r1": 0.818, "r3": None, "r10": None, "r20": None, "mrr": None, "absent": None},
}
# BASELINE_SERIAL is that run's serial latency, one request at a time, in milliseconds.
BASELINE_SERIAL = {"p50": 1034, "p95": 1522, "n": 120}

# FLOORS, ABSENT_CEILING and the defect ceiling below are the numbers this tree measured on the
# committed cases, each rounded down (or, for a ceiling, up) to the nearest whole point. They are
# nobody's target: they were read off the run this tree made, so the first run passes them by
# construction. What makes them a check is the run after a change - a floor missed there is a
# regression, where without the floors the same regression would have printed as one more row of
# a table nobody diffed.
FLOORS = {
    1: {"r1": 0.84, "r3": 0.90, "r20": 0.94},
    3: {"r1": 0.80, "r3": 0.84},
}
# ABSENT_CEILING is the share of a kind's cases whose answer is nowhere in the top 50: a ceiling,
# not a floor, since fewer is better.
ABSENT_CEILING = {1: 0.05}
# DEFECT_DEPTHS are the depths the kind-1 defect is held to: the issue's own document on the page
# while the issue itself is not, which is the defect this measurement was made for. Its ceiling is
# exactly zero at every one of them, because the fix is that an issue carries its spec's words in
# its own vector, and a single case defecting means that is no longer true.
DEFECT_DEPTHS = (1, 3, 10, 20, 50)
# COMMITTED is how many descriptors of each kind cases.json carries, which is the most a run can
# measure, and what MINIMUM_N is a share of.
COMMITTED = {1: 278, 3: 435}
# MINIMUM_N is how many cases of a kind a run must have measured for its rates to mean anything:
# nine tenths of the committed descriptors of that kind. A run refuses a case whose text has
# moved since the descriptors were built, and a few such refusals are normal; a run that measured
# a handful measured a broken fixture, a half-loaded corpus or a server that stopped answering,
# and its rates are void rather than a pass.
MINIMUM_N = {kind: count * 9 // 10 for kind, count in COMMITTED.items()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("rows")
    parser.add_argument("--serial", default="", help="a one-request-at-a-time run's rows")
    arguments = parser.parse_args()

    rows = [row for row in read(arguments.rows) if "rank" in row]
    refused = [row for row in read(arguments.rows) if "refused" in row]
    errors = [row for row in read(arguments.rows) if "error" in row]
    print(f"{len(rows)} runs, {len(refused)} cases refused, {len(errors)} errored")
    if errors:
        print("  errors: " + str(Counter(row["error"][:60] for row in errors).most_common(3)))
    print(f"  degraded: {dict(Counter(row.get('degraded') for row in rows))}")

    print("\n== recall, MRR and absence, beside the baseline (keyword+meaning) ==")
    header = f"{'case kind':<48} {'n':>4}  {'R@1':>14} {'R@3':>14} {'R@10':>14} {'R@20':>14} {'MRR':>14} {'absent@50':>14}"
    print(header)
    for kind in sorted(KIND_NAME):
        block([row for row in rows if row["kind"] == kind], KIND_NAME[kind], BASELINE[kind])
    block(rows, "both kinds together", combined())

    kind_one = [row for row in rows if row["kind"] == 1]
    if kind_one:
        print("\n== kind 1: the issue's own document comes back, the issue does not ==")
        for depth in DEFECT_DEPTHS:
            document = sum(1 for row in kind_one if hit(row.get("doc_rank"), depth))
            issue = sum(1 for row in kind_one if hit(row.get("rank"), depth))
            print(f"  @{depth:<3} document {document:>4} ({document / len(kind_one):6.1%})"
                  f"  issue {issue:>4} ({issue / len(kind_one):6.1%})"
                  f"  document but not the issue {defect(kind_one, depth):>4}"
                  f" ({defect(kind_one, depth) / len(kind_one):6.1%})")

        absent = [row for row in kind_one if row["rank"] is None]
        reachable = [row for row in absent if row.get("any_item_of_issue_rank")]
        print(f"\n  kind-1 cases whose issue is absent from the top 50: {len(absent)}"
              f" of {len(kind_one)} ({len(absent) / len(kind_one):.1%})")
        print(f"  of those, cases a reordering of what came back could fix: 0"
              f" (the issue is not in the page; {len(reachable)} have some other item of the issue in it)")

    latency(rows, "this run")
    if arguments.serial:
        serial = [row for row in read(arguments.serial) if "rank" in row]
        latency(serial, "serial, one request at a time")
        print(f"  baseline serial (hybrid, took_ms): p50 {BASELINE_SERIAL['p50']} ms,"
              f" p95 {BASELINE_SERIAL['p95']} ms, n={BASELINE_SERIAL['n']}")

    return check(rows)


def check(rows):
    """The floors, each with the n it was computed over, and what the run owes.

    The numbers are this tree's own measurement (FLOORS), so this passes by construction on the
    run they were read off: a later run failing here is the regression they exist to catch.
    """
    print("\n== floors: the numbers this tree measured, asserted ==")
    missed = []
    for kind in sorted(FLOORS):
        kind_rows = [row for row in rows if row["kind"] == kind]
        if len(kind_rows) < MINIMUM_N[kind]:
            missed.append(f"VOID kind {kind}: {len(kind_rows)} cases measured, below the minimum of"
                          f" {MINIMUM_N[kind]}. A run this small says nothing and is no pass:"
                          f" the committed set holds {COMMITTED[kind]} descriptors of this kind.")
            print(f"  kind {kind}  n={len(kind_rows)}  VOID: below the minimum of {MINIMUM_N[kind]}")
            continue
        for depth in (1, 3, 10, 20):
            if f"r{depth}" not in FLOORS[kind]:
                continue
            missed += bound_line(f"kind {kind}  R@{depth}", share(kind_rows, depth),
                                 "floor", FLOORS[kind][f"r{depth}"], len(kind_rows))
        if kind in ABSENT_CEILING:
            absent = sum(1 for row in kind_rows if row["rank"] is None) / len(kind_rows)
            missed += bound_line(f"kind {kind}  absent@50", absent,
                                 "ceiling", ABSENT_CEILING[kind], len(kind_rows))
        if kind == 1:
            for depth in DEFECT_DEPTHS:
                missed += bound_line(f"kind {kind}  document but not the issue @{depth}",
                                     defect(kind_rows, depth) / len(kind_rows),
                                     "ceiling", 0.0, len(kind_rows))
    if missed:
        print("\n== this run does not pass ==")
        for line_out in missed:
            print(f"  {line_out}")
        return 1
    print("\n  every floor held.")
    return 0


def bound_line(label, measured, sense, bound, n):
    """One rate against the bound beside it, printed with the n it was computed over.

    A floor is the least the rate may be and a ceiling the most, and the answer is the miss as a
    sentence, or nothing at all when the rate holds.
    """
    missed_by = bound - measured if sense == "floor" else measured - bound
    print(f"  {label:<44} {measured:>6.1%} (n={n})  {sense:<7} {bound:>5.1%}"
          f"  {'ok' if missed_by <= 0 else f'MISSED by {missed_by * 100:.1f} points'}")
    if missed_by <= 0:
        return []
    return [f"{label.strip()} is {measured:.1%} (n={n}),"
            f" {'below' if sense == 'floor' else 'above'} its {sense} of {bound:.1%}"
            f" by {missed_by * 100:.1f} points"]


def defect(kind_one, depth):
    """Kind-1 cases whose issue's document is on the page at depth while the issue is not."""
    return sum(1 for row in kind_one
               if hit(row.get("doc_rank"), depth) and not hit(row.get("rank"), depth))


def block(rows, label, baseline):
    if not rows:
        return
    n = len(rows)
    measured = {
        "r1": share(rows, 1), "r3": share(rows, 3), "r10": share(rows, 10), "r20": share(rows, 20),
        "mrr": sum(1 / row["rank"] for row in rows if row["rank"]) / n,
        "absent": sum(1 for row in rows if row["rank"] is None) / n,
    }
    cells = "".join(f" {beside(measured[name], baseline.get(name)):>14}"
                    for name in ("r1", "r3", "r10", "r20", "mrr", "absent"))
    print(f"{label:<48} {n:>4} {cells}")


def beside(value, baseline):
    """A measured share with the baseline's own beside it."""
    if baseline is None:
        return f"{value:.1%} (—)"
    return f"{value:.1%} ({baseline:.1%})"


def combined():
    """The baseline over the kinds together, weighted by each kind's own count."""
    total = sum(BASELINE[kind]["n"] for kind in BASELINE)
    out = {"n": total}
    for name in ("r1", "r3", "r10", "r20", "mrr", "absent"):
        values = [(BASELINE[kind]["n"], BASELINE[kind][name]) for kind in BASELINE]
        if any(value is None for _, value in values):
            out[name] = None
            continue
        out[name] = sum(n * value for n, value in values) / total
    return out


def share(rows, depth):
    return sum(1 for row in rows if hit(row["rank"], depth)) / len(rows)


def hit(rank, depth):
    return rank is not None and rank <= depth


def latency(rows, label):
    print(f"\n== latency: {label} ==")
    for column in ("took_ms", "wall_ms"):
        line(rows, column, "every case")
    for kind in sorted(KIND_NAME):
        line([row for row in rows if row["kind"] == kind], "took_ms", f"kind {kind} only")


def line(rows, column, label):
    values = sorted(row[column] for row in rows if row.get(column) is not None)
    if not values:
        return
    print(f"  {column:<8} {label:<12} n={len(values):<5} p50={percentile(values, 0.50):>8.1f} ms"
          f"  p95={percentile(values, 0.95):>8.1f} ms  p99={percentile(values, 0.99):>8.1f} ms"
          f"  max={values[-1]:>8.1f} ms  mean={statistics.mean(values):>8.1f} ms")


def percentile(values, fraction):
    """The nearest-rank percentile: the smallest value at or above the fraction of the sample."""
    index = max(0, min(len(values) - 1, int(round(fraction * len(values) + 0.5)) - 1))
    return values[index]


def read(path):
    with open(path) as handle:
        return [json.loads(line) for line in handle if line.strip()]


if __name__ == "__main__":
    status = main()
    sys.stdout.flush()
    sys.exit(status)
