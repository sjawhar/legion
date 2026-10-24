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

1. Change the tags with `agent-secrets secret retag`.
2. Make sure that the output of the retag shows `broker: serving`. This output tells you that the
   broker uses the new tags.
3. Set the private key.

Do this step on your own computer, not on the box. The agents on the box run as your user. They
can read a file that you keep there, and they can use a Google sign-in that you leave there.

On your own computer, sign in with `gcloud auth application-default login`, as
[Before you start](/legion/broker/guides/manage-a-secret/#before-you-start) in "Manage a secret"
tells you. Then build the file and set the secret.

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

If you must do this step on the box, do not run `gcloud auth application-default login` there. That
command replaces the credentials of the box for all programs. Follow the box route in
[Before you start](/legion/broker/guides/manage-a-secret/#before-you-start): it signs you in with
`gcloud auth login --no-activate` and puts the path of your credentials file in the `me` shell
variable. Then build `app-key.json` as above, and set the secret with your sign-in for this one
command only. Do not export the variable.

```sh
GOOGLE_APPLICATION_CREDENTIALS="$me" agent-secrets secret set GITHUB_APP_<PERSON> < app-key.json
rm app-key.json '<private key file>.pem'
gcloud auth revoke '<your email>'
```

The last command deletes your Google sign-in from the box, so the agents on the box cannot use it.

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
