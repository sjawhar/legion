#!/usr/bin/env bash
# Times Dispatch's six write routes against one running server, which is INFRA-325's A9: the
# median wall time of 50 requests to each of POST /api/v1/issues/{key}/comments, .../asks,
# .../messages, .../artifacts under a name that already exists, that same upload over a document
# whose latest version is 100-660 KB, and PATCH /api/v1/issues/{key} with a title alone.
#
# Five routes send a body of --body-bytes. The sixth sends the large document's own markdown
# back with one line added, so all 50 of its requests are an upload over a latest version in
# that band: a small body would shrink the document on the first request, and the other 49 would
# measure the case the two new triggers cost least on rather than most.
#
# It measures one server per run and prints one tab-separated line per route - the label, the
# route, the median in milliseconds and every reading - so the same command measures the base
# server and the head server, and a caller runs it as many times as the plan asks for:
#
#   for round in 1 2 3; do
#     scripts/route-timings.sh --url "$BASE_URL" --issue SEED1-1 --label "base-$round" >>runs.tsv
#     scripts/route-timings.sh --url "$HEAD_URL" --issue SEED1-1 --label "head-$round" >>runs.tsv
#   done
#   scripts/route-timings.sh --summarise runs.tsv
#
# --summarise reads those lines back and prints, for each route, every run's median, the highest
# base run - the reading a rise is judged against, since a rise inside the spread of the base
# runs is no rise - and each head run against it. A label beginning "base" is a base run and one
# beginning "head" a head run; any other label is reported and judged against nothing.
#
# The writes stay in the database: 50 comments, 50 asks, 50 messages, 100 document versions and
# 50 title edits for each run. That is what A9 measures, so measure the route timings after the
# statement timings, and re-seed before anything reads a row count again.
set -euo pipefail

requests=50
body_bytes=8192
label=""
url=""
issue=""
artifact=""
large_artifact=""
summarise=""
user="${DISPATCH_USER:-bench}"

usage() {
  cat >&2 <<'USAGE'
usage: route-timings.sh --url <base url> --issue <issue key> [options]
       route-timings.sh --summarise <file written by the runs above>

  --url <base url>        the running Dispatch to measure, without a trailing path
  --issue <key>           the issue every route writes to
  --label <name>          names the run in the output; "base-1", "head-1", and so on
  --requests <n>          requests to each route (default 50, which is what A9 asks for)
  --body-bytes <n>        size of each comment, message and small upload, 1024 to 20480
                          (default 8192); an ask takes the smaller of this and its own
                          2,000-character cap
  --artifact <name>       the existing document the small upload writes a version of
                          (default: the issue's first document that is not its primary one)
  --large-artifact <name> the 100-660 KB document the large upload writes a version of
                          (default: the issue's primary document)
  --summarise <file>      print each route's medians by run instead of measuring

DISPATCH_USER names the caller (default "bench"); the server must take it as a human identity.
USAGE
  exit 2
}

while (($#)); do
  case "$1" in
    --url) url="${2:?--url needs a value}"; shift 2 ;;
    --issue) issue="${2:?--issue needs a value}"; shift 2 ;;
    --label) label="${2:?--label needs a value}"; shift 2 ;;
    --requests) requests="${2:?--requests needs a value}"; shift 2 ;;
    --body-bytes) body_bytes="${2:?--body-bytes needs a value}"; shift 2 ;;
    --artifact) artifact="${2:?--artifact needs a value}"; shift 2 ;;
    --large-artifact) large_artifact="${2:?--large-artifact needs a value}"; shift 2 ;;
    --summarise) summarise="${2:?--summarise needs a value}"; shift 2 ;;
    -h | --help) usage ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage ;;
  esac
done

for tool in curl jq awk; do
  command -v "$tool" >/dev/null || { printf '%s is required\n' "$tool" >&2; exit 1; }
done

