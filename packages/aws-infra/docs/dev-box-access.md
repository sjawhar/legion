# Dev Box Access Guide

The dev box (`i-0d1244b687e204a01`, public IP `52.53.150.80`) is accessible via **AWS SSM Session Manager** and **Tailscale SSH**. Public SSH (port 22) is **closed** — this is intentional.

## Access Methods

### 1. SSM Session Manager (Primary)

SSM Session Manager provides shell access without requiring open inbound ports. It uses the SSM agent running on the instance, which connects outbound to AWS endpoints over HTTPS (port 443).

**Prerequisites:**
- AWS CLI installed: `brew install awscli`
- Session Manager plugin installed: `brew install --cask session-manager-plugin`
- IAM Identity Center (SSO) configured with a profile that has SSM access

**Start a session:**
```bash
aws ssm start-session --target i-0d1244b687e204a01 --profile dev
```

**First-time SSO login:**
```bash
aws sso login --profile dev
```

**SSO profile setup** (`~/.aws/config`):
```ini
[profile dev]
sso_start_url = https://trajectorylabs.awsapps.com/start
sso_account_id = <account-id>
sso_role_name = DevBoxAccess
sso_region = us-east-1
region = us-west-1
output = json
```

### 2. SSM Port Forwarding (Local Development)

Forward a remote port to your local machine for local development:

```bash
# Forward remote port 8080 to local port 8080
aws ssm start-session \
  --target i-0d1244b687e204a01 \
  --profile dev \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["8080"],"localPortNumber":["8080"]}'
```

### 3. Tailscale SSH

Tailscale SSH provides direct SSH access over the Tailscale overlay network. It is **not affected by AWS security groups** — Tailscale operates over WireGuard tunnels on UDP port 41641, which bypasses the VPC network interface entirely.

**Prerequisites:**
- Tailscale installed and connected: `brew install tailscale`
- Access to the `trajectorylabs` Tailscale network

**Connect:**
```bash
ssh sami@sami
```

Or using the Tailscale IP directly:
```bash
ssh sami@<tailscale-ip>
```

### 4. Docker Deployment (Tailscale SSH)

The `packages/envoy/infra/` Pulumi project deploys Docker containers to the dev box via Tailscale SSH. This is **unaffected** by the SSH port closure.

```bash
cd packages/envoy/infra
pulumi up -s prod
```

## Verification

After the security group change, verify the expected access posture:

```bash
# SSH from internet should be BLOCKED (connection refused or timeout)
nc -zw3 52.53.150.80 22
# Expected: exit code 1 (connection failed)

# SSM should work
aws ssm start-session --target i-0d1244b687e204a01 --profile dev
# Expected: interactive shell session

# Tailscale SSH should work (requires Tailscale connection)
ssh sami@sami echo "Tailscale SSH works"
# Expected: "Tailscale SSH works"

# Docker deployment should work
cd packages/envoy/infra && pulumi preview -s prod
# Expected: no errors
```

## Emergency Access

If SSM and Tailscale are both unavailable:

1. **Re-open SSH temporarily** via the AWS Console:
   - EC2 → Security Groups → dev-box-sg → Edit inbound rules
   - Add: Type=SSH, Source=My IP
   - Connect, fix the issue, remove the rule

2. **EC2 Instance Connect** (if enabled):
   - AWS Console → EC2 → Instances → Connect → EC2 Instance Connect

3. **AWS Systems Manager Run Command** (no interactive shell needed):
   ```bash
   aws ssm send-command \
     --instance-ids i-0d1244b687e204a01 \
     --document-name AWS-RunShellScript \
     --parameters commands=["<your-command>"] \
     --profile dev
   ```

## Infrastructure Management

The dev box security group is managed by Pulumi in `packages/aws-infra/`.

```bash
cd packages/aws-infra
pulumi stack select prod
pulumi preview   # See planned changes
pulumi up        # Apply changes
```

**To import the existing security group** (one-time, if not already imported):
```bash
# Get the security group ID
aws ec2 describe-instances \
  --instance-ids i-0d1244b687e204a01 \
  --query 'Reservations[].Instances[].SecurityGroups[].GroupId' \
  --output text \
  --profile dev

# Import into Pulumi state
pulumi import aws:ec2/securityGroup:SecurityGroup dev-box-sg <sg-id> -s prod
```

## Architecture Notes

- **No inbound SSH rule**: The security group has no inbound rule for port 22. This is intentional.
- **SSM agent connectivity**: The SSM agent connects outbound to `ssm.us-west-1.amazonaws.com` over HTTPS (port 443). The egress-allow-all rule covers this.
- **Tailscale independence**: Tailscale SSH is completely independent of AWS security groups. It uses the Tailscale daemon listening on the Tailscale network interface, not the public IP.
- **Automated scripts**: Any scripts that SSH directly to `52.53.150.80` will break. Update them to use SSM or Tailscale.
