# shellcheck shell=bash
# Shell helpers the generators in the directory above source. docs/site/scripts/generate.ts runs no
# file below generators/, so nothing here runs on its own.

# fence prints the code fence for the text on stdin: one backtick more than its longest run of
# backticks, and at least three, so no line of the text can close the block early.
fence() {
  local longest
  longest=$(awk '{ while (match($0, /`+/)) { if (RLENGTH > n) n = RLENGTH; $0 = substr($0, RSTART + RLENGTH) } } END { print n + 0 }')
  [ "$longest" -ge 3 ] || longest=2
  printf '%*s' $((longest + 1)) '' | tr ' ' '`'
}
