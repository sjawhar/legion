#!/usr/bin/env bash
# Tests for `check-body-embed-limit.sh`: the Dispatch body limit never exceeds the embedder's
# input limit, each number's site names the other, and the check refuses any tree it cannot read
# unambiguously. The failures it stops are silent ones — a body longer than `maxInputChars` is cut
# before it is embedded and its tail vanishes from search by meaning — so the suite pins the exit
# status and the wording the author of a drift has to act on, including that the remedy is to
# lower `DISPATCH_BODY_MAX` (or raise `maxInputChars` deliberately), never to lower the embedder's.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs. The fixtures' numbers are invented
# and are never the repository's real ones: a hardcoded copy of either real number in the script
# would fail this suite.
#
# Run from anywhere: .github/scripts/check-body-embed-limit.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-body-embed-limit.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name> [body-limit] [embed-limit]: a tree that passes — the body limit below the
# embedder's, each declaration's comment naming the other constant, and each AGENTS.md holding one
# line naming both. Echoes its root.
fixture() {
  local root="$work/$1" body=${2:-14500} embed=${3:-19000}
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/contracts/src" \
    "$root/packages/envoy/internal/dispatch/embed"
  ln -s "$check_script" "$root/.github/scripts/check-body-embed-limit.sh"
  cat > "$root/packages/contracts/src/dispatch-tools.ts" <<TS
export const DISPATCH_TITLE_MAX = 200;

// The longest body the server accepts, in UTF-16 code units. It must never exceed the embedder's
// maxInputChars (packages/envoy/internal/dispatch/embed/embed.go), or a body's tail is cut away
// before it is embedded.
export const DISPATCH_BODY_MAX = $body;
TS
  cat > "$root/packages/envoy/internal/dispatch/embed/embed.go" <<GO
package embed

const (
	// The longest text handed to the model, in runes; DISPATCH_BODY_MAX
	// (packages/contracts/src/dispatch-tools.ts) must never exceed it.
	maxInputChars = $embed
)
GO
  cat > "$root/packages/contracts/AGENTS.md" <<'MD'
# Contracts

DISPATCH_BODY_MAX must never exceed maxInputChars in the envoy embedder.
MD
  cat > "$root/packages/envoy/AGENTS.md" <<'MD'
# Envoy

maxInputChars bounds every embedded text, so DISPATCH_BODY_MAX must stay at or below it.
MD
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-body-embed-limit.sh" 2>&1)
  status=$?
  set -e
}

echo "case: the body limit fits the embedder's"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "names both numbers and both files" \
  "$(contains "$out" "body limit 14500 (packages/contracts/src/dispatch-tools.ts:6) fits the embedder's 19000 (packages/envoy/internal/dispatch/embed/embed.go:6), and each names the other")"

echo "case: the two are equal"
root=$(fixture equal 19000 19000)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reports the fit" "$(contains "$out" 'body limit 19000 .* fits the embedder.s 19000')"

echo "case: the body limit is one past the embedder's"
root=$(fixture over 19001 19000)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both numbers and both sites" \
  "$(contains "$out" 'DISPATCH_BODY_MAX is 19001 (packages/contracts/src/dispatch-tools.ts:6), past the embedder.s maxInputChars of 19000 (packages/envoy/internal/dispatch/embed/embed.go:6)')"
check "says the tail vanishes silently" \
  "$(contains "$out" 'vanishes from search by meaning with no error')"
check "the remedy is to lower the body limit" \
  "$(contains "$out" 'Lower DISPATCH_BODY_MAX back to at most 19000')"
check "or to raise the embedder's deliberately" \
  "$(contains "$out" 'or raise maxInputChars deliberately')"
check "counts the problem" "$(contains "$out" '::error::1 problem(s)')"

echo "case: comments naming other numbers, and 32,000 in the prose around them"
root=$(fixture prose)
cat > "$root/packages/contracts/src/dispatch-tools.ts" <<'TS'
// History: the body limit was 8,000 before dispatch://INFRA-42 raised it, and the embedder
// has been at 32,000 since.
export const DISPATCH_TITLE_MAX = 200;

