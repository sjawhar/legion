#!/usr/bin/env bash
# packages/envoy/scripts/airlock-lean-axioms.sh
#
# Holds the airlock model's theorems to the axioms Lean's own kernel admits, and is the one copy
# of that check: ci.yaml's airlock-lean job runs it, and so does check k15 of airlock-check.sh,
# so what the model must rest on is written here once.
#
# `#print axioms` names, per theorem, the axioms its proof rests on. propext, Classical.choice
# and Quot.sound are Lean's own three, which its kernel admits; any other name is an assumption
# this model added, and a `sorry` prints sorryAx. Fewer theorems printed than Axioms.lean names
# means one was renamed, dropped or no longer compiles, which would otherwise leave the caller
# printing nothing and passing.
#
# It runs `lake env lean Axioms.lean` in the Lean project directory it is given and writes what
# that printed to its standard output, so that a reader of either log sees the axioms. What is
# wrong goes to standard error and exits 1: an axiom outside the three, a sorryAx, a count of
# printed theorems other than the number of `#print axioms` lines in Axioms.lean, or a
# `lake env lean` that failed. --axioms-file keeps a copy of the axiom text for a caller that
# reads it back, which is what k15 does for its result block.
#
# Usage: airlock-lean-axioms.sh <lean project directory> [--axioms-file <path>]
#        --help prints this usage. A usage error exits 2. It needs lake from the Lean toolchain
#        this environment names.
set -uo pipefail

usage() {
	cat <<'EOF'
usage: airlock-lean-axioms.sh <lean project directory> [--axioms-file <path>]

Runs `lake env lean Axioms.lean` in that directory, prints what it printed, and exits non-zero
naming any axiom outside propext, Classical.choice and Quot.sound, any sorryAx, and a count of
printed theorems other than the number of `#print axioms` lines in Axioms.lean.

  <lean project directory>  where lakefile.toml and Axioms.lean are
  --axioms-file <path>      keep a copy of the axiom text here as well
  --help                    print this

It exits 0 when every theorem rests on those three alone, 1 when one does not, and 2 on a usage
error.
EOF
}

LEAN_DIR=""
AXIOMS_FILE=""
while [ $# -gt 0 ]; do
	case "$1" in
	--axioms-file)
		if [ $# -lt 2 ]; then
			echo "airlock-lean-axioms: --axioms-file wants a path" >&2
			usage >&2
			exit 2
		fi
		AXIOMS_FILE="$2"
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	-*)
		echo "airlock-lean-axioms: unexpected option $1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ -n "$LEAN_DIR" ]; then
			echo "airlock-lean-axioms: one directory at a time, not $LEAN_DIR and $1" >&2
			usage >&2
			exit 2
		fi
		LEAN_DIR="$1"
		;;
	esac
	shift
done

if [ -z "$LEAN_DIR" ]; then
	echo "airlock-lean-axioms: name the Lean project directory" >&2
	usage >&2
	exit 2
fi
if [ ! -f "$LEAN_DIR/Axioms.lean" ]; then
	echo "airlock-lean-axioms: $LEAN_DIR holds no Axioms.lean" >&2
	exit 2
fi

# How many theorems to expect: one per `#print axioms` line of Axioms.lean, counted rather than
# written down, so the theorems are named in one place and a theorem added there is one this
# check holds the model to. A file naming none would pass every check below without a theorem
# ever being read, so it is refused instead.
EXPECTED_THEOREMS="$(grep -c '^#print axioms ' "$LEAN_DIR/Axioms.lean")"
if [ "$EXPECTED_THEOREMS" = 0 ]; then
	echo "airlock-lean-axioms: $LEAN_DIR/Axioms.lean names no theorem to print the axioms of" >&2
	exit 2
fi

AXIOMS="$(mktemp "${TMPDIR:-/tmp}/airlock-axioms.XXXXXX")"
trap 'rm -f "$AXIOMS"' EXIT

(cd "$LEAN_DIR" && lake env lean Axioms.lean) >"$AXIOMS" 2>&1
LAKE_STATUS=$?
cat "$AXIOMS"
if [ -n "$AXIOMS_FILE" ]; then
	cat "$AXIOMS" >"$AXIOMS_FILE"
fi

BAD=0
if [ "$LAKE_STATUS" -ne 0 ]; then
	echo "lake env lean Axioms.lean exited $LAKE_STATUS" >&2
	BAD=1
fi
# One line per theorem, opening with its name in quotes, and the axioms between the brackets.
if ! awk -F'[][]' -v expected="$EXPECTED_THEOREMS" '
	/^.Airlock\./ {
		printed++
		split($2, names, ", ")
		for (i in names) {
			if (names[i] != "propext" && names[i] != "Classical.choice" && names[i] != "Quot.sound") {
				print "axiom outside the three: " names[i]
				bad = 1
			}
		}
	}
	END {
		if (printed != expected) {
			print "theorems printed: " printed + 0 ", expected " expected
			bad = 1
		}
		exit bad
	}' "$AXIOMS" >&2; then
	BAD=1
fi
if grep -q sorryAx "$AXIOMS"; then
	echo "a theorem rests on sorryAx, so a sorry is still in its proof" >&2
	BAD=1
fi

exit "$BAD"
