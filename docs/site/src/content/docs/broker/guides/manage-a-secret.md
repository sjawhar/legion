---
title: Manage a secret
description: Create an agent secret, change its value or tags, and delete or restore it with agent-secrets secret under your own cloud sign-in.
sidebar:
  order: 9
---

An agent secret is a secret in the broker's own store - AWS Secrets Manager or GCP Secret
Manager, whichever the broker's deployment uses - under the broker's namespace, encrypted with
its key, and tagged with owner, tier, optional kind and optional max grant
([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)).
`agent-secrets secret` manages them. It reads and writes the store itself, under your own cloud
sign-in, so that cloud's own IAM decides what you may do; the broker never writes a secret. After
every write the CLI asks the broker to reread that one secret, and the broker that answers tells
every other broker process serving the same database to reread it too, so the change is served
across the whole deployment within seconds rather than at each process's next read of the
namespace. The examples on this page are AWS's; GCP's
backend differs only where this page calls it out.

## Before you start

You need three things:

- **`agent-secrets` on your `PATH`.** Each `legion-envoy-v*` GitHub release ships it for Linux in
  `agent-secrets-amd64.tar.gz` and `agent-secrets-arm64.tar.gz`, beside the helper, and for macOS
  in `agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`, which hold the
  CLI alone. Managing secrets needs no helper, no machine login and no agent session.
- **`AGENT_SECRETS_URL`, the broker's address.** The CLI asks the broker for the namespace, the
  agent-secrets key, and that key's store and account/project (`GET /v1/settings`), so you
  configure none of them.
- **A cloud sign-in in the broker's account or project.** AWS: every form takes it from
  `--profile`, else `AWS_PROFILE`, else the AWS SDK's default credential chain; a write needs your
  own IAM Identity Center sign-in. GCP: every form takes your own Google sign-in, and a write never
  runs as a machine.

```sh
export AGENT_SECRETS_URL=https://secrets.internal.example
aws sso login --profile <profile>   # AWS
export AWS_PROFILE=<profile>
gcloud auth login                   # GCP, once
```

On GCP, `gcloud auth login` is all a laptop or a dev box needs. A read runs under your Application
Default Credentials, and so does a write where they are your own Google Account sign-in. Where they
are a machine's own identity - a dev box's workload identity federation credential, a service
account key, or a VM's or pod's metadata-server identity - a write runs under your gcloud login
instead, and so does every form on a machine with no Application Default Credentials. The CLI
asks the first `gcloud` on your `PATH` for that login (`gcloud auth list`, then
`gcloud auth print-access-token`), so gcloud keeps it fresh, and its own error says when you must
run `gcloud auth login` again; the CLI reads none of gcloud's files, and `gcloud auth login`
leaves the machine's Application Default Credentials as they are. gcloud runs with its log files
off and prints the token to the CLI alone, and with impersonation, an access token and a
credential file override cleared wherever they are set, so the token is your login's; a write
whose token Google answers as anyone else's is refused.

Your gcloud login is the account `gcloud auth list` names by a plain email address: not a service
account (`…gserviceaccount.com`), and not a workload or workforce identity federation login
(`principal://…`). Where it lists more than one person, the command uses the one
`CLOUDSDK_CORE_ACCOUNT` names, and a command it prints for running again keeps that variable:

```sh
CLOUDSDK_CORE_ACCOUNT=ada@example.com agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
```

`GOOGLE_APPLICATION_CREDENTIALS`, when you set it, names the credentials every form uses, and the
CLI never replaces them with your gcloud login: a write under a machine's credentials named there
is refused.

When you have made your writes, remove that sign-in from the machine, so nothing else that runs as
your user there can use it:

```sh
gcloud auth revoke '<your email>'
```

## Create a secret