# summarise reads the lines every measuring run appended and reports each route's medians: every
# run of that route, the highest base run, and each head run against it. The highest base run is
# what a rise is judged against, which is why at least three pairs are asked for: one base run is
# a machine's mood, three show the spread.
if [[ -n "$summarise" ]]; then
  [[ -f "$summarise" ]] || { printf 'no such file: %s\n' "$summarise" >&2; exit 1; }
  awk -F'\t' '
    # A line that is no measurement is skipped: a caller who kept the runs and the warnings of
    # this script in one file would otherwise be told about a route named by a warning.
    NF < 4 { next }
    {
      route = $2
      if (!(route in seen)) { order[++routes] = route; seen[route] = 1 }
      runs[route] = runs[route] sprintf("  %-10s %8.1f ms\n", $1, $3)
      if ($1 ~ /^base/) { bases[route]++; if ($3 > high[route]) high[route] = $3 }
      if ($1 ~ /^head/) { heads[route]++; headruns[route] = headruns[route] $1 " " $3 "\n" }
    }
    END {
      for (i = 1; i <= routes; i++) {
        route = order[i]
        printf "%s\n%s", route, runs[route]
        if (bases[route] == 0) { printf "  no base run\n\n"; continue }
        printf "  highest base run %.1f ms over %d base runs\n", high[route], bases[route]
        count = split(headruns[route], lines, "\n")
        for (j = 1; j <= count; j++) {
          if (lines[j] == "") continue
          split(lines[j], field, " ")
          printf "  %-10s %8.1f ms, %+.1f%% against the highest base run\n",
            field[1], field[2], (field[2] / high[route] - 1) * 100
        }
        if (bases[route] < 3 || heads[route] < 3) {
          printf "  %d base runs and %d head runs: the plan asks for at least three pairs\n",
            bases[route], heads[route]
        }
        printf "\n"
      }
    }
  ' "$summarise"
  exit 0
fi

[[ -n "$url" && -n "$issue" ]] || usage
url="${url%/}"
[[ -n "$label" ]] || label="run"
# A9's bodies are 1 to 20 KB. A size outside that band measures another case, and one over a
# route's own cap is refused by the server, so the band is checked here rather than found out at
# the first request.
((body_bytes >= 1024 && body_bytes <= 20480)) || {
  printf -- '--body-bytes is %s; A9 measures a body of 1 to 20 KB (1024 to 20480)\n' "$body_bytes" >&2
  exit 2
}
ask_bytes=$((body_bytes < 2000 ? body_bytes : 2000))

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

request() {
  curl -sS -H "X-Dispatch-User: ${user}" -H 'Content-Type: application/json' "$@"
}

# The issue's documents, read once: the upload routes write a version of a document that already
# exists, so both names have to be names the issue carries.
documents="$(request "${url}/api/v1/issues/${issue}/artifacts")"
if [[ -z "$large_artifact" ]]; then
  large_artifact="$(jq -r 'map(select(.kind == "doc" and .primary)) | .[0].name // empty' <<<"$documents")"
fi
[[ -n "$large_artifact" ]] || {
  printf '%s has no primary document; name one with --large-artifact\n' "$issue" >&2
  exit 1
}
if [[ -z "$artifact" ]]; then
  artifact="$(jq -r --arg large "$large_artifact" \
    'map(select(.kind == "doc" and .name != $large)) | .[0].name // empty' <<<"$documents")"
fi
[[ -n "$artifact" ]] || {
  printf '%s carries no document besides %s; name one with --artifact, since the small upload must not write over the large one\n' \
    "$issue" "$large_artifact" >&2
  exit 1
}
large_slug="$(jq -r --arg name "$large_artifact" \
  'map(select(.name == $name)) | .[0].slug // empty' <<<"$documents")"
[[ -n "$large_slug" ]] || { printf '%s carries no document named %s\n' "$issue" "$large_artifact" >&2; exit 1; }

# The large upload writes back the document's own current markdown with one line added, so every
# one of its requests is an upload over a latest version in the 100-660 KB band. A small body
# would shrink the document on the first request and the other 49 would measure a small document,
# which is not the case the two new triggers cost most on: each copies that document's vector.
request "${url}/api/v1/issues/${issue}/artifacts/${large_slug}/text" \
  | jq -r '.markdown' >"${workdir}/large.md"
large_bytes="$(wc -c <"${workdir}/large.md")"
if ((large_bytes < 100000)); then
  printf 'warning: %s is %d bytes, under the 100 KB the large-upload case is about\n' \
    "$large_artifact" "$large_bytes" >&2
fi

