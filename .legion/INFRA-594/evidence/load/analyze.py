#!/usr/bin/env python3
"""analyze.py: the before/after table of the INFRA-594 load measurement.

Reads runs/{before,after}-r*/{summary,meta,samples}.json (and runs/before-r*.natsspy.jsonl, the
consumer count and pull rate taken beside the BEFORE harness) and prints, per run and as median
[min - max] per side, every metric of the report. Trends (first vs last third of the window's 5 s
samples, RSS least-squares slope) are computed here from samples.json for both sides, with the same
rule as the AFTER harness's resources.trends.
"""
import glob
import json
import os
import statistics
import sys
from datetime import datetime

L = os.path.dirname(os.path.abspath(__file__))
CLK = 100.0


def ts(s):
    s = s.replace("Z", "+00:00")
    if "." in s:
        head, rest = s.split(".", 1)
        frac, tz = rest[:-6], rest[-6:]
        s = head + "." + frac[:6].ljust(6, "0") + tz
    return datetime.fromisoformat(s).timestamp()


def trend(win, name):
    k = len(win) // 3
    if k < 2:
        return None
    first, last = win[:k], win[-k:]

    def cores(part):
        a, b = part[0], part[-1]
        if name not in a["cpu_ticks"] or name not in b["cpu_ticks"]:
            return None
        dt = ts(b["at"]) - ts(a["at"])
        return (b["cpu_ticks"][name] - a["cpu_ticks"][name]) / CLK / dt

    def rss(part):
        return statistics.mean(s["rss_kib"].get(name, 0) / 1024 for s in part)

    t0 = ts(win[0]["at"])
    xs = [(ts(s["at"]) - t0) / 60 for s in win if name in s["rss_kib"]]
    ys = [s["rss_kib"][name] / 1024 for s in win if name in s["rss_kib"]]
    n = len(xs)
    sx, sy = sum(xs), sum(ys)
    sxx = sum(x * x for x in xs)
    sxy = sum(x * y for x, y in zip(xs, ys))
    d = n * sxx - sx * sx
    slope = (n * sxy - sx * sy) / d if d > 0 else 0.0
    c1, c2 = cores(first), cores(last)
    if c1 is None or c2 is None:
        return None
    return {"c1": c1, "c2": c2, "r1": rss(first), "r2": rss(last), "slope": slope, "rmax": max(ys) if ys else None}


def spy_stats(path, w0, w1):
    rows = []
    if os.path.exists(path):
        for line in open(path):
            try:
                r = json.loads(line)
            except ValueError:
                continue
            t = ts(r["at"])
            if w0 <= t <= w1 + 5:
                rows.append((t, r))
    if len(rows) < 2:
        return {}
    (ta, a), (tb, b) = rows[0], rows[-1]
    dt = tb - ta
    out = {"pulls": (b["pull_requests"] - a["pull_requests"]) / dt, "spulls": (b["session_pull_requests"] - a["session_pull_requests"]) / dt}
    cons = [r.get("js_consumers") for _, r in rows if r.get("js_consumers") is not None]
    if cons:
        out["consumers"] = max(cons)
    if "js_api_total" in a and "js_api_total" in b:
        out["api"] = (b["js_api_total"] - a["js_api_total"]) / dt
    return out


