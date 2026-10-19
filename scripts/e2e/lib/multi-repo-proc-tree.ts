import { mkdirSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { dir, nextRun } from "./multi-repo-acceptance-harness";

// A fake /proc tree for second-owner's as_role mechanism.

// The one path the prober accepts as a worker shim's executable: the image's own legion binary.
export const LEGION_BINARY = "/opt/legion/bin/legion";

// writeProcEntry builds one /proc/<pid>-shaped directory: status (PPid: line), cmdline
// (NUL-separated argv, trailing NUL), environ (NUL-separated VAR=value pairs, trailing NUL when
// non-empty) and, with `exe`, the /proc/<pid>/exe symlink to that path — exactly what
// scripts/e2e/lib/as-role.py reads.
export function writeProcEntry(
  root: string,
  pid: string,
  opts: {
    ppid: string;
    cmdline: string[];
    environ?: Record<string, string>;
    unreadableEnviron?: boolean;
    exe?: string;
  }
): void {
  const procDir = join(root, pid);
  mkdirSync(procDir, { recursive: true });
  writeFileSync(join(procDir, "status"), `Name:\tproc${pid}\nPPid:\t${opts.ppid}\n`);
  writeFileSync(join(procDir, "cmdline"), `${opts.cmdline.join("\0")}\0`);
  const pairs = Object.entries(opts.environ ?? {}).map(([k, v]) => `${k}=${v}`);
  writeFileSync(join(procDir, "environ"), pairs.length > 0 ? `${pairs.join("\0")}\0` : "");
  if (opts.unreadableEnviron) {
    writeFileSync(join(procDir, "environ"), "", { mode: 0o000 });
  }
  if (opts.exe !== undefined) {
    symlinkSync(opts.exe, join(procDir, "exe"));
  }
}

// fakeProcTree builds a minimal pod process tree under a fresh directory: pid 1 (the launcher, no
// role markers), one worker-shim per `shimPids` (default ["7"], parented by `shimParent` —
// default "1" — carrying `-- ` then `agentArgv` (default ["omp","--mode","rpc"]) in its own
// cmdline), and, unless `skipAgent`, one agent child per shim at `shimPid+2` whose own cmdline
// equals exactly that trailing argv — the one as_role must select by argv equality, never by mere
// parentage: with `extraShimChildren`, each shim also gets an `agent-secrets renew` child at
// `shimPid+1` and a `codegraph` child at `shimPid+4`, both real children of the shim carrying its
// own environment but a different argv, exactly as a broker-enrolled pod's shim does for its whole
// life — present to prove the agent is still picked out from among them. `duplicateAgent` adds a
// second child whose argv also matches, for the two-candidates refusal. pid 10 (a descendant of
// the first agent, e.g. a bash tool child, also carrying the role's own environment since it
// inherited it — must NOT be selected, since its parent is the agent, not the shim) and pid 15 (a
// zombie, child of the first agent, empty environ — skipped) are always present. `roleEnviron` is
// every shim and agent's environment (they share one in a real pod). Every worker-shim entry gets
// an `exe` symlink to the image's legion binary, as the prober requires; `shimExe` points it
// elsewhere, or null leaves it out.
export interface FakeProcTreeOptions {
  shimPids?: string[];
  skipAgent?: boolean;
  agentArgv?: string[];
  extraShimChildren?: boolean;
  duplicateAgent?: boolean;
  shimParent?: string;
  shimExe?: string | null;
}

export function fakeProcTree(
  roleEnviron: Record<string, string>,
  opts: FakeProcTreeOptions = {}
): string {
  const root = join(dir, `proc-${nextRun()}`);
  mkdirSync(root, { recursive: true });
  writeProcEntry(root, "1", { ppid: "0", cmdline: ["launcher"], environ: { PATH: "/usr/bin" } });
  const shimPids = opts.shimPids ?? ["7"];
  const agentArgv = opts.agentArgv ?? ["omp", "--mode", "rpc"];
  const shimParent = opts.shimParent ?? "1";
  const shimExe = opts.shimExe === undefined ? LEGION_BINARY : opts.shimExe;
  for (const shimPid of shimPids) {
    writeProcEntry(root, shimPid, {
      ppid: shimParent,
      cmdline: ["legion", "worker-shim", "--connect", "tcp://x:1", "--", ...agentArgv],
      environ: roleEnviron,
      ...(shimExe === null ? {} : { exe: shimExe }),
    });
    if (opts.skipAgent) continue;
    const agentPid = String(Number(shimPid) + 2);
    writeProcEntry(root, agentPid, { ppid: shimPid, cmdline: agentArgv, environ: roleEnviron });
    if (opts.extraShimChildren) {
      writeProcEntry(root, String(Number(shimPid) + 1), {
        ppid: shimPid,
        cmdline: ["agent-secrets", "renew", "--pod-token-file", "/x"],
        environ: roleEnviron,
      });
      writeProcEntry(root, String(Number(shimPid) + 4), {
        ppid: shimPid,
        cmdline: ["codegraph"],
        environ: roleEnviron,
      });
    }
    if (opts.duplicateAgent) {
      writeProcEntry(root, String(Number(shimPid) + 5), {
        ppid: shimPid,
        cmdline: agentArgv,
        environ: roleEnviron,
      });
    }
  }
  writeProcEntry(root, "10", {
    ppid: "9",
    cmdline: ["bash", "-c", "sleep 1"],
    environ: roleEnviron,
  });
  writeProcEntry(root, "15", { ppid: "9", cmdline: ["zombie"], environ: {} });
  return root;
}
