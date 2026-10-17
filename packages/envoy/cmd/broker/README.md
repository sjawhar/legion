# broker

The secrets broker: it enrolls agent sessions, decides their secret requests from each secret's
owner and tier tags or by a person's approval in Dispatch, records every request and decision, and
releases granted values.
The broker's documentation, generated reference included, is the "Secrets Broker" section of the
docs site, built from `docs/site/src/content/docs/broker/`.

## Build

```sh
cd packages/envoy
go build -o envoy-broker ./cmd/broker
```

The Envoy image (`packages/envoy/docker/Dockerfile`) ships it as `envoy-broker`.

## Run

The broker takes no flags and reads its configuration from `BROKER_*` environment variables
(`internal/broker/config/config.go`). It needs Postgres and the agent secrets in its secret store
(`BROKER_SECRETS_STORE`: AWS Secrets Manager, or Google Cloud Secret Manager under Application
Default Credentials): every secret under `BROKER_SECRETS_PREFIX`, carrying `owner` and `tier` (AWS
tags, or Secret Manager annotations) and encrypted with the agent-secrets key
(`BROKER_SECRETS_KMS_KEY_ARN`, or `BROKER_SECRETS_KMS_KEY_NAME`). The docs site's "Operating the broker" page
(`docs/site/src/content/docs/broker/operate.md`) gives a minimal production configuration and
everything the broker depends on, and its generated configuration reference lists every variable
with its default.

For a local stack (Postgres in Docker, or one you run named by `DEV_BROKER_POSTGRES_URL`, a fake
secrets file standing in for Secrets Manager, and the clients), run
`packages/envoy/scripts/dev-broker.sh`.

## Test

```sh
cd packages/envoy
go test ./internal/broker/... ./cmd/broker/...
```

Postgres-backed tests skip unless `BROKER_TEST_DATABASE_URL` is set;
`packages/envoy/scripts/dev-postgres.sh` starts a local Postgres for them. `packages/envoy/AGENTS.md`
("Secrets broker") is the contributor's guide to the code.
