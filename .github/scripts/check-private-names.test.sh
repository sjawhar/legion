#!/usr/bin/env bash
# Tests for `check-private-names.sh` and `check-pr-text.sh`: every spelling the guard refuses, the
# identifiers and the English word it must pass, what it skips, its two modes (the repository, and
# named paths such as the built site), file names, and a pull request's title, body, branch name,
# commits (message, author and committer), issue comments, reviews and review comments.
#
# Each case builds a miniature git repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs: it resolves its root from its own
# path, which the symlink puts at the fixture root. The names it hunts are assembled from pieces
# below, so this file holds none of them and the check scans it like any other. Every case names
# itself by an index or a neutral label, never the assembled form, since `check`'s PASS/FAIL line
# is this script's own stdout — the CI log; the last case runs the guard over that captured log.
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

# Every case's stdout below (echoed "case:" lines and `check`'s PASS/FAIL lines) is this script's
# own output — the Tests job's log — so it is also teed to self_log and, as the last case, scanned
# with the guard itself: a future case that prints an assembled form turns red here, in the suite,
# before it ever reaches a real job log. `exec 3>&1 4>&2` keeps the real terminal fds reachable so
# the redirection can be undone before reading the file: a process substitution's write end must
# close (and its process be waited on) before its output is guaranteed to be flushed to disk.
self_log="$work/suite-output.log"
exec 3>&1 4>&2
exec > >(tee "$self_log") 2>&1
tee_pid=$!

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
nbsp=$'\xc2\xa0'
en_dash=$'\xe2\x80\x93'
key="AGENT""C"
company="traj""ectory"
company_proper="T""rajectory"
labs="labs"
internal="internal"
private_host_suffix="private.example"
example_host_suffix="example"
vendored_nats="packages/claude-envoy/dist/envoy-channel.js"
tailnet_suffix="ts.net"
tailnet_label="realtailnet"
tailnet_example="example-tailnet"

# assert_each_refused <fixture-prefix> <label> <array-name> [line]: writes each line of the named
# array into its own fixture, on the given line (1 by default; 2 is preceded by a filler line), and
# checks the guard refuses it with that label there. Each case is named by its index, never the
# assembled form.
assert_each_refused() {
  local prefix=$1 label=$2 index
  local -n lines=$3
  local line=${4:-1}
  for index in "${!lines[@]}"; do
    root=$(fixture "$prefix-$index")
    if [ "$line" = 2 ]; then
      printf 'line one\n%s\n' "${lines[index]}" > "$root/docs/notes.md"
    else
      printf '%s\n' "${lines[index]}" > "$root/docs/notes.md"
    fi
    run_check "$root"
    check "$prefix-$index fails" "$(is "$status" 1)"
    check "$prefix-$index is named at its file and line" \
      "$(contains "$out" "docs/notes.md:$line: $label")"
  done
}

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
  "the agent${nbsp}c repository"
  "the agent""  c repository"
)
assert_each_refused repo "names the private deployment repository" mentions 2

echo "case: every spelling of the company is refused"
# shellcheck disable=SC2034 # read by name through assert_each_refused's nameref
mentions=(
  "$company_proper Labs"
  "$company-$labs-example/repo"
  "${company}_$labs"
  "https://dispatch.internal.$company$labs.com/"
  "$company.$labs"
  "${company_proper}${labs^}"
  "the $company_proper team"
  "($company_proper)"
  "$company$en_dash$labs"
  "$company%20$labs"
  "$company$nbsp$labs"
  "$company  $labs"
  "see the ${company^^} docs"
  "${company^^}_AWS_PROFILE=x"
  "${company_proper}_API_KEY=x"
  "MY_${company^^}_KEY=x"
)
assert_each_refused company "names the company" mentions