```console
$ agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
Value for DEMO_DEPLOY_TOKEN:
created DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=agent)
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

Every line names the store it wrote to or read, so a secret is never taken for one of the same
name in another store.

On GCP the same command writes as you on a dev box too: it uses your gcloud login where the
machine's Application Default Credentials are its own identity, and so does every write below.

- **The name** is the one agents ask for, and the environment variable their command gets it in:
  uppercase letters, digits and single underscores, starting with a letter. In Secrets Manager it
  is `<namespace>demo-deploy-token`. Any other name is refused at once (exit 2).
- **`--owner me`** writes your sign-in's email in lowercase: your Identity Center session name
  (AWS) or the Google account the write runs as (GCP: your Application Default Credentials', or
  your gcloud login's). It must be an email the broker accepts, or the command refuses before
  writing. The broker matches it against the email a person signs in to Dispatch with, so `me`
  names you where that session name or Google account is the same email. **`--owner shared`**
  makes the secret everyone's. GCP stores owner and tier as the secret's **annotations**, never
  labels, so they never collide with a label someone else applies.
- **`--tier agent`** lets the owner's own sessions use it without asking, and **`--tier human`**
  sends every use to a person for approval. Both flags are required.
- **`--kind`** is optional. Use `github-app` for a GitHub App key and `google-login` for an
  authorized-user Google login. Use `none`, or omit the flag, for a stored secret. A kind also
  writes `mint-service-account: kind-secret`, which makes an earlier broker refuse the secret
  rather than serve its stored key.
- **`--github-owners`** is required with `--kind github-app`, and refused with any other kind. It
  lists the GitHub accounts the broker mints installation tokens for, separated by spaces, so the
  shell quotes it: `--github-owners "acme-org ada"`. A comma is refused before anything is written.
- **`--max-grant`** is optional. It is the longest `--ttl` a request for this secret may use.
  It must be a duration of at least one second.
- **The value.** At a terminal the CLI reads one hidden line. Enter or Ctrl-D ends it; Backspace,
  Ctrl-W and the terminal's kill-line character edit it. An unhandled control byte, including
  Ctrl-S, Ctrl-Q, and Ctrl-C, Ctrl-\\ or Ctrl-Z inside a bracketed paste, refuses the value. The
  prompt keeps discarding through Enter or Ctrl-D and the paste's end before reporting the error
  and naming the pipe command. Anywhere
  else the CLI reads standard input to its end, less one trailing newline. Empty values and
  invalid UTF-8 are refused (exit 2) and write nothing.

The CLI checks your sign-in before it asks for the value, so a refused sign-in never has you type a
secret for nothing. The secret is created on the broker's key with its tags, and the broker serves
it from the next request. Secrets Manager refuses a name that is already taken, including one
scheduled for deletion: [restore](#delete-and-restore) that one instead.

The prompt takes one line. A bracketed paste of more than one line, such as a key, is read through
its closing mark and refused (exit 2), however long its writes take, as long as no more than 10
seconds of quiet pass between them: the bound counts the quiet since the last input, not the time
since the paste began. A paste that falls quiet for 10 seconds before its closing mark comes is
given up, and the command exits 1, storing nothing; input that arrives after it is given up reaches
your shell. Most terminal emulators and tmux support bracketed paste.

Without brackets the CLI drains input until 200 ms of quiet pass after the line, with no total
limit, and Ctrl-C ends the drain. A paste-start mark split so that only its first bytes arrive
before the line ends holds the drain for one second for the rest of the mark, so a late paste still
opens and is read through rather than reaching your shell. **That quiet window is not a guarantee
about an entire paste:** bytes arriving later can reach your shell. If those bytes are a second
line, the first line may already have been
accepted as the value. Pipe multi-line values in rather than pasting them, and use
`secret set NAME < FILE` to replace a value. The refusal names the command that pipes the value in:

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN
Value for DEMO_DEPLOY_TOKEN:
agent-secrets secret set: a value of more than one line must be piped in: agent-secrets secret set DEMO_DEPLOY_TOKEN < FILE
$ agent-secrets secret set DEMO_DEPLOY_TOKEN < key.pem
set a new value of DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```
Ctrl-C, Ctrl-\\, SIGTERM or SIGHUP at the prompt restores the foreground terminal and writes
nothing, then ends the CLI by that signal. A shell reports 130, 131, 143 or 129 respectively.
Ctrl-C also stops a shell's `;` list. On Unix, core dumps are disabled before the value is read and
stay disabled for the rest of the process. On other platforms, crash-dump policy is OS-managed.

