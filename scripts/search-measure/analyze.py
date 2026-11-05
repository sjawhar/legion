#!/usr/bin/env python3
"""Turn a run's rows into the numbers the measurement reports.

usage: analyze.py <out.jsonl> [--serial <out-serial.jsonl>]

Recall at 1, 3, 10 and 20, MRR and "absent from the page" per case kind, each beside the
baseline the hybrid route measured on the same cases (BASELINE below); the kind-1 breakdown of
how often the issue's own document comes back while the issue does not; and the latency of the
run, the server's own `took_ms` and the client's wall clock.
"""

import argparse
import json
import statistics
import sys
from collections import Counter

KIND_NAME = {
    1: "1 a sentence of the issue's own spec -> that issue",
    2: "2 a reworded title -> that issue",
    3: "3 an answered ask, in its own words -> that ask",
}

# BASELINE is the hybrid route's own numbers on these cases, measured against production on
# 2026-11-05 with meaning search live: recall at 1, 3, 10 and 20, MRR, absent from the top 50,
# and how many cases each was measured over. A number the baseline run did not report is None.
BASELINE = {
    1: {"n": 281, "r1": 0.025, "r3": 0.427, "r10": None, "r20": 0.669, "mrr": None, "absent": 0.263},
    2: {"n": 121, "r1": 0.306, "r3": 0.347, "r10": None, "r20": None, "mrr": None, "absent": None},
    3: {"n": 435, "r1": 0.818, "r3": None, "r10": None, "r20": None, "mrr": None, "absent": None},
}
# BASELINE_SERIAL is that run's serial latency, one request at a time, in milliseconds.
BASELINE_SERIAL = {"p50": 1034, "p95": 1522, "n": 120}


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
    for kind in (1, 2, 3):
        block([row for row in rows if row["kind"] == kind], KIND_NAME[kind], BASELINE[kind])
    block(rows, "all three kinds together", combined())

    kind_one = [row for row in rows if row["kind"] == 1]
    if kind_one:
        print("\n== kind 1: the issue's own document comes back, the issue does not ==")
        for depth in (1, 3, 10, 20, 50):
            document = sum(1 for row in kind_one if hit(row.get("doc_rank"), depth))
            issue = sum(1 for row in kind_one if hit(row.get("rank"), depth))
            both = sum(1 for row in kind_one
                       if hit(row.get("doc_rank"), depth) and hit(row.get("rank"), depth))
            document_only = document - both
            print(f"  @{depth:<3} document {document:>4} ({document / len(kind_one):6.1%})"
                  f"  issue {issue:>4} ({issue / len(kind_one):6.1%})"
                  f"  document but not the issue {document_only:>4} ({document_only / len(kind_one):6.1%})")

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
    """The baseline over the three kinds together, weighted by each kind's own count."""
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
    for kind in (1, 2, 3):
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
    main()
    sys.stdout.flush()
