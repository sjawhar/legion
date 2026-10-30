# agent-secrets

The secrets broker's client. An agent session runs it to use a secret:

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
```

It asks the broker for the named secrets, waits while a person approves them in Dispatch if the
secret's owner and tier require it, and runs the command with each granted value in its
environment, writing one line to standard error first: the names it injected, that the broker
`AGENT_SECRETS_URL` names gave them, and that store's kind, as `agent-secrets: NAME from the broker
AGENT_SECRETS_URL names (GCP Secret Manager)`, never a value. Every other line about a secret names
its source the same way - `request`, `status` and `self` as a `source:` line, a denial, a pending
request and every refusal as `; asked the broker AGENT_SECRETS_URL names (<store kind>)` - so an
answer of this CLI's is never taken for another secrets CLI's about the same name. Those lines are
quoted wherever an agent's output is, so none names the broker's host or the store's account,
region or project (only a broker that could not be reached is named, by its URL, so a stale
address reads as one); the secret forms below, which a person runs under their own cloud
sign-in, name the account or project they write to. No output of `agent-secrets`,
`agent-secrets-helper` or `agent-secrets-devrelay` prints a credential the broker's URL carries:
the URL prints only when it is provably a plain http(s) URL (no user info, query, fragment,
escape or control byte), and as `(not printed: the value carries a credential or is not a plain
http(s) URL)` otherwise. People and launchers use its other forms to log a machine in
(`agent-secrets machine
login`), register an agent session (`agent-secrets register [--session-id <id>] --exec -- <agent>`),
and inspect a
session's grants (`agent-secrets self`). People manage the agent secrets themselves with
`agent-secrets secret list|show|create|set|retag|delete|restore`, which calls the broker's own
store (AWS Secrets Manager or GCP Secret Manager) under their own cloud sign-in and then asks the
broker to reread each secret written, so the change
is served at once; those forms need no helper and no session. From their own shell on a machine
that runs the helper, they list and end their own machines' logins and their live grants with
`agent-secrets machine list|revoke` and `agent-secrets grant list|revoke`, under that machine's
login; a service's login is revoked on Dispatch's machine-login page. The helper refuses these
forms to a process inside a registered session's process tree (`IN_SESSION`), so an agent's own
commands cannot act as its operator. The check covers that tree only: a process a session sends
out of it (`( cmd & )`, `setsid -f`, a tmux server it started) passes, and the same user can stop
the helper anyway. Such a process can list and revoke its operator's own machine logins, never a
service's machine login, and its operator's grants, which include grants the operator approved on
any session. `agent-secrets enroll --helper` is open to any process of the user, so such a process
can also enroll a box and read the operator's agent secrets. `agent-secrets --help` lists every
form, and each form answers `-h`.

`register --session-id <id>` is the launcher saying which agent session the process it is about to
become belongs to. The helper records it at registration and every enrollment of that session
carries it, so a credential request names its session on Dispatch's approval page instead of
showing no session at all. The id comes from the launcher because the session cannot be believed
about it: a process may rewrite its own environment, so neither the request's environment nor the
pinned process's `/proc/<pid>/environ` is evidence of anything. Nothing inside the session can set
or change the recorded id — a register from a descendant is answered with that session's own — but
the helper does not check that the launcher's id names a real session either. Read it as a
correlation key: it says which session asked, never that the session was entitled to ask, which is
what the enrollment, its key and the approver rule decide. A launcher that does not know the id
leaves the flag out: the session registers, enrolls and gets secrets exactly the same, and the page
says no session is named.

On a machine that runs agents directly, `agent-secrets-helper` (`../agent-secrets-helper`) holds
each session's key and signs for it; in a container or a Kubernetes pod, the session's key lives in
`AGENT_SECRETS_KEY_DIR`.

The docs site's "Secrets Broker" section, built from `docs/site/src/content/docs/broker/`, has the
quickstart, the guides and the generated CLI reference.

## Build

```sh
cd packages/envoy
go build -o bin/ ./cmd/agent-secrets ./cmd/agent-secrets-helper
```

On macOS, build `./cmd/agent-secrets` alone: the helper builds for Linux only. Each
`legion-envoy-v*` GitHub release ships both binaries for Linux, as `agent-secrets-amd64.tar.gz` and
`agent-secrets-arm64.tar.gz`, and `agent-secrets` alone for macOS, as
`agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`.

The hidden value prompt is available on Linux and macOS for amd64 and arm64. Other targets,
including Windows, FreeBSD and other Linux architectures, build without a prompt and require
the value on stdin, for example `agent-secrets secret set DEMO_API_KEY < value.txt`.

## Test

```sh
cd packages/envoy
go test ./cmd/agent-secrets/... ./cmd/agent-secrets-helper/...
```
