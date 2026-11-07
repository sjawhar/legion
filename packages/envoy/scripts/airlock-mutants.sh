#!/usr/bin/env bash
# packages/envoy/scripts/airlock-mutants.sh
#
# Proves that the airlock's differential fuzz test and the Lean proofs catch a defect in the
# decision code, which is the whole claim behind a model that is only as good as the test
# tying it to the Go functions. Each file of internal/broker/airlock/testdata/mutants is one
# defect as a unified diff. For each, this script copies packages/envoy to a fresh directory
# under $TMPDIR, applies that one patch there with patch -p1, and requires the copy to fail:
#
#   - a Go mutant (go-*.patch) must fail the comparison fuzz test over the committed seed
#     corpus, run as `go test -run FuzzAirlock ./internal/broker/airlock/` with AIRLOCK_MODEL
#     naming the model this checkout already built;
#   - a model mutant (lean-*.patch) must fail `lake build` in the copy, because a proof no
#     longer holds, or, when it still builds, fail that same comparison against the mutated
#     model. The line says which of the two caught it.
#
# A mutant nothing catches is a hole in the test or in the proofs, and this script fails on
# it. The patches are never applied to this repository: every one is applied inside the copy,
# and the run ends with jj status on the repository to show it is untouched.
#
# Usage: packages/envoy/scripts/airlock-mutants.sh [<name>...]
#        with no name, every mutant of testdata/mutants; a name is a patch's file name, with
#        or without the .patch suffix (for example go-carries-value).
#
# Needs go, GNU patch or git (the patches are plain unified diffs, and `git apply -p1`
# applies them where patch is not installed), and, for the model mutants, lake from the Lean
# toolchain this environment names (ELAN_HOME and PATH are taken as they are); a missing
# lake exits 2. One PASS: or FAIL: line per mutant, and a non-zero exit when any mutant was
# not caught.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(cd "$ENVOY_DIR/../.." && pwd)"
AIRLOCK_PKG_DIR="$ENVOY_DIR/internal/broker/airlock"
MUTANT_DIR="$AIRLOCK_PKG_DIR/testdata/mutants"
MODEL="$AIRLOCK_PKG_DIR/lean/.lake/build/bin/airlock-model"

usage() {
	cat <<'EOF'
usage: airlock-mutants.sh [<name>...]

Applies each mutant of internal/broker/airlock/testdata/mutants to a copy of packages/envoy
under $TMPDIR and requires that copy to fail: a go-*.patch must fail the comparison fuzz test
over the committed seed corpus, and a lean-*.patch must fail `lake build`, because a proof no
longer holds, or that same comparison against the mutated model.

  <name>   a patch's file name, with or without the .patch suffix (for example
           go-carries-value); with no name, every mutant of testdata/mutants

No patch is ever applied to this repository. It prints one PASS: or FAIL: line per mutant and
a last line saying how many were caught, exits non-zero when a mutant was not caught, and
exits 2 when go, lake or both patch and git are missing.
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
esac

if ! command -v go >/dev/null 2>&1; then
	echo "airlock-mutants: go is not on PATH" >&2
	exit 2
fi
if ! command -v patch >/dev/null 2>&1 && ! command -v git >/dev/null 2>&1; then
	echo "airlock-mutants: neither patch nor git is on PATH, so no mutant can be applied" >&2
	exit 2
fi

# apply_patch applies one mutant inside the copy, never in the repository. GNU patch is the
# tool the patches are written for; git apply reads the same unified diff, and is what this
# script uses where patch is not installed.
apply_patch() {
	local copy="$1" patch_file="$2" log="$3"
	if command -v patch >/dev/null 2>&1; then
		patch -p1 -s --batch --forward -d "$copy" -i "$patch_file" >"$log" 2>&1
	else
		(cd "$copy" && git apply -p1 "$patch_file") >"$log" 2>&1
	fi
}