echo "case: a private internal host is refused, whatever its case"
# shellcheck disable=SC2034 # read by name through assert_each_refused's nameref
hosts=(
  "https://listener.$internal.$private_host_suffix"
  "https://listener.${internal^^}.$private_host_suffix"
  "https://listener.${internal^}.$private_host_suffix"
  "https://listener.$internal.${example_host_suffix}corp.net"
  "https://listener.$internal.$example_host_suffix.$private_host_suffix"
  "https://a.$internal.$example_host_suffix and https://b.${internal^^}.$private_host_suffix"
  "this.$internal.push(value)"
)
assert_each_refused host "names a private internal host" hosts

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

echo "case: a Tailscale MagicDNS host is refused, whatever its case"
# shellcheck disable=SC2034 # read by name through assert_each_refused's nameref
tailnet_hosts=(
  "https://nats.$tailnet_label.$tailnet_suffix:4222"
  "https://nats.${tailnet_label^^}.$tailnet_suffix:4222"
  "https://nats.$tailnet_label.${tailnet_suffix^^}"
  "https://a.$tailnet_example.$tailnet_suffix and https://b.$tailnet_label.$tailnet_suffix"
  "see _nats.$tailnet_label.${tailnet_suffix}_ in italics"
  "NATS_$tailnet_label.$tailnet_suffix"
)
assert_each_refused tailnet "names a Tailscale MagicDNS host" tailnet_hosts

echo "case: the reserved example tailnet passes, under any machine name, and a longer word is not" \
  "mistaken for one"
root=$(fixture public-tailnet)
{
  printf 'https://nats.%s.%s:4222\n' "$tailnet_example" "$tailnet_suffix"
  printf 'https://example-host-mx.%s.%s\n' "$tailnet_example" "$tailnet_suffix"
  printf 'nats://<tailnet>.%s:4222\n' "$tailnet_suffix"
  printf 'nats://<name>.<tailnet>.%s:4222\n' "$tailnet_suffix"
  printf 'host.%s.%swork, a longer word the boundary must not mistake for one\n' \
    "$tailnet_label" "$tailnet_suffix"
} > "$root/docs/notes.md"
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

echo "case: a non-ASCII file name reaches the rules unquoted"
root=$(fixture path-name-dash)
printf 'clean text\n' > "$root/docs/$repo_en_dash.md"
git -C "$root" add docs
run_check "$root"
check "a file named with a typographic dash between the halves fails" "$(is "$status" 1)"
check "masks it" "$(contains "$out" 'docs/<name>')"

root=$(fixture path-unicode)
printf '%s\n' "$key-7" > "$root/docs/café.md"
git -C "$root" add docs
run_check "$root"
check "a hit in a non-ASCII file fails" "$(is "$status" 1)"
check "names the file as it is, unquoted" "$(contains "$out" '^::error file=docs/café.md,line=1::')"

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

# write_pr_text <title> <body> <commit message> [branch] [author email] [committer email]: writes
# the fixture check-pr-text.sh reads as CI runs it. A pull_request event payload carries the title,
# body, branch name and the head the run checks (its base and head sha, and its one commit); a
# stand-in `gh` answers the compare route for exactly that base...head with that commit (message,
# author and committer) in the route's own JSON shape, so the real `.commits[] | .commit.message, …`
# filter that feeds the required check runs for real, rather than a filter the stub fakes. It
# matches that URL whole and refuses a call without --paginate, so losing either paging token
# fails every case below instead of passing on a list GitHub cut short at 250 commits. It answers
# the pull request's three comment routes (issue comments, reviews and review comments), which
# report no total, one page of 100 at a time: page N of each is `$work/<route>.N` (`comments`,
# `reviews` or `review-comments`), and a page no case wrote is an empty list, as GitHub answers past
# the last. It answers the pulls route with the pull request the payload carries (`$work/pull`),
# which an issue_comment payload names only by number. The stub also answers the pull request's
# own commits route, which reads whatever head the pull request has by then: the same commit, until
# the mid-run-push case appends a later one that names a private thing. A case rewrites
# `$work/compare` (one JSON object per page, as `gh api --paginate` prints them), `$work/pr-commits`
# or a comment route's pages, then calls run_pr_text, which runs the script and sets `out` and
# `status`; the stub logs each call it answers to `$work/calls`.
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PR_TEXT_TEST_DIR/calls"
# page <route> <url>: the page of a comment route the url's `page=N` asks for.
page() {
  local file="$PR_TEXT_TEST_DIR/$1.${2##*&page=}"
  if [ -e "$file" ]; then cat "$file"; else echo '[]'; fi
}
case "$1 $2" in
  "api repos/example/repo/compare/base-sha...event-head-sha?per_page=100")
    [[ " $* " == *" --paginate "* ]] || { echo "compare route read without --paginate: $*" >&2; exit 3; }
    cat "$PR_TEXT_TEST_DIR/compare" ;;
  "api repos/example/repo/issues/7/comments?per_page=100&page="[1-9]*) page comments "$2" ;;
  "api repos/example/repo/pulls/7/reviews?per_page=100&page="[1-9]*) page reviews "$2" ;;
  "api repos/example/repo/pulls/7/comments?per_page=100&page="[1-9]*) page review-comments "$2" ;;
  "api repos/example/repo/pulls/7/commits") cat "$PR_TEXT_TEST_DIR/pr-commits" ;;
  "api repos/example/repo/pulls/7") cat "$PR_TEXT_TEST_DIR/pull" ;;
  *) echo "unexpected gh call: $*" >&2; exit 3 ;;
