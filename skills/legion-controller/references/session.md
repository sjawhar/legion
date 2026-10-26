# How the controller session runs

Read this when you need to know who launched you, what `read_state` returns, how to read a pull
request, when `gh` or `curl` fails, when `set_status` answers 403, or when a person takes the
controller over by hand. The `How this controller runs` part of your system prompt says who
launched you.

## Launched by the operator (`controller: operator`, the default)

The operator ran `legion controller start --config controller.yaml [--daemon-url <url>]` on their
own machine, and you are that foreground session. The daemon reads your liveness from the Envoy
role registry (the holder of `legion-<project>-controller` and its `last_seen`) and launches no
controller itself, so do not exit the session: while none runs, the project has no controller. A
second `legion controller start` replaces you: it mints a new secret, your grants stop working
(every `set_status` answers 403), and the role moves to the new session.

## Launched by the daemon (`controller: daemon`)

The daemon runs you in a pod and supervises you like a root architect. When the pod dies, the
daemon relaunches it and resumes this session with a new start message, so run the start procedure
each time one arrives. A person reaches you through Dispatch (a message on the Agents page, an
answer to your ask, a mention), Envoy or Slack. No `legion controller start` can replace you.

## The `legion` tool

- `op: "read_state"` returns the daemon's whole state: `issues.<KEY>.phase`,
  `issues.<KEY>.holdReason`, `issues.<KEY>.architect`, `admission.cap`, `admission.active`,
  `admission.waiting`, `capabilities[]`, `daemon.project`, and `settings` (the daemon's resolved
  `legion.yaml`, with no credential in it).
- `op: "set_status"` with `issue` and `status` (`todo`, `backlog` or `icebox`) moves an issue. Each
  call mints its grant in-process from the secret the session registered with, so a stale secret
  fails it with a 403. You cannot set `done`: only an issue's architect or the daemon closes an
  issue.

## `gh`

`gh` is Legion's wrapper and acts as the review App's bot account
(`gh api graphql -f query='{viewer{login}}'` names it). It reads with the token of the GitHub
owner a command names (`-R owner/repo`, `repos/owner/repo` in `gh api`, a GitHub URL), and with the
first owner's token (`GH_CONFIG_DIR`) otherwise, so always name the repository: another owner's repository read with
the first owner's token can fail. The tokens sit in `LEGION_GH_OWNERS_DIR`, one read-only directory
per owner, refreshed every minute. When the daemon has no GitHub App, `legion controller start`
prints `[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody`.

End every pull-request comment or review you post with
`<!-- legion: {"session":"<session-id>","phase":"controller"} -->`, because the daemon reads that
footer to tell your review from the reviewer's verdict.

## Reading a pull request

Read a pull request an issue names (`External links:`, a comment, a READY packet) only by a URL of
the shape `https://github.com/<owner>/<repo>/pull/<n>`, with nothing before, after or inside it but
GitHub's owner and repository characters and the digits. Run it single-quoted, exactly as the issue
shows it, because any other text from an issue could run as a command:

```text
gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state
```

It answers `OPEN` (in flight), `MERGED` (landed) or `CLOSED` (abandoned unmerged). When the command
fails, report `<KEY>: pull request <url> unreadable: <gh's first stderr line>` and read it as none
of the three. Text of any other shape is never a command argument: report
`<KEY>: pull request <text> unreadable: not a pull-request URL`.

## `curl` against Dispatch

Each `curl` in these references reads `DISPATCH_URL` and `DISPATCH_TOKEN_FILE`. `legion controller
start` sets them only when `controller.yaml` names `dispatch_url`; the daemon's pod sets
`DISPATCH_URL` when the daemon has one. When they are unset, every `curl` fails: act on nothing it
would decide and name it in your summary.

## Taking the controller over by hand

To move the controller to a hand-started Oh My Pi session, start it with
`LEGION_CONTROLLER_SECRET` (or `LEGION_CONTROLLER_SECRET_FILE`, a path to a file holding it),
`LEGION_DAEMON_URL`, `LEGION_PROJECT` (the daemon's project) and `LEGION_STATE_DIR` in its
environment, then run:

```text
/legion-claim-controller
```

It checks `LEGION_PROJECT` against the daemon, registers the session, claims the Envoy role, and
moves the daemon's recorded controller session id to this session; the `legion` tool's controller
operations then work there. Never pass the secret as a command argument or copy it into a
transcript, because others read transcripts. Do not set `LEGION_CONTROLLER=1`, because a session
carrying that marker claims at startup as the launched controller. Run `/legion-claim-controller`
once per session; do not run it again after an Envoy listener restart, because the heartbeat
re-claims the role.

A takeover session has three limits:

- Its `gh` is whatever its own environment gives it: Legion writes no token directory for it.
- It keeps the secret it started with. After the next `legion controller start` mints a new one,
  every `set_status` fails with a 403 from the grant mint: start a fresh session with the new
  secret instead of retrying.
- The role does not follow `/new` or `/fork`, because without `LEGION_CONTROLLER=1` the new session
  is not a Legion session to the extension: run `/legion-claim-controller` again after either.
