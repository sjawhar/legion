# aws-infra

Pulumi project managing AWS infrastructure for the dev box.

## What's managed

- **Dev box security group** — controls inbound/outbound traffic to `i-0d1244b687e204a01`
  - SSH (port 22) from internet: **REMOVED** (use SSM or Tailscale instead)
  - All outbound traffic: allowed (required for SSM agent)

## Usage

```bash
# Install dependencies
npm install

# Select stack
pulumi stack select prod

# Preview changes
pulumi preview

# Apply changes
pulumi up
```

## First-time setup

1. Configure the security group ID in `Pulumi.prod.yaml`:
   ```bash
   aws ec2 describe-instances \
     --instance-ids i-0d1244b687e204a01 \
     --query 'Reservations[].Instances[].SecurityGroups[].GroupId' \
     --output text --profile dev
   ```

2. Import the existing security group:
   ```bash
   pulumi import aws:ec2/securityGroup:SecurityGroup dev-box-sg <sg-id> -s prod
   ```

3. Run `pulumi up -s prod` to apply the SSH removal.

## Access documentation

See [docs/dev-box-access.md](docs/dev-box-access.md) for how to access the dev box after SSH is closed.
