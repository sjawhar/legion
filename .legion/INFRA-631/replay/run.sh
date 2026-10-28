#!/usr/bin/env bash
# Replays real conflicting merges of packages/envoy/AGENTS.md and packages/envoy/cmd/dispatch/README.md
# on their new layout (INFRA-631), and the appended-items demo, with `git merge-tree --write-tree`.
#
#   bash .legion/INFRA-631/replay/run.sh [repo root]
#
# The repo root defaults to the checkout this script sits in (three directories above it). The two
# files are read from its git history at REPLAY_BASE (default 3c583c55, this pull request's
# regeneration of main 2df3353c's files, which the patches were written against), committed as the
# base of a throwaway repository under ${TMPDIR:-/tmp}/replay-$$ (set TMPDIR to keep it off /tmp),
# and each patch beside this script is applied with `git apply` on a branch of its own from that
# base. A later regeneration on a newer main lays the files out by the same rules, so the patches
# are not ported again each time main moves. For every pair the script prints its classification,
# computed from the two patches' hunks against the base file, then one line: merge, file, class,
# expected, observed, PASS or FAIL. It exits 1 on any FAIL.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root=$(cd -- "${1:-$here/../../..}" && pwd)
pin=${REPLAY_BASE:-3c583c55614c7082387a52fdf4070ce52c91639b}
agents=packages/envoy/AGENTS.md
readme=packages/envoy/cmd/dispatch/README.md

work=${TMPDIR:-/tmp}/replay-$$
mkdir -p "$work"
trap 'rm -rf -- "$work"' EXIT
git init -q "$work"
g() { git -C "$work" -c user.name=replay -c user.email=replay@example.invalid "$@"; }
for f in "$agents" "$readme"; do
  mkdir -p "$work/$(dirname "$f")"
  git -C "$root" show "$pin:$f" > "$work/$f"
done
g add -A
g commit -q -m base
g branch -M base

# One branch per patch, from base.
for p in "$here"/*.patch; do
  name=$(basename "$p" .patch)
  g checkout -q -b "$name" base
  g apply "$p"
  g commit -q -a -m "$name"
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
        d = x[2] - y[2]; if (d < 0) d = -d
        if (x[1] == "I" && y[1] == "I" && d == 0) same = same " " x[2]
        else if (d <= 1) near = near " ours " x[1] x[2] "~theirs " y[1] y[2] ";"
      }
      detail = "ours changes " ranges(kind[1, "C"]) " inserts after " ranges(kind[1, "I"]) \
        "; theirs changes " ranges(kind[2, "C"]) " inserts after " ranges(kind[2, "I"])
      if (same != "") print "same place\t" detail "; both insert after" same
      else if (near != "") print "same or adjacent\t" detail "; colliding (C changed, I inserts after):" near
      else print apart "\t" detail
    }
  ' <(edits "$1") <(edits "$2")
}

fails=0
# pair <name> <file> <ours patch> <theirs patch> <class when apart> <control: yes|no>
pair() {
  local name=$1 file=$2 ours=$3 theirs=$4 apart=$5 control=$6 class detail expected tree hunks observed verdict
  IFS=$'\t' read -r class detail < <(classify "$here/$ours.patch" "$here/$theirs.patch" "$apart")
  if [ "$control" = yes ] || [ "$class" != "$apart" ]; then expected=conflict; else expected=clean; fi
  if tree=$(g merge-tree --write-tree "$ours" "$theirs" 2>/dev/null); then
    observed=clean
  else
    hunks=$(g show "$(printf '%s\n' "$tree" | head -n 1):$file" | grep -c '^<<<<<<< ' || true)
    observed="conflict ($hunks hunks)"
  fi
  if [ "${observed%% *}" = "$expected" ]; then verdict=PASS; else verdict=FAIL; fails=$((fails + 1)); fi
  printf '  classification: %s\n' "$detail"
  printf '%s  %s  class=%s  expected=%s  observed=%s  %s\n' "$name" "$file" "$class" "$expected" "$observed" "$verdict"
}

s="different sentences"
i="different items"
pair 5e45356e68aa "$agents" 5e45356e68aa-ours 5e45356e68aa-theirs "$s" no
pair 4671996e14fd "$agents" 4671996e14fd-ours 4671996e14fd-theirs "$s" no
pair 5aee84d6930e "$agents" 5aee84d6930e-ours 5aee84d6930e-theirs "$s" no
pair 56d31191f8f1 "$readme" 56d31191f8f1-ours 56d31191f8f1-theirs "$s" no
pair 125a3bfb0484 "$readme" 125a3bfb0484-ours 125a3bfb0484-theirs "$s" no
pair 949ebe03bd4e "$readme" 949ebe03bd4e-ours 949ebe03bd4e-theirs "$s" no
pair "728b5e297fb5(control)" "$agents" 728b5e297fb5-ours 728b5e297fb5-theirs "$s" yes
pair "4440fc7b57a2(control)" "$readme" 4440fc7b57a2-ours 4440fc7b57a2-theirs "$s" yes
pair "append(a)" "$readme" append-readme-routes-a append-readme-routes-b "$i" no
pair "append(b)" "$agents" append-agents-where-a append-agents-where-b "$i" no
pair "append(c,control)" "$readme" append-readme-same-a append-readme-same-b "$i" yes

[ "$fails" -eq 0 ] || exit 1
