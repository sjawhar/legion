---
title: Coordinator API and CLI
parent: daemon
depends_on: [daemon-state, daemon-supervision, daemon-identity, envoy]
paths: [packages/daemon/cmd/legion, packages/daemon/internal/api, packages/daemon/internal/config, packages/daemon/internal/daemon]
---
Stages 1 and 2 on `main` provide the `legion` command (including `worker-shim` and `claims`), configuration, boot and shutdown assembly, the state and health routes, and the claim-registration and operator routes. Stages 3 and 4 complete the agent API and CLI commands. Its API compatibility with the language-neutral contracts is not a Go source dependency.

`legion controller start` hands the controller's session the secrets broker's route to a Dispatch credential where the machine proves an identity of its own (`controller_dispatch_token.go`): with `AGENT_SECRETS_MACHINE_IDENTITY` set the session's own Envoy client trades that identity for a DPoP-bound access token and renews it, so nobody copies a person's Dispatch token onto a machine. The start writes no token and hands the session no `DISPATCH_TOKEN_FILE` or `DISPATCH_TOKEN` — Dispatch refuses such a token presented as a bearer (RFC 9449 Section 7.2), and the client consults the broker only where no bearer resolves — and drops an inherited one; it trades once itself through `github.com/sjawhar/envoy/brokerclient`, before the daemon mints a capability, so a broker that refuses this machine refuses the start in the broker's own words. Unset, it does nothing and the controller reads the `dispatch_token_file` its configuration names, as it always has. That import is this component's one Go source dependency on Envoy.
