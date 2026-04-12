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
// SSM Session Manager — EC2 dev box access
// ---------------------------------------------------------------------------

// IAM role for EC2 instances to communicate with SSM
const ec2SsmRole = new aws.iam.Role("EC2SSMRole", {
  name: "EC2SSMRole",
  assumeRolePolicy: JSON.stringify({
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Principal: { Service: "ec2.amazonaws.com" },
        Action: "sts:AssumeRole",
      },
    ],
  }),
  description: "Allows EC2 instances to use SSM Session Manager",
});

// AmazonSSMManagedInstanceCore covers: ssm:*, ssmmessages:*, ec2messages:*, s3:GetObject (for patch manager)
new aws.iam.RolePolicyAttachment("EC2SSMRole-ssm-core", {
  role: ec2SsmRole.name,
  policyArn: "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
});

// Instance profile wrapping the role — attach this to the dev box
const ec2SsmInstanceProfile = new aws.iam.InstanceProfile("EC2SSMInstanceProfile", {
  name: "EC2SSMInstanceProfile",
  role: ec2SsmRole.name,
});

// ---------------------------------------------------------------------------
// Exports
// ---------------------------------------------------------------------------

export const administratorPermissionSetArn = administratorPermissionSet.arn;
export const developerPermissionSetArn = developerPermissionSet.arn;
export const readOnlyPermissionSetArn = readOnlyPermissionSet.arn;
export const adminsGroupId = adminsGroup.groupId;
export const engineersGroupId = engineersGroup.groupId;
export const ec2SsmRoleArn = ec2SsmRole.arn;
export const ec2SsmInstanceProfileArn = ec2SsmInstanceProfile.arn;
export const ec2SsmInstanceProfileName = ec2SsmInstanceProfile.name;
