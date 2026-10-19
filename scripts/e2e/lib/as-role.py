"""Resolve the role's agent process in this pod and run second-owner's credential probes as it.

scripts/e2e/multi-repo-acceptance.sh streams this file to `python3 -I -` over `kubectl exec -i`:

    python3 -I - <proc root> <T repo> <O repo> <O owner> <probe branch>

and reads the one JSON object it prints. `-I` (isolated mode: no cwd or user site on sys.path, no
PYTHON* variables) matters because the exec's working directory is the issue's workspace and its
HOME is one the agent writes to: a planted `hashlib.py` or `.pth` would otherwise run before this
file and forge the answer.

`kubectl exec` carries none of a role's own variables (only PATH, HOME and POD_UID); the role's
LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR, LEGION_GH_PATH and LEGION_WORKSPACE are plain variables its
own process was started with. So the probes run under exactly that process's /proc/<pid>/environ.

Which process is the role's agent: the one child of the one `worker-shim` (own parent PID 1) whose
argv equals the shim's own argv after its first `--`, or, when that argv is the install wrapper
`/bin/sh -c <script> legion-plugins <agent argv...>` (packages/daemon/internal/runtime/sandbox/
plugins.go, installArgv), the argv after the wrapper's four leading elements, since the wrapper's
`exec "$@"` replaces its image with the agent. A secrets-broker pod's shim also parents an
`agent-secrets renew` process and briefly a `codegraph` warm-up, so parentage alone is ambiguous.

What goes back is facts, never values: the pid, argv[0], the argument count and a sha256 of the
whole /proc/<pid>/cmdline (the agent runs with `--append-system-prompt <the composed prompt, the
deployment's own instructions>`), whether LEGION_GH_OWNERS_DIR/GH_CONFIG_DIR name the issue's own
owner directory, and each probe's exit status and output.

Everything the agent can write is kept out of the git probes: they run in a repository this file
creates in a fresh temporary directory (never the issue's clone, whose .git/config the agent
writes), with the system and global git config off, every GIT_* variable of the role's environment
dropped (GIT_CONFIG_COUNT, GIT_CONFIG_PARAMETERS, GIT_DIR, GIT_SSH_COMMAND, ...), only https allowed,
and the role's own credential helper named explicitly. A `url.<x>.insteadOf`, `http.proxy`, an
`ext::` remote or a `credential.helper` the agent planted therefore never reaches them.
"""

import hashlib
import json
import os
import subprocess
import sys
import tempfile

PROBE_TIMEOUT_SECONDS = 120


def fail(message):
    sys.stderr.write("as_role: %s\n" % message)
    sys.exit(1)


def read_file(proc_root, pid, name):
    with open(os.path.join(proc_root, pid, name), "rb") as handle:
        return handle.read()


def argv_of(raw):
    fields = raw.split(b"\x00")
    if fields and fields[-1] == b"":
        fields = fields[:-1]
    return fields


def parent_of(proc_root, pid):
    for line in read_file(proc_root, pid, "status").splitlines():
        if line.startswith(b"PPid:"):
            return line.split()[1].decode()
    return None


def pids_of(proc_root):
    try:
        return [entry for entry in os.listdir(proc_root) if entry.isdigit()]
    except OSError as error:
        fail("could not list %s: %s" % (proc_root, error))


def resolve_agent(proc_root):
    pids = pids_of(proc_root)

    shims = []
    for pid in pids:
        try:
            fields = argv_of(read_file(proc_root, pid, "cmdline"))
            parent = parent_of(proc_root, pid)
        except OSError:
            continue
        if b"worker-shim" in fields and parent == "1":
            shims.append(pid)
    if len(shims) != 1:
        fail(
            "expected exactly one worker-shim process whose own parent is PID 1, found %d (%s)"
            % (len(shims), " ".join(sorted(shims)))
        )
    shim = shims[0]

    shim_argv = argv_of(read_file(proc_root, shim, "cmdline"))
    if b"--" not in shim_argv:
        fail("worker-shim %s own cmdline carries no -- separator, so its agent argv is unknown" % shim)
    direct = shim_argv[shim_argv.index(b"--") + 1 :]
    expected = [direct]
    if (
        len(direct) >= 5
        and direct[0] == b"/bin/sh"
        and direct[1] == b"-c"
        and direct[3] == b"legion-plugins"
    ):
        expected.append(direct[4:])

    matches = []
    for pid in pids:
        if pid == shim:
            continue
        try:
            if parent_of(proc_root, pid) != shim:
                continue
            fields = argv_of(read_file(proc_root, pid, "cmdline"))
        except OSError:
            continue
        if fields in expected:
            matches.append(pid)
    if len(matches) != 1:
        fail(
            "expected exactly one child of worker-shim %s matching its own argv (directly, or through "
            "the install wrapper's exec), found %d (%s)" % (shim, len(matches), " ".join(sorted(matches)))
        )
    return matches[0]


