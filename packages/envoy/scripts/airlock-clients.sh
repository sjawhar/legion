#!/usr/bin/env bash
# packages/envoy/scripts/airlock-clients.sh
#
# The clients checks k6 and k7 of the airlock's verification document
# (dispatch://INFRA-1352/artifact/verification-md) drive, which a Legion issue pod does not have:
# the Azure CLI, the Google Cloud CLI, Pulumi with the Tailscale provider, and boto3. Each is
# installed at a pinned version whose SHA-256 is checked before anything of it runs, under
# ${TMPDIR:-/tmp}, and the script prints the exports a check needs:
#
#   eval "$(packages/envoy/scripts/airlock-clients.sh)"
#
# Everything it says about itself goes to stderr and every line of stdout is an export, so the
# line above is the whole of how a caller uses it. scripts/airlock-check.sh runs it that way for
# k6 and k7.
#
# A second run finds what the first installed and fetches nothing: each client is installed
# beside a stamp file holding the version and the digest this script pins, and a stamp that
# matches is the install. --force throws the stamps away and fetches again.
#
# Every fetch is a download with a checksum, which is this repository's one toolchain pattern
# (.github/actions/install-elan/action.yml, packages/daemon/docker/worker.Dockerfile): the bytes
# are written to a temporary name, their SHA-256 is compared with the digest pinned below, and
# only then are they unpacked or installed. A digest that does not match leaves nothing behind
# and ends the run.
#
# The two Python clients are pinned the same way rather than by `uv run --with <package>==<ver>`:
# that form pins the version but checks no digest here, and this script's whole point is that
# the bytes are checked before they run. The wheel of each is fetched and checked, then installed
# into an environment of its own with uv (`uv venv`, `uv pip install <the checked wheel>`), so the
# client itself is the pinned and checked artifact. Its dependencies are resolved by uv from that
# wheel's own requirements.
#
# Measured in this pod: it reaches get.pulumi.com, dl.google.com, pypi.org (and
# files.pythonhosted.org behind it), github.com and api.github.com, and uv is on PATH.
#
# Options:
#   --dir DIR   install under DIR rather than ${TMPDIR:-/tmp}/airlock-clients
#   --force     fetch and install again even where a stamp says it is there
#   --help      print this and exit
# A usage error exits 2.
set -euo pipefail

# --- What is pinned. Each client is a version, a URL and the SHA-256 of what that URL answers. ---
PULUMI_VERSION="3.274.1"
PULUMI_URL="https://get.pulumi.com/releases/sdk/pulumi-v${PULUMI_VERSION}-linux-x64.tar.gz"
PULUMI_SHA256="32ae6d09d0df51637db7a0affb3ceb7d0511093b22d477aff5f20214d0b7e471"

# The Tailscale provider plugin, which check k7's `pulumi preview` calls tailscale:getDevices
# through. Pulumi downloads a missing plugin itself at preview time; this script installs it from
# a checked tarball so that no byte of it arrives unchecked.
TAILSCALE_PLUGIN_VERSION="0.29.4"
TAILSCALE_PLUGIN_URL="https://github.com/pulumi/pulumi-tailscale/releases/download/v${TAILSCALE_PLUGIN_VERSION}/pulumi-resource-tailscale-v${TAILSCALE_PLUGIN_VERSION}-linux-amd64.tar.gz"
TAILSCALE_PLUGIN_SHA256="062a90a678ef2868880567b259174d7d7c24105e6b56d4d3518dacca2002f1ad"

GCLOUD_VERSION="592.0.0"
GCLOUD_URL="https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-${GCLOUD_VERSION}-linux-x86_64.tar.gz"
GCLOUD_SHA256="2d841a23a681ab94486af6841e5f1cd123cd300fb8c4be6ba5b51350fca3d50b"

AZURE_CLI_VERSION="2.92.0"
AZURE_CLI_URL="https://files.pythonhosted.org/packages/66/50/0e0d681e761eab74995375f2a0ac254e1e5bd1f5204d34f8a31c0bd087d3/azure_cli-${AZURE_CLI_VERSION}-py3-none-any.whl"
AZURE_CLI_SHA256="7257ab8c949e3d412fc220686d2adce56e72f5190c6ffa2799c91441a3210291"

