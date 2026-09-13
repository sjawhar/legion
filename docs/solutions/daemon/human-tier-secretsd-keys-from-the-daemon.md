---
title: "Reading a human-tier secretsd key from the daemon: stdin is the identity, the grant is the terminal's, and revoke last"
category: daemon
tags:
  - secretsd
  - secrets
  - human-tier
  - private_key_secret
  - github-apps
  - key-rotation
  - stdin
  - tty
date: 2026-09-13
status: active
module: packages/daemon (config.ts loadGitHubApps), docs/deployment.md
related_issues:
  - "LEGION-77"
  - "sjawhar/legion#1017"
  - "sjawhar/legion#1033"
  - "LEGION-74"
  - "LEGION-19"
---

# Reading a human-tier secretsd key from the daemon

## Context

On the shared box every process runs as one Unix user and can ask the `secrets` broker (secretsd)
for any agent-tier key, so a credential only the daemon may hold — the two GitHub App private keys —
cannot be agent-tier (LEGION-74 found every pane could mint App tokens). LEGION-77 added
`github_apps.<role>.private_key_secret: <NAME>` (`resolvePrivateKeySecret`, `config.ts`): the daemon
checks the key's tier with `secrets get <NAME> --no-request`, refuses to boot unless it is `human`,
then fetches it with `secrets get <NAME> --value`. Everything below was learned making that work
against the real broker (`secrets` 3.2.1; source github.com/sjawhar/forward, crates `secrets` and
`proto`) and is what a future worker touching a daemon-side `secrets` call needs first.

## Status on the LEGION deployment: not in use, by decision