esac
GH
chmod +x "$work/bin/gh"
# pr_commit <message> [author email] [committer email]: one commit as both routes list it.
pr_commit() {
  jq -nc --arg msg "$1" --arg author_email "${2:-dev@example.com}" \
    --arg committer_email "${3:-dev@example.com}" \
    '{commit: {message: $msg, author: {name: "Dev", email: $author_email},
               committer: {name: "Dev", email: $committer_email}}}'
}
write_pr_text() {
  local title=$1 body=$2 message=$3 branch=${4:-fix/keep-the-order} commit
  commit=$(pr_commit "$message" "${5:-}" "${6:-}")
  jq -nc --argjson commit "$commit" '{total_commits: 1, commits: [$commit]}' > "$work/compare"
  jq -nc --argjson commit "$commit" '[$commit]' > "$work/pr-commits"
  printf '%s' "$body" > "$work/pr-body"
  jq -n --arg title "$title" --arg branch "$branch" --rawfile body "$work/pr-body" \
    '{pull_request: {number: 7, title: $title, commits: 1,
                      head: {ref: $branch, sha: "event-head-sha"}, base: {sha: "base-sha"},
                      body: (if $body == "" then null else $body end)}}' \
    > "$work/event.json"
  jq '.pull_request' "$work/event.json" > "$work/pull"
  rm -f "$work"/comments.* "$work"/reviews.* "$work"/review-comments.*
}
run_pr_text() {
  if [ $# -gt 0 ]; then write_pr_text "$@"; fi
  : > "$work/calls"
  set +e
  out=$(GITHUB_EVENT_PATH="$work/event.json" GITHUB_REPOSITORY=example/repo \
    PR_TEXT_TEST_DIR="$work" PATH="$work/bin:$PATH" "$pr_text_script" 2>&1)
  status=$?
  set -e
}
# app: who posted an item a case writes, unless the case says otherwise: an App installed on the
# repository, which posts as a Bot, with the association every Legion App's comments carry here.
app='{"user": {"login": "dev-app[bot]", "type": "Bot"}, "author_association": "CONTRIBUTOR"}'
# items <id> <body> [<id> <body>...]: one page of a comment route, each item its id, body and
# author (app) in the route's own JSON shape.
items() {
  local list='[]'
  while [ $# -gt 0 ]; do
    list=$(jq -c --argjson id "$1" --arg body "$2" --argjson author "$app" \
      '. + [{id: $id, body: $body} + $author]' <<< "$list")
    shift 2
  done
  printf '%s\n' "$list"
}
# by <login> <type> <association> <page> [index]: rewrites a page's items (only the one at index,
# when given) as posted by that author.
by() {
  local filter='map(author)'
  if [ $# -gt 4 ]; then filter=".[$5] |= author"; fi
  jq -c --arg login "$1" --arg type "$2" --arg association "$3" \
    "def author: .user = {login: \$login, type: \$type} | .author_association = \$association; $filter" \
    "$4" > "$4.next"
  mv "$4.next" "$4"
}
# as_event <name>: rewrites the fixture's pull_request payload as that event's. A
# pull_request_review or pull_request_review_comment payload carries the pull request whole beside
# the review or comment; an issue_comment payload names it only as an issue (its number, title and
# a pull_request link), so the script reads the rest from the pulls route.
as_event() {
  case $1 in
    pull_request_review)
      jq '. + {action: "submitted", review: {id: 201, body: "Approved."}}' "$work/event.json" ;;
    pull_request_review_comment)
      jq '. + {action: "created", comment: {id: 301, body: "A nit."}}' "$work/event.json" ;;
    issue_comment)
      jq '{action: "created", comment: {id: 101, body: "Looks good."},
           issue: {number: .pull_request.number, title: .pull_request.title,
                   pull_request: {url: "https://api.github.com/repos/example/repo/pulls/7"}}}' \
        "$work/event.json" ;;
  esac > "$work/event.next"
  mv "$work/event.next" "$work/event.json"
}


