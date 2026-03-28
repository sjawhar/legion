#!/bin/bash
set -euo pipefail

key="${1:?usage: read-secret.sh KEY}"

if value=$(secrets get "$key" 2>/dev/null); then
  printf '%s' "$value"
  exit 0
fi

export SOPS_AGE_KEY_FILE="${SOPS_AGE_KEY_FILE:-$HOME/.config/sops/age/keys.txt}"
sops -d --ignore-mac --output-type dotenv "$HOME/.dotfiles/secrets.env" | grep "^${key}=" | cut -d= -f2-
