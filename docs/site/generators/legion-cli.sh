#!/usr/bin/env bash
# Writes Legion's CLI reference, <content dir>/legion/reference/cli.md, from the `legion` binary on
# PATH (docs/site/scripts/build-binaries.sh builds the Legion daemon's Go CLI and puts it there).
# `legion --help` lists the commands; each command's `--help` is its section, and a command whose
# usage line names subcommands (`usage: legion claims close|deliver|…`) gets one section per
# subcommand, from that subcommand's own `--help`, at any depth. Nothing here names a command: a
# command added to the binary appears in the reference on the next build.
#
#   docs/site/generators/legion-cli.sh <content dir>
#
# Every help is run with a scrubbed environment (no LEGION_* variable, a scratch HOME, stdin from
# /dev/null), so no command can find a grant, a daemon or a workspace to act on. A help that exits
# with anything but 0 or does not begin with a usage line (`usage: legion …`, or the flag
# package's `Usage of legion …`), or a `legion` whose --help lists no commands, fails the build:
# no page is published from a binary it cannot read, and none shows a panic or an error.
set -euo pipefail
# bash 5.2 reads `&` in a ${var//pattern/replacement} as the match; escape() needs it literal.
shopt -u patsub_replacement 2>/dev/null || true

me=${0##*/}
fail() {
  echo "$me: $*" >&2
  exit 1
}

[ $# -eq 1 ] || {
  echo "usage: $me <content dir>" >&2
  exit 2
}
content=$1
[ -d "$content" ] || fail "$content is not a directory"
legion=$(command -v legion) || fail "no legion on PATH; build it first (docs/site/scripts/build-binaries.sh)"

scratch=$(mktemp -d "${TMPDIR:-/tmp}/legion-cli-reference.XXXXXXXX")
trap 'rm -rf "$scratch"' EXIT

# help runs `legion <args…>` as nothing but a help reader and prints what it wrote, both streams.
# It must exit 0, and when its last argument is --help, print a usage.
help() {
  local code=0 output
  output=$(env -i PATH="$PATH" HOME="$scratch" LANG=C.UTF-8 timeout 30 "$legion" "$@" </dev/null 2>&1) || code=$?
  [ "$code" = 0 ] || fail "legion $* exited $code: $output"
  if [ "${*: -1}" = --help ]; then
    case $output in
    "usage: legion "* | "Usage of legion "*) ;;
    *) fail "legion $* printed no usage: $output" ;;
    esac
  fi
  printf '%s\n' "$output"
}

# escape makes text safe in a Markdown paragraph or table cell: `<team>` would be read as HTML.
escape() {
  local text=$1
  text=${text//&/&amp;}
  text=${text//</&lt;}
  text=${text//>/&gt;}
  text=${text//|/\\|}
  printf '%s' "$text"
}

# fence prints the code fence for the text on stdin: one backtick more than its longest run of
# backticks, and at least three, so no line of the text can close the block early.
fence() {
  local longest
  longest=$(awk '{ while (match($0, /`+/)) { if (RLENGTH > n) n = RLENGTH; $0 = substr($0, RSTART + RLENGTH) } } END { print n + 0 }')
  [ "$longest" -ge 3 ] || longest=2
  printf '%*s' $((longest + 1)) '' | tr ' ' '`'
}

top=$(help --help)
case $top in
"usage: legion <command> [flags]"*) ;;
*) fail "the legion on PATH ($legion) is not the Legion daemon's Go CLI: its --help does not begin 'usage: legion <command> [flags]'" ;;
esac

names=()
summaries=()
listing=0
while IFS= read -r line; do
  if [ "$line" = "Commands:" ]; then
    listing=1
    continue
  fi
  [ "$listing" = 1 ] || continue
  [ -n "$line" ] || break
  if [[ $line =~ ^\ \ ([a-z][a-z-]*)\ +(.*)$ ]]; then
    names+=("${BASH_REMATCH[1]}")
    summaries+=("${BASH_REMATCH[2]}")
  else
    fail "unexpected line in legion --help's command list: $line"
  fi
done <<<"$top"
[ "${#names[@]}" -gt 0 ] || fail "legion --help lists no commands"

# subcommands prints, one per line, the subcommands the usage lines of <help> name for <path>: the
# first word after `legion <path>` on a usage line, split at `|`, when it is a bare word rather than
# an argument (`<team>`), an option (`[--workspace …]`, `--`) or nothing.
subcommands() {
  local path=$1 text=$2 line word
  local pattern="^[[:space:]]*(usage:[[:space:]]+)?legion ${path} ([a-z][a-z-]*(\|[a-z][a-z-]*)*)( |$)"
  while IFS= read -r line; do
    if [[ $line =~ $pattern ]]; then
      for word in ${BASH_REMATCH[2]//|/ }; do
        printf '%s\n' "$word"
      done
    fi
  done <<<"$text"
}

out=$content/legion/reference
mkdir -p "$out"
page=$out/cli.md

# section writes the section of `legion <path…>` at heading <level>, then its subcommands' one
# level down. Each level's help must name the longer path for the next, so the recursion ends
# where the binary's subcommands do.
section() {
  local level=$1
  shift
  local path="$*" text sub hashes ticks
  hashes=$(printf '%*s' "$level" '' | tr ' ' '#')
  text=$(help "$@" --help)
  ticks=$(fence <<<"$text")
  {
    printf '%s legion %s\n\n' "$hashes" "$path"
    printf '%stext\n%s\n%s\n\n' "$ticks" "$text" "$ticks"
  } >>"$page"
  while IFS= read -r sub; do
    [ -n "$sub" ] || continue
    section $((level + 1)) "$@" "$sub"
  done < <(subcommands "$path" "$text")
}

version=$(help version)
{
  cat <<'EOF'
---
title: CLI reference
description: Every legion command, subcommand and flag, generated from the binary's own help.
---

EOF
  # shellcheck disable=SC2016 # the backticks are Markdown code spans, not command substitutions
  printf 'Generated when the site was built, from `legion --help` and each command'"'"'s `--help` (`%s`). Run `legion <command> --help` for the same text from the binary you have.\n\n' "$version"
  printf '| Command | What it does |\n| --- | --- |\n'
  for i in "${!names[@]}"; do
    # shellcheck disable=SC2016 # the backticks are a Markdown code span, not a command substitution
    printf '| [`legion %s`](#legion-%s) | %s |\n' "${names[$i]}" "${names[$i]}" "$(escape "${summaries[$i]}")"
  done
  printf '\n'
} >"$page"
for name in "${names[@]}"; do
  section 2 "$name"
done

echo "$me: wrote $page (${#names[@]} commands, from $legion)"
