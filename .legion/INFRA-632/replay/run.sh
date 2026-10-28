#!/usr/bin/env bash
# Replays real conflicting merges of AGENTS.md, scripts/e2e/README.md and docs/kubernetes.md on
# their new layout (INFRA-632), and the appended-items demo, with `git merge-tree --write-tree`.
#
#   bash .legion/INFRA-632/replay/run.sh [repo root]
#
# The repo root defaults to the checkout this script sits in (three directories above it). The
# three files are read from its git history at REPLAY_BASE (default 6b75da30, this pull request's
# regeneration of main a409c128's three guides, which the patches apply to), committed as the base
# of a throwaway repository under ${TMPDIR:-/tmp}/replay-$$ (set TMPDIR to keep it off /tmp), and
# each patch beside this script is checked with `git apply --check` and applied on a branch of its
# own from that base, where every file it changes must pass `bun scripts/semantic-line-breaks.ts`.
# A later regeneration on a newer main lays the files out by the same rules, so the patches are not
# ported again each time main moves. For every pair the script prints its classification, computed
# from the two patches' hunks against the base file, then one line: merge, file, class, expected,
# observed, PASS or FAIL. It exits 1 on any FAIL.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root=$(cd -- "${1:-$here/../../..}" && pwd)
pin=${REPLAY_BASE:-6b75da30384d98265811c5d48b3f6d824ce1dff9}
agents=AGENTS.md
e2e=scripts/e2e/README.md
k8s=docs/kubernetes.md

work=${TMPDIR:-/tmp}/replay-$$
mkdir -p "$work"
trap 'rm -rf -- "$work"' EXIT
git init -q "$work"
g() { git -C "$work" -c user.name=replay -c user.email=replay@example.invalid "$@"; }
for f in "$agents" "$e2e" "$k8s"; do
  mkdir -p "$work/$(dirname "$f")"
  git -C "$root" show "$pin:$f" > "$work/$f"
done
g add -A
g commit -q -m base
g branch -M base