def run_metrics(d, side):
    s = json.load(open(os.path.join(d, "summary.json")))
    m = json.load(open(os.path.join(d, "meta.json")))
    samples = json.load(open(os.path.join(d, "samples.json"))) if os.path.exists(os.path.join(d, "samples.json")) else []
    win = [x for x in samples if x.get("in_window")]
    e, r = s["events"], s.get("resources") or {}
    out = {
        "run": os.path.basename(d),
        "box_busy_before_%": (m.get("box_busy_before") or 0) * 100,
        "offered/s": e["offered_per_s"],
        "committed/s": e["committed_per_s"],
        "arrival p50 ms": e["delivery_ms_from_arrival"]["p50"],
        "arrival p99 ms": e["delivery_ms_from_arrival"]["p99"],
        "commit p50 ms": e["delivery_ms_from_commit"]["p50"],
        "commit p99 ms": e["delivery_ms_from_commit"]["p99"],
        "publish p50 ms": (e.get("delivery_ms_from_publish") or {}).get("p50") if (e.get("delivery_ms_from_publish") or {}).get("n") else None,
        "publish p99 ms": (e.get("delivery_ms_from_publish") or {}).get("p99") if (e.get("delivery_ms_from_publish") or {}).get("n") else None,
        "publish max ms": (e.get("delivery_ms_from_publish") or {}).get("max") if (e.get("delivery_ms_from_publish") or {}).get("n") else None,
        "receipts expected": e["expected_issue_topic_receipts"],
        "receipts got": e["issue_topic_receipts"],
        "lost (expected-got)": e["missing"],
        "listener cores": r.get("cpu_cores_mean", {}).get("listener"),
        "listener RSS max MiB": r.get("rss_mib_max", {}).get("listener"),
        "nats cores": r.get("cpu_cores_mean", {}).get("nats"),
        "nats RSS max MiB": r.get("rss_mib_max", {}).get("nats"),
        "dispatch cores": r.get("cpu_cores_mean", {}).get("dispatch"),
        "loadgen cores": r.get("cpu_cores_mean", {}).get("loadgen"),
        "box busy %": (r.get("sys_busy_frac") or 0) * 100,
        "stack exits": len(s.get("stack_exits") or []),
    }
    for name, key in (("listener", "listener"), ("nats", "nats")):
        t = trend(win, name)
        if t:
            out[f"{key} cores 1st/last third"] = (t["c1"], t["c2"])
            out[f"{key} RSS MiB 1st/last third"] = (t["r1"], t["r2"])
            out[f"{key} RSS slope MiB/min"] = t["slope"]
    if side == "after":
        out["consumers max"] = r.get("js_consumers_max")
        out["pull req/s"] = r.get("pull_requests_per_s")
        out["session pull req/s"] = r.get("session_pull_requests_per_s")
        out["JS API req/s"] = r.get("js_api_per_s")
        dlv = s["sessions"].get("delivery") or {}
        out["sessions on consumer"] = dlv.get("consumer_sessions")
        out["sessions on core"] = dlv.get("core_sessions")
        out["relay frames"] = dlv.get("frames")
        out["duplicates"] = dlv.get("duplicates")
        out["redelivered"] = dlv.get("redelivered")
    else:
        w0, w1 = ts(m["window_start"]), ts(m["window_end"])
        sp = spy_stats(d + ".natsspy.jsonl", w0, w1)
        out["consumers max"] = sp.get("consumers")
        out["pull req/s"] = sp.get("pulls")
        out["session pull req/s"] = sp.get("spulls")
        out["JS API req/s"] = sp.get("api")
    out["registered"] = s["sessions"]["registered"]
    out["heartbeat errors"] = s["sessions"]["heartbeat_errors"]
    out["nats slow consumers"] = r.get("nats_slow_consumers")
    return out


def fmt(v):
    if v is None:
        return "-"
    if isinstance(v, tuple):
        return " / ".join(fmt(x) for x in v)
    if isinstance(v, float):
        if v.is_integer():
            return f"{int(v):,}"
        if abs(v) >= 100:
            return f"{v:,.0f}"
        if abs(v) >= 1:
            return f"{v:.2f}"
        return f"{v:.3f}"
    return f"{v:,}" if isinstance(v, int) else str(v)


def agg(vals):
    vals = [v for v in vals if v is not None]
    if not vals:
        return "-"
    if isinstance(vals[0], tuple):
        parts = [agg([v[i] for v in vals]) for i in range(len(vals[0]))]
        return " / ".join(parts)
    med = statistics.median(vals)
    return f"{fmt(float(med))} [{fmt(float(min(vals)))} – {fmt(float(max(vals)))}]"


def main():
    pattern = sys.argv[1] if len(sys.argv) > 1 else "r[0-9]*"
    sides = {}
    for side in ("before", "after"):
        dirs = sorted(d for d in glob.glob(os.path.join(L, "runs", f"{side}-{pattern}")) if os.path.isdir(d) and os.path.exists(os.path.join(d, "summary.json")))
        sides[side] = [run_metrics(d, side) for d in dirs]
    keys = []
    for side in sides.values():
        for row in side:
            for k in row:
                if k not in keys and k != "run":
                    keys.append(k)
    print("| metric | before: median [range] (n=%d) | after: median [range] (n=%d) |" % (len(sides["before"]), len(sides["after"])))
    print("|---|---|---|")
    for k in keys:
        print(f"| {k} | {agg([r.get(k) for r in sides['before']])} | {agg([r.get(k) for r in sides['after']])} |")
    for side, rows in sides.items():
        print(f"\n### {side}: per run\n")
        print("| metric | " + " | ".join(r["run"] for r in rows) + " |")
        print("|---|" + "---|" * len(rows))
        for k in keys:
            print(f"| {k} | " + " | ".join(fmt(r.get(k)) for r in rows) + " |")


main()
