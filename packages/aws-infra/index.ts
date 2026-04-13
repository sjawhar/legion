import * as aws from "@pulumi/aws";
import * as pulumi from "@pulumi/pulumi";

const cfg = new pulumi.Config("aws-infra");

// Stack configuration
const managementAccountId = cfg.require("managementAccountId");
const devAccountId = cfg.require("devAccountId");
const stagingAccountId = cfg.get("stagingAccountId");
const prodAccountId = cfg.require("prodAccountId");

// Admin group members (emails of users provisioned via SCIM)
const adminEmails = cfg.requireObject<string[]>("adminEmails");
// Engineer group members (emails of users provisioned via SCIM)
const engineerEmails = cfg.requireObject<string[]>("engineerEmails");

// ---------------------------------------------------------------------------
// IAM Identity Center instance (singleton — looked up, not created)
// ---------------------------------------------------------------------------

const instances = pulumi.output(aws.ssoadmin.getInstances({}));
const instanceArn = instances.arns[0];
const identityStoreId = instances.identityStoreIds[0];

// ---------------------------------------------------------------------------
// Permission Sets
// ---------------------------------------------------------------------------

// Administrator — full admin, management account only, short session
const administratorPermissionSet = new aws.ssoadmin.PermissionSet("administrator", {
  name: "Administrator",
  instanceArn: instanceArn,
  sessionDuration: "PT1H",
  description: "Full administrator access — management account only, short session for safety",
});

new aws.ssoadmin.ManagedPolicyAttachment("administrator-policy", {
  instanceArn: instanceArn,
  permissionSetArn: administratorPermissionSet.arn,
  managedPolicyArn: "arn:aws:iam::aws:policy/AdministratorAccess",
});

// Developer — deploy + SSM access, 8-hour session
const developerPermissionSet = new aws.ssoadmin.PermissionSet("developer", {
  name: "Developer",
  instanceArn: instanceArn,
  sessionDuration: "PT8H",
  description: "Deploy, SSM access to dev instances, read most services",
});

new aws.ssoadmin.ManagedPolicyAttachment("developer-policy", {
  instanceArn: instanceArn,
  permissionSetArn: developerPermissionSet.arn,
  managedPolicyArn: "arn:aws:iam::aws:policy/PowerUserAccess",
});

// Developer inline policy: SSM session access (PowerUserAccess covers EC2 describe)
new aws.ssoadmin.PermissionSetInlinePolicy("developer-ssm-policy", {
  instanceArn: instanceArn,
  permissionSetArn: developerPermissionSet.arn,
  inlinePolicy: JSON.stringify({
    Version: "2012-10-17",
    Statement: [
      {
        Sid: "SSMSessionAccess",
        Effect: "Allow",
        Action: ["ssm:StartSession", "ssm:TerminateSession", "ssm:ResumeSession"],
        Resource: "*",
      },
    ],
  }),
});

// ReadOnly — auditing, on-call, prod read access, 8-hour session
const readOnlyPermissionSet = new aws.ssoadmin.PermissionSet("readOnly", {
  name: "ReadOnly",
  instanceArn: instanceArn,
  sessionDuration: "PT8H",
  description: "Auditing, on-call, and prod read access",
});

new aws.ssoadmin.ManagedPolicyAttachment("readOnly-policy", {
  instanceArn: instanceArn,
  permissionSetArn: readOnlyPermissionSet.arn,
  managedPolicyArn: "arn:aws:iam::aws:policy/ReadOnlyAccess",
});

// ---------------------------------------------------------------------------
// Groups (Google SCIM does NOT sync groups — must be created via Pulumi)
// ---------------------------------------------------------------------------

const adminsGroup = new aws.identitystore.Group("admins", {
  identityStoreId: identityStoreId,
  displayName: "Admins",
  description: "Platform administrators — Administrator permission set on management account",
});

const engineersGroup = new aws.identitystore.Group("engineers", {
  identityStoreId: identityStoreId,
  displayName: "Engineers",
  description: "All developers — Developer on dev/staging, ReadOnly on prod",
});

// ---------------------------------------------------------------------------
// Group Memberships
// Users are provisioned via Google SCIM — look them up by email.
// getUser() requires a plain string for identityStoreId, so we use apply()
// to unwrap the Output before calling the data source.
// Resource names use sanitized email to avoid churn on list reorder.
// ---------------------------------------------------------------------------

// Admin group memberships
adminEmails.forEach((email) => {
  const safeName = email.replace(/[^a-zA-Z0-9]/g, "-");
  const userId = identityStoreId.apply((storeId) =>
    aws.identitystore
      .getUser({
        identityStoreId: storeId,
        alternateIdentifier: {
          uniqueAttribute: {
            attributePath: "UserName",
            attributeValue: email,
          },
        },
      })
      .then((u) => u.userId)
  );

  new aws.identitystore.GroupMembership(`admin-member-${safeName}`, {
    identityStoreId: identityStoreId,
    groupId: adminsGroup.groupId,
    memberId: userId,
  });
});

