# IAM Identity Center SAML Setup Runbook

This runbook covers the manual steps required before Pulumi can manage IAM Identity Center
resources. These steps require console access and cannot be automated.

**Prerequisites:**
- AWS Organization must exist (IAM Identity Center requires an organization instance)
- Access to Google Admin Console with super admin privileges
- Access to AWS management account console

---

## 1. Enable IAM Identity Center

**Navigation:** AWS Console → IAM Identity Center → Enable

1. Sign in to the **AWS management account** console.
2. Navigate to **IAM Identity Center** (search "IAM Identity Center" in the top bar).
3. Click **Enable** on the IAM Identity Center welcome page.
4. Select **Enable with AWS Organizations** (required for permission sets across accounts).
5. Choose region: **us-west-1** (Identity Center is region-locked once enabled — do not change).
6. Click **Enable IAM Identity Center**.

**Success indicator:** IAM Identity Center dashboard shows "IAM Identity Center is enabled" with a green status badge.

> ⚠️ Identity Center is region-locked once enabled. Verify `us-west-1` is correct before proceeding.

---

## 2. Configure Google Workspace as SAML 2.0 IdP

This step establishes the trust relationship between Google Workspace and AWS IAM Identity Center.

### 2a. Download AWS metadata from IAM Identity Center

1. In the IAM Identity Center console, navigate to **Settings** → **Identity source**.
2. Click **Actions** → **Change identity source**.
3. Select **External identity provider**.
4. Under **Service provider metadata**, click **Download metadata file** to save `metadata.xml`.
5. Keep this page open — you will paste Google's metadata here in step 2c.

### 2b. Create the SAML app in Google Admin Console

1. Sign in to [Google Admin Console](https://admin.google.com) as a super admin.
2. Navigate to **Apps** → **Web and mobile apps**.
3. Click **Add app** → **Add custom SAML app**.
4. Enter app name: `AWS IAM Identity Center`. Click **Continue**.
5. On the **Google Identity Provider details** page, click **Download Metadata** to save Google's `metadata.xml`. Click **Continue**.
6. On the **Service provider details** page, enter:
   - **ACS URL**: Copy from the IAM Identity Center settings page (format: `https://us-west-1.signin.aws.amazon.com/platform/saml/acs/<instance-id>`)
   - **Entity ID**: Copy from the IAM Identity Center settings page (format: `urn:amazon:webservices:<instance-id>`)
   - **Name ID format**: `EMAIL`
   - **Name ID**: `Basic Information > Primary email`
7. Click **Continue**, then **Finish**.
8. On the app page, click **User access** and set to **ON for everyone** (or the appropriate org unit).

### 2c. Upload Google metadata to IAM Identity Center

1. Return to the IAM Identity Center **Change identity source** page (from step 2a).
2. Under **Identity provider metadata**, click **Browse** and upload the Google metadata file downloaded in step 2b.
3. Click **Next** → **Change identity source** → confirm by typing `ACCEPT`.

**Success indicator:** IAM Identity Center Settings page shows **Identity source: External identity provider** with Google's entity ID listed.

---

## 3. Enable SCIM User Provisioning

SCIM provisions users from Google Workspace to IAM Identity Center automatically.

> ⚠️ **The SCIM access token is shown exactly once.** Copy it immediately and store it securely (e.g., in 1Password). It cannot be retrieved after this step.
> 
> ⚠️ **Token expiry:** SCIM tokens expire after 1 year. Set a calendar reminder to rotate before expiry. To rotate: delete the old token in IAM Identity Center → Automatic provisioning, generate a new one, and update the Google SAML app.

### 3a. Generate SCIM token in IAM Identity Center

1. In IAM Identity Center, navigate to **Settings** → **Automatic provisioning**.
2. Click **Enable** next to "Automatic provisioning".
3. Copy the **SCIM endpoint URL** and **Access token** — store both securely now.
4. Click **Close**.

### 3b. Configure SCIM in Google Admin Console

1. In Google Admin Console, navigate to **Apps** → **Web and mobile apps** → **AWS IAM Identity Center**.
2. Click **Provisioning** → **Configure auto-provisioning**.
3. Enter:
   - **SCIM URL**: Paste the SCIM endpoint URL from step 3a.
   - **Access token**: Paste the access token from step 3a.
4. Click **Test connection** — verify it shows "Connection successful".
5. Under **Provisioning scope**, select the users/groups to sync.
6. Click **Save**.
7. Click **Start provisioning** to trigger the initial sync.

**Success indicator:** In IAM Identity Center → Users, provisioned users appear with source "External identity provider". Initial sync may take a few minutes.

> ⚠️ Google SCIM syncs **users only**, not groups. Groups must be created via Pulumi (`aws.identitystore.Group`). See `index.ts`.
> 
> ⚠️ If any Google user has multi-value attributes (e.g., multiple phone numbers), SCIM sync may fail silently. Check IAM Identity Center → Settings → Automatic provisioning for sync errors.

---

## 4. Verify SSO Login End-to-End

After provisioning at least one user, verify the full SSO flow works.

### 4a. Configure AWS CLI profiles

Add profiles to `~/.aws/config`:

```ini
[profile dev]
sso_start_url = https://<your-instance>.awsapps.com/start
sso_region = us-west-1
sso_account_id = <dev-account-id>
sso_role_name = Developer

[profile prod]
sso_start_url = https://<your-instance>.awsapps.com/start
sso_region = us-west-1
sso_account_id = <prod-account-id>
sso_role_name = ReadOnly
```

The `sso_start_url` is found in IAM Identity Center → Settings → AWS access portal URL.

### 4b. Log in and verify

```bash
# Log in via browser
aws sso login --profile dev

# Verify assumed role
aws sts get-caller-identity --profile dev
```

**Success indicator:** `aws sts get-caller-identity` returns a JSON response with an ARN in the format:
```
arn:aws:sts::<account-id>:assumed-role/AWSReservedSSO_Developer_<hash>/<email>
```

Repeat for `--profile prod` to verify ReadOnly access.
