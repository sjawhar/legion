---
name: agent-secrets
description: Use before a command that needs a token, a key or a password; before you say that a service or an API is out of reach; and when an instruction names `agent-secrets`.
---

# agent-secrets

The secrets broker holds the credentials this deployment gives its agents. `agent-secrets` is its
client. It asks the broker for a secret and gives it to one command, in that command's environment.
Your session never holds the value, and nothing of it reaches your transcript.

## The one form

```sh
agent-secrets NAME... [--reason TEXT] [--wait DURATION] [--ttl DURATION] -- <command> [args...]
```

- Each granted secret reaches `<command>`'s environment under its own name, as the value or, for
  a secret behind the airlock, as a placeholder standing for it (below). Nothing runs unless
  every name is granted: a denial of one name denies the whole request, so the command never starts
  with some of its secrets missing (`cmdExec`, `packages/envoy/cmd/agent-secrets/main.go`).
- `--reason` is what a person reads if the request waits for a decision. Write one.
- `agent-secrets --help` holds the rest: the flags that narrow a GitHub App token
  (`--github-owner`, `--github-repo`, `--github-read-only`), and how long a grant lasts.
- A narrowing and the airlock do not go together. Ask for a narrowing in a run whose grant holds
  a secret behind the airlock (below) and the broker refuses the release, naming that secret;
  nothing runs. Ask for the GitHub App secret in a run of its own, with no airlocked name beside
  it.

## The command reads the value, your shell does not

This form sends an empty value, and the API answers as if you had sent no credential:

```sh
# wrong
agent-secrets LEGION_SLACK_BOT_TOKEN -- \
  curl -sS -H "Authorization: Bearer $LEGION_SLACK_BOT_TOKEN" https://slack.com/api/auth.test
```

Your own shell expands `$LEGION_SLACK_BOT_TOKEN` before `agent-secrets` runs, and in your shell that
name is empty. Measured on 2026-11-04 from a Legion issue pod: Slack answered
`{"ok":false,"error":"not_authed"}`. What the client hands over — the value, or the placeholder of
a secret behind the airlock — exists only in the environment of the command the client starts, so
that command must read it itself. Put the command in single quotes, inside
`sh -c`, where the quotes keep the expansion for the child:

```sh
# right
agent-secrets LEGION_SLACK_BOT_TOKEN -- \
  sh -c 'curl -sS -H "Authorization: Bearer $LEGION_SLACK_BOT_TOKEN" https://slack.com/api/auth.test'
```

The repository uses the same form for a GitHub App token in `docs/deployment.md`:

```sh
agent-secrets LEGION_IMPLEMENT_APP --reason=… -- sh -c 'printf %s "$LEGION_IMPLEMENT_APP"'
```

## A secret behind the airlock

Your deployment can hold a secret's value back and give your command a stand-in. You see it in the
command's environment: the name holds `agent-secrets-placeholder-` and 32 letters and digits, not
the value. A length or a prefix probe there reads the placeholder, and proves nothing about the
real value.

A call to that secret's service goes through the broker's own proxy, which `agent-secrets` names
in the run's environment (`HTTPS_PROXY`, a `NO_PROXY` that keeps the secret's hosts on that proxy,
and a trust bundle). The proxy swaps the placeholder for the real value in the headers the
secret's rule names. Three things follow for the command you write:

- Make every call inside the `agent-secrets NAME -- command` run, with the environment that run
  gives it. A tool that keeps a token in a cache file across runs, or that makes its calls outside
  the run, stops working. Ask for the grant in the run that spends it.

- Pass no value you obtained some other way. The proxy refuses a request whose headers already
  carry the real value.

- The placeholder is that one grant's, and works only while the grant lives.

A refusal comes from the proxy, not from the service:

- An HTTP error naming the rule that refused it: a TLS name or `Host` that is not the host the
  command asked for, a method or a path outside the entry's `allow`, a path with a `.` or `..`
  segment, an `X-HTTP-Method-Override`, `X-HTTP-Method` or `X-Method-Override` header where the
  entry lists methods, plain `http://` to a host an entry binds, or a private address.

- `407` once the grant ends, on every call to any host. A WebSocket open at that moment is
  closed with the grant. A tunnel to a host no entry binds carries no value, so it runs to its
  idle limit instead.

- `503` when the broker could not read the grant at all. The grant has not ended and the call
  can be made again.

A retry fixes neither of the first two, and neither does another name. A `407` needs a new
grant. A rule's refusal needs a change to the rules file in the deployment's own repository,
which a person reviews. Report the refusal as you report a service out of reach: the command you
ran and the output you saw.

Two calls are not refused by the proxy. A call to a host no entry binds goes out unread, and a
placeholder in a header the entry does not name goes out as it is, so the service refuses the
placeholder and its own error is what you see.

## The value never reaches your transcript

Your transcript keeps everything a command prints.

- Never echo or print the value, and never run `curl -v`, `curl --trace` or a shell with `set -x`
  around it: each of those writes the header, and so the value, to your output.
- Check a value's shape by its length and its prefix alone. For a secret behind the airlock, that
  reads the placeholder's shape and not the value's:

  ```sh
  agent-secrets NAME -- sh -c 'echo "${#NAME}"; case "$NAME" in ghp_*) echo prefix-ok;; *) echo prefix-no;; esac'
  ```

- Compare two values only as equal or not equal. Never print, slice or diff their characters.

## Read the answer, not the status alone

Slack and most HTTP APIs answer `200` with `"ok": false`, so a call that failed can still exit 0.
Read the body. Send it to `jq` straight from `curl`, or through `printf '%s'`, and never through
`echo`: the pod's `sh` turns a `\n` inside a JSON string into a control character, which `jq` then
refuses.

## What the exit status means

After a grant the client writes one line to stderr, `agent-secrets: <names> from <source>`, and the
status is then the command's own. One case breaks that: a line `agent-secrets: exec <command>: …`
after it means the command could not start, and the status is 1.

Without the grant line the status is the client's own:

- **1** — a refusal or a failure that the message names: a name the broker does not serve
  (`UNKNOWN_SECRET`), a broker it cannot reach, a machine that is not logged in to the broker, a
  request cancelled or expired, `REQUEST_PENDING`, a grant that is no longer live, or a command
  that is not on `PATH`.
- **2** — a usage error: no broker identity at all (no key in the key dir and no helper socket), no
  `AGENT_SECRETS_URL`, no `--`, no `NAME`, or a flag value the client refuses.
- **75** — the request still waits for a person. The message names who decides it, where, and the
  `agent-secrets status <request_id>` that reads it.
- **77** — the request was denied.

The other statuses come from forms this skill does not use: 76 from `agent-secrets renew`, a
refusal that is permanent; 126 and 127 from `agent-secrets register --exec`, whose command could not
run or was not found; and 129, 130, 131 and 143 from a signal at the value prompt of
`agent-secrets secret create` or `secret set`. `agent-secrets --help` lists them all.

## Waiting, and a grant you already hold

- `--wait` waits 30 minutes by default. A session that must not block passes `--wait 0s`, reads the
  request id from the message, and reads the decision later with
  `agent-secrets status <request_id>`.
- A later run with the same names and the same `--ttl` uses the live grant again and asks nobody;
  while the first request still waits, such a run joins it. Any other run that would itself wait
  for a person and shares a name with a waiting request, with another `--ttl` or another set of
  names, is refused `REQUEST_PENDING`, which names the request already waiting.
- `agent-secrets identity` exits 0 when this session has a broker identity and 1 when it has none,
  and it asks the broker nothing. `agent-secrets self` prints this session's live grants.

## Where a name comes from, and how you may use it

The broker tells a session no names. A name comes from your deployment instructions, your role
prompt, a skill, or the issue you work on. There is no list to browse and nothing to guess.