// The longest body the server accepts, in UTF-16 code units. It must never exceed the embedder's
// maxInputChars (today 19000, and it was 32,000 before that).
export const DISPATCH_BODY_MAX = 14500;
TS
cat > "$root/packages/envoy/internal/dispatch/embed/embed.go" <<'GO'
package embed

// The model's window was 32,000 runes for the first provider.

const (
	// The longest text handed to the model, in runes. DISPATCH_BODY_MAX (14500 today, 8,000
	// before dispatch://INFRA-42) must never exceed it.
	maxInputChars = 19000
)
GO
printf 'Caps: DISPATCH_BODY_MAX <= maxInputChars. Both were 32,000 in the first draft.\n' \
  >> "$root/packages/contracts/AGENTS.md"
printf 'The embedder cut at 32,000 runes once; maxInputChars and DISPATCH_BODY_MAX move together.\n' \
  >> "$root/packages/envoy/AGENTS.md"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads the declared numbers, not the prose" \
  "$(contains "$out" 'body limit 14500 (packages/contracts/src/dispatch-tools.ts:7) fits the embedder.s 19000')"

echo "case: the body constant renamed"
root=$(fixture renamed)
sed -i 's/DISPATCH_BODY_MAX = /DISPATCH_BODY_LIMIT = /' "$root/packages/contracts/src/dispatch-tools.ts"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and the pattern it needs" \
  "$(contains "$out" 'packages/contracts/src/dispatch-tools.ts declares no DISPATCH_BODY_MAX this check can read: no line matches \^.*export.*DISPATCH_BODY_MAX')"
check "says a renamed constant is this failure" "$(contains "$out" 'A renamed constant, a moved file')"

echo "case: digit separators in place of a plain decimal"
root=$(fixture separators)
sed -i 's/DISPATCH_BODY_MAX = 14500/DISPATCH_BODY_MAX = 14_500/' \
  "$root/packages/contracts/src/dispatch-tools.ts"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/contracts/src/dispatch-tools.ts declares no DISPATCH_BODY_MAX this check can read')"
check "names the number written another way" \
  "$(contains "$out" 'digit separators, a leading zero, hexadecimal, an expression')"

echo "case: a leading zero on maxInputChars, which Go reads as octal"
root=$(fixture embed-octal 14500 017000)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/envoy/internal/dispatch/embed/embed.go declares no maxInputChars this check can read')"
check "refuses 017000 rather than reading it as 17000, because Go reads it as octal 7680" \
  "$(contains "$out" 'digit separators, a leading zero, hexadecimal, an expression')"

echo "case: a leading zero on the body limit, which TypeScript rejects outright"
root=$(fixture body-octal 014500)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses 014500 rather than reading it as 14500, since no decimal number is written that way" \
  "$(contains "$out" 'packages/contracts/src/dispatch-tools.ts declares no DISPATCH_BODY_MAX this check can read')"

echo "case: two maxInputChars declarations"
root=$(fixture ambiguous)
cat > "$root/packages/envoy/internal/dispatch/embed/embed.go" <<'GO'
package embed

const (
	// The longest text handed to the model, in runes; DISPATCH_BODY_MAX
	// (packages/contracts/src/dispatch-tools.ts) must never exceed it.
	maxInputChars = 19000
)

const (
	maxInputChars = 21000
)
GO
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both line numbers" \
  "$(contains "$out" 'declares maxInputChars more than once (packages/envoy/internal/dispatch/embed/embed.go:6, packages/envoy/internal/dispatch/embed/embed.go:10)')"
check "refuses to guess which is in force" \
  "$(contains "$out" 'cannot tell which number is in force')"

