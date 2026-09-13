# Deploying the Legion daemon on a shared box

This runbook covers the one deployment shape Legion has today: the daemon, its tmux server, and
every pane it launches run as the same Unix user on one machine, and every process on that machine
can ask the `secrets` broker (secretsd) for any agent-tier key. A credential that only the daemon
may hold therefore cannot live in the agent tier. Kubernetes deployments (`docs/kubernetes.md`)
get a pod boundary instead and do not need this page.

## Where daemon-only credentials live

The two GitHub App private keys (`GH_AGENT_APP_PRIVATE_KEY_B64` for the implement App,
`GH_REVIEW_APP_PRIVATE_KEY_B64` for the review App) are daemon-only: a pane that could read them
could mint installation tokens as either App and act on GitHub outside the daemon's grant path.

They live in secretsd's **human tier**: one file per key under a source root's `secrets.human.d/`,
written with `secrets edit-human <NAME>` (it accepts the value on stdin non-interactively). Never in
the agent-tier files (`secrets.env`, `secrets.local.env` in `~/.dotfiles` or `~/.dotfiles/.secrets`):
every process on the box reads those without approval. `secrets list` shows each key's tier;
`secrets get <NAME> --no-request` prints it as JSON without requesting anything
(`{"key":"<NAME>","tier":"human","grant":false}` is what you want to see). A key present in two
source roots is refused by secretsd rather than resolved, so remove the agent-tier copy when you
add the human-tier one.

## How the daemon launcher gets them

A human-tier key is released only to a caller secretsd can scope: a process holding an agent
session's token, or a process whose standard input is a terminal. The daemon must use the second
path — the first would make an agent session the grantee — so:

- Start the daemon in a terminal pane the operator opens by hand (a tmux window or pane in the
  operator's own session), in which no OMP or OpenCode agent session has run since secretsd last
  started. secretsd remembers a terminal an agent session has used and refuses tokenless requests
  from it with `a tokenless request came from a known agent terminal`.
- The daemon and its supervisor loop (for example `while :; do legion start <team> --config
  legion.yaml; sleep 5; done`) run inside that pane's process tree with **stdin left on the pane's
  terminal**: no `< /dev/null`, no `nohup`, no `setsid` with stdin redirected. secretsd identifies
  the caller by its standard input; a daemon whose stdin is not the terminal is refused with
  `there is neither a terminal tty nor a session token`.
- Unset `SECRETSD_SESSION_TOKEN_FILE` in that shell if it is set (`env -u SECRETSD_SESSION_TOKEN_FILE`);
  the daemon also drops it from the `secrets` children it runs, so the request always takes the
  terminal path.

The tap cost, as secretsd actually scopes grants: the first `secrets get <NAME> --value` from that
terminal makes the YubiKey blink once per key — two taps for the two App keys. The grant is held for
the pair (terminal, secretsd run), so every later request from that same terminal is silent: a
daemon restart in the same pane costs no taps. The grant ends, and the next daemon start needs two
taps again, when any of these happens: secretsd restarts (grants are memory-only), 12 hours pass
since the tap (`SECRETSD_MAX_GRANT_SECS`, default 43200), the pane's terminal is closed,
`secrets lock` runs, or the key is rewritten (a rotation). The daemon holds the decoded PEM in
memory for its whole lifetime, so a grant ending never interrupts a running daemon; only a restart
after it notices. A request nobody taps within 90 seconds fails with secretsd's timeout message and
the daemon refuses to start, quoting it.

## The `private_key_secret` form

```yaml
github_apps:
  implement:
    app_id: "3202636"
    private_key_secret: GH_AGENT_APP_PRIVATE_KEY_B64
  review:
    app_id: "3202653"
    private_key_secret: GH_REVIEW_APP_PRIVATE_KEY_B64
```

`private_key_secret` names a secretsd key whose value is the App's PEM, base64-encoded (the same
encoding the `_B64` keys already use). It is one of exactly three private-key sources per App —
`private_key` (inline PEM), `private_key_command` (a shell command whose stdout is the PEM), and
`private_key_secret` — and naming two of them refuses start-up with
`github_apps.<role> requires exactly one of private_key, private_key_command, or private_key_secret`.
On a box where panes share the daemon's user, `private_key_secret` is the only form the daemon can
verify: a shell string cannot be checked for what it reads, a key name can.

