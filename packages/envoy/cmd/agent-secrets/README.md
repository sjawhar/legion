# agent-secrets

The secrets broker's client. An agent session runs it to use a secret:

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
```

It asks the broker for the named secrets, waits while a person approves them on the broker's own
approval page if the secret's owner and tier require it, and runs the command with each granted
value in its
environment, writing one line to standard error first: the names it injected, that the broker
`AGENT_SECRETS_URL` names gave them, and that store's kind, as `agent-secrets: NAME from the broker
AGENT_SECRETS_URL names (GCP Secret Manager)`, never a value. Every other line about a secret names
its source the same way - `request`, `status` and `self` as a `source:` line, a denial, a pending
request and every refusal as `; asked the broker AGENT_SECRETS_URL names (<store kind>)` - so an
answer of this CLI's is never taken for another secrets CLI's about the same name. Those lines are
quoted wherever an agent's output is, so none names the store's account, region or project, and
none names the broker's host but two: a broker that could not be reached, named by its URL so a
stale address reads as one, and a pending secret request, whose line names the page on the broker
where its approver decides it, the `approve_url` the broker's own answer carries. The secret
forms below, which a person runs under their own cloud
sign-in, name the account or project they write to. No output of `agent-secrets`,
`agent-secrets-helper` or `agent-secrets-devrelay` prints a credential the broker's URL carries:
the URL prints only when it is provably a plain http(s) URL (no user info, query, fragment,
escape or control byte), and as `(not printed: the value carries a credential or is not a plain
http(s) URL)` otherwise. People and launchers use its other forms to log a machine in
(`agent-secrets machine
login`), register an agent session (`agent-secrets register --exec -- <agent>`), and inspect a
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

On a machine that runs agents directly, `agent-secrets-helper` (`../agent-secrets-helper`) holds
each session's key and signs for it; in a container or a Kubernetes pod, the session's key lives in
`AGENT_SECRETS_KEY_DIR`.

The docs site's "Secrets Broker" section, built from `docs/site/src/content/docs/broker/`, has the
quickstart, the guides and the generated CLI reference.

## Install

Each `legion-envoy-v*` release of this repository ships the client. The release tag names the Envoy
package, not `agent-secrets`, and the repository's newest release can be another package's, so
look the tag up rather than reading `releases/latest`. These commands need `curl`, `tar`, `sort`
and `sha256sum` (on Ubuntu: `apt-get install -y curl ca-certificates`), and install into
`~/.local/bin`. On Linux arm64 run `asset=agent-secrets-arm64.tar.gz` first, and on macOS
`asset=agent-secrets-darwin-arm64.tar.gz` (Apple silicon) or `asset=agent-secrets-darwin-amd64.tar.gz`
(Intel); the block uses `agent-secrets-amd64.tar.gz` otherwise. The install steps run in a subshell
with `set -eu`, so a failed download or checksum stops them before anything is installed:

```sh
asset=${asset:-agent-secrets-amd64.tar.gz}
tag=legion-envoy-$(curl -fsSL 'https://github.com/cybermonklabs/legion.git/info/refs?service=git-upload-pack' \
  | grep -aoE 'refs/tags/legion-envoy-v[0-9]+\.[0-9]+\.[0-9]+([^-0-9]|$)' | grep -aoE 'v[0-9.]+[0-9]' \
  | sort -u -t . -k 1.2,1n -k 2,2n -k 3,3n | tail -n 1)
