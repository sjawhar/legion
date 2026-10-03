#!/usr/bin/env bash
# docs/site/generators/broker-config.sh <content dir>
# Writes <content dir>/broker/reference/config.md: every environment variable the secrets broker
# reads, from its configuration loader.
exec "$(dirname "${BASH_SOURCE[0]}")/../../../scripts/docs/broker/refgen.sh" config "$@"
