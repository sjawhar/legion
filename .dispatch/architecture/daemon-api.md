---
title: Coordinator API and CLI
parent: daemon
depends_on: [daemon-state, daemon-supervision, daemon-identity, envoy]
paths: [packages/daemon/cmd/legion, packages/daemon/internal/api, packages/daemon/internal/config, packages/daemon/internal/daemon]
---
Stages 1 and 2 on `main` provide the `legion` command (including `worker-shim` and `claims`), configuration, boot and shutdown assembly, the state and health routes, and the claim-registration and operator routes. Stages 3 and 4 complete the agent API and CLI commands. Its API compatibility with the language-neutral contracts is not a Go source dependency.

`legion controller start` mints the controller's own Dispatch token where the machine proves an identity of its own (`controller_dispatch_token.go`): with `AGENT_SECRETS_MACHINE_IDENTITY` set it trades that identity at the secrets broker through `github.com/sjawhar/envoy/brokerclient`, writes the token 0600 under the state directory and points `DISPATCH_TOKEN_FILE` at it, keeping it fresh while Oh My Pi runs, so nobody copies a person's Dispatch token onto a machine. Unset, it does nothing and the controller reads the `dispatch_token_file` its configuration names, as it always has. That import is this component's one Go source dependency on Envoy.