// Engineer group memberships
engineerEmails.forEach((email) => {
  const safeName = email.replace(/[^a-zA-Z0-9]/g, "-");
  const userId = identityStoreId.apply((storeId) =>
    aws.identitystore
      .getUser({
        identityStoreId: storeId,
        alternateIdentifier: {
          uniqueAttribute: {
            attributePath: "UserName",
            attributeValue: email,
          },
        },
      })
      .then((u) => u.userId)
  );

  new aws.identitystore.GroupMembership(`engineer-member-${safeName}`, {
    identityStoreId: identityStoreId,
    groupId: engineersGroup.groupId,
    memberId: userId,
  });
});

// ---------------------------------------------------------------------------
// Account Assignments
// ---------------------------------------------------------------------------

// Admins → Management account → Administrator
new aws.ssoadmin.AccountAssignment("admins-management-administrator", {
  instanceArn: instanceArn,
  permissionSetArn: administratorPermissionSet.arn,
  principalId: adminsGroup.groupId,
  principalType: "GROUP",
  targetId: managementAccountId,
  targetType: "AWS_ACCOUNT",
});

// Engineers → Dev account → Developer
new aws.ssoadmin.AccountAssignment("engineers-dev-developer", {
  instanceArn: instanceArn,
  permissionSetArn: developerPermissionSet.arn,
  principalId: engineersGroup.groupId,
  principalType: "GROUP",
  targetId: devAccountId,
  targetType: "AWS_ACCOUNT",
});

// Engineers → Staging account → Developer (optional: only if stagingAccountId is configured)
if (stagingAccountId) {
  new aws.ssoadmin.AccountAssignment("engineers-staging-developer", {
    instanceArn: instanceArn,
    permissionSetArn: developerPermissionSet.arn,
    principalId: engineersGroup.groupId,
    principalType: "GROUP",
    targetId: stagingAccountId,
    targetType: "AWS_ACCOUNT",
  });
}

// Engineers → Prod account → ReadOnly
new aws.ssoadmin.AccountAssignment("engineers-prod-readOnly", {
  instanceArn: instanceArn,
  permissionSetArn: readOnlyPermissionSet.arn,
  principalId: engineersGroup.groupId,
  principalType: "GROUP",
  targetId: prodAccountId,
  targetType: "AWS_ACCOUNT",
});

// ---------------------------------------------------------------------------
// Dev Box Security Group (cross-account — lives in dev account)
// ---------------------------------------------------------------------------
// The dev box security group is an existing resource adopted via `pulumi import`.
// It blocks all inbound traffic (no SSH from internet) and allows all outbound
// (required for SSM agent to reach AWS endpoints).
//
// Before first `pulumi up`, the operator must:
// 1. Set devBoxSecurityGroupId in Pulumi config to the existing SG ID
// 2. Set devAccountRoleArn to an IAM role in the dev account that can be assumed
// 3. Import: pulumi import aws:ec2/securityGroup:SecurityGroup dev-box-sg <sg-id> --provider dev-account -s prod
// ---------------------------------------------------------------------------

const devBoxSecurityGroupId = cfg.get("devBoxSecurityGroupId");
const devAccountRoleArn = cfg.get("devAccountRoleArn");

let devBoxSgId: pulumi.Output<string> | undefined;

if (devBoxSecurityGroupId) {
  // Explicit provider for dev account resources — assumes a role in the dev
  // account so this management-account stack can manage cross-account resources.
  // If devAccountRoleArn is not set, falls back to ambient credentials (useful
  // when running with dev-account credentials directly).
  const devAccountProvider = new aws.Provider("dev-account", {
    region: "us-west-1",
    ...(devAccountRoleArn ? { assumeRole: { roleArn: devAccountRoleArn } } : {}),
  });

  const devBoxSg = new aws.ec2.SecurityGroup(
    "dev-box-sg",
    {
      description: "Dev box security group — SSM-only access, no public SSH",
      // No ingress rules — blocks all inbound including SSH (port 22)
      ingress: [],
      // Allow all outbound — required for SSM agent to reach AWS endpoints
      egress: [
        {
          protocol: "-1",
          fromPort: 0,
          toPort: 0,
          cidrBlocks: ["0.0.0.0/0"],
          description: "Allow all outbound (SSM agent, package updates, etc.)",
        },
      ],
      tags: {
        Name: "dev-box-sg",
        ManagedBy: "pulumi",
        Purpose: "SSM-only access — no public SSH",
      },
    },
    {
      provider: devAccountProvider,
      import: devBoxSecurityGroupId,
      // Protect against accidental deletion — this is a brownfield resource
      protect: true,
    }
  );

  devBoxSgId = devBoxSg.id;
}

// ---------------------------------------------------------------------------
// Exports
// ---------------------------------------------------------------------------

export const administratorPermissionSetArn = administratorPermissionSet.arn;
export const developerPermissionSetArn = developerPermissionSet.arn;
export const readOnlyPermissionSetArn = readOnlyPermissionSet.arn;
export const adminsGroupId = adminsGroup.groupId;
export const engineersGroupId = engineersGroup.groupId;
export { devBoxSgId as devBoxSecurityGroupId };