BOTO3_VERSION="1.43.125"
BOTO3_URL="https://files.pythonhosted.org/packages/b5/12/1e295c564a8319e6e27353c4c849ec31b0e3857b2614c36c7cc050ca1b9c/boto3-${BOTO3_VERSION}-py3-none-any.whl"
BOTO3_SHA256="101fe3ee99664e671efb9af65c8e98ec0bceddf6de922ea89685da4adc8c6907"

# Python requests, which check k7 drives beside urllib as one of its plain clients. It goes into
# the same environment as boto3: both are libraries a check imports, not commands.
REQUESTS_VERSION="2.34.2"
REQUESTS_URL="https://files.pythonhosted.org/packages/a0/f4/c67b0b3f1b9245e8d266f0f112c500d50e5b4e83cb6f3b71b6528104182a/requests-${REQUESTS_VERSION}-py3-none-any.whl"
REQUESTS_SHA256="2a0d60c172f83ac6ab31e4554906c0f3b3588d37b5cb939b1c061f4907e278e0"
usage() {
	cat <<'EOF'
usage: airlock-clients.sh [--dir DIR] [--force]

Installs the clients checks k6 and k7 drive — az, gcloud, pulumi with the Tailscale provider,
and boto3 — each at a pinned version whose SHA-256 is checked before anything of it runs, and
prints the exports a check needs on stdout:

  eval "$(packages/envoy/scripts/airlock-clients.sh)"

  --dir DIR   install under DIR (default ${TMPDIR:-/tmp}/airlock-clients)
  --force     fetch and install again even where a stamp says it is there
  --help      print this

A second run finds what the first installed and fetches nothing. A usage error exits 2.
EOF
}

ROOT="${AIRLOCK_CLIENTS_DIR:-${TMPDIR:-/tmp}/airlock-clients}"
FORCE=false
while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		[ $# -ge 2 ] || {
			echo "airlock-clients: --dir needs a directory" >&2
			exit 2
		}
		ROOT="$2"
		shift
		;;
	--force) FORCE=true ;;
	--help | -h)
		usage
		exit 0
		;;
	*)
		echo "airlock-clients: unexpected argument $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

for needed in curl tar sha256sum uv python3; do
	command -v "$needed" >/dev/null 2>&1 || {
		echo "airlock-clients: $needed is needed and is not on PATH" >&2
		exit 1
	}
done

mkdir -p "$ROOT" "$ROOT/bin" "$ROOT/downloads"
CACHE_DIR="$ROOT/uv-cache"
PULUMI_HOME="$ROOT/pulumi-home"
mkdir -p "$CACHE_DIR" "$PULUMI_HOME"
export UV_CACHE_DIR="$CACHE_DIR"

# --- The two steps every client is installed by -----------------------------------------------
#
# installed answers whether this client's stamp already names this version and digest, which is
# what makes a second run fetch nothing; stamp writes that stamp once the install is whole, so a
# run killed halfway leaves no stamp and the next run does the work again.

installed() { # <name> <version> <sha256> <a path that must exist>
	[ "$FORCE" = false ] || return 1
	[ -e "$4" ] || return 1
	[ -f "$ROOT/$1.installed" ] || return 1
	[ "$(cat "$ROOT/$1.installed")" = "$2 $3" ]
}

stamp() { # <name> <version> <sha256>
	printf '%s %s\n' "$2" "$3" >"$ROOT/$1.installed"
}

# fetch downloads one URL and answers its path, having checked its SHA-256 against the digest
# pinned above. The bytes land under a temporary name and are moved into place only once they
# have been checked, so nothing unchecked is ever at the name the install reads; a download that
# is already there and already matches is kept, which is what makes a second run fetch nothing.
fetch() { # <url> <sha256> <file name> -> FETCHED
	local url="$1" want="$2" name="$3" path="$ROOT/downloads/$3"
	if [ -f "$path" ] && [ "$(sha256sum <"$path" | cut -d' ' -f1)" = "$want" ]; then
		echo "airlock-clients: $name is already downloaded and its SHA-256 matches" >&2
		FETCHED="$path"
		return 0
	fi
	echo "airlock-clients: downloading $url" >&2
	curl -fsSL --retry 3 -o "$path.part" "$url"
	local got
	got="$(sha256sum <"$path.part" | cut -d' ' -f1)"
	if [ "$got" != "$want" ]; then
		rm -f "$path.part"
		echo "airlock-clients: $name is not what this script pins: its SHA-256 is $got and the pinned digest is $want" >&2
		exit 1
	fi
	mv "$path.part" "$path"
	echo "airlock-clients: $name checked (sha256 $got)" >&2
	FETCHED="$path"
}