# The mutants to run: the names given, else every patch of testdata/mutants, in name order.
mutants=()
if [[ $# -gt 0 ]]; then
	for name in "$@"; do
		name="${name%.patch}"
		if [[ ! -f "$MUTANT_DIR/$name.patch" ]]; then
			echo "airlock-mutants: no mutant named $name in $MUTANT_DIR" >&2
			exit 2
		fi
		mutants+=("$name")
	done
else
	for patch_file in "$MUTANT_DIR"/*.patch; do
		mutants+=("$(basename "$patch_file" .patch)")
	done
fi

needs_lake=false
needs_model=false
for name in "${mutants[@]}"; do
	case "$name" in
	lean-*) needs_lake=true ;;
	*) needs_model=true ;;
	esac
done

if [[ "$needs_lake" == true ]] && ! command -v lake >/dev/null 2>&1; then
	echo "airlock-mutants: lake is not on PATH, so the model mutants cannot be built." >&2
	echo "  Put the Lean toolchain on PATH (ELAN_HOME/bin), or name only the go-* mutants." >&2
	exit 2
fi
if [[ "$needs_model" == true && ! -x "$MODEL" ]]; then
	echo "airlock-mutants: no built model at $MODEL" >&2
	echo "  Build it: (cd $AIRLOCK_PKG_DIR/lean && lake build)" >&2
	exit 2
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/airlock-mutants.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

# copy_envoy writes a copy of packages/envoy into <dest>/packages/envoy, laid out as in the
# repository so the patches' own a/packages/envoy/... paths apply with patch -p1. A Go mutant
# takes no copy of the Lean build directory: it compares against the model this checkout
# already built, and .lake is the biggest thing in the tree.
copy_envoy() {
	local dest="$1" with_lake="$2"
	mkdir -p "$dest/packages"
	if [[ "$with_lake" == true ]]; then
		tar -C "$ENVOY_DIR" -cf - . | tar -C "$dest/packages" -xf - --one-top-level=envoy
	else
		tar -C "$ENVOY_DIR" -cf - --exclude=./internal/broker/airlock/lean/.lake . |
			tar -C "$dest/packages" -xf - --one-top-level=envoy
	fi
}

# compare runs the comparison fuzz test over the seed corpus in <copy>, against <model>, and
# answers 0 when the two sides disagreed, 1 when they agreed, 2 when the test did not run.
compare() {
	local copy="$1" model="$2" log="$3"
	if (cd "$copy/packages/envoy" && GOWORK=off AIRLOCK_MODEL="$model" \
		go test -count=1 -run FuzzAirlock ./internal/broker/airlock/) >"$log" 2>&1; then
		return 1
	fi
	# A non-zero exit is the mutant caught only when the comparison itself failed; a copy
	# that does not build or a model that does not answer is a broken run, not a catch.
	if grep -q -- '--- FAIL: FuzzAirlock' "$log"; then
		return 0
	fi
	return 2
}

# failing_seed names the first corpus seed the comparison failed on, for the PASS: line.
failing_seed() {
	sed -n 's|^ *--- FAIL: FuzzAirlock/\([^ ]*\).*|\1|p' "$1" | head -n 1
}

failures=0
for name in "${mutants[@]}"; do
	started=$SECONDS
	copy="$WORK_DIR/$name"
	log="$WORK_DIR/$name.log"
	case "$name" in
	lean-*) copy_envoy "$copy" true ;;
	*) copy_envoy "$copy" false ;;
	esac
	if ! apply_patch "$copy" "$MUTANT_DIR/$name.patch" "$log"; then
		echo "FAIL: $name — the patch does not apply: $(head -n 3 "$log" | tr '\n' ' ')"
		failures=$((failures + 1))
		continue
	fi

	if [[ "$name" == lean-* ]]; then
		if ! (cd "$copy/packages/envoy/internal/broker/airlock/lean" && lake build) >"$log" 2>&1; then
			reason="$(grep -m1 -E 'error:' "$log" | cut -c1-120)"
			echo "PASS: $name — caught by lake build, a proof no longer holds: ${reason:-see the build log} ($((SECONDS - started))s)"
			continue
		fi
		model="$copy/packages/envoy/internal/broker/airlock/lean/.lake/build/bin/airlock-model"
	else
		model="$MODEL"
	fi

	set +e
	compare "$copy" "$model" "$log"
	verdict=$?
	set -e
	case "$verdict" in
	0) echo "PASS: $name — caught by the comparison fuzz test on seed $(failing_seed "$log") ($((SECONDS - started))s)" ;;
	1)
		echo "FAIL: $name — nothing caught it: the mutated code still agrees with the model ($((SECONDS - started))s)"
		failures=$((failures + 1))
		;;
	*)
		echo "FAIL: $name — the comparison did not run: $(tail -n 3 "$log" | tr '\n' ' ')"
		failures=$((failures + 1))
		;;
	esac
done

echo
if [[ $failures -gt 0 ]]; then
	echo "$failures of ${#mutants[@]} mutants were not caught"
else
	echo "${#mutants[@]} mutants, each caught"
fi

# Last, the assurance that this run changed nothing here: every patch was applied inside
# WORK_DIR, which the EXIT trap removes, and never in the repository.
echo "The repository itself was never patched; jj -R $REPO_DIR status --no-pager:"
if command -v jj >/dev/null 2>&1; then
	jj -R "$REPO_DIR" status --no-pager
else
	echo "  jj is not on PATH; every patch was applied under $WORK_DIR, never in $REPO_DIR."
fi

[[ $failures -eq 0 ]]
