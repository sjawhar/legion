#!/bin/sh
# Writes <out>: the license notices of every third-party Go module the named packages compile in,
# and of the Go standard library and runtime every Go program links. Run it from the directory the
# packages are built from, with the GOOS, GOARCH and CGO_ENABLED the build uses, since they decide
# which files, and so which modules, a program compiles in:
#
#   scripts/go-third-party-notices.sh <out> <package>...
#   scripts/go-third-party-notices.sh --install   # only build go-licenses, for an early image layer
#
# The modules are read with go-licenses (GO_LICENSES_VERSION below), which takes as a module's
# license file the nearest one whose license it can identify. It fails, writing nothing, when a
# module has none, or when go-licenses classes its license as forbidden or of an unknown type. The
# repository's own modules (every module `go list -m` names from here) are left out: they are this
# repository's code.
#
# POSIX sh, so it runs in the golang:*-alpine build stages as well as on a runner.
set -eu

GO_LICENSES_VERSION=v2.0.1

if [ "${1-}" != --install ] && [ "$#" -lt 2 ]; then
  echo "usage: scripts/go-third-party-notices.sh <out> <package>... | --install" >&2
  exit 2
fi

# The toolchain's own go, never a wrapper on PATH: a version manager's shim can rewrite GOBIN.
go_cmd="$(go env GOROOT)/bin/go"
go_version="$("$go_cmd" env GOVERSION)"
goroot="$("$go_cmd" env GOROOT)"

# go-licenses runs on this machine whatever GOOS the packages target, so it is built for the host
# (empty GOOS and GOARCH), outside any module, once per version and toolchain.
tool_dir="${XDG_CACHE_HOME:-$HOME/.cache}/legion/go-licenses-$GO_LICENSES_VERSION-$go_version"
tool="$tool_dir/go-licenses"
if [ ! -x "$tool" ]; then
  (cd / && GOOS='' GOARCH='' GOFLAGS='' GOBIN="$tool_dir" \
    "$go_cmd" install "github.com/google/go-licenses/v2@$GO_LICENSES_VERSION")
fi
if [ "$1" = --install ]; then
  exit 0
fi
out=$1
shift

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
# One --ignore per module path; module paths hold no whitespace, so the split is the list.
# shellcheck disable=SC2046
set -- "$@" $("$go_cmd" list -m -f '--ignore={{.Path}}')

# Exits 1, naming the library, for one with no identifiable license file or a forbidden or unknown
# license type.
if ! "$tool" check "$@" 2>"$scratch/check.log"; then
  cat "$scratch/check.log" >&2
  echo "go-third-party-notices: a module's license could not be determined or is not allowed (above)" >&2
  exit 1
fi

tab="$(printf '\t')"
printf '{{range .}}{{.Name}}\t{{.Version}}\t{{.LicenseName}}\t{{.LicensePath}}\n{{end}}' \
  >"$scratch/report.tmpl"
if ! "$tool" report "$@" --template "$scratch/report.tmpl" >"$scratch/report.tsv" \
  2>"$scratch/report.log"; then
  cat "$scratch/report.log" >&2
  exit 1
fi

# One row per module, the licenses its license file holds joined, sorted by module.
awk -F "$tab" -v OFS="$tab" '
  !($1 in path) { order[++n] = $1; version[$1] = $2; path[$1] = $4; licenses[$1] = $3; next }
  { licenses[$1] = licenses[$1] ", " $3 }
  END { for (i = 1; i <= n; i++) print order[i], version[order[i]], licenses[order[i]], path[order[i]] }
' "$scratch/report.tsv" | LC_ALL=C sort >"$scratch/modules.tsv"

rule="$(printf '%080d' 0 | tr 0 =)"
# A module's license file, then each NOTICE file beside it, which Apache-2.0 requires passing on,
# each without its leading and trailing blank lines.
section() {
  printf '\n%s\n\n%s\nLicense: %s\n' "$rule" "$1" "$2"
  for file in "$3" "$(dirname "$3")/NOTICE" "$(dirname "$3")/NOTICE.txt" "$(dirname "$3")/NOTICE.md"; do
    [ -f "$file" ] || continue
    printf '\n--- %s ---\n\n' "$(basename "$file")"
    awk 'NF == 0 { if (started) blank++; next } { for (; blank > 0; blank--) print ""; print; started = 1 }' "$file"
  done
}

{
  echo "Third-party software compiled into the Go programs these notices ship with, with each module's license."
  echo "Each module's source is published at the version shown under its module path."
  section "Go standard library and runtime $go_version" "BSD-3-Clause" "$goroot/LICENSE"
  while IFS="$tab" read -r name version licenses path; do
    section "$name@$version" "$licenses" "$path"
  done <"$scratch/modules.tsv"
} >"$scratch/notices"

mv "$scratch/notices" "$out"
