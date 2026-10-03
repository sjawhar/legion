#!/usr/bin/env bash
# This repository is public. The private repository Legion is deployed from, its private internal
# hosts, and the company that runs it are named nowhere in docs, skills, role prompts, CHANGELOGs,
# code comments, tests or fixtures, since the published site renders text from all of them. This
# check fails on any text file naming one.
#
#   1. the deployment repository — "agent", an optional "-" or "_", then "c" — in any case, standing
#      as a token of its own: no letter or digit on either side. That is the repository's name, its
#      Dispatch project key and every issue key under it, and the name inside a session id between
#      underscores, but not a camelCase or snake_case identifier that merely starts with those
#      letters (agentCursor, AgentConversation, subagentContext, AGENT_COMPOSER_SELECTOR);
#   2. the company — "trajectory" and "labs", joined or split by one "-", "_", " " or ".", in any
#      case (its hostnames included), and the first of those words capitalized, as a whole word,
#      which is the company's proper noun. The lowercase English word names nothing and passes;
#   3. a private host below `.internal.`, except a reserved example host below `.internal.example`.
#
# With no arguments it scans the repository: every file git tracks plus every untracked file git
# does not ignore, so a new file is caught before it is added. With arguments it scans each named
# directory or file instead, tracked or not; docs.yaml passes it the built site (docs/site/dist),
# whose generated reference pages and copied media captions exist only after the build. Binary files
# are skipped, and so are lockfiles (bun.lock, go.sum, go.work.sum), whose integrity hashes are
# random text. Every hit is printed as its file and line, never its text, so the log does not
# repeat the name; the only non-zero exits are named hits (1) and a target or search that failed (2).
#
# The rules below are spelled so that no line of this file matches them, which is why the check
# needs no exception for itself.
#
# Run from anywhere: .github/scripts/check-private-names.sh [path...]
# CI runs it over the repository in the lint job of pr-and-main.yaml, over the built site in
# docs.yaml, and its tests in pr-and-main.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

repository_rule='(^|[^[:alnum:]])agent[-_]?c([^[:alnum:]]|$)'
company_rule='trajectory[-_ .]?labs'
proper_noun_rule='(^|[^[:alnum:]_])[T]rajectory([^[:alnum:]_]|$)'
private_host_rule='[[:alnum:]-]+[.]internal[.][[:alnum:].-]+'
example_host_rule='[[:alnum:]-]+[.]internal[.]example([.][[:alnum:]-]+)*'
javascript_property_rule='this[.]internal[.][[:alnum:]_]+'

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
trap 'rm -f "$matches" "$hits"' EXIT

# search <label> <rule> [grep flag...]: appends one "path:line: label" row per matching line.
search() {
  local label=$1 rule=$2
  shift 2
  local status=0
  if [ ${#targets[@]} -eq 0 ]; then
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=(":(exclude,glob)**/$lockfile"); done
    git grep -n -I --untracked -E "$@" -e "$rule" -- . "${excludes[@]}" > "$matches" || status=$?
  else
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=("--exclude=$lockfile"); done
    grep -r -n -H -I -E "${excludes[@]}" "$@" -e "$rule" -- "${targets[@]}" > "$matches" || status=$?
  fi
  if [ "$status" -gt 1 ]; then
    echo "check-private-names: the search for what $label failed (exit $status)" >&2
    exit 2
  fi
  cut -d: -f1,2 "$matches" | sed "s|\$|: $label|" >> "$hits"
}

# search_private_host finds internal hostnames while permitting reserved example hosts and the
# generated JavaScript `this.internal` property. A line with both an allowed value and a private
# host remains a hit: only each whole allowed match is removed before checking what remains.
search_private_host() {
  local status=0 line remaining path rest
  if [ ${#targets[@]} -eq 0 ]; then
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=(":(exclude,glob)**/$lockfile"); done
    git grep -n -I --untracked -E -i -e "$private_host_rule" -- . "${excludes[@]}" > "$matches" || status=$?
  else
    local excludes=()
    for lockfile in "${lockfiles[@]}"; do excludes+=("--exclude=$lockfile"); done
    grep -r -n -H -I -E -i "${excludes[@]}" -e "$private_host_rule" -- "${targets[@]}" > "$matches" || status=$?
  fi
  if [ "$status" -gt 1 ]; then
    echo "check-private-names: the search for private internal hosts failed (exit $status)" >&2
    exit 2
  fi
  while IFS= read -r line; do
    remaining=$line
    while [[ "$remaining" =~ $example_host_rule ]]; do
      remaining=${remaining/"${BASH_REMATCH[0]}"/}
    done
    while [[ "$remaining" =~ $javascript_property_rule ]]; do
      remaining=${remaining/"${BASH_REMATCH[0]}"/}
    done
    if [[ "$remaining" =~ $private_host_rule ]]; then
      path=${line%%:*}
      rest=${line#*:}
      printf '%s:%s: %s\n' "$path" "${rest%%:*}" "$private_host_label" >> "$hits"
    fi
  done < "$matches"
}

search "$repository_label" "$repository_rule" -i
search "$company_label" "$company_rule" -i
search "$company_label" "$proper_noun_rule"
search_private_host

if [ ${#targets[@]} -eq 0 ]; then
  scanned="$(git ls-files --cached --others --exclude-standard | sort -u | wc -l) files in the repository"
else
  scanned="$(find "${targets[@]}" -type f | wc -l) files under ${targets[*]}"
fi

if [ -s "$hits" ]; then
  sort -t: -k1,1 -k2,2n -u -o "$hits" "$hits"
  while IFS= read -r hit; do
    path=${hit%%:*}
    rest=${hit#*:}
    echo "::error file=$path,line=${rest%%:*}::$hit"
  done < "$hits"
  echo "check-private-names: $(wc -l < "$hits") line(s) name the private deployment repository," \
    "company, or an internal host ($scanned). This repository is public: write \"the deployment" \
    "repository\" or a placeholder such as <deployment repo>, a neutral project key such as ACME," \
    "and a reserved example host in a fixture or example."
  exit 1
fi

echo "check-private-names: no text file names the private deployment repository, company, or" \
  "internal host ($scanned)"
