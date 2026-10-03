#!/bin/sh
# The command an agent runs with DEMO_API_KEY in the demo: it shows the key reached this process
# without printing it — its length and last four characters (the rig's value is made up).
if [ -z "${DEMO_API_KEY:-}" ]; then
  echo "DEMO_API_KEY is not set" >&2
  exit 1
fi
last4="$(printf '%s' "$DEMO_API_KEY" | tail -c 4)"
echo "DEMO_API_KEY reached this command: ${#DEMO_API_KEY} characters, ending in ${last4}"
