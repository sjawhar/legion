#!/usr/bin/env bash
# This repository is public. The private repository Legion is deployed from, its private internal
# hosts, and the company that runs it are named nowhere in docs, skills, role prompts, CHANGELOGs,
# code comments, tests, fixtures or file names, since the published site renders text from all of
# them. This check fails on any text line or file path naming one:
#
#   1. the deployment repository — "agent", then "c", with nothing, a "-" or "_", a space, a
#      typographic dash or a percent-encoded byte between them — in any case, standing as a token
#      of its own: no letter or digit on either side, and a percent-encoded byte before it counts
#      as a separator too. That is the repository's name, its Dispatch project key and every issue
#      key under it, and the name inside a session id between underscores, but not a camelCase or
#      snake_case identifier that merely starts with those letters (agentCursor, AgentConversation,
#      subagentContext, AGENT_COMPOSER_SELECTOR, agent-composer);
#   2. the company — its two words joined or split by one "-", "_", " " or ".", in any case (its
#      hostnames included), and its first word capitalized, as a whole word, which is the
#      company's proper noun wherever it stands, at the start of a sentence or a heading too. The
#      lowercase English word names nothing and passes;
#   3. a private host below `.internal.`, in any case. A reserved example host (`<name>` then
#      `.internal.example`, optionally `.com`, `.net` or `.org`) passes, and so does the NATS
#      client's own `this` property of that name in the one bundle that vendors it
#      (packages/claude-envoy/dist/envoy-channel.js), and nowhere else.
#
# With no arguments it scans the repository: every file git tracks, a file under an ignored path
# included, plus every untracked file git does not ignore, so a new file is caught before it is
# added. With arguments it scans each named directory or file instead, tracked or not; docs.yaml
# passes it the built site (docs/site/dist), whose generated reference pages and copied media
# captions exist only after the build, and check-pr-text.sh a pull request's title and body. The
# first two rules also read every file's path, directories included. Binary files are skipped, and
# so are lockfiles (bun.lock, go.sum, go.work.sum), whose integrity hashes are random text. Every
# hit is printed as its file and line, never its text, and a path component a rule matches is
# printed as <name>, so the log does not repeat the name; the only non-zero exits are named hits
# (1) and a target or search that failed (2).
#
# The rules below are spelled so that no line of this file matches them, which is why the check
# needs no exception for itself.
#
# Run from anywhere: .github/scripts/check-private-names.sh [path...]
# CI runs it over the repository in the lint job of pr-and-main.yaml, over the built site in
# docs.yaml, over a pull request's title and body in pr-title.yaml (check-pr-text.sh), and its tests
# in pr-and-main.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

encoded_byte='%[[:xdigit:]]{2}'
dashes='‐|‑|‒|–|—|―|−|﹘|﹣|－'
repository_rule="(^|[^[:alnum:]]|$encoded_byte)agent([-_ ]|$encoded_byte|$dashes)?c([^[:alnum:]]|\$)"
company_rule='t''rajectory[-_ .]?labs'
proper_noun_rule='(^|[^[:alnum:]_])T''rajectory([^[:alnum:]_]|$)'
private_host_rule='[[:alnum:]-]+[.]internal[.][[:alnum:].-]+'
# Whole matches of the host rule that name no private host: a reserved example host (a trailing
# full stop is the sentence's), and the NATS client's property in the bundle that vendors it.
allowed_host='[[:alnum:]-]+[.]internal[.]example([.](com|net|org))?[.]*'
vendored_nats='packages/claude-envoy/dist/envoy-channel[.]js'
vendored_property='this[.]internal[.][[:alnum:]_]+'

repository_label='names the private deployment repository or its Dispatch project key'
company_label='names the company'
private_host_label='names a private internal host'

lockfiles=(bun.lock go.sum go.work.sum)
targets=("$@")

for target in "${targets[@]}"; do
  if [ ! -e "$target" ]; then
    echo "check-private-names: $target does not exist, so there is nothing to scan there" >&2
    exit 2
  fi
done

matches=$(mktemp)
hits=$(mktemp)
paths=$(mktemp)
trap 'rm -f "$matches" "$hits" "$paths"' EXIT

