---
title: Manage a secret
description: Create an agent secret, change its value, owner or tier, and delete or restore it with agent-secrets secret, under your own AWS sign-in, and see the broker serve each change at once.
sidebar:
  order: 9
---

An agent secret is a secret in AWS Secrets Manager under the broker's namespace, encrypted with
its key and tagged with its owner and tier
([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)).
`agent-secrets secret` manages them. It reads and writes Secrets Manager itself, under your own AWS
sign-in, so IAM decides what you may do; the broker never writes a secret. After every write the
CLI asks the broker to reread that one secret, so the change is served at once rather than at the
broker's next read of the namespace.

## Before you start

You need three things:

- **`agent-secrets` on your `PATH`.** Each `legion-envoy-v*` GitHub release ships it for Linux in
  `agent-secrets-amd64.tar.gz` and `agent-secrets-arm64.tar.gz`, beside the helper, and for macOS
  in `agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`, which hold the
  CLI alone. Managing secrets needs no helper, no machine login and no agent session.
- **`AGENT_SECRETS_URL`, the broker's address.** The CLI asks the broker for the namespace, the
  agent-secrets key, and that key's AWS account and region (`GET /v1/settings`), so you configure
  none of them.
- **An AWS sign-in in the broker's account.** Every form takes it from `--profile`, else
  `AWS_PROFILE`, else the AWS SDK's default credential chain. A write needs your own IAM Identity
  Center sign-in.

```sh
export AGENT_SECRETS_URL=https://secrets.internal.example
aws sso login --profile <profile>
export AWS_PROFILE=<profile>
```

## Create a secret

```console
$ agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
Value for DEMO_DEPLOY_TOKEN:
created DEMO_DEPLOY_TOKEN (owner=ada@example.com, tier=agent)
broker: serving DEMO_DEPLOY_TOKEN
```

- **The name** is the one agents ask for, and the environment variable their command gets it in:
  uppercase letters, digits and single underscores, starting with a letter. In Secrets Manager it
  is `<namespace>demo-deploy-token`. Any other name is refused at once (exit 2).
- **`--owner me`** writes your email: your sign-in's session name in lowercase, which for an IAM
  Identity Center sign-in is your user name there. The broker matches an owner against the email
  a person signs in to Dispatch with, so `me` names you where your Identity Center user name is
  that email. **`--owner shared`** makes the secret everyone's.
- **`--tier agent`** lets the owner's own sessions use it without asking, and **`--tier human`**
  sends every use to a person for approval. Both flags are required.
- **The value.** At a terminal the CLI prompts for it and reads one line with echo off, so the
  value never shows on the screen; Enter or Ctrl-D ends it. Anywhere else it reads standard input
  to its end, less one trailing newline, so a pipe or a file works too. An empty value (Enter or
  Ctrl-D alone at the prompt, or empty standard input) is refused (exit 2) and writes nothing.

