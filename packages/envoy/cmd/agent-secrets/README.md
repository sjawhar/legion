# agent-secrets

The secrets broker's client. An agent session runs it to use a secret:

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
```

It asks the broker for the named secrets, waits while a person approves them in Dispatch if the
secret's owner and tier require it, and runs the command with each granted value in its
environment. People and launchers use its other forms to log a machine in (`agent-secrets launcher
login`), register an agent session (`agent-secrets register --exec -- <agent>`), and inspect a
session's grants (`agent-secrets self`). People manage the agent secrets themselves with
`agent-secrets secret list|show|create|set|retag|delete|restore`, which calls AWS Secrets Manager
under their own AWS sign-in and then asks the broker to reread each secret written, so the change
is served at once; those forms need no helper and no session. `agent-secrets --help` lists every
form, and each form answers `-h`.

On a machine that runs agents directly, `agent-secrets-helper` (`../agent-secrets-helper`) holds
each session's key and signs for it; in a container or a Kubernetes pod, the session's key lives in
`AGENT_SECRETS_KEY_DIR`.

The docs site's "Secrets Broker" section, built from `docs/site/src/content/docs/broker/`, has the
quickstart, the guides and the generated CLI reference.

## Build

```sh
cd packages/envoy
go build -o bin/ ./cmd/agent-secrets ./cmd/agent-secrets-helper
```

On macOS, build `./cmd/agent-secrets` alone: the helper builds for Linux only. Each
`legion-envoy-v*` GitHub release ships both binaries for Linux, as `agent-secrets-amd64.tar.gz` and
`agent-secrets-arm64.tar.gz`, and `agent-secrets` alone for macOS, as
`agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`.

The hidden value prompt is available on Linux and macOS for amd64 and arm64. Other targets,
including Windows, FreeBSD and other Linux architectures, build without a prompt and require
the value on stdin, for example `agent-secrets secret set DEMO_API_KEY < value.txt`.

## Test

```sh
cd packages/envoy
go test ./cmd/agent-secrets/... ./cmd/agent-secrets-helper/...
```
