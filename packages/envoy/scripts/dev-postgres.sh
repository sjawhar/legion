#!/usr/bin/env bash
set -euo pipefail

docker run -d --name dispatch-pg -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432 pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a
printf '%s\n' 'DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable'