At start-up the daemon runs `secrets get <NAME> --no-request` and reads the tier. It refuses to
start with

    App private key <NAME> is readable by agent-tier callers; move it to a daemon-only store

when the tier is anything but `human` — that refusal means the key is still in an agent-tier file
and every pane can read it; move it with `secrets edit-human` and remove the agent-tier entry. Then
it logs `[legion] requesting <NAME> from secretsd (human tier; a YubiKey tap may be needed)`, runs
`secrets get <NAME> --value`, base64-decodes the output, and refuses unless the result begins
`-----BEGIN`. Each of these also refuses start-up, naming `github_apps.<role>.private_key_secret`
and the key (never the value): `secrets` not on the launcher shell's PATH, a key secretsd does not
know (`secret '<NAME>' not found`), a status it cannot parse, a value that is not a PEM, and any
secretsd refusal (timeout, denial, no terminal scope) quoted from `secrets`' stderr.
`legion start --check-config` validates the key name only and never runs `secrets`.

`secrets` is found on the PATH of the shell that starts the daemon (configuration loads before the
daemon resolves its `mise` environment); under a login shell on the shared box that is
`~/.mise/shims/secrets`.

## Rotating the App keys

Rotation is pointless while panes can still read the keys, so the order is:

1. Deploy the pane fix from LEGION-74 ("Every Legion pane inherits the daemon's GitHub App private
   keys"): no pane inherits the keys from the daemon's environment or the tmux server.
2. Move the two keys to the human tier: `secrets edit-human GH_AGENT_APP_PRIVATE_KEY_B64` and
   `secrets edit-human GH_REVIEW_APP_PRIVATE_KEY_B64` with the current values, then remove both from
   the agent-tier `secrets.env`. `secrets get <NAME> --no-request` must now report `"tier":"human"`.
3. Switch `legion.yaml` to `private_key_secret` for both Apps and restart the daemon from the
   launcher pane described above (two taps).
4. Prove a pane cannot read them: from a fresh worker pane run
   `secrets get GH_AGENT_APP_PRIVATE_KEY_B64 --no-request` and
   `secrets get GH_REVIEW_APP_PRIVATE_KEY_B64 --no-request`. Both must print
   `"tier":"human","grant":false`: the key is in no agent-tier file, and the pane holds no grant,
   so the only way it could obtain the key is a request on the operator's YubiKey. Do not prove it
   with `secrets <KEY> -- <cmd>` from the pane: on a human-tier key that command is not refused
   but queued on the operator's YubiKey under the pane's session (it blinks until tapped or 90
   seconds pass), an unwatched request a stray tap would satisfy; the status check proves the same
   boundary without opening a request. Meanwhile `legion gh -- auth status` from that same pane
   still succeeds because the daemon mints the token.
5. Rotate: in each GitHub App's settings, generate a new private key. Store each with
   `secrets edit-human <NAME>` under the same names (base64-encode the downloaded PEM first:
   `base64 -w0 < key.pem | secrets edit-human <NAME>`). Do not revoke anything yet.
6. Restart the daemon from the launcher pane (the rewritten keys need a fresh grant: two taps).
   Confirm a grant mints with the new keys: `legion gh -- auth status` from a fresh worker pane.
7. Only now revoke the old private keys in GitHub App settings. The order matters because the
   running daemon signs every installation-token request with the key it decoded at start-up and
   holds in memory until it restarts — revoking first would break `legion gh`, the `jj git push`
   credential, and identity leases in every pane until step 6 completes, with no rollback if that
   restart fails (an untapped request, a bad base64 paste).

Record the rotation on LEGION-74 as its spec asks (dates and key fingerprints only; never key
material).
