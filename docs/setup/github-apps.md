# GitHub App Setup for Worker Identity

Legion uses GitHub Apps to give each worker role a distinct identity. This enables proper PR review flows where the implementer and reviewer are different actors — GitHub blocks same-actor PR approvals.

## Why GitHub Apps?

- **PR compliance**: Implementer and reviewer must be different identities
- **Least privilege**: Each role gets only the permissions it needs
- **Free**: GitHub Apps don't consume org seats
- **Audit trail**: Commits and PR actions are attributed to specific roles
- **Process isolation**: Each role runs in a separate serve process with its own credentials

## Architecture

Two GitHub Apps, one per role group:

| App Name | Used By | Permissions |
|----------|---------|-------------|
| `legion-impl` | Implementer, Merger | Contents: write, Pull requests: write, Issues: write |
| `legion-review` | Reviewer, Tester, Architect, Planner, Controller | Pull requests: write, Issues: write |

### Isolation Model

When configured, the daemon starts **separate `opencode serve` instances per role**:

```
Daemon Process
├── Controller serve (port 13381) — user's personal identity
├── impl serve (port 13382) — legion-impl[bot] token
└── review serve (port 13383) — legion-review[bot] token
```

Each serve process has:
- Role-specific `GH_TOKEN` (installation token, auto-refreshed)
- `GH_CONFIG_DIR=/dev/null` (prevents gh CLI from finding personal credentials)
- Scrubbed `GITHUB_TOKEN`, `GH_HOST` (prevents ambient credential leakage)

Workers on one serve **cannot read another serve's environment variables** (separate processes).

**Remaining limitations**: Workers share the OS user. A determined misbehaving worker with knowledge of the host filesystem could potentially read `/proc/{pid}/environ` of other serves. For full isolation, use separate OS users (future improvement).

## Step 1: Create the GitHub Apps

For each of the two apps (`legion-impl`, `legion-review`):

1. Go to **GitHub Settings → Developer settings → GitHub Apps → New GitHub App**
2. Set **GitHub App name** to the app name (e.g., `legion-impl`)
3. Set **Homepage URL** to your repository URL
4. Uncheck **Webhook → Active** (not needed)
5. Under **Permissions**, set the permissions from the table above:
   - For `legion-impl`: Repository → Contents: Read & Write, Pull requests: Read & Write, Issues: Read & Write
   - For `legion-review`: Repository → Pull requests: Read & Write, Issues: Read & Write
6. Under **Where can this GitHub App be installed?**, select "Only on this account" (or make public if installing across orgs)
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

# Reviewer + Tester + Architect + Planner identity
export LEGION_GITHUB_APP_REVIEW_ID="<app-id>"
export LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY_PATH="/etc/legion/keys/legion-review.pem"
export LEGION_GITHUB_APP_REVIEW_INSTALLATION_ID="<installation-id>"
```

## Step 5: Verify

After starting the daemon, verify credentials are working by dispatching a test worker
and checking its injected environment:

```bash
DAEMON_PORT=13370  # or your configured port

# Dispatch a test worker
curl -s http://127.0.0.1:$DAEMON_PORT/workers -X POST \
  -H 'content-type: application/json' \
  -d '{"issueId": "verify-test", "mode": "implement", "workspace": "/tmp/verify"}' | jq .
# Expected: {"id": "verify-test-implement", "port": ..., "sessionId": ...}

# Check the worker's env for injected credentials (credential values are stripped for security)
curl -s http://127.0.0.1:$DAEMON_PORT/workers/verify-test-implement/env | jq .
# Expected: {"env": {}} or {"env": {"CUSTOM_KEY": "value"}} (only non-credential vars shown)

# Check daemon logs for credential injection messages
# You should see: 'Role serves started: impl:13382, review:13383'
# If a role fails, you'll see: 'Failed to inject role credentials for ...'

# Cleanup the test worker
curl -s http://127.0.0.1:$DAEMON_PORT/workers/verify-test-implement -X DELETE | jq .
```

If no role serves start (no `Role serves started` log line), check that all three env vars
are set for at least one role. Partial configuration (e.g., missing `_PRIVATE_KEY_PATH`) silently
skips that role.

## Token Lifecycle

- Installation tokens expire after **1 hour**
- The daemon caches tokens and refreshes automatically when within **5 minutes** of expiry
- The controller fetches fresh credentials before each worker dispatch
- The daemon health loop monitors role serves and restarts unhealthy ones with fresh tokens

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| 400 `github_apps_not_configured` | No App credentials in env | Set the `LEGION_GITHUB_APP_*` env vars |
| 404 `role_not_configured` | Specific role not configured | Set all 3 env vars for that role |
| 500 `token_generation_failed` | Key file unreadable or API error | Check key path permissions, verify installation ID |
| JWT errors in daemon logs | Key format issue | Ensure `.pem` file is PKCS#1 or PKCS#8 format |
| Worker uses wrong identity | Missing env fetch at startup | Check worker's startup log for env fetch errors |