**Ctrl-Z discards the entry.** The CLI discards what it holds of the value and whatever the
terminal holds unread when it stops, which is everything that arrived with Ctrl-Z or before it, so
none of that reaches your shell, and it refuses the entry rather than storing a partial secret.
Keys typed after the stop has taken effect go to your shell, which then holds the terminal. After
`fg`, input stays hidden only while the remainder is discarded: press Enter, then run the command
shown in the message and type the whole value again.
The command exits 2 and stores nothing. After `bg`, the reader waits without touching the
terminal until `fg` gives it back, then discards the entry in the same way. SIGTERM or SIGHUP
while it waits there ends it at once by that signal, leaving the terminal to your shell. If the
shell that ran it exits first, it exits 2, naming the command that pipes the value in, and leaves
the terminal untouched.
A wrapper that ignores SIGTSTP keeps it ignored, without discarding the entry.

**A command started with `&` waits for `fg`.** It shows no prompt and reads nothing while your
shell holds the terminal. `fg` brings it forward, the label appears, and the value reads hidden.
When the command ends, the terminal is left the way `fg` handed it over. A stop before the label
appears discards nothing, since you have typed nothing for it yet. If no shell can ever bring it
forward, for example because it was started from a subshell that has since exited, it shows
nothing and exits 2, naming the command that pipes the value in.

The hidden prompt is available on Linux and macOS for amd64 and arm64. Builds for other targets
refuse an interactive value with exit 2, naming the command that pipes the value in. None of the
prompt's macOS-specific code has run on a macOS machine: that includes the stop and resume path,
the discard of unread input at Ctrl-Z, the two checks behind the refusal of a command started with
`&` (an orphaned process group, and a shell that has exited), keeping an inherited ignored signal
ignored, and disabling core dumps. It is built and checked for macOS, and read from its kernel
source, but not exercised.

On macOS, `poll(2)` may not report `/dev/tty` ready, so reading the value from it directly
(`agent-secrets secret set NAME < /dev/tty`) can leave the prompt waiting after you press Enter;
Ctrl-C ends it and restores the terminal. Run the command from the terminal's own standard input.

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

