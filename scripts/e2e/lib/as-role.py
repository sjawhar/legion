"""Resolve the role's agent process in this pod and run second-owner's credential probes.

scripts/e2e/multi-repo-acceptance.sh streams this file to `python3 -I -` over `kubectl exec -i`:

    python3 -I - <proc root> <T repo> <O repo> <O owner> <GitHub base URL> <trusted root>

and reads the one JSON object it prints. `-I` (isolated mode: no cwd or user site on sys.path, no
PYTHON* variables) matters because the exec's working directory is the issue's workspace and its
HOME is one the agent writes to: a planted `hashlib.py` or `.pth` would otherwise run before this
file and forge the answer. <trusted root> is empty in a pod; a test points it at a directory laid out
like the image so the prober's absolute paths below resolve to fakes.

Which process is the role's agent: the one child of the one `worker-shim` (own parent PID 1,
executing the image's root-owned /opt/legion/bin/legion, which rules out a renamed process or a
copy of the binary) whose argv equals the shim's own argv after its first `--`, or, when that argv
is the install wrapper `/bin/sh -c <script> legion-plugins <agent argv...>`
(packages/daemon/internal/runtime/sandbox/plugins.go, installArgv), the argv after the wrapper's
four leading elements, since the wrapper's `exec "$@"` replaces its image with the agent. A
secrets-broker pod's shim also parents an `agent-secrets renew` process and briefly a `codegraph`
warm-up, so parentage alone is ambiguous. The agent's /proc/<pid>/environ is read for two things: to
compare what the agent was told (LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR, LEGION_GH_PATH) with the
values the image fixes, a difference failing the check, and for the proxy variables (below).

What the prober runs and trusts is the image's, never the agent's. The agent's own code runs in its
process (the workspace's .omp/extensions, the profile plugins), and a process can rewrite its own
/proc/<pid>/environ in place, so no executable, search path or credential directory is taken from
it. `legion git-credential get` and `gh` run by absolute path, root-owned under /opt/legion/bin,
with PATH, LEGION_GH_OWNERS_DIR (the role's read-only gh Secret projection) and LEGION_GH_PATH set
to the image's constants and a HOME that does not exist. Only the proxy variables come from the
role's environment: TLS is verified against the root-owned CA store, so a rewritten proxy can drop a
request and cannot answer one.

What goes back is facts, never values: the pid, argv[0], the argument count and a sha256 of the
whole /proc/<pid>/cmdline (the agent runs with `--append-system-prompt <the composed prompt, the
deployment's own instructions>`), whether the agent's gh variables are the image's, the `gh` read of
T, and, for each of O and T, the HTTP status, content type and body (never the token) GitHub answers
to the receive-pack advertisement request a `git push` opens with. A token that can push to a
repository gets 200 and the advertisement; one that cannot gets 403.

No git runs: git reads ~/.netrc and ~/.gitconfig, the repository's own .git/config and the GIT_*
variables, any of which can supply a credential or redirect a URL and all of which the agent's uid
can write. The request is Python's urllib, which reads no netrc and no git config, with default TLS
verification.
"""

import base64
import hashlib
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

PROBE_TIMEOUT_SECONDS = 120
LEGION_BINARY = "/opt/legion/bin/legion"
GH_WRAPPER = "/opt/legion/bin/gh"
REAL_GH = "/usr/local/bin/gh"
OWNERS_DIR = "/var/run/legion/gh"
IMAGE_PATH = ("/opt/legion/bin", "/opt/omp/bin", "/opt/codegraph/bin", "/usr/local/bin", "/usr/bin", "/bin")


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


def runs_legion(proc_root, pid):
    """Whether the process executes the image's own, root-owned legion binary: a process the agent
    started under another name or from a copy elsewhere is not a shim."""
    try:
        return os.readlink(os.path.join(proc_root, pid, "exe")) == LEGION_BINARY
    except OSError:
        return False


def resolve_agent(proc_root):
    pids = pids_of(proc_root)

    shims = []
    for pid in pids:
        try:
            fields = argv_of(read_file(proc_root, pid, "cmdline"))
            parent = parent_of(proc_root, pid)
        except OSError:
            continue
        if b"worker-shim" in fields and parent == "1" and runs_legion(proc_root, pid):
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


def run(argv, env, stdin=None):
    feed = {"input": stdin} if stdin is not None else {"stdin": subprocess.DEVNULL}
    try:
        done = subprocess.run(
            argv,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=PROBE_TIMEOUT_SECONDS,
            **feed,
        )
    except subprocess.TimeoutExpired as expired:
        return {"status": 124, "output": (expired.output or b"").decode("utf-8", "replace").rstrip("\n")}
    except OSError as error:
        return {"status": 127, "output": "%s: %s" % (argv[0], error)}
    return {"status": done.returncode, "output": done.stdout.decode("utf-8", "replace").rstrip("\n")}