if [ ${#targets[@]} -eq 0 ]; then
  git ls-files --cached --others --exclude-standard | sort -u > "$paths"
  scanned="$(wc -l < "$paths") files in the repository"
else
  find "${targets[@]}" > "$paths"
  scanned="$(find "${targets[@]}" -type f | wc -l) files under ${targets[*]}"
fi

# names <text>: whether one of the first two rules matches text.
names() {
  local found=1
  shopt -s nocasematch
  if [[ $1 =~ $repository_rule || $1 =~ $company_rule ]]; then found=0; fi
  shopt -u nocasematch
  [ "$found" -eq 0 ] || [[ $1 =~ $proper_noun_rule ]]
}

# mask <path>: path with each component the first two rules match replaced by <name>.
mask() {
  local IFS=/ part
  local -a parts masked=()
  read -r -a parts <<< "$1"
  for part in "${parts[@]}"; do
    if names "$part"; then masked+=("<name>"); else masked+=("$part"); fi
  done
  printf '%s' "${masked[*]}"
}

# run <command...>: appends the command's output to $matches; a failed search (exit 2 or more,
# where grep's 1 means no match) stops the check.
run() {
  local status=0
  "$@" >> "$matches" || status=$?
  if [ "$status" -gt 1 ]; then
    echo "check-private-names: the search with $1 failed (exit $status)" >&2
    exit 2
  fi
}

# search <label> <rule> [grep flag...]: appends one "path:line: label" row per line where the rule
# matches something other than an allowed host.
search() {
  local label=$1 rule=$2 path line
  shift 2
  : > "$matches"
  if [ ${#targets[@]} -eq 0 ]; then
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=(":(exclude,glob)**/$lockfile"); done
    # --untracked adds the files git has not been told about, but skips a tracked file under an
    # ignored path, which the plain pass reads.
    run git grep -n -o -I --untracked -E "$@" -e "$rule" -- . "${excludes[@]}"
    run git grep -n -o -I -E "$@" -e "$rule" -- . "${excludes[@]}"
  else
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=("--exclude=$lockfile"); done
    run grep -r -n -o -H -I -E "${excludes[@]}" "$@" -e "$rule" -- "${targets[@]}"
  fi
  { grep -v -i -E -e "^[^:]*:[0-9]+:($allowed_host)\$" \
    -e "^$vendored_nats:[0-9]+:$vendored_property\$" "$matches" || true; } |
    cut -d: -f1,2 | while IFS=: read -r path line; do
      printf '%s:%s: %s\n' "$(mask "$path")" "$line" "$label"
    done >> "$hits"
}

# search_paths <label> <rule> [grep flag...]: appends one "path:0: label in its path" row per file
# or directory whose path the rule matches.
search_paths() {
  local label=$1 rule=$2 path
  shift 2
  : > "$matches"
  run grep -E "$@" -e "$rule" "$paths"
  while IFS= read -r path; do
    printf '%s:0: %s in its path\n' "$(mask "$path")" "$label"
  done < "$matches" >> "$hits"
}

search "$repository_label" "$repository_rule" -i
search "$company_label" "$company_rule" -i
search "$company_label" "$proper_noun_rule"
search "$private_host_label" "$private_host_rule" -i
search_paths "$repository_label" "$repository_rule" -i
search_paths "$company_label" "$company_rule" -i
search_paths "$company_label" "$proper_noun_rule"

if [ -s "$hits" ]; then
  sort -t: -k1,1 -k2,2n -u -o "$hits" "$hits"
  while IFS= read -r hit; do
    path=${hit%%:*}
    rest=${hit#*:}
    line=${rest%%:*}
    if [ "$line" = 0 ]; then
      echo "::error::$path: ${rest#*: }"
    else
      echo "::error file=$path,line=$line::$hit"
    fi
  done < "$hits"
  echo "check-private-names: $(wc -l < "$hits") finding(s) ($scanned). This repository is public, so:"
  echo "  - for the deployment repository write \"the deployment repository\" or <deployment repo>, and"
  echo "    use a neutral project key such as ACME in a fixture or example;"
  echo "  - for the company write \"the company\". Its capitalized first word is the company's name"
  echo "    wherever it stands, at the start of a sentence or a heading too, so reword that sentence;"
  echo "  - for a host write a reserved example host such as <name>.internal.example;"
  echo "  - for a file or directory whose path is reported with <name> in it, rename it."
  exit 1
fi

echo "check-private-names: no text file or path names the private deployment repository, the" \
  "company, or a private internal host ($scanned)"