GCP write checks are the same shape, with one difference: where the Application Default
Credentials are not a person's own Google Account sign-in (a service account key file, a workload
identity federation credential such as a dev box's, a workforce identity federation sign-in, or a
GCE/GKE metadata-server identity), a write runs under your gcloud login
([Before you start](#before-you-start)) rather than being refused, and leaves those credentials as
they are. It is refused, naming what it found, only where gcloud has no login to lend:

```console
$ agent-secrets secret create DEMO_DEPLOY_TOKEN --owner me --tier agent
agent-secrets secret create: a secret is written under your own Google sign-in, never this machine's (its Application Default Credentials are credentials of type "external_account"), so a write uses your gcloud login, but gcloud holds no person's login: sign in once with `gcloud auth login`, which leaves this machine's own credentials as they are
```

A machine's credentials that `GOOGLE_APPLICATION_CREDENTIALS` names are refused for a write
rather than replaced, since the variable is your own choice.

GCP has no wrong-account read check: every call already names `projects/<project number>/...`
explicitly in the resource, so IAM on that resource, not this CLI, is what decides whether a read
succeeds.

A read (`list`, `show`) takes any sign-in the cloud's own IAM lets read the namespace's secrets, a
machine's role or service account included. These
checks are the CLI's own; what a sign-in may actually read or change is each cloud's IAM to decide,
and [Operating the broker](/legion/broker/operate/#people-who-manage-secrets) lists the calls each
form makes.

## See what is there

`list` shows every agent secret, including one scheduled for deletion:

```console
$ agent-secrets secret list
NAME               OWNER            TIER   KIND          MAX_GRANT  VALUE  DELETED  EARLIEST_PURGE
DEMO_DEPLOY_TOKEN  ada@example.com  agent  -             -          yes    -        -
DEMO_GITHUB_APP    ada@example.com  agent  github-app    -          yes    -        -
DEMO_LOGIN         ada@example.com  human  google-login  1h         yes    -        -
Every row is a secret in AWS Secrets Manager account 111122223333 in us-east-1.
```

The last line goes to standard error, so the table stays one row per secret for a script.
`VALUE` says whether a version of the secret carries `AWSCURRENT`, the label Secrets Manager reads
a value from. The broker serves a secret only while one does, so a secret another tool created
without a value shows `no` until someone runs `secret set` on it. A missing tag shows as `-`.
`show` prints one secret's store, tags, dates and versions:

```console
$ agent-secrets secret show DEMO_DEPLOY_TOKEN
name: DEMO_DEPLOY_TOKEN
store: AWS Secrets Manager account 111122223333 in us-east-1
secret_name: example/agent-secrets/demo-deploy-token
owner: ada@example.com
tier: agent
kind: -
max_grant: -
created: 2026-10-06T20:31:08Z
last_changed: 2026-10-06T20:31:08Z
version: 8ae2ea20-8e93-4f1a-b3d1-2e5396ecef72 AWSCURRENT
```

Neither ever prints a value, and both take `--json`.

## Change its value

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN
Value for DEMO_DEPLOY_TOKEN:
set a new value of DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

`set` reads the value as `create` does: one line at a prompt with echo off, or all of standard
input, which is how a value of more than one line goes in. The broker never keeps a value; it
reads it from Secrets Manager each time a session reads a grant, so the next read of a live grant
gets the new one.

## Change its owner, tier, kind or max grant

```console
$ agent-secrets secret retag DEMO_DEPLOY_TOKEN --tier human
retagged DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=human)
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

Name `--owner`, `--tier`, `--kind`, `--github-owners`, `--max-grant` or a combination. A retag that
sets tags reads the owner and tier tags the secret holds and sends both in one request. On AWS,
this lets an IAM condition inspect both tags. Every other tag or annotation stays as it is. For a
secret missing an owner or tier tag, name that tag too.

`--kind none` removes the kind tag and the `mint-service-account` tag written with it,
`--github-owners none` removes the GitHub owners and `--max-grant none` removes the max grant. A
removal is a retag of its own: it changes no other tag, so the cloud refusing it leaves the secret
as it was. A retag that names `none` beside another change is refused before it writes anything.
So is a retag that would leave a `github-app` secret without `--github-owners`, as the broker would
then refuse to serve it, and one that names `--github-owners` on a secret of another kind.
Run the removal and the other change as two retags:

```console
$ agent-secrets secret retag DEMO_LOGIN --max-grant none
retagged DEMO_LOGIN in AWS Secrets Manager account 111122223333 in us-east-1 (removed max-grant)
broker: serving DEMO_LOGIN from AWS Secrets Manager account 111122223333 in us-east-1
```

To make a stored secret a kind, retag it first, and only once the retag exits 0, showing
`broker: serving NAME (kind <kind>)`, store the App key or the Google login with `secret set`. A
reread can read the tags stale (Secret Manager's metadata can lag a write by hours), and while the
broker still serves the secret as stored, a grant of it releases its stored value raw. So a retag
whose reread answers another kind than the one it wrote exits 1: the broker still sees the old
tags; run the retag again, and store no value until it exits 0. A release that finds the secret
retagged while it reads the store releases nothing and ends that grant.

Exit 0 is the answer of the one broker process that served the reread, not of every process. That
process tells the others over Postgres and they reread within seconds, so exit 0 is the usual case
and the one below is the exception. Five log lines say a process may be behind, and only two of
them name the secret:

| Log line | Names a secret? | What it means |
| --- | --- | --- |
| `broker: the other pods were not told to reread a secret secret=… error=…` | **yes**, `secret` | The answering process's own `NOTIFY` failed, so no peer was told at all. Its 200 is unchanged. |
| `broker: a notification's reread of a secret did not run secret=… sender=… error=…` | **yes**, `secret` and the peer that sent it | A peer was told and its reread failed. This is the strongest evidence available: a named process behind on a named secret. |
| `broker: a Postgres listener has no connection; redialing channels=… error=… attempt=… wait_ms=…` | no | That process is outside the fan-out **for every name** while it redials, and catches nothing up when it reconnects. |
| `broker: serving without the boot's catch-up policy load … error=…` | no | A booting process could not take its catch-up read, so a retag served while it was starting may not be in force there. |
| `broker: binding before the reread listener has had an answer waited=…` | no | A booting process bound without its `LISTEN`, so notifications sent in that gap reached it not at all. |

A process left behind serves the secret as stored, and a grant of it there releases the stored
value raw.

**The way to avoid the question is not to convert.** Create the secret with its kind from the
start, under a name no agent holds a grant on, and store the App key or the Google login there.
A name that was never served as an ordinary secret has no grant that can release a raw value, so
none of what follows applies to it. Converting a name agents already hold grants on is what opens
the window, and it is worth a new name to close it.

Where you must convert, before you store an App key or a Google login:

- Read the broker's log since the retag. **If any of those five lines appears, do not store yet** —
  not only one naming your secret. Three of the five carry no secret at all, so a process that
  logged one may be behind on yours and nothing in the line will say so.
- Run the retag again and require another exit 0 with none of those lines after it. That says
  **the answering process** has the new tags: its reread reads the one secret directly
  (`DescribeSecret`, `GetSecret`) rather than through a listing, and the retag exits 1 unless the
  kind it reads back is the one it wrote. On Secrets Manager that read answers a write at once;
  on Secret Manager a single read is eventually consistent like the listing, which is why a retag
  there can need running more than once.
- **It says nothing about the peers, and no log line will.** A peer's reread reads the one name
  too, so on Secret Manager it can succeed while still reading the old tags: the broker warns
  only when that read returns an error, and a successful read of stale tags is not one. The
  notification is asynchronous besides, so a healthy peer need not have applied the retag by the
  time exit 0 is printed. Neither a second exit 0 nor a clean log establishes the peers — that is
  the residual `dispatch://INFRA-375` tracks, and the reason the paragraph above recommends a new
  name instead.
- If you cannot read the log, wait at least five minutes after exit 0. That is the pod's own
  reload interval, and the wait is **the right action rather than a guarantee**: the reload is
  another listing read, and a store's listing lags a write by up to five minutes on Secrets
  Manager and up to six hours on Secret Manager, so on Secret Manager five minutes is not a bound
  at all. `dispatch://INFRA-375` tracks that
  lag and the two ways to close it.

Waiting costs nothing; storing early can hand the raw key to an agent through a process that still
serves the secret as stored.

A retag checks the tags it read. On GCP it writes against that read, so a retag another person
wrote in between refuses it, with `another write changed the secret after this retag read it`,
and nothing is written. AWS has no such check, so retag secrets there one writer at a time: a retag
reads the tags back after its write, and when another writer's change left a kind without the
GitHub owners it needs, it puts back the tags it read, which undoes that writer's change too, and
exits 1 naming the race.

You can retag your own secret, to `--owner shared` too. Once a secret is shared, its owner and
tier are an administrator's to change: on AWS, by the IAM condition above; on GCP, because a
deployment that wants this protection grants `secretmanager.secrets.update` on a shared secret
only to administrators. When the cloud refuses a retag that sets the tags of a shared secret, the
CLI prints that refusal, then `a shared secret's owner and tier are an administrator's to change`,
and exits 1. It prints the same line when the cloud refuses a retag to `--owner shared`; for your
own secret, that refusal means your access does not include the retag. A refused removal prints
the cloud's refusal alone
([Troubleshooting](/legion/broker/guides/troubleshooting/#managing-a-secret)).

New tags decide what sessions get from the next request, and reach some grants already given:
[Approvals](/legion/broker/concepts/#approvals) says which.
[Revoke a session or a grant](/legion/broker/guides/revoke-a-session/#end-every-sessions-access-to-a-secret)
uses a retag to end every session's automatic access to a secret.

## Delete and restore

```console
$ agent-secrets secret delete DEMO_DEPLOY_TOKEN
deleted DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: DEMO_DEPLOY_TOKEN is deleted in AWS Secrets Manager account 111122223333 in us-east-1 (restorable until 2026-11-05T20:22:33Z)
```

`delete` schedules the deletion with a 30-day recovery window, the longest Secrets Manager allows,
and never forces it. The date it prints is the one Secrets Manager answered for the deletion: the
end of that window. The broker stops serving the secret at once, so a grant that held it releases
nothing (`GRANT_NOT_LIVE`). Until you restore it, Secrets Manager keeps the secret but refuses to
read its value, to change its value or tags (`set`, `retag`), and to `create` another of the same
name. `delete` and `restore` are permissions of their own (`secretsmanager:DeleteSecret`,
`secretsmanager:RestoreSecret`): where your access does not include one, AWS refuses it with
`AccessDeniedException`, your own secret included. GCP has no separate recovery window: `delete`
sets the secret's `expire_time` to 30 days out in one `UpdateSecret` call and prints it; the secret
and every version stay readable and restorable until that moment, and GCP deletes them for good,
irreversibly, once it passes. `restore` clears `expire_time` the same way.

`list` and `show` keep showing a deleted secret, with when it was deleted (AWS) or GCP's
`expire_time`, the future moment it purges for good (GCP - no separate EARLIEST_PURGE there, since
that moment is both), and the **earliest** it can be purged:

```console
$ agent-secrets secret list
NAME               OWNER            TIER   KIND  MAX_GRANT  VALUE  DELETED               EARLIEST_PURGE
DEMO_DEPLOY_TOKEN  ada@example.com  human  -     -          yes    2026-10-06T20:22:33Z  2026-10-13T20:22:33Z
DEMO_EMPTY_KEY     ada@example.com  human  -     -          yes    -                     -
DEMO_SHARED_KEY    shared           human  -     -          yes    -                     -
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
restored DEMO_DEPLOY_TOKEN in AWS Secrets Manager account 111122223333 in us-east-1
broker: serving DEMO_DEPLOY_TOKEN from AWS Secrets Manager account 111122223333 in us-east-1
```

## What the broker answers after a write

After each write, the CLI asks the broker to reread the secret (`POST /v1/secrets/{name}/reread`)
and prints its answer on a line of its own, naming the store the secret is in:

| Line | What it means |
| --- | --- |
| `broker: serving NAME from <store>` | The broker serves the secret as just written, as a stored secret. |
| `broker: serving NAME from <store> (kind <kind>)` | The broker serves the secret as that kind: it mints from the stored value and never returns it. |
| `broker: NAME is deleted in <store> (restorable until <date>)` | After a delete: the broker no longer serves it. |
| `broker: refusing NAME from <store> (reason=<reason>)` | The broker leaves the secret out: `absent` (it found no such secret, or one scheduled for deletion), or one of the refusals [Operating the broker](/legion/broker/operate/#health-and-logs) lists, such as `no-current-value`. |

`<store>` is the store the broker's settings name: `AWS Secrets Manager account <id> in <region>`,
or `GCP Secret Manager project <number>`. The command exits 0 when the answer is what the write
meant. It exits 1 when the answer contradicts the write, and says so on standard error: `the
broker at <host> refuses NAME after the write to <store> (reason=…)` after a create, set, retag or
restore, or `the broker at <host> still serves NAME after its delete in <store>`. The write itself
stands in the store either way, so fix what the reason names rather than repeating the write:

```console
$ agent-secrets secret retag DEMO_EMPTY_KEY --tier human
retagged DEMO_EMPTY_KEY in AWS Secrets Manager account 111122223333 in us-east-1 (owner=ada@example.com, tier=human)
broker: refusing DEMO_EMPTY_KEY from AWS Secrets Manager account 111122223333 in us-east-1 (reason=no-current-value)
agent-secrets secret retag: the broker at secrets.internal.example refuses DEMO_EMPTY_KEY after the write to AWS Secrets Manager account 111122223333 in us-east-1 (reason=no-current-value)
```

Here the secret has no value; `secret set DEMO_EMPTY_KEY` gives it one, and the broker serves it.

When the broker cannot be asked at all (it is unreachable, or its
[reread limit](/legion/broker/operate/#network-and-trust) refuses the call), the write still stands
and the command exits 1:

```console
$ agent-secrets secret set DEMO_DEPLOY_TOKEN < token.txt
set a new value of DEMO_DEPLOY_TOKEN
agent-secrets secret set: the write to the secret store stands, but the broker could not be asked to reread DEMO_DEPLOY_TOKEN, so it serves the change only from its next reload: too many secret rereads; try again later (RATE_LIMITED)
```

Do not repeat the write. A new value is read at the next grant read anyway, and a create, a
restore, or a first value for a secret that had none is served on the first request naming it,
since the broker rereads a name it does not serve before refusing it. A retag or a delete of a
secret the broker serves takes effect at its next read of the namespace, within about ten minutes,
or at once when you ask for the reread yourself — one call, whichever process it reaches, since
that process tells the others:

```sh
curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/DEMO_DEPLOY_TOKEN/reread"
```

[Troubleshooting](/legion/broker/guides/troubleshooting/#managing-a-secret) lists the messages the
`secret` forms print and what to do about each.
