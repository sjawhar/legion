---
title: GitHub App for a development box
description: Register a GitHub App, store its key, and use its installation tokens from a development box.
sidebar:
  order: 10
---

# GitHub App for a development box

Use one GitHub App for one person. Store the private key of that App in the secret of the person.
The broker makes new installation tokens from the key each time a session reads the secret.

Replace each `<placeholder>` before you run a command.

## Register the App

Register the App on your own GitHub account, not on an organization. Sign in to GitHub, then open
this link:

```text
https://github.com/settings/apps/new?name=<person>-development-box&url=https://example.invalid&public=true&contents=write&pull_requests=write&issues=write&workflows=write&metadata=read
```

The link fills in the settings of the App. Make sure that the page shows the settings in this
reference list, then click **Create GitHub App**.

```yaml
name: <person>-development-box
url: https://example.invalid
public: true
permissions:
  contents: write
  pull_requests: write
  issues: write
  workflows: write
  metadata: read
webhook: off
```

GitHub sets webhooks off by default. Do not enable them. Do not add permissions.

The App must be public, because you install it on more than one account. GitHub lets other
accounts install a public App. Install it only on the accounts that the box must use.

## Generate a private key

1. Open the settings page of the App. GitHub opens it when you create the App.
2. Find the **App ID** on the **General** page. Record it.
3. Under **Private keys**, click **Generate a private key**.
4. GitHub downloads a `.pem` file. Move it to a folder that only you can read.

## Install the App

1. On the settings page of the App, open **Install App**.
2. Select an account that owns repositories that the box must use.
3. Select the repositories that the box must use.
4. Click **Install**.

Do these steps for each account that the box must use. If you do not own an organization, GitHub
sends your request to its owners. An owner must approve the request before the box can use the App.

## Tell the administrator which accounts the box uses

The broker makes tokens only for the accounts that the secret's `github-owners` tag lists. Anyone
can install a public App. The tag stops a token for an account that you did not choose.

You cannot change the tag. The deployment sets it when it creates your secret. Send the
administrator the account names where you installed the App. Ask them to add any account that the
deployment does not list.

The broker returns one token for each installation that is not suspended, on an account that the
tag lists. An installation on any other account gets no token.

## Store the App key

The deployment creates an empty secret named `GITHUB_APP_<PERSON>`, with the `kind: github-app`
and `github-owners` tags. Your access lets you set its value.

Set the private key only in a secret that has the `kind: github-app` tag. If the secret has no
`kind` tag, a session that has a grant for it gets the stored value. That value is your private
key. To change a secret without a kind to `github-app`, do these steps in this sequence:

1. Change the tags with `agent-secrets secret retag NAME --kind github-app --github-owners "<owners>"`.
2. Make sure that the retag exits 0 and shows `broker: serving NAME (kind github-app)`. This tells
   you that the broker uses the new tags. If the retag exits 1 and tells you that the broker still
   sees the old tags, do the retag again. Do not set the private key until a retag exits 0.
3. Set the private key.

You can do this step on your own computer or on the box. The procedure is the same. The agents on
the box run as your user. They can read a file that you keep there, and they can use a Google
sign-in that you leave there. Thus, on the box, delete the key files and delete your sign-in when
you complete the step.

