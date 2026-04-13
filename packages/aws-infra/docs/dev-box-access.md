# Dev Box Access Guide

Public SSH (port 22) has been removed from the dev box security group. All access is now via
**SSM Session Manager** or **Tailscale SSH**.

## Access Methods

| Method | When to Use | Requires |
|--------|------------|----------|
| SSM Session Manager | Primary access — interactive shell, port forwarding | AWS SSO login (`Developer` role on dev account) |
| Tailscale SSH | Alternative if you have Tailscale configured | Tailscale client connected to the tailnet |

---

## SSM Session Manager (Primary)

### Prerequisites

1. **AWS CLI v2** installed: https://docs.aws.amazon.com/cli/latest/userguide/install-cliv2.html
2. **Session Manager plugin** installed: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html
3. **AWS SSO profile** configured (see below)

### Configure AWS SSO Profile

Add to `~/.aws/config`:

```ini
[profile dev]
sso_start_url = https://<your-instance>.awsapps.com/start
sso_region = us-west-1
sso_account_id = <dev-account-id>
sso_role_name = Developer
```

The `sso_start_url` is in IAM Identity Center > Settings > AWS access portal URL.

### Log In

```bash
aws sso login --profile dev
```

This opens a browser for Google SSO authentication. The session lasts 8 hours.

### Start a Session

```bash
aws ssm start-session --target i-0d1244b687e204a01 --profile dev
```

This opens an interactive shell on the dev box.

### Port Forwarding

Forward a remote port to your local machine (useful for accessing services running on the dev box):

```bash
# Forward remote port 8080 to local port 8080
aws ssm start-session \
  --target i-0d1244b687e204a01 \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8080"],"localPortNumber":["8080"]}' \
  --profile dev
```

### Port Forwarding to Remote Host

Forward through the dev box to another host on its network:

```bash
aws ssm start-session \
  --target i-0d1244b687e204a01 \
  --document-name AWS-StartPortForwardingSessionToRemoteHost \
  --parameters '{"host":["remote-host.internal"],"portNumber":["5432"],"localPortNumber":["5432"]}' \
  --profile dev
```

---

## Tailscale SSH (Alternative)

Tailscale SSH operates over WireGuard tunnels, completely independent of AWS security groups.
The removal of port 22 from the AWS security group does **not** affect Tailscale SSH.

### Connect

```bash
ssh sami@sami
```

Where `sami` is the Tailscale hostname of the dev box.

### Docker Deployment via Tailscale

Docker deployment via `pulumi up` in `packages/envoy/infra/` uses Tailscale SSH and is
**unaffected** by the security group change:

```bash
cd packages/envoy/infra
pulumi up -s prod
```

---

## Emergency Access

If both SSM and Tailscale are unavailable:

1. **Re-open SSH temporarily** via AWS Console:
   - EC2 > Security Groups > find the dev box SG
   - Add inbound rule: SSH (22), Source: your IP (`/32`)
   - Connect, diagnose, fix
   - **Remove the rule immediately after**

2. **Or use Pulumi** to temporarily add an ingress rule:
   - This requires removing the `protect: true` flag or using `pulumi state unprotect`
   - Not recommended — prefer the console for emergency access

3. **Check SSM agent health** from the console:
   - Systems Manager > Fleet Manager > find the instance
   - Verify the SSM agent is running and the instance is "Online"
   - Common issues: SSM agent stopped, instance has no outbound HTTPS access

---

## Verification Commands

```bash
# SSH should be blocked (timeout or connection refused)
nc -zw3 52.53.150.80 22

# SSM should work
aws ssm start-session --target i-0d1244b687e204a01 --profile dev

# Tailscale SSH should work (if Tailscale is configured)
ssh sami@sami

# Docker deployment should work (via Tailscale)
cd packages/envoy/infra && pulumi preview -s prod
```

---

## Troubleshooting

### "TargetNotConnected" on SSM

The SSM agent on the instance can't reach AWS endpoints. Possible causes:
- Instance is stopped
- SSM agent service is not running
- Instance has no outbound HTTPS (port 443) access — check the security group egress rules
- Instance is in a private subnet without NAT gateway or VPC endpoints for SSM

### "ExpiredToken" on AWS CLI

Your SSO session has expired. Re-authenticate:

```bash
aws sso login --profile dev
```

### "An error occurred (ForbiddenException)"

Your IAM Identity Center user doesn't have the `Developer` permission set assigned.
Check with an admin that you're in the `Engineers` group in IAM Identity Center.