echo "case: embed.go missing"
root=$(fixture missing)
rm "$root/packages/envoy/internal/dispatch/embed/embed.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/envoy/internal/dispatch/embed/embed.go is missing: this check cannot prove the limits agree without it')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a file that is not valid UTF-8"
root=$(fixture not-utf8)
printf '# Contracts\n\nDISPATCH_BODY_MAX <= maxInputChars \xff\n' > "$root/packages/contracts/AGENTS.md"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/contracts/AGENTS.md is not readable as UTF-8 text')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a directory where embed.go belongs"
# A directory, not a mode-000 file: root may read a file no mode forbids it, and the suite must
# behave the same under either user.
root=$(fixture embed-is-a-directory)
rm "$root/packages/envoy/internal/dispatch/embed/embed.go"
mkdir "$root/packages/envoy/internal/dispatch/embed/embed.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file it could not read" \
  "$(contains "$out" '^::error file=packages/envoy/internal/dispatch/embed/embed.go::packages/envoy/internal/dispatch/embed/embed.go cannot be read: ')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "nothing is listed beside it" "$(is "$(contains "$out" 'problem(s)')" false)"

echo "case: a form feed in the comment above a declaration"
# `str.splitlines()` breaks on \f, U+2028, U+0085 and \v, which an editor does not, so a line
# number counted that way names a line the reader cannot open. The declaration below carries
# four newlines before it and so sits on line 5, whatever the comment holds.
root=$(fixture form-feed)
printf 'package embed\n\nconst (\n\t// The longest text handed to the model,\fin runes.\n\tmaxInputChars = 19000\n)\n' \
  > "$root/packages/envoy/internal/dispatch/embed/embed.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reports the line a reader opens, not one shifted by the form feed" \
  "$(contains "$out" 'the comment above maxInputChars (packages/envoy/internal/dispatch/embed/embed.go:5) does not name DISPATCH_BODY_MAX')"

echo "case: the body limit's comment does not name maxInputChars"
root=$(fixture body-no-xref)
cat > "$root/packages/contracts/src/dispatch-tools.ts" <<'TS'
// The longest body the server accepts, in UTF-16 code units.
export const DISPATCH_BODY_MAX = 14500;
TS
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site that must name the other" \
  "$(contains "$out" 'the comment above DISPATCH_BODY_MAX (packages/contracts/src/dispatch-tools.ts:2) does not name maxInputChars')"
check "says what to write there" \
  "$(contains "$out" 'write maxInputChars into the comment above DISPATCH_BODY_MAX')"

echo "case: the embedder's comment does not name DISPATCH_BODY_MAX"
root=$(fixture embed-no-xref)
cat > "$root/packages/envoy/internal/dispatch/embed/embed.go" <<'GO'
package embed

const (
	// The longest text handed to the model, in runes.
	maxInputChars = 19000
)
GO
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site that must name the other" \
  "$(contains "$out" 'the comment above maxInputChars (packages/envoy/internal/dispatch/embed/embed.go:5) does not name DISPATCH_BODY_MAX')"

echo "case: a /* ... */ block whose body lines carry no leading *"
root=$(fixture block-comment)
cat > "$root/packages/contracts/src/dispatch-tools.ts" <<'TS'
export const DISPATCH_TITLE_MAX = 200;

/*
  The longest body the server accepts, in UTF-16 code units. It must never exceed the embedder's
  maxInputChars (packages/envoy/internal/dispatch/embed/embed.go).
*/
export const DISPATCH_BODY_MAX = 14500;
TS
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads the whole block, not its closing line alone" \
  "$(contains "$out" 'body limit 14500 (packages/contracts/src/dispatch-tools.ts:7) fits the embedder.s 19000')"

echo "case: a Go doc comment written on the const ( group, not on its one member"
root=$(fixture group-doc-comment)
cat > "$root/packages/envoy/internal/dispatch/embed/embed.go" <<'GO'
package embed

// The longest text handed to the model, in runes; DISPATCH_BODY_MAX
// (packages/contracts/src/dispatch-tools.ts) must never exceed it.
const (
	maxInputChars = 19000
)
GO
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads the group's doc comment as its member's" \
  "$(contains "$out" 'fits the embedder.s 19000 (packages/envoy/internal/dispatch/embed/embed.go:6), and each names the other')"

