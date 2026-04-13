# SSM Session Manager Setup Runbook

This runbook covers deploying the SSM IAM resources via Pulumi and attaching the instance
profile to the dev box. The IAM resources (role + instance profile) are Pulumi-managed;
the instance profile attachment is a one-time CLI operation since the EC2 instance itself
is not Pulumi-managed.

**Prerequisites:**
- Pulumi stack `prod` is configured with `devAccountRoleArn` (or running with dev-account credentials)
- AWS CLI v2 with access to the dev account
- Dev box instance ID: `i-0d1244b687e204a01` (us-west-1)

---

## 1. Deploy IAM Resources via Pulumi

```bash
cd packages/aws-infra
pulumi up -s prod
```

This creates:
- **EC2SSMRole** — IAM role with `ec2.amazonaws.com` trust policy
- **EC2SSMInstanceProfile** — Instance profile wrapping EC2SSMRole
- **AmazonSSMManagedInstanceCore** policy attachment on the role

Verify the outputs:

```bash
pulumi stack output ec2SsmRoleArn -s prod
pulumi stack output ec2SsmInstanceProfileArn -s prod
pulumi stack output ec2SsmInstanceProfileName -s prod
```

---

## 2. Check Existing Instance Profile

Before attaching, check if the instance already has an instance profile:

```bash
aws ec2 describe-instances \
  --instance-ids i-0d1244b687e204a01 \
  --query 'Reservations[].Instances[].IamInstanceProfile' \
  --output json \
  --profile dev
```

- If the result is `null` or `[]` — proceed to step 3.
- If a different profile is attached — you must disassociate it first:
  ```bash
  # Get the association ID
  aws ec2 describe-iam-instance-profile-associations \
    --filters Name=instance-id,Values=i-0d1244b687e204a01 \
    --query 'IamInstanceProfileAssociations[0].AssociationId' \
    --output text \
    --profile dev

  # Replace the profile (no instance stop required)
  aws ec2 replace-iam-instance-profile-association \
    --iam-instance-profile Name=EC2SSMInstanceProfile \
    --association-id <association-id> \
    --profile dev
  ```

---

## 3. Attach Instance Profile

Attaching a new instance profile to a running instance does **not** require stopping it
(only replacing an existing one may, in some older API versions — but `associate` always works
on instances with no profile).

```bash
aws ec2 associate-iam-instance-profile \
  --iam-instance-profile Name=EC2SSMInstanceProfile \
  --instance-id i-0d1244b687e204a01 \
  --profile dev
```

Verify:

```bash
aws ec2 describe-instances \
  --instance-ids i-0d1244b687e204a01 \
  --query 'Reservations[].Instances[].IamInstanceProfile.Arn' \
  --output text \
  --profile dev
```

The output should contain `EC2SSMInstanceProfile`.

---

## 4. Verify SSM Agent

SSH into the dev box (via Tailscale) and check the SSM agent:

```bash
ssh sami@sami
sudo systemctl status amazon-ssm-agent
```

- If **active (running)** — proceed to step 5.
- If **not found** — install it:
  ```bash
  # Ubuntu
  sudo snap install amazon-ssm-agent --classic
  sudo systemctl enable --now snap.amazon-ssm-agent.amazon-ssm-agent.service

  # Amazon Linux 2
  sudo yum install -y amazon-ssm-agent
  sudo systemctl enable --now amazon-ssm-agent
  ```
- If **inactive** — start it:
  ```bash
  sudo systemctl start amazon-ssm-agent
  ```

---

## 5. Verify SSM Connectivity

The SSM agent needs outbound HTTPS (port 443) to reach these endpoints:
- `ssm.us-west-1.amazonaws.com`
- `ssmmessages.us-west-1.amazonaws.com`
- `ec2messages.us-west-1.amazonaws.com`

Check from the dev box:

```bash
curl -s -o /dev/null -w "%{http_code}" https://ssm.us-west-1.amazonaws.com
# Should return 200 or 403 (reachable — 403 just means no valid auth header)
```

Check that the instance appears in SSM:

```bash
aws ssm describe-instance-information \
  --filters "Key=InstanceIds,Values=i-0d1244b687e204a01" \
  --profile dev
```

The instance should appear with `PingStatus: Online`. If it doesn't appear, wait 2-3
minutes for the agent to register after the instance profile was attached.

---

## 6. Test a Session

```bash
aws ssm start-session --target i-0d1244b687e204a01 --profile dev
```

This should open an interactive shell. Type `whoami` to confirm you're on the instance,
then `exit` to close the session.

### With IAM Identity Center (SSO) credentials:

```bash
aws sso login --profile dev
aws ssm start-session --target i-0d1244b687e204a01 --profile dev
```

---

## Troubleshooting

### Instance not appearing in SSM

1. Verify instance profile is attached (step 3 verification command)
2. Verify SSM agent is running (step 4)
3. Verify outbound HTTPS connectivity (step 5)
4. Check agent logs: `sudo journalctl -u amazon-ssm-agent -n 50` (or snap equivalent)
5. Wait 2-5 minutes after attaching the profile — the agent re-registers on a timer

### "TargetNotConnected" error

Same as above — the instance isn't registered with SSM. Most common causes:
- Instance profile not attached
- SSM agent not running
- No outbound HTTPS to SSM endpoints (check security group egress rules)

### "AccessDeniedException" on start-session

The IAM user/role doesn't have `ssm:StartSession` permission. For IAM Identity Center
users, verify they have the `Developer` permission set assigned (which includes the
SSM inline policy).
