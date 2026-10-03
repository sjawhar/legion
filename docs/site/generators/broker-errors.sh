#!/usr/bin/env bash
# docs/site/generators/broker-errors.sh <content dir>
# Writes <content dir>/broker/reference/errors.md: every refusal code the secrets broker, its host
# helper and the agent-secrets CLI answer, from their source.
exec "$(dirname "${BASH_SOURCE[0]}")/../../../scripts/docs/broker/refgen.sh" errors "$@"
