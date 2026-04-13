---
title: "Tailscale OAuth Client Auth for tsnet Deployments"
category: envoy
tags:
  - tailscale
  - tsnet
  - oauth
  - config-resolution
  - backward-compatibility
  - infrastructure
  - pulumi
date: 2026-04-13
status: active
module: envoy
related_issues:
  - "510"
symptoms:
  - "ENVOY_TSNET_AUTH_KEY and ENVOY_TSNET_OAUTH_CLIENT_SECRET are mutually exclusive"
  - "ENVOY_TSNET_TAGS is required when using OAuth credentials"
  - "tsnet authentication requires manual login URL visit"
  - "how to use Tailscale OAuth client with tsnet"
---

# Tailscale OAuth Client Auth for tsnet Deployments

## Context

Tailscale's tsnet library embeds a Tailscale node into a Go process. Authentication
typically requires a `TS_AUTHKEY` — but for production, you want OAuth client credentials
(rotatable, org-scoped, no human-in-the-loop). The mechanism for doing this is documented
but non-obvious.

## Key Learning: OAuth Secret as Auth Key

**Tailscale's OAuth client secrets can be used directly as tsnet auth keys** by appending
URL-style parameters:

```
<client_secret>?ephemeral=false&preauthorized=true&tags=tag:envoy
```

This is documented in [Tailscale's OAuth client docs](https://tailscale.com/kb/1215/oauth-clients)
but easy to miss. The client secret doubles as an auth key when formatted this way.

Important nuances:
- The **OAuth client ID is not part of the auth key** — only the secret is used. The ID
  exists for validation (both halves configured) and potential future API token generation.
- **Tags are required** with OAuth credentials — Tailscale requires tagged authentication
  for non-personal nodes.
- The node registers as **non-ephemeral and preauthorized** — it persists in the tailnet
  and doesn't need admin approval.

## Pattern: Resolve Complex Config at the Boundary

The implementation uses a `resolveAuthKey()` function that encapsulates all the auth
mode logic:

```go
func resolveAuthKey() (string, error) {
    // Read all env vars
    // Detect which mode (OAuth vs legacy vs none)
    // Validate mutual exclusion
    // Construct final auth key string
    return authKey, nil
}
```

The rest of the codebase (`server.go`, etc.) sees only a single `AuthKey` string — it
doesn't know or care whether it came from OAuth credentials or a legacy key. This kept
the change surface minimal: only `config.go` needed logic changes.

**When to use this pattern:** Any time you have multiple ways to configure the same
value (credential rotation, migration from one provider to another, dev vs prod paths).
Resolve at the config boundary so consumers remain simple.

## Pattern: Backward-Compatible Migration via Mutual Exclusion

Rather than removing the legacy `ENVOY_TSNET_AUTH_KEY`, the implementation:
1. Keeps the old path working unchanged
2. Adds the new OAuth path
3. Errors if both are configured simultaneously

This prevents silent misconfiguration where the wrong credential wins. The error message
explicitly says "mutually exclusive" so operators know to remove one.

## Gotchas

### Partial OAuth config triggers validation

Setting _either_ `ENVOY_TSNET_OAUTH_CLIENT_ID` or `ENVOY_TSNET_OAUTH_CLIENT_SECRET`
(but not both) triggers the OAuth path, which then fails validation. This is intentional
— it catches partial configuration — but could surprise someone who sets only the ID
thinking it's inert.

### Docker compose passes empty strings, not absent vars

`${ENVOY_TSNET_OAUTH_CLIENT_ID:-}` defaults to empty string, so the env var is always
present in the container (just empty). The Go code's `strings.TrimSpace` + empty check
is load-bearing for this deployment path.

### Tags must coordinate across infra and Go layers

Tags come from the machine config YAML (`Pulumi.prod.yaml`), while OAuth credentials
come from Pulumi secrets. They must be coordinated — OAuth requires tags — but there's
no infra-level validation. The Go code catches the mismatch at runtime, which is late
in the deployment cycle.

## Testing Pattern: Env Var Isolation for Multi-Mode Config

When testing multiple auth modes, each test helper must clear the _other_ mode's env vars
to prevent cross-test leakage:

```go
func setTsnetEnv(t *testing.T, ...) {
    // Set legacy vars
    t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_ID", "")     // Clear OAuth
    t.Setenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET", "")
    t.Setenv("ENVOY_TSNET_TAGS", "")
}

func setOAuthEnv(t *testing.T, ...) {
    // Set OAuth vars
    t.Setenv("ENVOY_TSNET_AUTH_KEY", "")  // Clear legacy
}
```

Without this, test ordering can cause false passes/failures when env vars leak.

## Testing Pattern: Length-Based Assertions for Pulumi Outputs

Pulumi `Output` objects can't be compared as strings in tests. The Pulumi tests use
`envs.length` assertions to verify optional env vars are included/excluded. This is
pragmatic but fragile — adding an unrelated env var breaks all length assertions.
Prefer prefix-based checks where possible:

```ts
// Fragile: breaks if any new env var is added
expect(envs.length).toBe(6);

// Better: checks presence of specific env
const hasTagsEnv = envs.some((e) => typeof e === "string" && e.startsWith("ENVOY_TSNET_TAGS="));
expect(hasTagsEnv).toBe(true);
```