Sign in one time with `gcloud auth login`. This command does not change the credentials of the
box. When the credentials of the box are not your own, `agent-secrets` writes the secret with your
gcloud login. For more information, refer to
[Before you start](/legion/broker/guides/manage-a-secret/#before-you-start) in "Manage a secret".
Then build the file and set the secret.

Build `app-key.json` from the App ID and the private key file that GitHub gave you. A PEM key
spans several lines, which JSON must escape, so let `jq` write the file. Do not paste the key into
the file.

```sh
(umask 077 && jq -n --arg app_id '<app-id>' --rawfile private_key '<private key file>.pem' \
  '{app_id: $app_id, private_key: $private_key}' > app-key.json)
```

Set the secret from standard input, then delete the two key files. Do not put the private key on a
command line.

```sh
agent-secrets secret set GITHUB_APP_<PERSON> < app-key.json
rm app-key.json '<private key file>.pem'
```

If you do this step on the box, delete your Google sign-in from the box after you set the secret:

```sh
gcloud auth revoke '<your email>'
```

This command deletes your Google sign-in from the box, so the agents on the box cannot use it.

The broker returns a JSON value with one entry for each active installation. The account name of
each installation is the entry key.

```json
{
  "installations": {
    "<owner>": {
      "installation_id": 123,
      "token": "<token>",
      "expires_at": "2026-01-01T01:00:00Z"
    }
  }
}
```

The broker never returns the private key. Each time a session reads the secret, the broker makes a
new token for each installation. If GitHub refuses a token request, the broker returns
`MINT_FAILED` with the HTTP status code from GitHub.

## Keep each release until it expires

Each token is valid until its `expires_at` time, about one hour after the release. Keep the release
JSON and use it again until that time. Read the secret again only when the token expires.

Do not read the secret for each Git operation. Each read sends a request to GitHub for each
installation, and the secret store counts each read against its quota.

Keep the release in memory, or in a file that only your user can read.

## Configure Git

Your Git credential helper must get the release JSON with
`agent-secrets GITHUB_APP_<PERSON> -- <command>`, which runs `<command>` with the release JSON in
the `GITHUB_APP_<PERSON>` environment variable. The helper must read the repository owner from
the Git URL. Set `credential.useHttpPath` so that Git sends the URL path to the helper:

```sh
git config --global credential.https://github.com.useHttpPath true
```

The helper must select `.installations["<owner>"].token` from the release JSON that it keeps, and
write these two lines to Git:

```text
username=x-access-token
password=<token>
```

Do not put the token in the Git remote URL. Do not put the token in a command argument. Give the
token to Git on standard output.

## Use GitHub CLI

Get the release JSON through `agent-secrets`. Select the token for the owner that you will access.
Set `GH_TOKEN` only for the `gh` command that needs it.

```sh
agent-secrets --reason "GitHub CLI <owner>" GITHUB_APP_<PERSON> -- sh -ceu '
token=$(printf %s "$GITHUB_APP_<PERSON>" | jq -r ".installations[\"<owner>\"].token")
GH_TOKEN=$token gh repo view <owner>/<repository>
'
```

The token expires at the time in `expires_at`. Read the secret again after the token expires.

## Give a narrowed token to another machine

A token you give to another machine, such as a virtual machine that must only read one
repository, must be narrowed. Use these flags on the `agent-secrets` command that reads the
secret:

- `--github-owner <owner>` gives a token for the installation on that account only. The account
  must be in the `github-owners` tag of the secret.
- `--github-repo <repository>` limits the token to that repository of the account. Use the flag
  one time for each repository. Give the repository name without the account. The token keeps
  every permission the App holds, its organization permissions too, unless you also give
  `--github-read-only`.
- `--github-read-only` limits the token to `contents: read` and `metadata: read`. A machine that
  must only read needs it, with or without `--github-repo`.

`--github-repo` and `--github-read-only` need `--github-owner`.

```sh
agent-secrets --reason "read <repository> on <machine>" GITHUB_APP_<PERSON> \
  --github-owner <owner> --github-repo <repository> --github-read-only -- sh -ceu '
printf %s "$GITHUB_APP_<PERSON>" | jq -er ".installations[].token"
'
```

The release JSON has one entry, keyed by the account's login as GitHub spells it, so the example
reads that one entry whatever case you typed. The broker sends the narrowing to GitHub, which
refuses a repository or permission the installation does not have: `agent-secrets` then exits 1
with `NARROWING_REFUSED`, naming the repositories, and an account the `github-owners` tag does
not list exits 1 with `NARROWING_OWNER_NOT_LISTED`. The broker gives out the token only when
GitHub's answer shows that the token holds no more than you asked. The command runs only when
the broker tells `agent-secrets` that it applied the narrowing. A broker that cannot narrow
releases the token for all repositories, so `agent-secrets` runs nothing and exits 1.

The narrowing applies to this read only. The next read without these flags gives the token for
all repositories of each installation. Keep a narrowed token apart from the other tokens, so
that you do not use one in place of the other.