# --- Pulumi, and the Tailscale provider plugin k7 previews with -------------------------------

PULUMI_DIR="$ROOT/pulumi-$PULUMI_VERSION"
if installed pulumi "$PULUMI_VERSION" "$PULUMI_SHA256" "$PULUMI_DIR/pulumi/pulumi"; then
	echo "airlock-clients: pulumi $PULUMI_VERSION is installed ($PULUMI_DIR/pulumi/pulumi)" >&2
else
	fetch "$PULUMI_URL" "$PULUMI_SHA256" "pulumi-v$PULUMI_VERSION-linux-x64.tar.gz"
	rm -rf "$PULUMI_DIR"
	mkdir -p "$PULUMI_DIR"
	tar -xzf "$FETCHED" -C "$PULUMI_DIR"
	stamp pulumi "$PULUMI_VERSION" "$PULUMI_SHA256"
	echo "airlock-clients: pulumi $PULUMI_VERSION installed" >&2
fi
ln -sfn "$PULUMI_DIR/pulumi/pulumi" "$ROOT/bin/pulumi"

# A plugin lives under PULUMI_HOME/plugins/resource-<name>-<version>, with a .partial file while
# it is being written; writing the binary and then removing nothing is how `pulumi plugin install
# --file` leaves it, and laying it out here keeps the install one checked tarball and no network
# call of Pulumi's own.
PLUGIN_DIR="$PULUMI_HOME/plugins/resource-tailscale-$TAILSCALE_PLUGIN_VERSION"
if installed pulumi-tailscale "$TAILSCALE_PLUGIN_VERSION" "$TAILSCALE_PLUGIN_SHA256" "$PLUGIN_DIR/pulumi-resource-tailscale"; then
	echo "airlock-clients: the Pulumi Tailscale provider $TAILSCALE_PLUGIN_VERSION is installed ($PLUGIN_DIR)" >&2
else
	fetch "$TAILSCALE_PLUGIN_URL" "$TAILSCALE_PLUGIN_SHA256" "pulumi-resource-tailscale-v$TAILSCALE_PLUGIN_VERSION-linux-amd64.tar.gz"
	rm -rf "$PLUGIN_DIR"
	mkdir -p "$PLUGIN_DIR"
	tar -xzf "$FETCHED" -C "$PLUGIN_DIR"
	chmod +x "$PLUGIN_DIR/pulumi-resource-tailscale"
	stamp pulumi-tailscale "$TAILSCALE_PLUGIN_VERSION" "$TAILSCALE_PLUGIN_SHA256"
	echo "airlock-clients: the Pulumi Tailscale provider $TAILSCALE_PLUGIN_VERSION installed" >&2
fi

# --- The Google Cloud CLI ----------------------------------------------------------------------
#
# The tarball is run where it is unpacked: its own install.sh writes a profile and offers to send
# usage data, and neither belongs in a check's environment. CLOUDSDK_PYTHON names the interpreter,
# which this tarball (the one without a bundled Python) needs.

GCLOUD_DIR="$ROOT/google-cloud-cli-$GCLOUD_VERSION"
if installed gcloud "$GCLOUD_VERSION" "$GCLOUD_SHA256" "$GCLOUD_DIR/google-cloud-sdk/bin/gcloud"; then
	echo "airlock-clients: gcloud $GCLOUD_VERSION is installed ($GCLOUD_DIR/google-cloud-sdk/bin/gcloud)" >&2
else
	fetch "$GCLOUD_URL" "$GCLOUD_SHA256" "google-cloud-cli-$GCLOUD_VERSION-linux-x86_64.tar.gz"
	rm -rf "$GCLOUD_DIR"
	mkdir -p "$GCLOUD_DIR"
	tar -xzf "$FETCHED" -C "$GCLOUD_DIR"
	stamp gcloud "$GCLOUD_VERSION" "$GCLOUD_SHA256"
	echo "airlock-clients: gcloud $GCLOUD_VERSION installed" >&2
