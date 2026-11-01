# shellcheck shell=bash
# The model endpoint a key file's key is for: Anthropic's own, or the Anthropic-compatible gateway
# (a LiteLLM proxy, say) LEGION_E2E_MODEL_BASE_URL names. Sourced by claude-backend-tmux.sh and
# claude-backend-sandbox.sh, whose own note and fail model_key_preflight calls, and by
# lib/install-key-file-model-route.sh, which checks its --base-url with model_base_url_refusal.

# anthropic_base_url is the endpoint when no base URL is named.
anthropic_base_url=https://api.anthropic.com

# model_base_url_refusal URL prints why URL cannot be a model base URL, and nothing when it can: an
# http:// or https:// URL of letters, digits and :/._~- alone, so it carries no user or password
# and stays a plain YAML scalar in an Oh My Pi models.yml and legion.yaml, ending in neither a colon
# (YAML reads that as a mapping key) nor a slash (every caller appends /v1/… to it).
model_base_url_refusal() {
  case "$1" in
  *[!A-Za-z0-9:/._~-]*) echo "holds a character other than letters, digits and :/._~- (so a URL carrying a user or password is refused)" ;;
  *:) echo "ends in a colon, which YAML reads as a mapping key" ;;
  */) echo "ends in a slash, which the /v1/… every caller appends would double" ;;
  http://?* | https://?*) ;;
  *) echo "is not an http:// or https:// URL" ;;
  esac
}

# model_get URL HEADER BODY prints the HTTP code GET URL answered, the response body going to BODY,
# sent with the key's header file HEADER (no key when HEADER is empty) and the anthropic-version
# header Anthropic's API requires; `unreachable (curl exited N)` when nothing answered. The key
# reaches curl by file, never in an argv.
model_get() {
  local code
  local -a args=(-sS --max-time 30 -o "$3" -w '%{http_code}' -H 'anthropic-version: 2023-06-01')
  [ -z "$2" ] || args+=(-H "@$2")
  code=$(curl "${args[@]}" "$1") || code="unreachable (curl exited $?)"
  printf '%s\n' "$code"
}

# model_key_preflight BASE KEY HEADER WHAT holds the model endpoint BASE (Anthropic's own when
# empty) to the key in KEY, the run's 0600 copy of the key file WHAT names, before anything is
# created: GET /v1/models with the key must answer 200. With BASE named, two more. GET /v1/models
# with no key must answer something else, or the endpoint checks no key and its 200 proves nothing
# about this one. GET /key/info with the key, which a LiteLLM proxy answers with the key's
# `key_alias`, notes that non-secret name, or that the endpoint names no alias, and never refuses.
# The key reaches curl in HEADER, a 0600 header file removed before this returns (a refusal is the
# caller's fail, an exit, so the caller's cleanup removes HEADER too); neither the key nor a
# response body is printed.
model_key_preflight() {
  local base=$1 key=$2 header=$3 what=$4 endpoint code alias
  endpoint=${base:-$anthropic_base_url}
  (umask 077 && printf 'x-api-key: %s\n' "$(cat "$key")" >"$header")
  code=$(model_get "$endpoint/v1/models" "$header" /dev/null)
  if [ "$code" != 200 ]; then
    rm -f "$header"
    fail "the model endpoint $endpoint refuses the key in $what: GET /v1/models answered $code"
  fi
  if [ -n "$base" ]; then
    code=$(model_get "$endpoint/v1/models" "" /dev/null)
    case "$code" in
    200)
      rm -f "$header"
      fail "the model endpoint $endpoint answered GET /v1/models with no key 200: it does not check keys, so its 200 to the key in $what proves nothing about that key"
      ;;
    [0-9][0-9][0-9]) note "the model endpoint $endpoint checks keys: GET /v1/models with no key answered $code" ;;
    *)
      rm -f "$header"
      fail "the model endpoint $endpoint did not answer GET /v1/models with no key ($code), so whether it checks keys is unknown"
      ;;
    esac
    # The body may echo the key back, so it is 0600 and read for key_alias alone.
    code=$(umask 077 && model_get "$endpoint/key/info" "$header" "$header.key-info")
    alias=
    [ "$code" != 200 ] || alias=$(jq -r '(.key_alias // .info.key_alias) | strings' "$header.key-info" 2>/dev/null) || alias=
    rm -f "$header.key-info"
    if [[ $alias =~ ^[[:graph:]][[:print:]]*$ ]]; then
      note "the model key is $alias: the endpoint's name for the key in $what (GET /key/info)"
    else
      note "the model endpoint $endpoint names no key alias for the key in $what: GET /key/info answered $code"
    fi
  fi
  rm -f "$header"
}
