# Deploying the Legion daemon on a shared box

This runbook covers the one deployment shape Legion has today: the daemon, its tmux server, and
every pane it launches run as the same Unix user on one machine, and every process on that machine
can ask the `secrets` broker (secretsd) for any agent-tier key. A credential that only the daemon
may hold therefore cannot live in the agent tier without every pane being able to read it.
Kubernetes deployments (`docs/kubernetes.md`) get a pod boundary instead and do not need this page.

## The pane guard

Because every pane runs as the daemon's own user, the Legion extension (`packages/pi-envoy`) holds
each phase worker's and root architect's tool calls, and those of any `task` subagent they spawn,
to a boundary before they run; it refuses the shared jj operation-log rewrites (LEGION-45) and,
since LEGION-121, destructive commands and signals outside the pane's own work. A `bash` command,
`eval` code, or `hub` process start may delete (`rm`, `unlink`, `find -delete`, `find -exec rm`,
`shred`), move (`mv`), truncate (`truncate`), overwrite by redirection or `tee` (an existing file
only), or recursively change the mode or owner (`chmod -R`, `chown -R`) of paths under the pane's
issue workspace (`LEGION_WORKSPACE`, its `.jj` included) and any directory below `/tmp` except
`/tmp` itself, a glob over it, its tmux and ssh socket directories, and the `/tmp` directory that
holds the pane's `HOME` or `TMUX_TMPDIR` when either sits there, by that directory's exact name (an
e2e rig's run directory holds its Oh My Pi home). The guard cannot tell which other permitted
`/tmp` directory belongs to this pane. It parses the command with a bash parser and resolves each
target as bash would: through `$HOME`, `~`, variables set earlier in the same command,
`$(mktemp -d)`, `cd`, braces, the paths `realpath`, `dirname`, `basename` and `readlink -f` print,
pattern replacement and removal of a known ASCII value (`${v//a/b}`, `${v#*:}`), a function's
output, command substitutions, and the scripts the command runs (`bash <file>`, `sh -c`, `source`,
a heredoc fed to a shell, a script run by path, python/node/bun scripts, one it writes first), with
the arguments it gives them: an argument loop (`while [ $# -gt 0 ]; do case "$1" in ...`) over
arguments it knows is walked as bash runs it. It follows the writes to a variable it sees (a
builtin's, arithmetic, `${v:=x}`, a function's `local`), and a write it cannot model, such as one
under a name computed at run time or `eval "$(tool)"`, leaves every variable unknown, so a target
built from one afterwards is refused; a nameref (`declare -n`) is refused outright. It does not
follow a `source` of a path it cannot read at check time (a process substitution, `/dev/fd/N`),
which it takes as sourcing nothing, nor an assignment to `IFS`: it splits an unquoted value on
whitespace alone. A script the command writes and then runs is refused as one the guard cannot
read when a value it cannot know is written into it by `printf %s`, `printf %q` or `echo` (bash
parses the value as code, so a `;`, a quote or a newline in it escapes any position), when `echo`
takes an option first, or when an unquoted here-document holds text bash expands; `printf %d`
writes only digits and a sign, so it is read. It is refused too when the write sits anywhere the
shell may not have performed it: inside an `if`, `elif` or `else` body, an `&&` or `||`
right-hand side, a `case` arm the guard cannot decide, a `while`, `until`, `for`, `for ((;;))` or
`select` body, a handler whose signal may never arrive or that a body the shell may skip
registered, or one definition of several a name may hold — and inside a command this shell does
not wait for: a background command, a coprocess, a coprocess-like earlier part of the same
pipeline, or a process substitution. Whether such a body runs is a fact of the run, so the guard
cannot decide it and will not assume it; write the script with the write tool first. A write on
the straight-line path is read as before, and so is one in a subshell, a brace group, a command
substitution, a called function or an `EXIT` handler this shell certainly registered. The same
rule governs a file holding a pid that `kill "$(<file)"` reads, except that a body which can only
replace one pid this shell started with another leaves it signalable. A shell or interpreter
reading such a here-document as its program is refused too. A target with no proven path prefix
is refused, as is a command the
parser reports as malformed. An unknown trailing component under a prefix already proven inside a
permitted root remains allowed. `pkill`, `killall`, and `fuser -k` are refused outright. For
`tmux kill-*`, the guard resolves the socket as tmux does
(`-S`, then `-L` under `TMUX_TMPDIR` or `/tmp`, then `$TMUX`, then the default socket) and refuses
a resolved path outside the pane roots. `kill` only reaches a pid that `/proc` shows descending
from the pane's own Oh My Pi process. Every refusal names the target, where it resolved, and the
rule, so the agent can rewrite the command.

A word the guard cannot read whole is not read as harmless. Where a word selects a dangerous
option or subcommand — an extract mode and `-C` for `tar`, `-d` for `unzip`, `-k` for `fuser`,
a kill subcommand for `tmux`, `-R` for `chmod` and `chown`, a `find` predicate, `-n` for
`declare`, `-v` for `printf`, an interpreter's `-c`, a wrapper's own options (`sudo`, `env`,
`nice`, `nohup`, …), and a `busybox` or `toybox` applet — the guard takes it as that option and
the command's other arguments decide what that means, so `tar "$mode" -C .` runs and
`tar "$mode" -C "$HOME"` does not. Both spellings count, since each of those options has a long
form: `tar --"$m" -C "$HOME"` is refused as `tar -"$m" -C "$HOME"` is.

Three things keep that from refusing ordinary scripts. The guard reads a word's leading literal
prefix, so `local root="$1"` is an assignment and never an option and `--socket="$s"` is never
`-k`. It judges the readings together rather than taking the worst of each: read as `-R` a word is
no path, so `chmod +x "$out"` has no path left to change recursively, and read as a predicate a
word is no search root, so `find "$dir" -type f` searches the working directory. And it counts the
fields bash will make rather than the words written, so an unquoted word bash may split, which can
supply the option and leave every operand standing at once, is judged where its quoted form is
not.

An option may also carry its value inside its own word, wherever the option stands (`-C"$dir"`,
`-xC"$dir"`, `--directory="$d"`), and that carried value is the one judged. `tmux` is scanned per
segment, a `;` starting another whether it stands as its own word or at the end of one (real tmux
takes `'kill-server;'` as a kill), since a command after a `;` reaches the same server; a word the
guard cannot read may itself be that `;`, and its server options are read by the prefix it can
see, so `-S"$sock"` is an option and not the subcommand. `timeout` has one reading judged per word
standing where its duration could, since each may be an option instead. A `busybox` or `toybox`
applet word the guard cannot read is refused outright: no
reading of it is harmless, because that binary carries `halt`, `poweroff` and `reboot` besides
`rm`, `sh` and `killall`, and those need no operand.

Two members of the family stay open, and both name an open set of programs the residual below
already covers: a command whose own name the guard cannot read, and the program word of `xargs`.
A wrapper's unreadable option word is not one of them, since the program it runs is written
plainly after it. Two narrower residuals are known and measured: a tmux segment that exists only
because a word may have been the `;` and whose subcommand is also unreadable, which is two
unknowns deep and so the same shape as an unreadable command name; and, in the other direction,
one over-refusal — an unquoted `chmod $mode <path>`, where the one word may be both the option
and a path.

The guard reads text before it runs, so it cannot see what is only decided at run time: a compiled
program or anything a command runs without naming it on the command line (a `make` target, a test
runner, `npm run`), a program whose name is itself a variable or a command's output
(`$cmd`, `eval "$(tool)"`), Python or JavaScript whose paths or pids come from values it cannot
evaluate (the `eval` tool's kernels included; known prefixes are still judged), and interpreters
the guard does not read (`perl`, `ruby`, `awk`), commands outside the families above that overwrite
a destination (`cp`, `dd`, `ln -f`, `install`), an edit in place (`sed -i`), a directory reached
through a `cd` that failed, and the `write` and `edit` tools. A `>>` append to a file the guard
holds no model of — one no redirect or `tee` in the same command named — leaves that file's
contents unknown, never empty: the append is allowed, and running that file in the same command is
refused rather than read as holding only what was appended. A file the same command wrote is read
as its model says, so writing a script with `>` and then running it still works. The model is per
command, and it records a write whose content the guard renders (`echo >`, `printf >`,
`cat > <<EOF`, `tee f <<EOF`) wherever it read that write, whether or not the shell would run it:
the guard reads every region whose execution it cannot decide, so such a write under
`false &&`, `true ||`, an `if` or `case` arm the shell skips, a `while`/`until` body that never
runs, or a `for` whose word list it cannot decide still counts, and an append after it is read
rather than unknown. That is a residual of this model, and it closes once a write the shell may
never reach leaves the file unknown. `tee -a` is stricter: it leaves the file unknown whatever the
guard knew, so appending with it to a file this command wrote and then running that file is refused
where `>>` is allowed. Neither rule asks the filesystem what a file holds, since an earlier stage of
the same command can change that. The guard checks a destination for `rsync`, extract-mode `tar`,
and `unzip` when it can identify one. It also cannot distinguish one pane's allowed `/tmp` directory
from another. A running shell started through `hub` can receive later unguarded input, and
`xd://debug` can launch an unguarded program. The documented residuals are `git -C <path> clean`,
Python loop values, an aliased CommonJS `require`, an `eval` trap whose outer exit timing is not
modeled, an `rsync` destination followed by an unrecognised valued option, and a `TMUX` value that
begins with a comma and therefore names no socket path.
This is a mistake-guard, not a sandbox: it exists because an agent probe deleted the operator's home
directory. LEGION-122 and the Kubernetes pod boundary are the hard isolation controls.

## Current decision for the LEGION deployment

On 2026-09-13 Sami decided that the two GitHub App private keys stay in the secrets store's agent
tier on this machine, readable by every pane, until the daemon runs in its own Kubernetes pod
(LEGION-19, "Legion on Kubernetes"). Do not move them to the human tier and do not change the
daemon's launcher on this deployment. The rotation happens at that cutover: the new keys are
generated straight into the cluster's secret store, the App keys are mounted into the daemon pod
only, and the old keys are revoked once the shared-box daemon stops using them. That rotation is
recorded on LEGION-74. The rest of this page documents the human-tier mechanism and
`private_key_secret` for a shared-box deployment that chooses that boundary.

## Where daemon-only credentials live

The two GitHub App private keys (`LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64` for the implement App,
`GH_REVIEW_APP_PRIVATE_KEY_B64` for the review App) are daemon-only: a pane that could read them
could mint installation tokens as either App and act on GitHub outside the daemon's grant path.
`GH_AGENT_APP_PRIVATE_KEY_B64` is the sjawhar-agent App's key and must not be configured for Legion.

On a shared box that chooses the human-tier boundary, they live in secretsd's **human tier**: one
file per key under a source root's `secrets.human.d/`, written with `secrets edit-human <NAME>` (it
accepts the value on stdin non-interactively). Never in
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
    private_key_secret: LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64
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

When a shared-box deployment rotates its keys under the human-tier boundary, rotation is pointless
while panes can still read the keys, so the order is:

1. Deploy the pane fix from LEGION-74 ("Every Legion pane inherits the daemon's GitHub App private
   keys"): no pane inherits the keys from the daemon's environment or the tmux server.
2. Move the two keys to the human tier: `secrets edit-human LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64` and
   `secrets edit-human GH_REVIEW_APP_PRIVATE_KEY_B64` with the current values, then remove both from
   the agent-tier `secrets.env`. `secrets get <NAME> --no-request` must now report `"tier":"human"`.
3. Switch `legion.yaml` to `private_key_secret` for both Apps and restart the daemon from the
   launcher pane described above (two taps).
4. Prove a pane cannot read them: from a fresh worker pane run
   `secrets get LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 --no-request` and
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
