# shellcheck shell=bash disable=SC2154 # the variables named below are the caller's to set
# listener-api.sh — the listener's HTTP surface as a smoke beside it drives it by hand: one raw
# call either way (listener_get, listener_post), the three requests a client built before
# INFRA-783 sends (old_register, old_claim, old_handback), and the readers each row states its
# outcome in — the fields of GET /v1/roles/<role> (role_field and the four readers over it,
# role_unrenewed) and of GET /v1/registry/<session> (registered, unregistered).
#
# smoke-role-takeover.sh sources it, as it sources local-stack.sh beside this file: what that
# smoke knows about the stack is in that one, and what it knows about the listener's answers is
# in this one.
#
# Sourced, never run: it defines functions, starts no process and writes nothing but the answer
# body each call leaves behind for its reader.
#
# The caller sets
#   envoy_url      the listener's base URL
#   work           the scratch directory each answer body is left in (${work}/answer.json)
#   curl_max_time  how long one call to the listener may take

# listener_get PATH: one raw read of the listener: the status is printed and the answer left in
# ${work}/answer.json for the caller to read.
listener_get() {
  curl -sS -o "${work}/answer.json" -w '%{http_code}' --max-time "$curl_max_time" "${envoy_url}$1" 2>/dev/null ||
    printf '000'
}

# listener_post PATH BODY: one raw call to the listener, as listener_get is for a read: the
# status is printed and the answer left in ${work}/answer.json for the row to read. It is how a
# row drives a client older than this change, which has no process of its own to state and which
# no client in this checkout can be made to impersonate.
listener_post() {
  curl -sS -o "${work}/answer.json" -w '%{http_code}' --max-time "$curl_max_time" \
    -H 'Content-Type: application/json' --data "$2" "${envoy_url}$1" 2>/dev/null ||
    printf '000'
}

# old_register SESSION INSTANCE: one registration sent by curl rather than by a client process.
# An empty INSTANCE sends no client_instance_id at all, which is what every client sent before a
# registration named its process; self_subscribed is what makes the listener write the session
# record the role lookup reads a holder's liveness from.
old_register() {
  listener_post /v1/interests/subscribe "$(jq -nc --arg s "$1" --arg i "$2" \
    '{session_id: $s, self_subscribed: true, topics: []}
     + (if $i == "" then {} else {client_instance_id: $i} end)')"
}

# old_claim SESSION ROLE INSTANCE: one claim sent by curl, last-claim-wins, stating INSTANCE as
# the process making it or no process at all when INSTANCE is empty.
old_claim() {
  listener_post /v1/roles/set "$(jq -nc --arg s "$1" --arg r "$2" --arg i "$3" \
    '{session_id: $s, role: $r}
     + (if $i == "" then {} else {client_instance_id: $i} end)')"
}

# old_handback SESSION TOPIC: the hand-back a client built before this change sends — one role's
# own lane topic, under that client's session id, stating no process. It can free no claim that
# names a process, so the listener marks that claim handed back instead and keeps the lane topic
# in the session's interest record.
old_handback() {
  listener_post /v1/interests/unsubscribe "$(jq -nc --arg s "$1" --arg t "$2" \
    '{session_id: $s, topics: [$t]}')"
}

# role_field ROLE STATUSES FILTER: one field of GET /v1/roles/<role>, read by FILTER from the
# answer the lookup gave, and empty where the lookup answered some status other than the
# space-separated STATUSES — which is each reader's way of saying the answer it reads that field
# off is the only one the field means anything on. The four readers below are this call and
# nothing else, so a row that reads two of them makes two lookups and reads each answer whole.
role_field() {
  local code
  code="$(listener_get "/v1/roles/$1")"
  [[ " $2 " == *" ${code} "* ]] || return 0
  jq -r "$3" "${work}/answer.json"
}

# role_holder ROLE: the session id the listener names as the role's holder, empty for none.
role_holder() {
  role_field "$1" 200 '.holder // ""'
}

# role_instance ROLE: the client instance the listener names as the holder's process, empty for a
# role no live holder's claim names (GET /v1/roles/<role>'s client_instance_id).
role_instance() {
  role_field "$1" 200 '.client_instance_id // ""'
}

# role_handed_back ROLE: "true" where the lookup reports the hand-back mark the role's claim
# carries (GET /v1/roles/<role>'s `handed_back`, on the 200 and on a lapsed holder's 404 alike),
# and empty where it reports none. The listener leaves the key out of a claim no hand-back
# reached, so an empty answer is a claim nobody gave back.
role_handed_back() {
  role_field "$1" "200 404" 'if .handed_back == true then "true" else "" end'
}

# role_reason ROLE: the reason the lookup's 404 carries — "unclaimed" for a role no claim record
# stands for, "holder_lapsed" for a claim whose holder is not live — and empty while a live
# holder answers.
role_reason() {
  role_field "$1" 404 '.reason // ""'
}

# role_unrenewed ROLE: nobody has answered for the role's claim inside the live window. The
# lookup's 404 is the listener's own word for it: a claim is renewed by its holder's
# registrations, and every write that renews one — the claim itself included — refreshes that
# holder's registration too, so a holder the lookup reads as lapsed is a claim past the window.
# It reads one way only. A claim the listener marked handed back is renewed by nobody while its
# holder goes on registering, so such a claim is past the window with this lookup still answering
# 200; the rows that turn on a mark read role_handed_back instead.
role_unrenewed() {
  [[ "$(listener_get "/v1/roles/$1")" == 404 ]]
}

registered() {
  [[ "$(listener_get "/v1/registry/$1")" == 200 ]]
}

unregistered() {
  [[ "$(listener_get "/v1/registry/$1")" == 404 ]]
}
