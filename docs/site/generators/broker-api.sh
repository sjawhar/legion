#!/usr/bin/env bash
# docs/site/generators/broker-api.sh <content dir>
# Writes <content dir>/broker/reference/api.md: the secrets broker's HTTP API, from its route table
# and handlers.
exec "$(dirname "${BASH_SOURCE[0]}")/../../../scripts/docs/broker/refgen.sh" api "$@"
