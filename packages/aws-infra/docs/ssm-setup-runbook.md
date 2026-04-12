# SSM Session Manager Setup Runbook

This runbook covers the one-time operational steps to enable SSM Session Manager access for the dev box (`i-0d1244b687e204a01` in `us-west-1`). The IAM role and instance profile are managed by Pulumi (`EC2SSMRole`, `EC2SSMInstanceProfile`).

## Prerequisites

- Pulumi stack applied: `EC2SSMInstanceProfile` exists in AWS
- AWS CLI configured with credentials that have `ec2:AssociateIamInstanceProfile` and `ssm:*` permissions
- SSH access to the dev box via Tailscale (for SSM agent verification)

---

## Step 1: Apply Pulumi Stack

```bash
cd packages/aws-infra
npm install
pulumi stack select prod
pulumi up
```

Expected new resources:
- `aws:iam/role:Role` — `EC2SSMRole`
- `aws:iam/rolePolicyAttachment:RolePolicyAttachment` — `EC2SSMRole-ssm-core`
- `aws:iam/instanceProfile:InstanceProfile` — `EC2SSMInstanceProfile`

---

## Step 2: Attach Instance Profile to Dev Box

Check if the instance already has a profile attached:

```bash
aws ec2 describe-iam-instance-profile-associations \
  --filters "Name=instance-id,Values=i-0d1244b687e204a01" \
  --region us-west-1
```

**If no profile is attached** (empty `IamInstanceProfileAssociations`):

```bash
aws ec2 associate-iam-instance-profile \
  --instance-id i-0d1244b687e204a01 \
  --iam-instance-profile Name=EC2SSMInstanceProfile \
  --region us-west-1
```

**If a different profile is already attached**, you must disassociate it first:

```bash
# Get the association ID from the describe command above
ASSOC_ID=$(aws ec2 describe-iam-instance-profile-associations \
  --filters "Name=instance-id,Values=i-0d1244b687e204a01" \
  --region us-west-1 \
  --query 'IamInstanceProfileAssociations[0].AssociationId' \
  --output text)

# Replace with EC2SSMInstanceProfile
aws ec2 replace-iam-instance-profile-association \
  --association-id "$ASSOC_ID" \
  --iam-instance-profile Name=EC2SSMInstanceProfile \
  --region us-west-1
```

> **Note:** Attaching or replacing an instance profile on a running instance does NOT require stopping the instance.

---

## Step 3: Verify SSM Agent on Dev Box

SSH into the dev box via Tailscale and check the SSM agent:

```bash
# Check agent status
sudo systemctl status amazon-ssm-agent

# If not installed (Ubuntu):
sudo snap install amazon-ssm-agent --classic
sudo systemctl enable amazon-ssm-agent
sudo systemctl start amazon-ssm-agent

# If not installed (Amazon Linux 2):
sudo yum install -y amazon-ssm-agent
sudo systemctl enable amazon-ssm-agent
sudo systemctl start amazon-ssm-agent
```

Expected output: `Active: active (running)`

---

## Step 4: Verify Outbound HTTPS Connectivity

The SSM agent requires outbound HTTPS (port 443) to these endpoints:

- `ssm.us-west-1.amazonaws.com`
- `ssmmessages.us-west-1.amazonaws.com`
- `ec2messages.us-west-1.amazonaws.com`

Test from the dev box:

```bash
curl -s -o /dev/null -w "%{http_code}" https://ssm.us-west-1.amazonaws.com
# Expected: 404 (endpoint reachable, no valid request body)
```

If the instance is in a VPC without a NAT gateway or internet gateway, SSM will not connect. Verify the instance has a route to the internet.

---

## Step 5: Verify SSM Registration

After attaching the instance profile and confirming the agent is running, wait 2–5 minutes for the agent to register with SSM, then verify:

```bash
aws ssm describe-instance-information \
  --filters "Key=InstanceIds,Values=i-0d1244b687e204a01" \
  --region us-west-1
```

Expected: The instance appears in the response with `PingStatus: Online`.

Also visible in the AWS Console: **Systems Manager → Fleet Manager → Managed nodes**.

---

## Step 6: Test Session

```bash
# Using IAM Identity Center credentials (via aws sso login)
aws sso login --profile dev
aws ssm start-session --target i-0d1244b687e204a01 --region us-west-1 --profile dev
```

Expected: A shell session opens on the dev box.

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| Instance not in Fleet Manager after 5 min | Agent not running or no outbound HTTPS | Check Step 3 and Step 4 |
| `start-session` fails with `TargetNotConnected` | Agent not registered | Wait longer or check agent logs: `sudo journalctl -u amazon-ssm-agent -n 50` |
| `start-session` fails with `AccessDenied` | IAM Identity Center permission set missing SSM actions | Verify Developer permission set has `ssm:StartSession` |
| Profile attachment fails | Instance already has a conflicting profile | Use `replace-iam-instance-profile-association` (Step 2) |
