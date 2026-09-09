#!/usr/bin/env bash
set -euo pipefail

docker run -d --name dispatch-pg -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432 postgres:16
printf '%s\n' 'DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable'
