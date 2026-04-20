---
title: "Pulumi Cross-Account Resource Management and Brownfield Adoption"
category: infra
tags:
  - pulumi
  - aws
  - cross-account
  - security-group
  - brownfield
  - import
  - multi-account
date: 2026-04-13
status: historical
related_issues:
  - "453"
symptoms:
  - "Security group created in wrong VPC after removing import option"
  - "Cross-account resource requires explicit provider with assumeRole"
  - "Pulumi import works but resource definition is incomplete"
  - "Brownfield resource accidentally deleted by Pulumi"
  - "Region hardcoded on cross-account provider instead of config-driven"
---

# Pulumi Cross-Account Resource Management and Brownfield Adoption

## Context

When a Pulumi stack runs against one AWS account (e.g., management account for IAM Identity
Center) but needs to manage resources in a different account (e.g., a security group in the
dev account), you need cross-account providers. When those resources already exist in AWS
(brownfield), you also need to import them safely.

Issue #453 established these patterns for managing a dev box security group that lives in the
dev account from the management-account Pulumi stack.

## Pattern 1: Cross-Account Provider with Optional assumeRole

Create an explicit `aws.Provider` for the target account. Make `assumeRole` optional so the
same code works both with cross-account role assumption AND with direct target-account
credentials.

```typescript
const devAccountRoleArn = cfg.get("devAccountRoleArn");

const devAccountProvider = new aws.Provider("dev-account", {
  region: cfg.require("devAccountRegion"), // <-- derive from config, don't hardcode
  ...(devAccountRoleArn ? { assumeRole: { roleArn: devAccountRoleArn } } : {}),
});
```

**Gotcha: Don't hardcode region.** The original implementation hardcoded `region: "us-west-1"`
on the cross-account provider while the default provider derived region from config. If the
stack moves regions, the cross-account provider silently points at the wrong region. Always
derive region from stack config or a dedicated config key.

## Pattern 2: Brownfield Import with `protect: true`

When adopting existing AWS resources into Pulumi, use `import` to adopt without recreation,
and `protect: true` to prevent accidental deletion.

```typescript
const devBoxSg = new aws.ec2.SecurityGroup("dev-box-sg", {
  /* resource properties */
}, {
  provider: devAccountProvider,
  import: devBoxSecurityGroupId,
  protect: true,  // critical for brownfield
});
```

**Why protect matters:** Without `protect`, a `pulumi destroy` or accidental removal of the
resource from code would delete the actual AWS resource — dangerous for shared infrastructure
that existed before IaC.

## Pattern 3: Pin All Identifying Properties on Imported Resources

**This is the most important learning from this PR.**

When importing a resource, Pulumi reads the current state from AWS and fills in properties
you didn't declare. This means a resource definition can "work" with `import` while being
incomplete. If someone later removes the `import` line (e.g., after first successful import),
Pulumi creates a NEW resource with only the declared properties — potentially in the wrong
VPC, subnet, or AZ.

**Wrong — works with import but breaks without it:**
```typescript
new aws.ec2.SecurityGroup("dev-box-sg", {
  description: "Dev box SG",
  ingress: [],
  egress: [{ /* ... */ }],
  // No vpcId — import fills it in from AWS state
}, {
  import: sgId,
});
```

**Correct — self-contained, works with or without import:**
```typescript
new aws.ec2.SecurityGroup("dev-box-sg", {
  description: "Dev box SG",
  vpcId: cfg.require("devBoxVpcId"),  // <-- always pin placement properties
  ingress: [],
  egress: [{ /* ... */ }],
}, {
  import: sgId,
});
```

**Rule:** For any `import`-based resource, explicitly set all identifying/placement properties
(VPC, subnet, AZ, region, etc.) so the resource definition is self-contained and correct
even if the `import` option is removed.

## Pattern 4: Config-Gated Resource Blocks

Use `cfg.get()` (returns `undefined` if absent) to make cross-account resources a no-op
until the operator sets the required config values. This enables a safe onboarding flow:
set config, run import, then apply.

```typescript
const devBoxSecurityGroupId = cfg.get("devBoxSecurityGroupId");

if (devBoxSecurityGroupId) {
  // Cross-account provider and resource only created when config is set
  const provider = new aws.Provider("dev-account", { /* ... */ });
  new aws.ec2.SecurityGroup("dev-box-sg", { /* ... */ }, { provider, import: devBoxSecurityGroupId });
}
```

This pattern is also used for optional account assignments (e.g., staging) in the same
codebase.

## Pattern 5: Explicit Ingress/Egress Declaration

When the intent is "no inbound traffic," declare `ingress: []` explicitly rather than omitting
the property. This makes the security posture clear in code and ensures Pulumi actively
removes any existing inbound rules.

```typescript
new aws.ec2.SecurityGroup("dev-box-sg", {
  ingress: [],  // Explicitly empty — Pulumi removes all existing inbound rules
  egress: [{
    protocol: "-1",
    fromPort: 0,
    toPort: 0,
    cidrBlocks: ["0.0.0.0/0"],
    description: "Allow all outbound (SSM agent, package updates, etc.)",
  }],
});
```

## Checklist for Cross-Account Brownfield Resources

- [ ] Cross-account provider region derived from config, not hardcoded
- [ ] `assumeRole` is optional (conditional spread) for credential flexibility
- [ ] `import` option set to existing resource ID
- [ ] `protect: true` to prevent accidental deletion
- [ ] All identifying/placement properties declared (VPC, subnet, AZ) — not just relying on import state
- [ ] Resource block gated on config existence (`cfg.get()` + `if` check)
- [ ] Config file includes comments with lookup commands for the required IDs
- [ ] Tags include `ManagedBy: "pulumi"` for console discoverability
