"""Check k1's scanner: it starts the command and scans the descendant it started.

/proc/sys/kernel/yama/ptrace_scope is 1 in a Legion pod, so the reader has to be an ancestor of
what it reads; a scanner started beside the command reads "permission denied". Every needle
reaches it in a file, never on a command line, which /proc shows to everybody.

Usage: memscan.py <ready file> <needles file> <out file> <working directory> <command>...
"""

import json, os, subprocess, sys, time

ready, needles_file, out_file = sys.argv[1], sys.argv[2], sys.argv[3]
cwd, cmd = sys.argv[4], sys.argv[5:]
needles = [bytes.fromhex(line.strip()) for line in open(needles_file) if line.strip()]

child = subprocess.Popen(cmd, cwd=cwd)
# The command writes the ready file once it has made its calls and before it sleeps.
deadline = time.time() + 120
while time.time() < deadline and not os.path.exists(ready) and child.poll() is None:
    time.sleep(0.1)

result = {"pid": child.pid, "scanned": False, "regions": 0, "bytes": 0,
          "memory_hits": [0] * len(needles), "environ_hits": [0] * len(needles),
          "scan_error": ""}
try:
    with open(f"/proc/{child.pid}/environ", "rb") as f:
        environ = f.read()
    for i, n in enumerate(needles):
        result["environ_hits"][i] = environ.count(n)
except OSError as e:
    result["scan_error"] = f"environ: {e}"

try:
    maps = open(f"/proc/{child.pid}/maps").read().splitlines()
    with open(f"/proc/{child.pid}/mem", "rb", 0) as mem:
        for line in maps:
            fields = line.split()
            if len(fields) < 2 or "r" not in fields[1]:
                continue
            lo, hi = (int(x, 16) for x in fields[0].split("-"))
            # [vsyscall] sits above the top of the address space a seek offset can hold, and
            # a very large mapping is a reservation nothing has written to.
            if hi - lo > (1 << 30) or lo >= (1 << 63):
                continue
            try:
                mem.seek(lo)
                data = mem.read(hi - lo)
            except (OSError, OverflowError, ValueError):
                continue
            result["regions"] += 1
            result["bytes"] += len(data)
            for i, n in enumerate(needles):
                result["memory_hits"][i] += data.count(n)
    result["scanned"] = result["regions"] > 0
except OSError as e:
    result["scan_error"] = (result["scan_error"] + " " + f"mem: {e}").strip()

result["exit"] = child.wait()
with open(out_file, "w") as f:
    json.dump(result, f)
