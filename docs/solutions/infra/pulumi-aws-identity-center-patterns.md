---
title: "Pulumi AWS Identity Center (SSO) Resource Patterns"
category: infra
tags:
  - pulumi
  - aws
  - identity-center
  - sso
  - iam
  - permission-sets
  - groups
  - scim
date: 2026-04-13
status: active
module: aws-infra
related_issues:
  - "sjawhar-legion-451"
symptoms:
  - "pulumi.output() needed for async data sources"
  - "getUser() type error: Input<string> not assignable to string"
  - "GroupMembership churn on list reorder"
  - "SCIM groups not syncing from Google Workspace"
  - "identityStoreId not available in resource args"
---

# Pulumi AWS Identity Center (SSO) Resource Patterns

## Context

AWS IAM Identity Center (formerly SSO) uses a singleton instance per AWS organization. Pulumi
manages it via `@pulumi/aws` with the `ssoadmin` and `identitystore` namespaces. Several
non-obvious patterns are required to use these resources correctly.

## Pattern 1: Wrap Async Data Sources with `pulumi.output()`

`aws.ssoadmin.getInstances()` (and similar data source functions) return `Promise<T>`, not
`Output<T>`. Pulumi resource args require `Input<T>` (which accepts `Output<T>` but not
`Promise<T>`).

**Wrong:**
```typescript
const instances = await aws.ssoadmin.getInstances({});
// instances is T, not Output<T> — can't use in resource args
```

**Correct:**
```typescript
const instances = pulumi.output(aws.ssoadmin.getInstances({}));
// instances is Output<T> — works in resource args
const instanceArn = instances.apply(i => i.arns[0]);
const identityStoreId = instances.apply(i => i.identityStoreIds[0]);
```

## Pattern 2: Unwrap `Output<string>` Before Calling Data Source Functions

`aws.identitystore.getUser()` takes a plain `string` for `identityStoreId`, not
`Input<string>`. When `identityStoreId` is an `Output<string>` (from the instances data
source), you must use `.apply()` to unwrap it before calling the function.

**Wrong:**
```typescript
// TypeScript error: Output<string> not assignable to string
const user = aws.identitystore.getUser({
  identityStoreId: identityStoreId,  // Output<string> — type error
  alternateIdentifier: { ... },
});
```

**Correct:**
```typescript
const userId = identityStoreId.apply(id =>
  aws.identitystore.getUser({
    identityStoreId: id,  // plain string — correct
    alternateIdentifier: {
      uniqueAttribute: {
        attributePath: "UserName",
        attributeValue: email,
      },
    },
  }).then(u => u.userId)
);
```

## Pattern 3: Stable GroupMembership Resource Names

Use a stable identifier (sanitized email) as the Pulumi resource name for
`aws.identitystore.GroupMembership`, not the array index. If the member list is reordered,
index-based names cause Pulumi to delete and recreate all memberships unnecessarily.

**Wrong:**
```typescript
members.forEach((email, i) => {
  new aws.identitystore.GroupMembership(`admins-member-${i}`, { ... });
  // Reordering members → Pulumi sees resource name change → delete + recreate
});
```

**Correct:**
```typescript
members.forEach(email => {
  const safeName = email.replace(/[^a-zA-Z0-9]/g, "-");
  new aws.identitystore.GroupMembership(`admins-member-${safeName}`, { ... });
  // Reordering members → resource names unchanged → no churn
});
```

## Pattern 4: SCIM Syncs Users, Not Groups

Google Workspace SCIM provisioning syncs **users** to AWS Identity Center automatically,
but does **not** sync groups. Groups must be created via Pulumi using
`aws.identitystore.Group`. User membership in those groups references SCIM-provisioned
user IDs via `aws.identitystore.getUser` data source lookup by email.

```typescript
// Group created by Pulumi (not SCIM)
const adminsGroup = new aws.identitystore.Group("admins", {
  identityStoreId,
  displayName: "Admins",
});

// User provisioned by SCIM — look up by email
const userId = identityStoreId.apply(id =>
  aws.identitystore.getUser({ identityStoreId: id, ... }).then(u => u.userId)
);

// Membership managed by Pulumi
new aws.identitystore.GroupMembership("admins-member-alice", {
  identityStoreId,
  groupId: adminsGroup.groupId,
  memberId: userId,
});
```

## Pattern 5: Optional Account Assignments

Use `cfg.get("key")` (returns `undefined` if absent) vs `cfg.require("key")` (throws if
absent) for optional infrastructure like staging account assignments.

```typescript
const stagingAccountId = cfg.get("stagingAccountId");
if (stagingAccountId) {
  new aws.ssoadmin.AccountAssignment("engineers-staging-developer", {
    instanceArn,
    permissionSetArn: developerPermissionSet.arn,
    principalId: engineersGroup.groupId,
    principalType: "GROUP",
    targetId: stagingAccountId,
    targetType: "AWS_ACCOUNT",
  });
}
```

## Checklist for Identity Center Resources

- [ ] `getInstances()` wrapped with `pulumi.output()` → `Output<T>`
- [ ] `getUser()` called inside `.apply(id => ...)` to unwrap `identityStoreId`
- [ ] GroupMembership resource names use sanitized email, not array index
- [ ] Groups created via Pulumi (not expected from SCIM)
- [ ] Optional assignments use `cfg.get()` not `cfg.require()`
- [ ] `identityStoreId` sourced from `instances.apply(i => i.identityStoreIds[0])`
- [ ] `instanceArn` sourced from `instances.apply(i => i.arns[0])`