(
  set -eu
  test "$tag" != legion-envoy-
  echo "installing $tag"
  base=https://github.com/cybermonklabs/legion/releases/download/$tag
  curl -fsSLO "$base/$asset"
  curl -fsSLO "$base/SHA256SUMS"
  grep " $asset\$" SHA256SUMS > "$asset.sha256"
  if command -v sha256sum >/dev/null; then sha256sum -c "$asset.sha256"; else shasum -a 256 -c "$asset.sha256"; fi
  tar -xzf "$asset"
  mkdir -p ~/.local/bin
  install -m 0755 agent-secrets/bin/* ~/.local/bin/
)
export PATH="$HOME/.local/bin:$PATH"
agent-secrets --version
```

`sha256sum -c` (`shasum` on macOS) prints `<asset>: OK`; any other result stops the subshell.
If `~/.local/bin` is not already on your `PATH`, add the `export PATH=...` line to your shell
profile. The Linux archives hold `agent-secrets` and `agent-secrets-helper`; the macOS archives
hold `agent-secrets` alone. `agent-secrets --version` prints the release tag, such as
`agent-secrets legion-envoy-v28.1.1`. To also check that this repository's release
workflow on `main` built the archive from the commit the tag names, run, in the same shell and with
the GitHub CLI signed in:

```sh
commit=$(gh api "repos/cybermonklabs/legion/commits/$tag" --jq .sha) &&
  test -n "$commit" &&
  gh attestation verify "$asset" --repo cybermonklabs/legion \
    --cert-identity https://github.com/cybermonklabs/legion/.github/workflows/release-envoy-listener.yaml@refs/heads/main \
    --source-ref refs/heads/main --source-digest "$commit" --deny-self-hosted-runners
```

## Give a new machine access

A machine that runs agent sessions needs the helper and a machine login that its operator (the
person whose agents it runs) approves in Dispatch. The helper runs on Linux only, as a
`systemd --user` service. Replace `<broker URL>` with your deployment's broker address and
`<you@example.com>` with your Dispatch login:

```sh
export AGENT_SECRETS_URL=<broker URL>
mkdir -p ~/.config/agent-secrets
echo '<you@example.com>' > ~/.config/agent-secrets/operator
agent-secrets-helper install --url "$AGENT_SECRETS_URL"
loginctl enable-linger "$USER"
agent-secrets machine login
```

`loginctl enable-linger` keeps your `systemd --user` services, the helper among them, running
after you log out; without it, the helper stops when your last login session ends.

`agent-secrets machine login` prints a confirmation code and waits. Open Dispatch's machine-login
page (`/credentials/machine`; set `AGENT_SECRETS_APPROVE_URL` to Dispatch's address and the
command prints the full link), type the code, check that it matches your terminal, and approve. The
command then exits 0. Only the person named in the operator file can approve. The helper keeps the
login in `~/.config/agent-secrets/machine.json` and refreshes it itself, so you log a machine in
once; `agent-secrets machine login-status` prints `issued` while it holds the login.

Put `export AGENT_SECRETS_URL=<broker URL>` in the profile your shells and agents start from, and
start each agent as a machine session, so the helper signs for it:

```sh
agent-secrets register --wait 10 --exec -- <agent command>
```

A Kubernetes pod or a Google Cloud VM does not use the helper or a person's approval: the Legion
daemon enrolls its pods with their service-account tokens, and a VM enrolls itself (above). The
docs site's [Log a machine in](https://cybermonklabs.github.io/legion/broker/guides/log-a-machine-in/)
guide covers each step in full.

## Common commands

Each grant is for one session, and lasts until that session ends or for `--ttl`. What happens to a
request depends on the secret's `owner` and `tier` tags. Your own `tier=agent` secret is granted at
once to a session on a machine you logged in, and goes to you for approval from any other session.
Your own `tier=human` secret always goes to you for approval. A `shared` agent-tier secret is
granted at once; a `shared` human-tier one waits for anyone who signs in to the broker. A
service's secret goes only to that service's pods and VMs. Approvals happen on the broker's
approval page, which the approver's Dispatch Inbox links to. The docs site's
[Concepts](https://cybermonklabs.github.io/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) page has the full
table.

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
agent-secrets request DEMO_API_KEY --reason "Deploy" --ttl 1h
agent-secrets status <request_id>
agent-secrets self
agent-secrets revoke <grant_id>
```

In order: get a secret and run a command with it in its environment; ask without running
anything; show a request's state; show this session's enrollment and live grants; end one of this
session's grants.

From your own shell (outside every agent session), under this machine's login:

```sh
agent-secrets machine list
agent-secrets machine revoke <credential_id>
agent-secrets grant list
agent-secrets grant revoke <grant_id>
agent-secrets-helper sessions
```

`machine list` shows your machines' logins, `grant list` the live grants of your sessions and
those you approved, and `agent-secrets-helper sessions` the sessions this machine's helper holds.

Owners manage the secrets themselves under their own cloud sign-in, with no helper:

```sh
agent-secrets secret list
agent-secrets secret show NAME
agent-secrets secret create NAME --owner me --tier agent
agent-secrets secret set NAME
agent-secrets secret retag NAME --tier human
agent-secrets secret delete NAME
agent-secrets secret restore NAME
```

`agent-secrets --help` and `agent-secrets-helper --help` list every form and environment variable.

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