# body writes a body of the asked-for size out of a fixed word list, so each write carries real
# words for the search vectors and every request of a route carries the same work. The round
# number leads it, so no two writes are the same text and a version is always written.
body() {
  awk -v bytes="${2:-$body_bytes}" -v round="$1" 'BEGIN {
    split("alidade astrolabe quadrant rete plateau survey azimuth meridian ecliptic limb " \
          "gnomon vernier sextant horizon parallax zenith nadir ephemeris theodolite transit", words, " ")
    text = "Bench round " round ": "
    index_ = 0
    while (length(text) < bytes) { index_++; text = text words[(index_ % 20) + 1] " " }
    printf "%s", substr(text, 1, bytes)
  }'
}

# time_request sends one request and answers its wall time in milliseconds. A request the server
# refuses stops the run: a 4xx answered in a millisecond would be the fastest reading there is.
time_request() {
  local method="$1" path="$2" payload="$3" answer code seconds
  answer="$(request -o /dev/null -w '%{http_code} %{time_total}' \
    -X "$method" --data-binary "@${payload}" "${url}${path}")"
  read -r code seconds <<<"$answer"
  [[ "$code" == 2* ]] || {
    printf '%s %s answered %s\n' "$method" "$path" "$code" >&2
    exit 1
  }
  awk -v seconds="$seconds" 'BEGIN { printf "%.3f", seconds * 1000 }'
}

# measure runs one route the asked-for number of times and prints its line: the label, the route,
# the median of the readings and every reading. The median of an even count is its upper middle,
# as the Go harness takes the middle of its eleven.
measure() {
  local route="$1" method="$2" path="$3" kind="$4" readings=() round median
  for ((round = 1; round <= requests; round++)); do
    payload "$kind" "$round" >"${workdir}/payload.json"
    readings+=("$(time_request "$method" "$path" "${workdir}/payload.json")")
  done
  median="$(printf '%s\n' "${readings[@]}" | sort -g | awk -v count="${#readings[@]}" \
    'NR == int(count / 2) + 1 { printf "%.3f", $1 }')"
  printf '%s\t%s\t%s\t%s\n' "$label" "$route" "$median" "$(IFS=,; printf '%s' "${readings[*]}")"
}

# payload writes one request's JSON body for a route. An ask's question is capped at
# contracts.AskQuestionMax, 2,000 characters, so the ask route is measured at that cap rather
# than at --body-bytes: a larger question is 400 CAP_EXCEEDED.
#
# The large upload appends one paragraph to the document's own markdown each round, so no two
# uploads are the same text and a version is written every time, while the document stays the
# size it was. The line is plain prose: an upload is parsed as a Proof document, which accepts
# no HTML comment.
payload() {
  local kind="$1" round="$2"
  case "$kind" in
    comment) jq -nc --arg body "$(body "$round")" '{body: $body}' ;;
    ask) jq -nc --arg question "$(body "$round" "$ask_bytes")" '{question: $question}' ;;
    message) jq -nc --arg body "$(body "$round")" '{body: $body}' ;;
    upload) jq -nc --arg name "$artifact" --arg content "$(body "$round")" '{name: $name, content: $content}' ;;
    title) jq -nc --arg title "Bench round ${round} survey" '{title: $title}' ;;
    large-upload)
      printf '\nBench round %s of the survey.\n' "$round" >>"${workdir}/large.md"
      jq -nc --arg name "$large_artifact" --rawfile content "${workdir}/large.md" \
        '{name: $name, content: $content}'
      ;;
    *) printf 'no such body: %s\n' "$kind" >&2; exit 1 ;;
  esac
}

printf 'route timings: %s requests each against %s, issue %s, body %s bytes, documents %s and %s (%s bytes)\n' \
  "$requests" "$url" "$issue" "$body_bytes" "$artifact" "$large_artifact" "$large_bytes" >&2

measure "POST /issues/{key}/comments" POST "/api/v1/issues/${issue}/comments" comment
measure "POST /issues/{key}/asks" POST "/api/v1/issues/${issue}/asks" ask
measure "POST /issues/{key}/messages" POST "/api/v1/issues/${issue}/messages" message
measure "POST /issues/{key}/artifacts" POST "/api/v1/issues/${issue}/artifacts" upload
measure "POST /issues/{key}/artifacts, large document" POST "/api/v1/issues/${issue}/artifacts" large-upload
measure "PATCH /issues/{key}" PATCH "/api/v1/issues/${issue}" title
