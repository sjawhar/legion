---
title: GitHub App for a development box
description: Create a GitHub App, store its key, and use its installation tokens from a development box.
sidebar:
  order: 10
---

# GitHub App for a development box

Use one GitHub App for one person. Store that App's private key in the person's secret. The broker
creates a new installation token for each release of the secret.

Replace each `<placeholder>` before you run a command.

## Create the App

1. Sign in to GitHub as an organization owner.
2. Open the organization settings.
3. Create a GitHub App.
4. Use this manifest for the App.

```yaml
name: <person>-development-box
url: https://example.invalid
description: Development-box access for <person>
public: true
default_permissions:
  contents: write
  pull_requests: write
  issues: write
  workflows: write
  metadata: read
default_events: []
```

Do not enable webhooks. Do not give the App more permissions. GitHub lets other accounts install a
public App. Install it only on the organizations that the box must use.

Create a private key for the App. Save the key in a file that only you can read.

## Install the App

An organization owner must install the App before a box can use it.

1. Open the App's installation page.
2. Select the organization that owns the repositories.
3. Select the repositories the box must use.
4. Complete the installation.

You can install this public App in more than one organization. The broker returns one token for
each installation that is not suspended.

## Store the App key

The deployment creates an empty secret named `GITHUB_APP_<PERSON>`. Sign in to your Google Cloud
Application Default Credentials before you set its value.

Check the secret before you store the key.

```sh
agent-secrets secret show GITHUB_APP_<PERSON>
```

Make sure that the output contains `kind: github-app`. If it does not, stop. Ask an administrator to
set the kind before you store the key.

Build `app-key.json` from the GitHub App identifier and the private key file GitHub gave you. A
PEM key spans several lines, which JSON must escape, so let `jq` write the file rather than pasting
the key into it.

```sh
(umask 077 && jq -n --arg app_id '<app-id>' --rawfile private_key '<private key file>.pem' \
  '{app_id: $app_id, private_key: $private_key}' > app-key.json)
```

Set the secret from standard input, then delete the file. Do not put the private key on a command
line.

```sh
agent-secrets secret set GITHUB_APP_<PERSON> < app-key.json
rm app-key.json
```

The broker reads the secret's `kind: github-app` tag. It returns a JSON value with one entry for
each active installation. The account name of each installation is the entry key.

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

The broker never returns the private key. It returns a new installation token each time a session
reads the secret. If GitHub refuses a token request, the broker returns `MINT_FAILED` with GitHub's
status.

## Configure Git

Your Git credential helper must request `GITHUB_APP_<PERSON>` with
`agent-secrets GITHUB_APP_<PERSON> -- <command>`, which runs `<command>` with the release JSON in
the `GITHUB_APP_<PERSON>` environment variable. The helper must read the repository owner from
the Git URL. Set `credential.useHttpPath` so that Git sends the URL path to the helper:

```sh
git config --global credential.https://github.com.useHttpPath true
```

The helper must select `.installations["<owner>"].token` from the release JSON and write these two
lines to Git:

```text
username=x-access-token
password=<token>
```

Do not put the token in the Git remote URL. Do not put the token in a command argument. Give the
token to Git on standard output.

## Use GitHub CLI

Get the release JSON through `agent-secrets`. Select the token for the owner you will access. Set
`GH_TOKEN` only for the `gh` command that needs it.

```sh
agent-secrets --reason "GitHub CLI <owner>" GITHUB_APP_<PERSON> -- sh -ceu '
token=$(printf %s "$GITHUB_APP_<PERSON>" | jq -r ".installations[\"<owner>\"].token")
GH_TOKEN=$token gh repo view <owner>/<repository>
'
```

The token expires at the time in `expires_at`. Request the secret again after the token expires.
