# shellcheck shell=bash disable=SC2154,SC2034 # the variables below are the caller's (listed above op)
# The namespace rig of a stage proof that runs pods in a shared cluster namespace: everything the
# run creates carries its own project label (legion.dev/project), the teardown deletes by exact name
# every Sandbox the run recorded and then everything labelled with that exact project — never by
# label existence — and namespace-clean compares the namespace with the snapshot taken before the
# run, restricted to objects of this run or of no project, so another tree's objects cannot fail it.
# Nothing outside the namespace is touched.
#
# Sourced, never run. The caller sets
#   operator         the admin kubectl context the operator steps use
#   namespace        the namespace the run works in
#   project          the run's project label, which starts with project_prefix
#   project_prefix   the prefix every run project of this proof carries (e.g. s4a-): the teardown
#                    refuses any other, so a mistyped project can never select another run's objects
#   record           a file holding one Sandbox name per line, each deleted by name
#   work, evidence   the run's scratch and evidence directories
#   torn_down, compared   empty
# and defines begin, note, pass and fail (which exits).

op() { kubectl --context "$operator" -n "$namespace" "$@"; }

# snapshot FILE: the namespace's Sandboxes, Secrets, PVCs, and pods that carry this run's project
# label or no project label at all, as the operator sees them.
snapshot() {
  {
    op get sandboxes,secrets,pvc,pods -l '!legion.dev/project' -o name
    op get sandboxes,secrets,pvc,pods -l "legion.dev/project=$project" -o name
  } | sort >"$1"
}

# teardown: this run's objects, and nothing else, gone. Never fails; runs once.
teardown() {
  [ -z "$torn_down" ] || return 0
  torn_down=1
  echo "== teardown"
  case "$project" in
    "$project_prefix"?*) ;;
    *)
      echo "   refused: the run project '$project' lacks the reserved prefix $project_prefix"
      return 0
      ;;
  esac
  local name left i failures=0 swept=
  if [ -s "$record" ]; then
    while read -r name; do
      op delete sandbox "$name" --ignore-not-found --wait=false || true
    done < <(sort -u "$record")
  fi
  op delete sandboxes -l "legion.dev/project=$project" --ignore-not-found --wait=false || true
  for i in $(seq 1 150); do
    if ! left=$(op get sandboxes,secrets,pvc,pods -l "legion.dev/project=$project" -o name 2>"$work/teardown.err"); then
      # A transient API error is waited out; three in a row mean the context cannot answer.
      failures=$((failures + 1))
      if [ "$failures" -ge 3 ]; then
        echo "   [operator] context $operator cannot list project $project's objects; they may remain: $(cat "$work/teardown.err")"
        return 0
      fi
      sleep 2
      continue
    fi
    failures=0
    [ -n "$left" ] || break
    # Secrets and PVCs the Sandboxes' own deletion has not taken by the 90th listing are deleted by
    # label, once, after the first listing from then on that answers.
    if [ "$i" -ge 90 ] && [ -z "$swept" ]; then
      op delete secrets,pvc -l "legion.dev/project=$project" --ignore-not-found --wait=false || true
      swept=1
    fi
    sleep 2
  done
  echo "   [operator] left of project $project: ${left:-nothing}"
  return 0
}

namespace_clean() {
  compared=1
  begin namespace-clean
  snapshot "$evidence/namespace-after.txt" || fail "the operator could not list namespace $namespace"
  if ! diff -u "$evidence/namespace-before.txt" "$evidence/namespace-after.txt"; then
    fail "namespace $namespace differs from its snapshot (restricted to project $project and unlabelled objects)"
  fi
  note "[operator] $(wc -l <"$evidence/namespace-after.txt") objects before and after, identical (unlabelled or project $project; sandboxes, secrets, pvc, pods)"
  pass
}