echo "case: a trailing same-line comment on each declaration, with none above either"
root=$(fixture trailing-comment)
cat > "$root/packages/contracts/src/dispatch-tools.ts" <<'TS'
export const DISPATCH_BODY_MAX = 14500; // never past maxInputChars (embed.go)
TS
cat > "$root/packages/envoy/internal/dispatch/embed/embed.go" <<'GO'
package embed

const (
	maxInputChars = 19000 // bounds DISPATCH_BODY_MAX (packages/contracts/src/dispatch-tools.ts)
)
GO
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "credits the comment the declaration patterns already accept" \
  "$(contains "$out" 'body limit 14500 (packages/contracts/src/dispatch-tools.ts:1) fits the embedder.s 19000 (packages/envoy/internal/dispatch/embed/embed.go:4), and each names the other')"

echo "case: a /* ... */ block that does not name maxInputChars"
root=$(fixture block-comment-no-xref)
cat > "$root/packages/contracts/src/dispatch-tools.ts" <<'TS'
/*
  The longest body the server accepts, in UTF-16 code units.
*/
export const DISPATCH_BODY_MAX = 14500;
TS
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the site that must name the other" \
  "$(contains "$out" 'the comment above DISPATCH_BODY_MAX (packages/contracts/src/dispatch-tools.ts:4) does not name maxInputChars')"
check "says what to write there" \
  "$(contains "$out" 'write maxInputChars into the comment above DISPATCH_BODY_MAX')"

echo "case: a // comment line that ends with */ above the declaration"
# RED (tester, round 2): `attached_comment` tests `stripped.endswith("*/")` before
# `stripped.startswith("//")`, so an ordinary `//` line whose text happens to end with `*/`
# is taken for a block comment's closing line. The walk then consumes upward looking for an
# opening `/*`, and when the file holds none it credits every line above the declaration -
# here a `maxInputChars` mention on line 1 that is attached to nothing. The declaration's own
# comment names nothing about the embedder, so this tree must be refused.
root=$(fixture starslash-comment)
cat > "$root/packages/contracts/src/dispatch-tools.ts" <<'TS'
// History: maxInputChars was named here once, and this line documents nothing below it.
export const DISPATCH_TITLE_MAX = 200;

// The longest body the server accepts, in UTF-16 code units.
// The deprecated spelling of this comment was a block: /* in UTF-16 units */
export const DISPATCH_BODY_MAX = 14500;
TS
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "does not credit a mention attached to nothing far above" \
  "$(contains "$out" 'the comment above DISPATCH_BODY_MAX (packages/contracts/src/dispatch-tools.ts:6) does not name maxInputChars')"

echo "case: contracts/AGENTS.md names each constant on a different line"
root=$(fixture contracts-agents)
cat > "$root/packages/contracts/AGENTS.md" <<'MD'
# Contracts

DISPATCH_BODY_MAX is the longest body the server accepts.
The envoy embedder cuts at maxInputChars.
MD
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and asks for one line" \
  "$(contains "$out" 'no line of packages/contracts/AGENTS.md names both DISPATCH_BODY_MAX and maxInputChars')"
check "says what that line must say" \
  "$(contains "$out" 'must never exceed maxInputChars, so a reader raising either one finds the other')"

echo "case: envoy/AGENTS.md names each constant on a different line"
root=$(fixture envoy-agents)
cat > "$root/packages/envoy/AGENTS.md" <<'MD'
# Envoy

maxInputChars bounds every embedded text.
A body is capped separately by DISPATCH_BODY_MAX.
MD
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'no line of packages/envoy/AGENTS.md names both DISPATCH_BODY_MAX and maxInputChars')"

echo "case: the real repository passes"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reports the fit" \
  "$(contains "$out" '^body limit .* (packages/contracts/src/dispatch-tools.ts:.*) fits the embedder.s .* (packages/envoy/internal/dispatch/embed/embed.go:.*), and each names the other$')"

summary "check-body-embed-limit.sh"