echo "case: a pull request's title, body, branch name and commits, which reach main and its release notes"
run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
check "a clean title, body and commits pass" "$(is "$status" 0)"

run_pr_text "fix(dispatch): keep the inbox order ($key-7)" "Refs LEGION-7" "fix: keep the order"
check "a title naming a deployment issue key fails" "$(is "$status" 1)"
check "names the title" "$(contains "$out" 'title:1: names the private deployment repository')"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

run_pr_text "fix(dispatch): keep the inbox order" "$(printf 'Line one.\nAs %s Labs found.\n' "$company_proper")" "fix: keep the order"
check "a body naming the company fails" "$(is "$status" 1)"
check "names the body's line" "$(contains "$out" 'body:2: names the company')"

run_pr_text "fix(dispatch): keep the inbox order" "" "fix: keep the order"
check "an empty body passes" "$(is "$status" 0)"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "fix/$key-7"
check "a branch naming a deployment issue key fails" "$(is "$status" 1)"
check "names the branch" "$(contains "$out" 'branch:1: names the private deployment repository')"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" \
  "$(printf 'fix: keep the order\n\nCo-authored-by: A <a@%s%s.example>\n' "$company" "$labs")"
check "a commit message naming the company fails" "$(is "$status" 1)"
check "names the commit messages' line" "$(contains "$out" 'commits:3: names the company')"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "fix/keep-the-order" \
  "a@$company$labs.example"
check "a commit's author email naming the company fails, with a clean message" "$(is "$status" 1)"
check "names the commits line" "$(contains "$out" 'commits:3: names the company')"
check "does not print the domain" "$(is "$(contains "$out" "$company$labs")" false)"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "fix/keep-the-order" \
  "dev@example.com" "a@$company$labs.example"
check "a commit's committer email naming the company fails, with a clean message and author" "$(is "$status" 1)"
check "names the commits line" "$(contains "$out" 'commits:5: names the company')"

echo "case: the commits of the head the run checks, when a later push lands while it runs"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
later_commit=$(pr_commit "fix: a later push ($key-7)")
jq -c --argjson later "$later_commit" '. + [$later]' "$work/pr-commits" > "$work/pr-commits.next"
mv "$work/pr-commits.next" "$work/pr-commits"
run_pr_text
check "a pull request whose head moved on mid-run passes" "$(is "$status" 0)"

# The same commit, this time on the head the run checks: the case above passes because the script
# read that head, not because the later push's name is one the guard would have let through.
jq -c --argjson later "$later_commit" '{total_commits: 2, commits: (.commits + [$later])}' \
  "$work/compare" > "$work/compare.next"
mv "$work/compare.next" "$work/compare"
run_pr_text
check "that same commit, on the run's own head, fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'commits:6: names the private deployment repository')"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

