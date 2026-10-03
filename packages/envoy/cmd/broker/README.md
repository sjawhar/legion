# broker

The secrets broker: it enrolls agent sessions, decides their secret requests by its rules or by a
person's approval in Dispatch, records every request and decision, and releases granted values.
The broker's documentation, generated reference included, is the "Secrets broker" section of the
docs site, built from `docs/site/src/content/docs/broker/`.

## Build

```sh
cd packages/envoy
go build -o envoy-broker ./cmd/broker
```

The Envoy image (`packages/envoy/docker/Dockerfile`) ships it as `envoy-broker`.

## Run

The broker takes no flags and reads its configuration from `BROKER_*` environment variables
(`internal/broker/config/config.go`; the docs site's configuration reference lists every one). It
needs Postgres, a rules file and a secret store, and the AWS SDK needs a region to reach the last
two:

```sh
BROKER_DATABASE_URL=postgres://... \
BROKER_PUBLIC_URL=https://secrets.internal.example \
BROKER_UI_TOKEN_FILE=/run/secrets/broker-ui-token \
BROKER_RULES_S3_URI=s3://<bucket>/agent-secret-rules.yaml \
AWS_REGION=<region> \
envoy-broker
```

For a local stack (Postgres in Docker, or one you run named by `DEV_BROKER_POSTGRES_URL`, example
rules, fake secret values, and the clients), run `packages/envoy/scripts/dev-broker.sh`.

## Test

```sh
cd packages/envoy
go test ./internal/broker/... ./cmd/broker/...
```

Postgres-backed tests skip unless `BROKER_TEST_DATABASE_URL` is set;
`packages/envoy/scripts/dev-postgres.sh` starts a local Postgres for them. `packages/envoy/AGENTS.md`
("Secrets broker") is the contributor's guide to the code.