def trusted_environment(root, environ, owner):
    """The environment `legion git-credential` and `gh` run under, and whose proxy the GitHub
    requests use: the image's own constants, with GH_CONFIG_DIR at the issue owner's directory,
    never values read from the agent's environment, which its own code can rewrite in place. Only
    the proxy variables come from the role's environment: TLS is verified against the root-owned CA
    store, so a rewritten proxy can drop a request and cannot answer one."""
    owners_dir = root + OWNERS_DIR
    env = {
        "HOME": "/nonexistent",
        "PATH": ":".join(root + directory for directory in IMAGE_PATH),
        "LEGION_GH_OWNERS_DIR": owners_dir,
        "LEGION_GH_PATH": root + REAL_GH,
        "GH_CONFIG_DIR": owners_dir + "/" + owner,
    }
    for name in ("https_proxy", "HTTPS_PROXY", "no_proxy", "NO_PROXY"):
        if name in environ:
            env[name] = environ[name]
    return env


def credential_for(root, repo, env):
    """What the image's own `legion git-credential get` answers for github.com/<repo>: a
    (username, password) pair, or None and why not."""
    asked = run(
        [root + LEGION_BINARY, "git-credential", "get"],
        env,
        stdin=("protocol=https\nhost=github.com\npath=%s\n\n" % repo).encode(),
    )
    answer = dict(
        line.split("=", 1) for line in asked["output"].splitlines() if "=" in line
    )
    if asked["status"] != 0 or "password" not in answer:
        return None, "legion git-credential answered nothing for %s (exit %d)" % (repo, asked["status"])
    return (answer.get("username", ""), answer["password"]), ""


def advertisement_of(root, base_url, repo, env):
    """GitHub's answer to the request a `git push` of <repo> opens with, made with the credential
    the image's own helper hands out: 200 and the receive-pack advertisement for a token that can
    push, 403 for one that cannot."""
    pair, why = credential_for(root, repo, env)
    if pair is None:
        return {"status": 0, "content_type": "", "output": why}
    username, password = pair
    proxy = env.get("https_proxy") or env.get("HTTPS_PROXY")
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"https": proxy} if proxy else {})
    )
    request = urllib.request.Request(
        "%s/%s.git/info/refs?service=git-receive-pack" % (base_url, repo),
        headers={
            "Authorization": "Basic " + base64.b64encode(("%s:%s" % (username, password)).encode()).decode(),
            "User-Agent": "legion-second-owner-probe",
            "Accept": "*/*",
        },
    )
    try:
        with opener.open(request, timeout=PROBE_TIMEOUT_SECONDS) as response:
            content_type = response.headers.get("Content-Type", "")
            response.read(1)
            return {"status": response.status, "content_type": content_type, "output": ""}
    except urllib.error.HTTPError as error:
        body = error.read(4096).decode("utf-8", "replace").replace(password, "<token>")
        return {
            "status": error.code,
            "content_type": error.headers.get("Content-Type", ""),
            "output": body.strip(),
        }
    except (urllib.error.URLError, OSError) as error:
        return {"status": 0, "content_type": "", "output": str(error).replace(password, "<token>")}


def main():
    if len(sys.argv) != 7:
        fail("usage: as-role.py <proc root> <T repo> <O repo> <O owner> <GitHub base URL> <trusted root>")
    proc_root, t_repo, o_repo, o_owner, base_url, root = sys.argv[1:]

    pid = resolve_agent(proc_root)
    cmdline = read_file(proc_root, pid, "cmdline")
    argv = argv_of(cmdline)
    environ = environment_of(proc_root, pid)

    # What the agent was told, compared with what the image fixes. A difference fails the check:
    # either the pod is not the shape the check is about, or the agent rewrote its environment.
    env_match = (
        environ.get("LEGION_GH_OWNERS_DIR") == OWNERS_DIR
        and environ.get("GH_CONFIG_DIR") == OWNERS_DIR + "/" + o_owner
        and environ.get("LEGION_GH_PATH") == REAL_GH
    )
    env = trusted_environment(root, environ, o_owner)

    print(
        json.dumps(
            {
                "pid": pid,
                "argv0": os.fsdecode(argv[0]) if argv else "",
                "argc": len(argv),
                "sha256": hashlib.sha256(cmdline).hexdigest(),
                "env_match": env_match,
                "read": run([root + GH_WRAPPER, "api", "repos/" + t_repo, "--jq", ".full_name"], env),
                "push_o": advertisement_of(root, base_url, o_repo, env),
                "push_t": advertisement_of(root, base_url, t_repo, env),
            }
        )
    )


main()
