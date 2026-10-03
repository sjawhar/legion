#!/usr/bin/env bash
# Tests for `check-private-names.sh`: every spelling it refuses, the identifiers and the English word
# it must pass, what it skips, and both of its modes (the repository, and named paths such as the
# built site).
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
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }

repo="agent""-c"
repo_snake="agent""_c"
repo_joined="agent""c"
key="AGENT""C"
company="trajectory"
company_proper="T""rajectory"
labs="labs"

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
check "says so" "$(contains "$out" 'no text file names the private deployment repository or the company')"

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
  "$company-$labs-pbc/repo"
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

echo "case: identifiers that merely start with those letters, and the English word, pass"
root=$(fixture lookalikes)
cat > "$root/docs/notes.md" <<'TEXT'
agentCursor AgentConversationPage subagentContext agentCount agentCalls agentCard
leaveAgentComposer AGENT_COMPOSER_SELECTOR agent-composer agent-cursor agent-card agent-credentials
The models may take a different trajectory in tool calling; trajectory, plotted. TrajectoryPlot.
TEXT
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
check "counts the lines" "$(contains "$out" '1 line(s) name the private deployment repository or the company')"

echo "case: a line naming both is reported once"
root=$(fixture both)
printf '%s, by %s Labs\n' "$repo" "$company_proper" > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "one line counted" "$(contains "$out" '1 line(s) name')"

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

echo "case: a named path that does not exist"
root=$(fixture missing)
run_check "$root" dist
check "exits 2" "$(is "$status" 2)"
check "names it" "$(contains "$out" 'dist does not exist')"

summary "check-private-names.sh"