The CLI checks your sign-in before it asks for the value, so a refused sign-in never has you type a
secret for nothing. The secret is created on the broker's key with both tags, and the broker serves
it from the next request. Secrets Manager refuses a name that is already taken, including one
scheduled for deletion: [restore](#delete-and-restore) that one instead.

The prompt takes one line. A value of more than one line pasted there, such as a key, is refused
(exit 2): nothing is written, and the CLI discards the rest of the paste, so none of it reaches
your shell. That holds whole on a terminal that marks pastes (bracketed paste, which most terminal
emulators and tmux support): the CLI reads such a paste through its end however slowly it
arrives. On a terminal without it, the CLI waits for 200 ms of quiet after the line, so a paste
delivered slowly over a bad link can be split, and its later lines reach your shell; pipe a
value of more than one line in rather than pasting it. The refusal names the command that pipes
the value in; put the value in a file and run that:

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN
Value for DEMO_DEPLOY_TOKEN:
agent-secrets secret set: a value of more than one line must be piped in: agent-secrets secret set DEMO_DEPLOY_TOKEN < FILE
$ agent-secrets secret set DEMO_DEPLOY_TOKEN < key.pem
set a new value of DEMO_DEPLOY_TOKEN
broker: serving DEMO_DEPLOY_TOKEN
```

Ctrl-C or a termination signal (`SIGTERM`) at the prompt puts your terminal back as it was, echo
on, writes nothing, and ends the CLI by that signal, so a script or a `;` list running it stops
there too.

## Which sign-in may do what

Before anything is read or written, every form asks AWS whose sign-in it holds
(`sts:GetCallerIdentity`, which needs no permission). A sign-in in any account but the broker's is
refused, and the refusal names both accounts:

```console
$ agent-secrets secret list
agent-secrets secret list: your AWS sign-in arn:aws:sts::999999999999:assumed-role/AWSReservedSSO_User_abc123/Ada@Example.com is in account 999999999999, but the agent secrets are in account 111122223333: sign in to account 111122223333 (--profile or AWS_PROFILE names the sign-in)
```

A write (`create`, `set`, `retag`, `delete`, `restore`) also refuses any sign-in that is not a
person's own Identity Center sign-in in that account, naming what it found, so a machine's role,
an assumed service role or an IAM user writes no secret:

```console
$ agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
agent-secrets secret create: arn:aws:sts::111122223333:assumed-role/example-devbox-instance-role/i-0abc is not a person's Identity Center sign-in: a secret is written under your own sign-in to account 111122223333 (aws sso login, then --profile or AWS_PROFILE), never a machine's role
```

A read (`list`, `show`) takes any sign-in in the broker's account, a machine's role included. These
checks are the CLI's own; what a sign-in may actually read or change is IAM's to decide, and
[Operating the broker](/legion/broker/operate/#people-who-manage-secrets) lists the calls each form
makes.

## See what is there

`list` shows every agent secret, including one scheduled for deletion:

```console
$ agent-secrets secret list
NAME               OWNER            TIER   VALUE  DELETED  EARLIEST_PURGE
DEMO_DEPLOY_TOKEN  ada@example.com  agent  yes    -        -
DEMO_EMPTY_KEY     ada@example.com  agent  no     -        -
DEMO_SHARED_KEY    shared           human  yes    -        -
```

`VALUE` says whether a version of the secret carries `AWSCURRENT`, the label Secrets Manager reads
a value from. The broker serves a secret only while one does, so a secret another tool created
without a value shows `no` until someone runs `secret set` on it. A missing tag shows as `-`.
`show` prints one secret's tags, dates and versions:

```console
$ agent-secrets secret show DEMO_DEPLOY_TOKEN
name: DEMO_DEPLOY_TOKEN
secret_name: example/agent-secrets/demo-deploy-token
owner: ada@example.com
tier: agent
created: 2026-10-06T20:31:08Z
last_changed: 2026-10-06T20:31:08Z
version: 8ae2ea20-8e93-4f1a-b3d1-2e5396ecef72 AWSCURRENT
```

Neither ever prints a value, and both take `--json`.

## Change its value

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN
Value for DEMO_DEPLOY_TOKEN:
set a new value of DEMO_DEPLOY_TOKEN
broker: serving DEMO_DEPLOY_TOKEN
```

`set` reads the value as `create` does: one line at a prompt with echo off, or all of standard
input, which is how a value of more than one line goes in. The broker never keeps a value; it
reads it from Secrets Manager each time a session reads a grant, so the next read of a live grant
gets the new one.

## Change its owner or tier

```console
$ agent-secrets secret retag DEMO_DEPLOY_TOKEN --tier human
retagged DEMO_DEPLOY_TOKEN (owner=ada@example.com, tier=human)
broker: serving DEMO_DEPLOY_TOKEN
```

Name `--owner`, `--tier` or both. `retag` reads the tags the secret holds and sends both in one
request, the unchanged one as it is, so an IAM condition on the request's tags (one that lets a
person tag only their own secret, say) sees both. For a secret missing a tag, name that one too.