fails=0
# One branch per patch, from base; each file a patch changes must keep the layout.
for p in "$here"/*.patch; do
  name=$(basename "$p" .patch)
  g checkout -q -b "$name" base
  g apply --check "$p"
  g apply "$p"
  g commit -q -a -m "$name"
  while IFS= read -r f; do
    if ! bun "$root/scripts/semantic-line-breaks.ts" "$work/$f" > /dev/null; then
      printf 'layout: %s breaks the layout of %s  FAIL\n' "$name" "$f"
      fails=$((fails + 1))
    fi
  done < <(g diff --name-only base "$name")
done
g checkout -q base

# Changed lines (C <n>) and insertion points (I <n>: inserts after base line n) of a patch.
edits() {
  awk '
    function flush() { if (run && !minus) print "I", start - 1; run = 0; minus = 0 }
    /^@@ / { split($2, a, ","); old = -a[1]; left = (a[2] == "" ? 1 : a[2]);
             split($3, b, ","); right = (b[2] == "" ? 1 : b[2]); inhunk = 1; next }
    !inhunk { next }
    /^\\/ { next }
    /^ / { flush(); old++; left--; right-- }
    /^-/ { if (!run) { run = 1; start = old }; minus = 1; print "C", old; old++; left-- }
    /^\+/ { if (!run) { run = 1; start = old }; right-- }
    { if (left <= 0 && right <= 0) { flush(); inhunk = 0 } }
    END { flush() }
  ' "$1"
}

# classify <ours patch> <theirs patch> <class when apart>: prints "<class>\t<detail>".
# Two changed lines collide when they are the same line or next to each other. A changed line c
# and an insertion after line n collide when the insertion touches c: n is c, or n is c - 1. Two
# insertions collide only at the same point; any unchanged line between two edits keeps them apart.
classify() {
  awk -v apart="$3" '
    function ranges(list,   n, i, out, lo, prev, v, s) {
      n = split(list, v, " "); out = ""
      for (i = 1; i <= n; i++) {
        if (i > 1 && v[i] == prev + 1) { prev = v[i]; continue }
        if (i > 1) out = out (lo == prev ? lo : lo "-" prev) ","
        lo = v[i]; prev = v[i]
      }
      if (n) out = out (lo == prev ? lo : lo "-" prev)
      return out == "" ? "none" : out
    }
    FNR == 1 { side++ }
    { kind[side, $1] = kind[side, $1] (kind[side, $1] == "" ? "" : " ") $2; key[side, ++cnt[side]] = $1 " " $2 }
    END {
      same = ""; near = ""
      for (i = 1; i <= cnt[1]; i++) for (j = 1; j <= cnt[2]; j++) {
        split(key[1, i], x, " "); split(key[2, j], y, " ")
        if (x[1] == "I" && y[1] == "I") { if (x[2] == y[2]) same = same " " x[2]; continue }
        if (x[1] == "C" && y[1] == "C") { d = x[2] - y[2]; hit = (d >= -1 && d <= 1) }
        else { c = (x[1] == "C" ? x[2] : y[2]); n = (x[1] == "I" ? x[2] : y[2]); hit = (c == n || c == n + 1) }
        if (hit) near = near " ours " x[1] x[2] "~theirs " y[1] y[2] ";"
      }
      detail = "ours changes " ranges(kind[1, "C"]) " inserts after " ranges(kind[1, "I"]) \
        "; theirs changes " ranges(kind[2, "C"]) " inserts after " ranges(kind[2, "I"])
      if (same != "") print "same place\t" detail "; both insert after" same
      else if (near != "") print "same or adjacent\t" detail "; colliding (C changed, I inserts after):" near
      else print apart "\t" detail
    }
  ' <(edits "$1") <(edits "$2")
}

# pair <name> <file> <patch stem> <class when apart> <kind: pair|control|evidence>
# The ours patch is <stem>-ours.patch and the theirs patch <stem>-theirs.patch. A control is
# expected to conflict, and an evidence pair is recorded with whatever git does (expected=either).
pair() {
  local name=$1 file=$2 stem=$3 apart=$4 kind=$5 class detail expected tree hunks observed verdict
  IFS=$'\t' read -r class detail < <(classify "$here/$stem-ours.patch" "$here/$stem-theirs.patch" "$apart")
  if [ "$kind" = evidence ]; then
    expected=either
  elif [ "$kind" = control ] || [ "$class" != "$apart" ]; then
    expected=conflict
  else
    expected=clean
  fi
  if tree=$(g merge-tree --write-tree "$stem-ours" "$stem-theirs" 2>/dev/null); then
    observed=clean
  else
    hunks=$(g show "$(printf '%s\n' "$tree" | head -n 1):$file" | grep -c '^<<<<<<< ' || true)
    observed="conflict ($hunks hunks)"
  fi
  if [ "$expected" = either ] || [ "${observed%% *}" = "$expected" ]; then
    verdict=PASS
  else
    verdict=FAIL
    fails=$((fails + 1))
  fi
  printf '  classification: %s\n' "$detail"
  printf '%s  %s  class=%s  expected=%s  observed=%s  %s\n' "$name" "$file" "$class" "$expected" "$observed" "$verdict"
}

s="different sentences"
i="different items"
pair dadb5f515a3e "$agents" dadb5f515a3e-agents "$s" pair
pair 19aaa96fe03b "$agents" 19aaa96fe03b-agents "$s" pair
pair 9c96283b82a0 "$agents" 9c96283b82a0-agents "$s" pair
pair fb434588aa78 "$agents" fb434588aa78-agents "$s" pair
pair 2dce61d9cb6e "$agents" 2dce61d9cb6e-agents "$s" pair
pair 2fc4b487c8fa "$agents" 2fc4b487c8fa-agents "$s" pair
pair 22817ba1d2a3 "$agents" 22817ba1d2a3-agents "$i" pair
pair b1d8a21b1e8f "$agents" b1d8a21b1e8f-agents "$s" pair
pair "760468db6c78(control)" "$agents" 760468db6c78-agents "$s" control
pair "82fd7d27b4f2(control)" "$agents" 82fd7d27b4f2-agents "$s" control
pair "append(agents)" "$agents" append-agents-commands "$i" pair
pair "append(agents,control)" "$agents" append-agents-same "$i" control
pair 9a8af2f1743e "$e2e" 9a8af2f1743e-e2e "$i" pair
pair ee9ec60b7930 "$e2e" ee9ec60b7930-e2e "$i" pair
pair f95c4f77352a "$e2e" f95c4f77352a-e2e "$i" pair
pair 990ebd7341e0 "$e2e" 990ebd7341e0-e2e "$i" pair
pair f714cec148b6 "$e2e" f714cec148b6-e2e "$i" pair
pair bc1c2593a9cf "$e2e" bc1c2593a9cf-e2e "$i" pair
pair dadb5f515a3e "$e2e" dadb5f515a3e-e2e "$s" pair
pair 141989f72b63 "$e2e" 141989f72b63-e2e "$i" pair
pair "3ac0f1f0ca6e(control)" "$e2e" 3ac0f1f0ca6e-e2e "$s" control
pair "9164e75b4c3c(control)" "$e2e" 9164e75b4c3c-e2e "$s" control
pair "append(e2e)" "$e2e" append-e2e-index "$i" pair
pair "append(e2e,control)" "$e2e" append-e2e-same "$i" control
pair 0019fc59433e "$k8s" 0019fc59433e-k8s "$i" pair
pair 600bb5490bb2 "$k8s" 600bb5490bb2-k8s "$i" pair
pair 3264771a1719 "$k8s" 3264771a1719-k8s "$i" pair
pair 4209950d9193 "$k8s" 4209950d9193-k8s "$i" pair
pair f22898635c73 "$k8s" f22898635c73-k8s "$i" pair
pair "b1d8a21b1e8f(evidence)" "$k8s" b1d8a21b1e8f-k8s "$s" evidence
pair "2fc4b487c8fa(evidence)" "$k8s" 2fc4b487c8fa-k8s "$s" evidence
pair "e0ab76dcd45f(control)" "$k8s" e0ab76dcd45f-k8s "$s" control
pair "702806bf4da7(control)" "$k8s" 702806bf4da7-k8s "$s" control
pair "eceefebffe3f(control)" "$k8s" eceefebffe3f-k8s "$s" control
pair "append(k8s)" "$k8s" append-k8s-tools "$i" pair
pair "append(k8s,control)" "$k8s" append-k8s-same "$i" control

[ "$fails" -eq 0 ] || exit 1
