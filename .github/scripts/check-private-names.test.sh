#!/usr/bin/env bash
# Tests for `check-private-names.sh` and `check-pr-text.sh`: every spelling the guard refuses, the
# identifiers and the English word it must pass, what it skips, its two modes (the repository, and
# named paths such as the built site), file names, and a pull request's title and body.
#
# Each case builds a miniature git repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs: it resolves its root from its own
# path, which the symlink puts at the fixture root. The names it hunts are assembled from pieces
# below, so this file holds none of them and the check scans it like any other.
#
# Run from anywhere: .github/scripts/check-private-names.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-private-names.sh"
pr_text_script="$script_dir/check-pr-text.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }

repo="agent""-c"
repo_snake="agent""_c"
repo_joined="agent""c"
repo_spaced="agent"" c"
repo_en_dash="agent""–c"
repo_nonbreaking_hyphen="agent""‑c"
repo_percent_hyphen="agent""%2Dc"
key="AGENT""C"
company="traj""ectory"
company_proper="T""rajectory"
labs="labs"
internal="internal"
private_host_suffix="private.example"
example_host_suffix="example"
vendored_nats="packages/claude-envoy/dist/envoy-channel.js"

# fixture <name>: a clean repository with one tracked file. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/docs"
  ln -s "$check_script" "$root/.github/scripts/check-private-names.sh"
  printf '# Notes\n\nNothing private here.\n' > "$root/docs/notes.md"
  git -C "$root" init -q
  git -C "$root" add docs/notes.md
  echo "$root"
}

# run_check <root> [path...]: sets `out` to the combined output and `status` to the exit code.
run_check() {
  local root=$1
  shift
  set +e
  out=$(cd "$root" && .github/scripts/check-private-names.sh "$@" 2>&1)
  status=$?
  set -e
}