echo "case: a commit list the compare route cut short, and one spread over pages"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
jq -c '.total_commits = 300' "$work/compare" > "$work/compare.next"
mv "$work/compare.next" "$work/compare"
run_pr_text
check "a list shorter than the route's own total fails loudly" "$(is "$status" 1)"
check "names both counts" "$(contains "$out" 'reports 300 commits .* but listed 1')"

write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
second_commit=$(pr_commit "$(printf 'fix: keep the order\n\nCo-authored-by: A <a@%s%s.example>\n' "$company" "$labs")")
jq -c '.total_commits = 2' "$work/compare" > "$work/compare.next"
jq -nc --argjson second "$second_commit" '{total_commits: 2, commits: [$second]}' >> "$work/compare.next"
mv "$work/compare.next" "$work/compare"
run_pr_text
check "a commit on the compare route's second page naming the company fails" "$(is "$status" 1)"
check "names its line, counting the first page's commit" "$(contains "$out" 'commits:8: names the company')"
check "counts both pages' commits as the whole list" "$(is "$(contains "$out" 'but listed')" false)"

echo "case: a compare answer the script cannot check against a total"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
jq -nc '{commits: []}' > "$work/compare"
run_pr_text
check "an answer carrying no total_commits fails" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'total_commits, so the commit list cannot be checked')"

: > "$work/compare"
run_pr_text
check "an empty answer fails the same way" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'total_commits, so the commit list cannot be checked')"

# A total of the wrong type, with fewer commits listed than it claims: `[`'s integer test would
# fail inside the guard's `if`, where set -e does not apply, and skip the guard.
for total in '"300"' 300.5; do
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  jq -c --argjson total "$total" '.total_commits = $total' "$work/compare" > "$work/compare.next"
  mv "$work/compare.next" "$work/compare"
  run_pr_text
  check "a total_commits of $total fails" "$(is "$status" 1)"
  check "names the cause" "$(contains "$out" 'total_commits, so the commit list cannot be checked')"
done

long_body=$(printf '%.0s–' {1..50000})
run_pr_text "fix(dispatch): keep the inbox order" "$long_body" "fix: keep the order"
check "a body larger than one environment variable may hold is read" "$(is "$status" 0)"

echo "case: a pull request's issue comments, reviews and review comments, public on its page"
clean_pr() { write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"; }
clean_pr
items 101 "Looks good." 102 "$(printf 'Two lines.\nThe second is clean too.')" > "$work/comments.1"
items 201 "Approved." 202 "" > "$work/reviews.1"
jq -c '.[1].body = null' "$work/reviews.1" > "$work/reviews.next" && mv "$work/reviews.next" "$work/reviews.1"
items 301 "A nit on this line." > "$work/review-comments.1"
run_pr_text
check "a clean set of comments, reviews (one with no body) and review comments passes" "$(is "$status" 0)"
check "reads every issue comment" "$(contains "$(< "$work/calls")" '^api repos/example/repo/issues/7/comments?per_page=100&page=1$')"
check "reads every review" "$(contains "$(< "$work/calls")" '^api repos/example/repo/pulls/7/reviews?per_page=100&page=1$')"
check "reads every review comment" "$(contains "$(< "$work/calls")" '^api repos/example/repo/pulls/7/comments?per_page=100&page=1$')"

clean_pr
items 101 "Looks good." 102 "$(printf 'Line one.\nAs %s Labs found.\n' "$company_proper")" > "$work/comments.1"
run_pr_text
check "an issue comment naming the company fails" "$(is "$status" 1)"
check "names the comment by its id and line" "$(contains "$out" 'comment:102:2: names the company')"
check "names no other comment" "$(is "$(contains "$out" 'comment:101')" false)"
check "does not print the name" "$(is "$(contains "$out" "$company_proper")" false)"
check "says how to take a comment down" "$(contains "$out" 'delete each comment')"

clean_pr
items 201 "Approved." 202 "Blocking: see $key-7." > "$work/reviews.1"
run_pr_text
check "a review body naming a deployment issue key fails" "$(is "$status" 1)"
check "names the review by its id and line" "$(contains "$out" 'review:202:1: names the private deployment repository')"
check "does not call it a review comment" "$(is "$(contains "$out" 'review-comment:202')" false)"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"

clean_pr
items 301 "A nit." 302 "$(printf 'Nit.\n\nSee https://listener.%s.%s/x\n' "$internal" "$private_host_suffix")" > "$work/review-comments.1"
run_pr_text
check "a review comment naming a private host fails" "$(is "$status" 1)"
check "names the review comment by its id and line" "$(contains "$out" 'review-comment:302:3: names a private internal host')"
check "does not print the host" "$(is "$(contains "$out" "$private_host_suffix")" false)"
check "says how to take a review comment down" "$(contains "$out" 'delete each comment and review comment')"

clean_pr
printf 'A NUL here:\0 the %s repository\n' "$repo" > "$work/comment-body"
jq -nc --rawfile body "$work/comment-body" --argjson author "$app" '[{id: 103, body: $body} + $author]' \
  > "$work/comments.1"
run_pr_text
check "a comment holding a NUL byte is still read" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'comment:103:1: names the private deployment repository')"