def environment_of(proc_root, pid):
    environ = {}
    for item in read_file(proc_root, pid, "environ").split(b"\x00"):
        if item:
            key, _, value = item.partition(b"=")
            environ[os.fsdecode(key)] = os.fsdecode(value)
    return environ


def run(argv, env):
    try:
        done = subprocess.run(
            argv,
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=PROBE_TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired as expired:
        return {"status": 124, "output": (expired.output or b"").decode("utf-8", "replace").rstrip("\n")}
    except OSError as error:
        return {"status": 127, "output": "%s: %s" % (argv[0], error)}
    return {"status": done.returncode, "output": done.stdout.decode("utf-8", "replace").rstrip("\n")}


def main():
    if len(sys.argv) != 6:
        fail("usage: as-role.py <proc root> <T repo> <O repo> <O owner> <probe branch>")
    proc_root, t_repo, o_repo, o_owner, branch = sys.argv[1:]

    pid = resolve_agent(proc_root)
    cmdline = read_file(proc_root, pid, "cmdline")
    argv = argv_of(cmdline)
    environ = environment_of(proc_root, pid)
    owners_dir = environ.get("LEGION_GH_OWNERS_DIR", "")

    result = {
        "pid": pid,
        "argv0": os.fsdecode(argv[0]) if argv else "",
        "argc": len(argv),
        "sha256": hashlib.sha256(cmdline).hexdigest(),
        "env_match": owners_dir != "" and environ.get("GH_CONFIG_DIR") == owners_dir + "/" + o_owner,
        "read": run(["gh", "api", "repos/" + t_repo, "--jq", ".full_name"], environ),
    }

    probe_env = {key: value for key, value in environ.items() if not key.startswith("GIT_")}
    probe_env.update(
        {
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": "/dev/null",
            "GIT_ALLOW_PROTOCOL": "https",
            "GIT_TERMINAL_PROMPT": "0",
        }
    )
    with tempfile.TemporaryDirectory(prefix="as-role-") as scratch:
        git = [
            "git",
            "-C",
            scratch,
            "-c",
            "core.hooksPath=/dev/null",
            "-c",
            "credential.helper=",
            "-c",
            "credential.helper=!legion git-credential",
            "-c",
            "credential.useHttpPath=true",
            "-c",
            "credential.interactive=false",
            "-c",
            "protocol.allow=never",
            "-c",
            "protocol.https.allow=always",
        ]
        made = run(["git", "init", "-q", scratch], probe_env)
        if made["status"] != 0:
            fail("could not create the probe repository: %s" % made["output"])
        made = run(
            git
            + [
                "-c",
                "user.name=legion-probe",
                "-c",
                "user.email=legion-probe@localhost.invalid",
                "-c",
                "commit.gpgsign=false",
                "commit",
                "-q",
                "--allow-empty",
                "-m",
                "probe",
            ],
            probe_env,
        )
        if made["status"] != 0:
            fail("could not commit in the probe repository: %s" % made["output"])
        for name, repo in (("push_o", o_repo), ("push_t", t_repo)):
            result[name] = run(
                git
                + [
                    "push",
                    "--no-verify",
                    "--dry-run",
                    "https://github.com/" + repo,
                    "HEAD:refs/heads/" + branch,
                ],
                probe_env,
            )

    print(json.dumps(result))


main()