echo "case: a clean repository"
root=$(fixture clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: every spelling of the deployment repository and its project key is refused"
mentions=(
  "the $repo repository"
  "see $repo's README"
  "${repo}#20006"
  "$repo_snake as a constant"
  "the $repo_joined checkout"
  "Agent""-C, capitalized"
  "issue $key-393"
  "dispatch://$key-1/ask/1"
  "project: \"$key\""
  "LEGION_PROJECT=$repo_joined"
  "ses_${repo_joined}_controller_title"
  "${key}_TOKEN"
  "the $repo_spaced repository"
  "the $repo_en_dash repository"
  "the $repo_nonbreaking_hyphen repository"
  "the $repo_percent_hyphen repository"
  "https://github.com/search?q=repo%3Aexample-org%2F$repo"
  "?q=the%20$repo_joined%20repo"
)
for index in "${!mentions[@]}"; do
  root=$(fixture "repo-$index")
  printf 'line one\n%s\n' "${mentions[index]}" > "$root/docs/notes.md"
  run_check "$root"
  check "'${mentions[index]}' fails" "$(is "$status" 1)"
  check "'${mentions[index]}' is named at its file and line" \
    "$(contains "$out" 'docs/notes.md:2: names the private deployment repository')"
done

echo "case: every spelling of the company is refused"
mentions=(
  "$company_proper Labs"
  "$company-$labs-example/repo"
  "${company}_$labs"
  "https://dispatch.internal.$company$labs.com/"
  "$company.$labs"
  "${company_proper}${labs^}"
  "the $company_proper team"
  "($company_proper)"
)
for index in "${!mentions[@]}"; do
  root=$(fixture "company-$index")
  printf '%s\n' "${mentions[index]}" > "$root/docs/notes.md"
  run_check "$root"
  check "'${mentions[index]}' fails" "$(is "$status" 1)"
  check "'${mentions[index]}' is named at its file and line" \
    "$(contains "$out" 'docs/notes.md:1: names the company')"
done

echo "case: a private internal host is refused, whatever its case"
hosts=(
  "https://listener.$internal.$private_host_suffix"
  "https://listener.${internal^^}.$private_host_suffix"
  "https://listener.${internal^}.$private_host_suffix"
  "https://listener.$internal.${example_host_suffix}corp.net"
  "https://listener.$internal.$example_host_suffix.$private_host_suffix"
  "https://a.$internal.$example_host_suffix and https://b.${internal^^}.$private_host_suffix"
  "this.$internal.push(value)"
)
for index in "${!hosts[@]}"; do
  root=$(fixture "host-$index")
  printf '%s\n' "${hosts[index]}" > "$root/docs/notes.md"
  run_check "$root"
  check "'${hosts[index]}' fails" "$(is "$status" 1)"
  check "'${hosts[index]}' is named at its file and line" \
    "$(contains "$out" 'docs/notes.md:1: names a private internal host')"
done

echo "case: a reserved example host passes, and the vendored NATS client's property in its own file"
root=$(fixture public-host)
printf 'https://listener.%s.%s\n' "$internal" "$example_host_suffix" > "$root/docs/notes.md"
printf 'nats://nats.%s.%s.com:4222\n' "$internal" "$example_host_suffix" >> "$root/docs/notes.md"
printf 'Point it at api.%s.%s.\n' "$internal" "$example_host_suffix" >> "$root/docs/notes.md"
mkdir -p "$root/$(dirname "$vendored_nats")"
printf '        this.%s.push(sv);\n' "$internal" > "$root/$vendored_nats"
git -C "$root" add "$vendored_nats"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: identifiers that merely start with those letters, and the English word, pass"
root=$(fixture lookalikes)
{
  printf 'agentCursor AgentConversationPage subagentContext agentCount agentCalls agentCard\n'
  printf 'leaveAgentComposer AGENT_COMPOSER_SELECTOR agent-composer agent-cursor agent-card agent-credentials\n'
  printf 'agent cards, an agent class, the agents C and D\n'
  printf 'The models may take a different %s in tool calling; %s, plotted. %sPlot.\n' \
    "$company" "$company" "$company_proper"
} > "$root/docs/notes.md"
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: the log names the file and line, never the text"
root=$(fixture quiet)
printf 'the %s repository\n' "$repo" > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "annotates the file and line for GitHub" \
  "$(contains "$out" '^::error file=docs/notes.md,line=1::docs/notes.md:1:')"
check "does not repeat the name" "$(is "$(contains "$out" "$repo")" false)"
check "counts the findings" "$(contains "$out" '^check-private-names: 1 finding(s)')"

echo "case: a line naming both is reported once"
root=$(fixture both)
printf '%s, by %s Labs\n' "$repo" "$company_proper" > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "one finding counted" "$(contains "$out" '^check-private-names: 1 finding(s)')"

echo "case: a file git has not been told about still counts; an ignored one does not"
root=$(fixture untracked)
printf '%s\n' "$key-7" > "$root/docs/new.md"
run_check "$root"
check "an untracked file fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'docs/new.md:1:')"

root=$(fixture ignored)
printf 'dist/\n' > "$root/.gitignore"
git -C "$root" add .gitignore
mkdir -p "$root/dist"
printf '%s\n' "$key-7" > "$root/dist/page.html"
run_check "$root"
check "an ignored file passes the repository scan" "$(is "$status" 0)"

echo "case: a file git tracks under an ignored path still counts"
root=$(fixture tracked-ignored)
printf 'notes/\n' > "$root/.gitignore"
git -C "$root" add .gitignore
mkdir -p "$root/notes"
printf '%s\n' "$key-7" > "$root/notes/report.md"
git -C "$root" add -f notes/report.md
run_check "$root"
check "a tracked file under an ignored path fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'notes/report.md:1:')"

echo "case: a name in a file's path is refused, and the log does not print it"
root=$(fixture path-name)
mkdir -p "$root/docs/notes"
printf 'clean text\n' > "$root/docs/notes/$key-7-runbook.md"
git -C "$root" add docs/notes
run_check "$root"
check "a tracked path naming the project key fails" "$(is "$status" 1)"
check "names the directory it is in" "$(contains "$out" 'docs/notes/<name>')"
check "does not print the name" "$(is "$(contains "$out" "$key")" false)"

root=$(fixture path-name-dir)
mkdir -p "$root/docs/${company_proper}"
printf 'clean text\n' > "$root/docs/${company_proper}/notes.md"
git -C "$root" add docs
run_check "$root"
check "a directory naming the company fails" "$(is "$status" 1)"
check "does not print the directory's name" "$(is "$(contains "$out" "$company_proper")" false)"

echo "case: lockfiles and binary files are skipped"
root=$(fixture skipped)
mkdir -p "$root/packages/x"
printf '"%s": "sha512-%s"\n' "$repo" "$key" > "$root/bun.lock"
printf 'example.com/%s v1 h1:x\n' "$repo" > "$root/packages/x/go.sum"
printf '%s\0binary\n' "$repo" > "$root/docs/image.bin"
git -C "$root" add bun.lock packages/x/go.sum docs/image.bin
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: named paths are scanned instead of the repository, ignored or not"
root=$(fixture paths)
printf 'dist/\n' > "$root/.gitignore"
mkdir -p "$root/dist/reference"
printf '<p>clean</p>\n' > "$root/dist/index.html"
run_check "$root" dist
check "a clean built tree passes" "$(is "$status" 0)"
check "says what it scanned" "$(contains "$out" '(1 files under dist)')"

printf '<pre>{"project": "%s"}</pre>\n' "$key" > "$root/dist/reference/tools.html"
printf 'WEBVTT\n\n00:00.000 --> 00:01.000\n%s Labs\n' "$company_proper" > "$root/dist/captions.vtt"
run_check "$root" dist
check "a built page and a caption naming them fail" "$(is "$status" 1)"
check "names the page" "$(contains "$out" 'dist/reference/tools.html:1: names the private deployment repository')"
check "names the caption" "$(contains "$out" 'dist/captions.vtt:4: names the company')"

printf '%s\n' "$key-7" > "$root/docs/notes.md"
rm "$root/dist/reference/tools.html" "$root/dist/captions.vtt"
run_check "$root" dist
check "a named path is all that is scanned" "$(is "$status" 0)"

echo "case: a single named file is reported under its own name"
root=$(fixture single-file)
printf 'line one\n%s\n' "$key-7" > "$root/body.md"
run_check "$root" body.md
check "fails" "$(is "$status" 1)"
check "names the file and line" "$(contains "$out" '^::error file=body.md,line=2::body.md:2: names the private deployment repository')"

echo "case: a named path that does not exist"
root=$(fixture missing)
run_check "$root" dist
check "exits 2" "$(is "$status" 2)"
check "names it" "$(contains "$out" 'dist does not exist')"

# run_pr_text <title> <body>: runs check-pr-text.sh as pr-title.yaml does, setting `out` and `status`.
run_pr_text() {
  set +e
  out=$(PR_TITLE=$1 PR_BODY=$2 "$pr_text_script" 2>&1)
  status=$?
  set -e
}

echo "case: a pull request's title and body, which become main's squash commit and release notes"
run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7"
check "a clean title and body pass" "$(is "$status" 0)"

run_pr_text "fix(dispatch): keep the inbox order ($key-7)" "Refs LEGION-7"
check "a title naming a deployment issue key fails" "$(is "$status" 1)"
check "names the title" "$(contains "$out" 'title:1: names the private deployment repository')"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

run_pr_text "fix(dispatch): keep the inbox order" "$(printf 'Line one.\nAs %s Labs found.\n' "$company_proper")"
check "a body naming the company fails" "$(is "$status" 1)"
check "names the body's line" "$(contains "$out" 'body:2: names the company')"

run_pr_text "fix(dispatch): keep the inbox order" ""
check "an empty body passes" "$(is "$status" 0)"

summary "check-private-names.sh"