echo "case: comment lists GitHub serves over several pages"
clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (1000 + .), body: "fine"} + $author]' > "$work/comments.1"
items 1101 "fine" 1102 "$company_proper Labs" > "$work/comments.2"
run_pr_text
check "a comment on the second page naming the company fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'comment:1102:1: names the company')"
check "stops at the short second page" \
  "$(is "$(contains "$(< "$work/calls")" 'issues/7/comments?per_page=100&page=3')" false)"

clean_pr
jq -nc --argjson author "$app" '[range(1; 101) | {id: (2000 + .), body: "fine"} + $author]' > "$work/reviews.1"
run_pr_text
check "a full first page of reviews and an empty second pass" "$(is "$status" 0)"
check "reads the second page after a full first" \
  "$(contains "$(< "$work/calls")" '^api repos/example/repo/pulls/7/reviews?per_page=100&page=2$')"

echo "case: a comment list the script cannot read whole"
clean_pr
printf '{"message": "Not Found"}\n' > "$work/review-comments.1"
run_pr_text
check "an answer that is not a list fails" "$(is "$status" 1)"
check "names the route" "$(contains "$out" 'pulls/7/comments route is not a list')"

clean_pr
items 101 "fine" > "$work/comments.1"
jq -c '.[0].id = "101"' "$work/comments.1" > "$work/comments.next" && mv "$work/comments.next" "$work/comments.1"
run_pr_text
check "an item whose id is not a whole number fails" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'whose id is not a whole number')"

echo "case: the events pr-title.yaml runs on for a comment or review"
clean_pr
as_event pull_request_review
items 202 "Blocking: see $key-7." > "$work/reviews.1"
run_pr_text
check "a pull_request_review payload is checked like a pull_request one" "$(is "$status" 1)"
check "names the review" "$(contains "$out" 'review:202:1: names the private deployment repository')"
check "reads the pull request from the payload" "$(is "$(contains "$(< "$work/calls")" '^api repos/example/repo/pulls/7$')" false)"

clean_pr
as_event pull_request_review_comment
run_pr_text
check "a clean pull_request_review_comment payload passes" "$(is "$status" 0)"

clean_pr
as_event issue_comment
items 101 "Looks good." > "$work/comments.1"
run_pr_text
check "a clean issue_comment payload passes" "$(is "$status" 0)"
check "reads the pull request from the pulls route" "$(contains "$(< "$work/calls")" '^api repos/example/repo/pulls/7$')"
check "and its commits between that pull request's base and head" \
  "$(contains "$(< "$work/calls")" '^api repos/example/repo/compare/base-sha...event-head-sha?per_page=100 --paginate$')"

jq '.title = "fix(dispatch): keep the order (" + $key + "-7)"' --arg key "$key" "$work/pull" > "$work/pull.next"
mv "$work/pull.next" "$work/pull"
run_pr_text
check "an issue_comment run still checks the title the pulls route answers" "$(is "$status" 1)"
check "names the title" "$(contains "$out" 'title:1: names the private deployment repository')"