The mechanism is merged and tested but the LEGION deployment does not use it. On 2026-09-13 Sami
answered the store question (dispatch://LEGION-77/ask/5e3d2fa8-49d0-49d8-b4ea-5355b8f9b742) with
**Wait for Kubernetes**: the two App keys stay agent-tier on this machine, readable by every pane,
until the daemon runs in its own pod (LEGION-19), and the rotation happens at that cutover (recorded
on LEGION-74). Do not move the keys or the launcher here. `docs/deployment.md` opens with that
decision and presents everything below as the option a shared-box deployment may choose; how the
runbook came to be corrected after the fact is
`docs/solutions/legion/building-ahead-of-an-open-human-decision.md`.

## secretsd identifies a tokenless caller by its standard input

`caller_tty()` in `crates/proto/src/client.rs` is `isatty(0)` then `readlink /proc/self/fd/0`,
accepted when it starts with `/dev/`. No environment variable names the terminal. Two consequences:

- The `secrets` child must **inherit the daemon's stdin**: `spawnSync("secrets", args, { stdio:
  ["inherit", "pipe", "pipe"] })`. `spawnSync`'s default piped stdin gives the child a pipe on fd 0
  and the broker answers `there is neither a terminal tty nor a session token`.
- The **daemon itself** must be started with stdin on a terminal: no `< /dev/null`, `nohup`,
  `setsid` with redirected input, or systemd unit. A `while :; do legion start …; done` loop in a
  tmux pane inherits the pane's tty and is fine. The daemon does not pre-check `process.stdin.isTTY`;
  secretsd's refusal is quoted verbatim in the boot error and already names the cause.

The unit test for this asserts **equality**, not "is a tty": the fake `secrets` records
`readlink /proc/self/fd/0` and the test compares it to `fs.readlinkSync("/proc/self/fd/0")` of the
test process itself (under `bun test` that is a `socket:[…]`, which is exactly the point — the child
got the parent's fd 0, whatever it is).

## Drop `SECRETSD_SESSION_TOKEN_FILE`, keep `SECRETSD_SOCK`

With `SECRETSD_SESSION_TOKEN_FILE` set the client presents that token and the broker checks the
caller descends from the registering session's process tree (`ForeignCaller` otherwise). A daemon
started from inside an OMP session's tree would therefore *succeed* on the token path and land the
App keys' grant on that agent session — the exposure the feature exists to close. So the two
`secrets` children get the daemon's own `process.env` minus exactly that one variable (they are the
daemon acting for itself, like `private_key_command`; panes get the allow-listed `paneEnv`
instead — LEGION-74), and the request always takes the terminal path. `SECRETSD_SOCK` is the
broker's socket override
(`BrokerClient::from_environment`), not a session; dropping it would break the client. The
runbook's `env -u SECRETSD_SESSION_TOKEN_FILE` in the launcher shell is belt to this brace.

One more terminal rule: a tty from which any OMP/OpenCode session has presented a token since the
broker last started is remembered as an agent terminal (`agent_ttys`) and tokenless requests from it
are refused (`a tokenless request came from a known agent terminal`). The launcher pane must be one
no agent session has run in.

## The grant is the terminal's, for the broker's lifetime or 12 h

A tokenless grant is scoped `Scope::Tty { tty, boot_id }` (`crates/secrets/src/grants.rs`): the
terminal device plus the broker's own boot id. Every later `secrets get <NAME> --value` from a new
process on the same terminal reuses it silently. So the tap cost is two taps (one per App key) per
launcher pane, not per daemon start: a restart or crash-relaunch in the same pane costs nothing.
The grant ends on: broker restart (grants are memory-only), 12 h (`SECRETSD_MAX_GRANT_SECS`,
default 43200), the tty device vanishing, `secrets lock`, or the key's stored file changing (a
rotation). The daemon holds the decoded PEM in memory for its lifetime, so a grant ending never
interrupts a running daemon — only its next start.

## The tier check is free; requests are one at a time by construction

`secrets get <NAME> --no-request` prints `{"key":"<NAME>","tier":"agent"}` or
`{"key":"<NAME>","tier":"human","grant":<bool>}` and never opens a request. Only a `human` key is
fetched; anything else refuses with `App private key <NAME> is readable by agent-tier callers; move
it to a daemon-only store`. `loadGitHubApps` iterates the two roles and `spawnSync` blocks, so at
most one broker request is pending — the using-secrets "one at a time" rule holds without code.
There is deliberately no daemon-imposed timeout: the broker's 90 s approval window (the CLI waits
120 s) is the failure, reported as `secrets get <NAME> --value failed (exit N): <stderr>`.

An agent-tier key's `--value` also works tokenless from any stdin — that is what let the live boot
use a fake `secrets` that fabricated only the tier status and delegated `--value` to the real binary
(`docs/solutions/testing/fake-cli-on-path-outputs-from-files-and-a-call-log.md`).

## Proving "a pane cannot read it" without blinking anyone's key

From a worker pane: `secrets get <NAME> --no-request` must print `"tier":"human","grant":false`.
Never `secrets <NAME> -- <cmd>` or `secrets get <NAME>` (no `--no-request`) from a pane once the key
is human-tier: the pane carries an agent session token, so the broker does not refuse — it **queues
a request on the operator's YubiKey under the pane's session**, blinking until tapped or 90 s pass.
That is an unwatched request a stray tap would satisfy (the spec's Rejected list, and the architect's
ruling that replaced the plan's `secrets <KEY> -- true` step). The status check proves the same
boundary — no agent-tier copy, no grant — with no request opened.

## Rotating a key the daemon holds in memory: rotate, store, restart and confirm, then revoke

The reviewer caught the runbook revoking before restarting. The running daemon signs every
installation-token request with the PEM it decoded at start-up, so revoking the old key first breaks
`legion gh`, the `jj git push` credential, and identity leases in every pane until the restart —
with no rollback if that restart fails (an untapped request, a bad base64 paste). Order:

1. Generate the new key in GitHub App settings; store it under the same name
   (`base64 -w0 < key.pem | secrets edit-human <NAME>`). Revoke nothing yet.
2. Restart the daemon from the launcher pane (the rewritten file needs a fresh grant: two taps).
   Confirm a token mints with the new key: `legion gh -- auth status` from a fresh worker pane.
3. Only now revoke the old key.

The same order applies to any credential a long-running process caches at boot.

## Two small code notes

- The exactly-one-of-three rule is `[hasInlineKey, hasCommand, hasSecret].filter(Boolean).length !==
  1`. The old two-source check was `hasInlineKey === hasCommand` (an XNOR that happened to mean "both
  or neither"); do not "simplify" the three-way version back to chained equality — it would accept
  two sources.
- `readSecretName` rejects whitespace only because the name is one argv token handed to `secrets`
  directly, never through a shell: whitespace means the operator pasted a command.

## Related

- `docs/deployment.md`: the operator runbook these facts feed (launcher pane, tap cost, rotation).
- `docs/solutions/testing/fake-cli-on-path-outputs-from-files-and-a-call-log.md`: how the stdin and
  env facts above are asserted from outside the child, and how a fake `secrets` boots a real daemon.
- `docs/solutions/integration-patterns/secret-file-pointer-precedence.md`: Legion's own `*_FILE`
  pointer mechanism for panes — a different secret path from the broker's.