You can retag your own secret, to `--owner shared` too. Once a secret is shared, its owner and
tier are an administrator's to change: when AWS refuses a retag of a shared secret, the CLI prints
AWS's refusal, then `a shared secret's owner and tier are an administrator's to change`, and exits
1. It prints the same line when AWS refuses a retag to `--owner shared`; for your own secret, that
refusal means your access does not include the retag
([Troubleshooting](/legion/broker/guides/troubleshooting/#managing-a-secret)).

New tags decide what sessions get from the next request, and reach some grants already given:
[Approvals](/legion/broker/concepts/#approvals) says which.
[Revoke a session or a grant](/legion/broker/guides/revoke-a-session/#end-every-sessions-access-to-a-secret)
uses a retag to end every session's automatic access to a secret.

## Delete and restore

```console
$ agent-secrets secret delete DEMO_DEPLOY_TOKEN
deleted DEMO_DEPLOY_TOKEN
broker: DEMO_DEPLOY_TOKEN is deleted (restorable until 2026-11-05T20:22:33Z)
```

`delete` schedules the deletion with a 30-day recovery window, the longest Secrets Manager allows,
and never forces it. The date it prints is the one Secrets Manager answered for the deletion: the
end of that window. The broker stops serving the secret at once, so a grant that held it releases
nothing (`GRANT_NOT_LIVE`). Until you restore it, Secrets Manager keeps the secret but refuses to
read its value, to change its value or tags (`set`, `retag`), and to `create` another of the same
name. `delete` and `restore` are permissions of their own (`secretsmanager:DeleteSecret`,
`secretsmanager:RestoreSecret`): where your access does not include one, AWS refuses it with
`AccessDeniedException`, your own secret included.

`list` and `show` keep showing a deleted secret, with when it was deleted and the **earliest** it
can be purged:

```console
$ agent-secrets secret list
NAME               OWNER            TIER   VALUE  DELETED               EARLIEST_PURGE
DEMO_DEPLOY_TOKEN  ada@example.com  human  yes    2026-10-06T20:22:33Z  2026-10-13T20:22:33Z
DEMO_EMPTY_KEY     ada@example.com  human  yes    -                     -
DEMO_SHARED_KEY    shared           human  yes    -                     -
EARLIEST_PURGE is the deletion plus Secrets Manager's 7-day minimum recovery window: restore works at least until then; the window the delete chose may be longer.
```

`EARLIEST_PURGE` is the deletion plus 7 days, Secrets Manager's shortest recovery window: Secrets
Manager answers when a secret was deleted, but not the window its delete chose. A delete through
this CLI stays restorable for 30 days, and one from the AWS console or another tool for between 7
and 30, so `restore` works at least until that date and perhaps well after it. The legend goes to
standard error, so standard output stays one row per secret. `show` prints the same two dates as
`deleted:` and `earliest_purge:`.

`restore` cancels the deletion, and the broker serves the secret again:

```console
$ agent-secrets secret restore DEMO_DEPLOY_TOKEN
restored DEMO_DEPLOY_TOKEN
broker: serving DEMO_DEPLOY_TOKEN
```

## What the broker answers after a write

After each write, the CLI asks the broker to reread the secret (`POST /v1/secrets/{name}/reread`)
and prints its answer on a line of its own:

| Line | What it means |
| --- | --- |
| `broker: serving NAME` | The broker serves the secret as just written. |
| `broker: NAME is deleted (restorable until <date>)` | After a delete: the broker no longer serves it. |
| `broker: refusing NAME (reason=<reason>)` | The broker leaves the secret out: `absent` (it found no such secret, or one scheduled for deletion), or one of the refusals [Operating the broker](/legion/broker/operate/#health-and-logs) lists, such as `no-current-value`. |

The command exits 0 when the answer is what the write meant. It exits 1 when the answer contradicts
the write, and says so on standard error: `the broker refuses NAME after the write (reason=…)`
after a create, set, retag or restore, or `the broker still serves NAME after its delete`. The
write itself stands in Secrets Manager either way, so fix what the reason names rather than
repeating the write:

```console
$ agent-secrets secret retag DEMO_EMPTY_KEY --tier human
retagged DEMO_EMPTY_KEY (owner=ada@example.com, tier=human)
broker: refusing DEMO_EMPTY_KEY (reason=no-current-value)
agent-secrets secret retag: the broker refuses DEMO_EMPTY_KEY after the write (reason=no-current-value)
```

Here the secret has no value; `secret set DEMO_EMPTY_KEY` gives it one, and the broker serves it.

When the broker cannot be asked at all (it is unreachable, or its
[reread limit](/legion/broker/operate/#network-and-trust) refuses the call), the write still stands
and the command exits 1:

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN < token.txt
set a new value of DEMO_DEPLOY_TOKEN
agent-secrets secret set: the write to Secrets Manager stands, but the broker could not be asked to reread DEMO_DEPLOY_TOKEN, so it serves the change only from its next reload: too many secret rereads; try again later (RATE_LIMITED)
```

Do not repeat the write. A new value is read at the next grant read anyway, and a create, a
restore, or a first value for a secret that had none is served on the first request naming it,
since the broker rereads a name it does not serve before refusing it. A retag or a delete of a
secret the broker serves takes effect at its next read of the namespace, within about ten minutes,
or at once when you ask for the reread yourself:

```sh
curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/DEMO_DEPLOY_TOKEN/reread"
```

[Troubleshooting](/legion/broker/guides/troubleshooting/#managing-a-secret) lists the messages the
`secret` forms print and what to do about each.
