# agent-secrets

The secrets broker's client. An agent session runs it to use a secret:

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
```

It asks the broker for the named secrets, waits while a person approves them in Dispatch if the
rules require it, and runs the command with each granted value in its environment. People and
launchers use its other forms to log a machine in (`agent-secrets launcher login`), register an
agent session (`agent-secrets register --exec -- <agent>`), and inspect a session's grants
(`agent-secrets self`). `agent-secrets --help` lists every form, and each form answers `-h`.

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

Each `legion-envoy-v*` GitHub release ships both binaries for Linux as
`agent-secrets-amd64.tar.gz` and `agent-secrets-arm64.tar.gz`.

## Test

```sh
cd packages/envoy
go test ./cmd/agent-secrets/... ./cmd/agent-secrets-helper/...
```
