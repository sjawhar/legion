import * as aws from "@pulumi/aws";
import * as pulumi from "@pulumi/pulumi";

const cfg = new pulumi.Config("aws-infra");

// Dev box configuration
// Set devBoxSecurityGroupId in Pulumi.<stack>.yaml before running pulumi up.
// Get it with: aws ec2 describe-instances --instance-ids <id> --query 'Reservations[].Instances[].SecurityGroups[].GroupId' --output text
const devBoxInstanceId = cfg.require("devBoxInstanceId");
const devBoxPublicIp = cfg.require("devBoxPublicIp");
const devBoxSecurityGroupId = cfg.require("devBoxSecurityGroupId");

// Dev box security group — imported from AWS.
//
// To import the existing security group on first run:
//   pulumi import aws:ec2/securityGroup:SecurityGroup dev-box-sg <security-group-id> -s prod
//
// Security posture after this change:
//   - SSH (port 22) from internet: REMOVED — use SSM Session Manager or Tailscale SSH instead
//   - HTTPS outbound (port 443): PRESERVED — required for SSM agent to reach AWS endpoints
//
// Access paths after this change:
//   1. SSM Session Manager: aws ssm start-session --target <instance-id> --profile dev
//   2. Tailscale SSH: ssh sami@sami (Tailscale overlay network, unaffected by AWS SGs)
//   3. Docker deployment: pulumi up in packages/envoy/infra/ (uses Tailscale SSH)
const devBoxSg = new aws.ec2.SecurityGroup(
  "dev-box-sg",
  {
    description: "Dev box security group — SSH-free, SSM + Tailscale access only",
    // Egress: allow all outbound (required for SSM agent, package updates, etc.)
    egress: [
      {
        protocol: "-1",
        fromPort: 0,
        toPort: 0,
        cidrBlocks: ["0.0.0.0/0"],
        ipv6CidrBlocks: ["::/0"],
        description: "Allow all outbound traffic",
      },
    ],
    // Ingress: NO SSH (port 22) rule — intentionally omitted.
    // SSM Session Manager does not require any inbound rules.
    // Tailscale operates over WireGuard on port 41641 (UDP) via the Tailscale overlay network,
    // which is not subject to AWS security group rules on the VPC interface.
    ingress: [],
    tags: {
      Name: "dev-box-sg",
      ManagedBy: "pulumi",
      Purpose: "dev-box",
    },
  },
  {
    // Import the existing security group rather than creating a new one.
    // Run: pulumi import aws:ec2/securityGroup:SecurityGroup dev-box-sg <sg-id> -s prod
    import: devBoxSecurityGroupId,
  }
);

// Export useful values for reference and verification
export const instanceId = devBoxInstanceId;
export const publicIp = devBoxPublicIp;
export const securityGroupId = devBoxSg.id;
export const accessInstructions = pulumi.interpolate`
Dev Box Access (${devBoxInstanceId} / ${devBoxPublicIp}):
  SSM Session:    aws ssm start-session --target ${devBoxInstanceId} --profile dev
  Tailscale SSH:  ssh sami@sami
  Public SSH:     DISABLED (port 22 closed — intentional)

Verify SSH is blocked:
  nc -zw3 ${devBoxPublicIp} 22  # should fail/timeout

Verify SSM works:
  aws ssm start-session --target ${devBoxInstanceId} --profile dev
`;