fi
ln -sfn "$GCLOUD_DIR/google-cloud-sdk/bin/gcloud" "$ROOT/bin/gcloud"

# --- The Azure CLI and boto3, each from a checked wheel into an environment of its own ----------

AZURE_DIR="$ROOT/azure-cli-$AZURE_CLI_VERSION"
if installed azure-cli "$AZURE_CLI_VERSION" "$AZURE_CLI_SHA256" "$AZURE_DIR/bin/az"; then
	echo "airlock-clients: az $AZURE_CLI_VERSION is installed ($AZURE_DIR/bin/az)" >&2
else
	fetch "$AZURE_CLI_URL" "$AZURE_CLI_SHA256" "azure_cli-$AZURE_CLI_VERSION-py3-none-any.whl"
	rm -rf "$AZURE_DIR"
	uv venv --quiet "$AZURE_DIR" >&2
	uv pip install --quiet --python "$AZURE_DIR/bin/python" "$FETCHED" >&2
	stamp azure-cli "$AZURE_CLI_VERSION" "$AZURE_CLI_SHA256"
	echo "airlock-clients: az $AZURE_CLI_VERSION installed" >&2
fi
ln -sfn "$AZURE_DIR/bin/az" "$ROOT/bin/az"

PYTHON_DIR="$ROOT/python-boto3-$BOTO3_VERSION"
if installed boto3 "$BOTO3_VERSION" "$BOTO3_SHA256" "$PYTHON_DIR/bin/python"; then
	echo "airlock-clients: boto3 $BOTO3_VERSION is installed ($PYTHON_DIR/bin/python)" >&2
else
	fetch "$BOTO3_URL" "$BOTO3_SHA256" "boto3-$BOTO3_VERSION-py3-none-any.whl"
	rm -rf "$PYTHON_DIR"
	uv venv --quiet "$PYTHON_DIR" >&2
	uv pip install --quiet --python "$PYTHON_DIR/bin/python" "$FETCHED" >&2
	stamp boto3 "$BOTO3_VERSION" "$BOTO3_SHA256"
	echo "airlock-clients: boto3 $BOTO3_VERSION installed" >&2
fi

if installed requests "$REQUESTS_VERSION" "$REQUESTS_SHA256" "$PYTHON_DIR/bin/python"; then
	echo "airlock-clients: requests $REQUESTS_VERSION is installed (in $PYTHON_DIR)" >&2
else
	fetch "$REQUESTS_URL" "$REQUESTS_SHA256" "requests-$REQUESTS_VERSION-py3-none-any.whl"
	uv pip install --quiet --python "$PYTHON_DIR/bin/python" "$FETCHED" >&2
	stamp requests "$REQUESTS_VERSION" "$REQUESTS_SHA256"
	echo "airlock-clients: requests $REQUESTS_VERSION installed" >&2
fi

# --- What a caller exports ----------------------------------------------------------------------
#
# stdout is exports and nothing else. AIRLOCK_BOTO3_PYTHON rather than anything on PATH: boto3
# and requests are libraries, and what a check needs of them is the interpreter that imports them.

cat <<EOF
export AIRLOCK_CLIENTS_DIR=$ROOT
export PATH=$ROOT/bin:\$PATH
export CLOUDSDK_PYTHON=$(command -v python3)
export PULUMI_HOME=$PULUMI_HOME
export UV_CACHE_DIR=$CACHE_DIR
export AIRLOCK_BOTO3_PYTHON=$PYTHON_DIR/bin/python
export AIRLOCK_PULUMI_TAILSCALE_VERSION=$TAILSCALE_PLUGIN_VERSION
EOF

cat >&2 <<EOF
airlock-clients: ready.
  pulumi               $PULUMI_VERSION      sha256 $PULUMI_SHA256
  pulumi-tailscale     $TAILSCALE_PLUGIN_VERSION       sha256 $TAILSCALE_PLUGIN_SHA256
  gcloud               $GCLOUD_VERSION     sha256 $GCLOUD_SHA256
  azure-cli            $AZURE_CLI_VERSION      sha256 $AZURE_CLI_SHA256
  boto3                $BOTO3_VERSION    sha256 $BOTO3_SHA256
  requests             $REQUESTS_VERSION       sha256 $REQUESTS_SHA256
  everything is under $ROOT; the exports are on stdout.
EOF