clean_pr
jq '{action: "created", comment: {id: 101, body: "fine"}, issue: {number: 7, title: "a plain issue"}}' \
  "$work/event.json" > "$work/event.next"
mv "$work/event.next" "$work/event.json"
run_pr_text
check "an issue_comment on an issue that is no pull request is refused" "$(is "$status" 1)"
check "names the cause" "$(contains "$out" 'names no pull request')"

echo "case: only the comments of an App installed here or of the repository's own people count"
# On a public repository anyone signed in can comment or review, and every run reports the required
# check, so a run that read a stranger's text would let them turn it red on anyone's head.
# three_items: a comment, a review and a review comment, each naming something private.
three_items() {
  items 401 "As $company_proper Labs found." > "$work/comments.1"
  items 402 "Blocking: see $key-7." > "$work/reviews.1"
  items 403 "$(printf 'Nit.\n\nthe %s repository\n' "$repo")" > "$work/review-comments.1"
}
clean_pr
three_items
by drive-by User NONE "$work/comments.1"
by drive-by User NONE "$work/reviews.1"
by drive-by User CONTRIBUTOR "$work/review-comments.1"
run_pr_text
check "a stranger's comment, review and review comment naming private things pass" "$(is "$status" 0)"
check "names the skipped comment and its author" \
  "$(contains "$out" '^skipped comment:401: author drive-by (NONE) is not trusted$')"
check "names the skipped review" "$(contains "$out" '^skipped review:402: author drive-by (NONE) is not trusted$')"
check "names the skipped review comment, whose author contributed before" \
  "$(contains "$out" '^skipped review-comment:403: author drive-by (CONTRIBUTOR) is not trusted$')"
check "does not print the company" "$(is "$(contains "$out" "$company_proper")" false)"
check "does not print the key" "$(is "$(contains "$out" "$key")" false)"
check "does not print the repository" "$(is "$(contains "$out" "$repo")" false)"

clean_pr
items 404 "fine" > "$work/comments.1"
jq -c '.[0].user = null | .[0].author_association = "NONE"' "$work/comments.1" > "$work/comments.next"
mv "$work/comments.next" "$work/comments.1"
run_pr_text
check "a comment GitHub names no author for is skipped" \
  "$(contains "$out" '^skipped comment:404: author <none> (NONE) is not trusted$')"

for author in "dev-app[bot] Bot CONTRIBUTOR" "new-app[bot] Bot NONE" "owner User OWNER" \
  "member User MEMBER" "collaborator User COLLABORATOR"; do
  read -r login type association <<< "$author"
  clean_pr
  three_items
  for page in comments.1 reviews.1 review-comments.1; do by "$login" "$type" "$association" "$work/$page"; done
  run_pr_text
  check "the same three from $login ($type, $association) fail" "$(is "$status" 1)"
  check "names the comment" "$(contains "$out" 'comment:401:1: names the company')"
  check "names the review" "$(contains "$out" 'review:402:1: names the private deployment repository')"
  check "names the review comment" "$(contains "$out" 'review-comment:403:3: names the private deployment repository')"
  check "skips none" "$(is "$(contains "$out" '^skipped ')" false)"
done

clean_pr
as_event issue_comment
items 405 "As $company_proper Labs found." 406 "All clear now." > "$work/comments.1"
by drive-by User NONE "$work/comments.1" 1
run_pr_text
check "a stranger's clean comment after an App's red one still fails, so it clears nothing" "$(is "$status" 1)"
check "names the App's comment" "$(contains "$out" 'comment:405:1: names the company')"
check "skips the stranger's" "$(contains "$out" '^skipped comment:406: author drive-by (NONE) is not trusted$')"

echo "case: the suite's own output never names a private thing"
exec 1>&3 2>&4
wait "$tee_pid" 2> /dev/null || true
suite_status=0
"$check_script" "$self_log" > /dev/null 2>&1 || suite_status=$?
check "the suite's own PASS/FAIL log passes the guard" "$(is "$suite_status" 0)"

summary "check-private-names.sh"