- Request a secret only to use it for the work in front of you. A request made to learn whether a
  secret exists reaches a person's Inbox.
- `UNKNOWN_SECRET` and a denial are final. Do not ask again, and do not try a name of your own.
- A secret that a service owns, such as `LEGION_SLACK_BOT_TOKEN`, goes to that service's own pods
  and VMs and to nothing else: `Evaluate` in `packages/envoy/internal/broker/policy/policy.go`
  grants a service-owned secret at once when the session's own service is the secret's owner, and
  denies every other session, with no person asked.
- The `agent-secrets secret` forms (`list`, `show`, `create`, `set`, `retag`, `delete`, `restore`)
  act under a person's own cloud sign-in. They belong to the secret's owner, not to an agent.
- Slack's read methods go through `curl` under this client. Every post to Slack stays a `slack_post`
  call, which holds the reply to one thread, to 4,000 characters and to one send.

## Before you say that a service is out of reach

1. Run the call through `agent-secrets`, with the secret your instructions name.
2. Report a service unreachable only with the command you ran and the output you saw.
3. When no instruction names a secret for that service, report that instead, with what you checked:
   `agent-secrets identity`, and which instructions you read. Request no name you guessed.

## On a person's own machine

A session on a person's own development box is not a pod, and `agent-secrets identity` exits 1 there
for two reasons:

- **The machine is not logged in.** `agent-secrets machine login-status` prints `issued` only while
  the machine holds its credential. The person logs the machine in; step 4b of `docs/joining.md` in
  theorem-labs/agent-infra, "Log the VM in to the secrets broker", is that step.
- **The agent is not a broker session.** The helper knows only the process tree it registered. The
  person restarts the agent as one:
  `agent-secrets register --wait 10 --exec -- <the agent's command>`.

On `UNKNOWN_SECRET` you stop, as above. You may tell the secret's owner how to find the cause:
`agent-secrets secret list` and `agent-secrets secret show NAME`, under their own cloud sign-in
(`gcloud auth login` against a broker on Google Cloud, their `AWS_PROFILE` against one on AWS).

An owner who must store a secret does every write under their own cloud sign-in.
`agent-secrets secret set NAME` replaces the value of a secret that exists. A new one is
`agent-secrets secret create NAME --owner me|shared --tier agent|human`. Both read the value from
standard input or from a prompt with echo off, never from an argument. Type it at the prompt, so it
reaches no shell history.

## A worked example: one page of a Slack thread

```sh
agent-secrets LEGION_SLACK_BOT_TOKEN --reason 'read the thread of a Slack mention' --wait 0s -- sh -c '
  curl -sS -G https://slack.com/api/conversations.replies \
    -H "Authorization: Bearer $LEGION_SLACK_BOT_TOKEN" \
    --data-urlencode channel=<channel_id> \
    --data-urlencode ts=<thread_ts> \
    --data-urlencode limit=200'
```

Measured on 2026-11-04 from a Legion issue pod: it exited 0 and answered `"ok": true` with the 12
messages of the thread. A `ts` that names no message answers
`{"ok":false,"error":"thread_not_found"}`, also at exit 0, which is why the body decides and not the
status. A thread read needs the app's `channels:history` scope in a public channel and
`groups:history` in a private one; without them Slack answers `missing_scope`, which only a person
in the Slack workspace can fix. A session with a broker identity that is not one of the
`legion-daemon` service's own is denied this token, and the example exits 77; a session with no
broker identity exits 2.

Paging: Slack recommends a `limit` of at most 200. While the answer's `has_more` is true, pass its
`response_metadata.next_cursor` as `cursor` to the next call. Each page repeats the thread's root
message, so drop repeats by `ts` and sort by `ts`.

A message from the client that this skill does not cover is in the broker's troubleshooting guide,
`/legion/broker/guides/troubleshooting/` in the documentation site.
