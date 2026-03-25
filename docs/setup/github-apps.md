# GitHub App Setup for Worker Identity

Legion uses GitHub Apps to give each worker role a distinct identity. This enables proper PR review flows where the implementer and reviewer are different actors — GitHub blocks same-actor PR approvals.

## Why GitHub Apps?

- **PR compliance**: Implementer and reviewer must be different identities
- **Least privilege**: Each role gets only the permissions it needs
- **Free**: GitHub Apps don't consume org seats
- **Audit trail**: Commits and PR actions are attributed to specific roles

## Architecture

Three GitHub Apps, one per role group:

| App Name | Used By | Permissions |
|----------|---------|-------------|
| `legion-impl` | Implementer, Merger | Contents: write, Pull requests: write, Issues: write |
| `legion-review` | Reviewer | Pull requests: write, Issues: write |
| `legion-ops` | Tester, Architect, Planner, Controller | Contents: read, Pull requests: read, Issues: write |

## Step 1: Create the GitHub Apps

For each of the three apps (`legion-impl`, `legion-review`, `legion-ops`):

1. Go to **GitHub Settings → Developer settings → GitHub Apps → New GitHub App**
2. Set **GitHub App name** to the app name (e.g., `legion-impl`)
3. Set **Homepage URL** to your repository URL
4. Uncheck **Webhook → Active** (not needed)
5. Under **Permissions**, set the permissions from the table above:
   - For `legion-impl`: Repository → Contents: Read & Write, Pull requests: Read & Write, Issues: Read & Write
   - For `legion-review`: Repository → Pull requests: Read & Write, Issues: Read & Write
   - For `legion-ops`: Repository → Contents: Read-only, Pull requests: Read-only, Issues: Read & Write
6. Under **Where can this GitHub App be installed?**, select "Only on this account"
7. Click **Create GitHub App**
8. **Note the App ID** from the app's settings page (shown near the top)

## Step 2: Generate Private Keys

For each app:

1. On the app's settings page, scroll to **Private keys**
2. Click **Generate a private key**
3. A `.pem` file downloads automatically
4. Store it securely (e.g., `/etc/legion/keys/legion-impl.pem`)
5. Set permissions: `chmod 600 /etc/legion/keys/*.pem`

## Step 3: Install Apps on Repository

For each app:

1. On the app's settings page, click **Install App** in the sidebar
2. Select your organization/account
3. Choose **Only select repositories** and pick the target repo
4. Click **Install**
5. **Note the Installation ID** from the URL: `https://github.com/settings/installations/<INSTALLATION_ID>`

## Step 4: Configure Daemon Environment

Set these environment variables before starting the daemon. A role is configured only when all three variables are set for that role:

```bash
# Implementer + Merger identity
export LEGION_GITHUB_APP_IMPL_ID="<app-id-from-step-1>"
export LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH="/etc/legion/keys/legion-impl.pem"
export LEGION_GITHUB_APP_IMPL_INSTALLATION_ID="<installation-id-from-step-3>"

# Reviewer identity
export LEGION_GITHUB_APP_REVIEW_ID="<app-id>"
export LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY_PATH="/etc/legion/keys/legion-review.pem"
export LEGION_GITHUB_APP_REVIEW_INSTALLATION_ID="<installation-id>"

# Ops identity (tester, architect, planner)
export LEGION_GITHUB_APP_OPS_ID="<app-id>"
export LEGION_GITHUB_APP_OPS_PRIVATE_KEY_PATH="/etc/legion/keys/legion-ops.pem"
export LEGION_GITHUB_APP_OPS_INSTALLATION_ID="<installation-id>"
```

## Step 5: Verify

After starting the daemon, test each role's credentials endpoint:

```bash
DAEMON_PORT=13370  # or your configured port

# Test impl credentials
curl -s http://127.0.0.1:$DAEMON_PORT/credentials/impl | jq .
# Expected: {"token":"ghs_...","expiresAt":"...","gitIdentity":{"name":"legion-impl[bot]","email":"..."}}

# Test review credentials
curl -s http://127.0.0.1:$DAEMON_PORT/credentials/review | jq .

# Test ops credentials
curl -s http://127.0.0.1:$DAEMON_PORT/credentials/ops | jq .
```

A 404 response means that role's credentials are not configured — the daemon will fall back to ambient credentials (the user's personal token) for that role.

## Partial Configuration

Any subset of roles works. If you only need separate identities for implementation and review:

```bash
# Only configure impl and review — ops workers use ambient credentials
export LEGION_GITHUB_APP_IMPL_ID="..."
export LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH="..."
export LEGION_GITHUB_APP_IMPL_INSTALLATION_ID="..."

export LEGION_GITHUB_APP_REVIEW_ID="..."
export LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY_PATH="..."
export LEGION_GITHUB_APP_REVIEW_INSTALLATION_ID="..."
```

## Token Lifecycle

- Installation tokens expire after **1 hour**
- The daemon caches tokens and refreshes automatically when within **5 minutes** of expiry
- The controller fetches fresh credentials before each worker dispatch
- No background refresh — tokens are generated lazily on demand

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| 400 `github_apps_not_configured` | No App credentials in env | Set the `LEGION_GITHUB_APP_*` env vars |
| 404 `role_not_configured` | Specific role not configured | Set all 3 env vars for that role |
| 500 `token_generation_failed` | Key file unreadable or API error | Check key path permissions, verify installation ID |
| JWT errors in daemon logs | Key format issue | Ensure `.pem` file is PKCS#1 or PKCS#8 format |
