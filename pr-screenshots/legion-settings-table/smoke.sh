#!/usr/bin/env bash
# Throwaway smoke for LEGION-488: drives one envoy-dispatch binary through the settings its boot,
# serving and subcommand paths show, and prints what it observed with times, paths and the run's
# label normalised away, so a build of main and a build of the branch can be diffed.
# Usage: smoke.sh <binary> <label> <nats url> <dispatch port> <listener port> <postgres port>
set -uo pipefail
bin=$1 label=$2 natsurl=$3 port=$4 lport=$5 pgport=$6
scratch=/tmp/buildsettings.UhRg/smoke-$label
rm -rf "$scratch"
mkdir -p "$scratch/dist"
printf 'SMOKE-DIST index\n' >"$scratch/dist/index.html"
db="postgres://postgres:postgres@127.0.0.1:$pgport/smoke_$label?sslmode=disable"
origin="http://127.0.0.1:$port"
listenerlog=/tmp/buildsettings.UhRg/listener.log
: >"$listenerlog"

normalise() {
	sed -E -e 's/^time=[^ ]+ //' -e "s#$scratch#<scratch>#g" -e "s/smoke_$label/smoke_X/g" -e 's/"commit":[^,}]*/"commit":<c>/'
}

# home <name> <envoy.json>: a fresh HOME holding that envoy.json.
home() {
	local dir="$scratch/home-$1"
	mkdir -p "$dir/.config/opencode"
	printf '%s' "$2" >"$dir/.config/opencode/envoy.json"
	printf '%s' "$dir"
}

# boot <name> <envoy.json> <VAR=value>...: a boot that should refuse; prints its exit and first error.
boot() {
	local name=$1 json=$2
	shift 2
	local dir out code
	dir=$(home "$name" "$json")
	out=$(cd "$scratch" && env -i PATH="$PATH" HOME="$dir" "$@" timeout 30 "$bin" 2>&1)
	code=$?
	echo "[boot $name] exit=$code $(printf '%s\n' "$out" | grep -m1 -E 'level=ERROR|panic' | normalise)"
}

# sub <name> <envoy.json> <args...> -- <VAR=value>...: a subcommand's exit and output.
sub() {
	local name=$1 json=$2
	shift 2
	local args=()
	while [[ $1 != -- ]]; do
		args+=("$1")
		shift
	done
	shift
	local dir out code
	dir=$(home "$name" "$json")
	out=$(cd "$scratch" && env -i PATH="$PATH" HOME="$dir" "$@" timeout 60 "$bin" "${args[@]}" 2>&1)
	code=$?
	echo "[sub $name] exit=$code"
	printf '%s\n' "$out" | grep -v -E '^(time=.*level=(INFO|WARN)|[0-9a-f-]{36} )' | normalise | sed 's/^/    /'
}

# serve <name> <envoy.json> <VAR=value>...: starts the server, waits for /healthz, leaves its pid in
# $server and its HOME in $serverhome.
serve() {
	local name=$1 json=$2
	shift 2
	serverhome=$(home "$name" "$json")
	(cd "$scratch" && exec env -i PATH="$PATH" HOME="$serverhome" "$@" "$bin" >"$scratch/$name.log" 2>&1) &
	server=$!
	for _ in $(seq 1 150); do
		curl -fsS "$origin/healthz" >/dev/null 2>&1 && break
		sleep 0.2
	done
	echo "[serve $name] healthz $(curl -sS "$origin/healthz" | normalise)"
}

stop() {
	kill "$server"
	wait "$server" 2>/dev/null
	echo "    boot log: $(grep -E 'loaded github app|no app credentials|NATS publisher disabled|webhook redelivery off|dev sign-in mounted|listening' "$scratch/$1.log" | sed -E 's/^time=[^ ]+ //' | normalise | tr '\n' '|')"
}

cookies() { # the Set-Cookie attributes a response carries, values dropped
	grep -i '^set-cookie:' | sed -E 's/^[Ss]et-[Cc]ookie: ([^=]+)=[^;]*/\1=<v>/' | tr -d '\r' | sort | tr '\n' '|'
}

base=(DATABASE_URL="$db" DISPATCH_AGENT_TOKEN=agent-token DISPATCH_ALLOWED_LOGINS=alice)
local_server=(DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port" DISPATCH_SERVER_URL="$origin")
listener=(ENVOY_URL="http://127.0.0.1:$lport" ENVOY_TOKEN=listener-token)
filejson='{"natsUrls":["nats://127.0.0.1:1"],"dispatch":{"serverUrl":"https://from-envoy-json.example"}}'

echo "== boot refusals"
boot no-env '{}'
boot no-token '{}' DATABASE_URL="$db"
boot no-logins '{}' DATABASE_URL="$db" DISPATCH_AGENT_TOKEN=agent-token
boot header-untrusted '{}' "${base[@]}" DISPATCH_IDENTITY=header:X-User DISPATCH_APP_CLIENT_ID=Iv1.x
boot bad-port '{}' "${base[@]}" DISPATCH_PORT=70000
boot bad-identity '{}' "${base[@]}" DISPATCH_IDENTITY=basic
boot empty-server-url "$filejson" "${base[@]}" DISPATCH_SERVER_URL=
boot empty-nats-urls "$filejson" "${base[@]}" NATS_URLS=
boot remote-nats '{}' "${base[@]}" NATS_URLS=nats://nats.example:4222
boot remote-nats-from-envoy-json '{"natsUrls":["nats://nats.example:4222"]}' "${base[@]}"
boot bad-seed '{}' "${base[@]}" NATS_URLS=nats://127.0.0.1:1 NATS_NKEY_SEED=not-a-seed
boot seed-file-wins '{}' "${base[@]}" NATS_URLS=nats://127.0.0.1:1 NATS_NKEY_SEED=not-a-seed NATS_NKEY_SEED_FILE=/nonexistent/seed
boot empty-seed-file '{}' "${base[@]}" NATS_URLS=nats://127.0.0.1:1 NATS_NKEY_SEED_FILE=
boot remote-allowed '{}' "${base[@]}" ENVOY_ALLOW_REMOTE_NATS=1 NATS_URLS=nats://nats.example:4222 NATS_NKEY_SEED=not-a-seed
boot app-without-secret '{}' "${base[@]}" DISPATCH_NATS_DISABLED=1 DISPATCH_APP_CLIENT_ID=Iv1.x
boot app-bad-pem '{}' "${base[@]}" DISPATCH_NATS_DISABLED=1 DISPATCH_APP_CLIENT_ID=Iv1.x DISPATCH_APP_CLIENT_SECRET=s DISPATCH_APP_PEM_B64='%%%'
boot app-bad-id '{}' "${base[@]}" DISPATCH_NATS_DISABLED=1 DISPATCH_APP_CLIENT_ID=Iv1.x DISPATCH_APP_CLIENT_SECRET=s DISPATCH_APP_ID=abc
boot broker-without-token '{}' "${base[@]}" DISPATCH_AGENT_SECRETS_URL=https://broker.example
boot broker-token-file-missing '{}' "${base[@]}" DISPATCH_AGENT_SECRETS_URL=https://broker.example DISPATCH_AGENT_SECRETS_TOKEN=bare DISPATCH_AGENT_SECRETS_TOKEN_FILE=/nonexistent/token
boot oidc-half '{}' "${base[@]}" DISPATCH_OIDC_ISSUER=https://issuer.example
boot dev-signin-with-key '{}' "${base[@]}" "${local_server[@]}" DISPATCH_DEV_SIGNIN=1 DISPATCH_SIGNING_KEY=k
boot dev-signin-remote-allowed '{}' "${base[@]}" "${local_server[@]}" DISPATCH_DEV_SIGNIN=1 ENVOY_ALLOW_REMOTE_NATS=1
boot dev-signin-origin-from-envoy-json "$filejson" "${base[@]}" DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port" DISPATCH_NATS_DISABLED=1 DISPATCH_DEV_SIGNIN=1

echo "== subcommands"
sub settings '{}' settings --
sub census '{}' census -- DATABASE_URL="$db"
sub census-no-url '{}' census --
sub rebuild-refs-origin-from-env "$filejson" rebuild-refs -- DATABASE_URL="$db" DISPATCH_SERVER_URL="$origin"
sub rebuild-refs-no-origin '{}' rebuild-refs -- DATABASE_URL="$db"
sub redeliver-remote '{}' redeliver-webhooks --since 1h -- NATS_URLS=nats://nats.example:4222
sub redeliver-no-key "$filejson" redeliver-webhooks --since 1h -- NATS_URLS="$natsurl" DISPATCH_APP_CLIENT_ID=Iv1.x DISPATCH_APP_CLIENT_SECRET=s

echo "== dev sign-in server, NATS off"
serve dev-signin "$filejson" "${base[@]}" "${local_server[@]}" "${listener[@]}" DISPATCH_NATS_DISABLED=1 DISPATCH_DEV_SIGNIN=1 DISPATCH_WEB_DIST="$scratch/dist"
echo "    GET /: $(curl -sS "$origin/")"
echo "    sign-in cookies: $(curl -sS -o /dev/null -D - "$origin/auth/_dev/signin?login=alice&next=/" | cookies)"
echo "    GET /api/v1/agents: $(curl -sS -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer agent-token' "$origin/api/v1/agents")"
echo "    listener saw: $(tr '\n' '|' <"$listenerlog")"
: >"$listenerlog"
stop dev-signin

serve dev-signin-insecure "$filejson" "${base[@]}" "${local_server[@]}" "${listener[@]}" DISPATCH_NATS_DISABLED=1 DISPATCH_DEV_SIGNIN=1 DISPATCH_INSECURE_COOKIE=1 DISPATCH_WEB_DIST="$scratch/dist"
echo "    sign-in cookies: $(curl -sS -o /dev/null -D - "$origin/auth/_dev/signin?login=alice&next=/" | cookies)"
stop dev-signin-insecure

echo "== server on NATS with an App from the environment"
serve nats-app "$filejson" "${base[@]}" "${local_server[@]}" "${listener[@]}" NATS_URLS="$natsurl" DISPATCH_SIGNING_KEY=table-key \
	DISPATCH_APP_CLIENT_ID=Iv1.smoke DISPATCH_APP_CLIENT_SECRET=s DISPATCH_APP_SLUG=smoke-app DISPATCH_WEB_DIST="$scratch/dist"
start=$(curl -sS -o /dev/null -D - "$origin/auth/start?next=/")
echo "    /auth/start redirect_uri: $(printf '%s\n' "$start" | grep -i '^location:' | grep -o 'redirect_uri=[^&]*' | tr -d '\r')"
echo "    /auth/start cookies: $(printf '%s\n' "$start" | cookies)"
echo "    signing-key file: $(ls "$serverhome/.local/share/dispatch/signing-key" 2>&1 | normalise)"
stop nats-app

serve nats-app-insecure "$filejson" "${base[@]}" "${local_server[@]}" NATS_URLS="$natsurl" DISPATCH_INSECURE_COOKIE=1 \
	DISPATCH_APP_CLIENT_ID=Iv1.smoke DISPATCH_APP_CLIENT_SECRET=s DISPATCH_WEB_DIST="$scratch/dist"
echo "    /auth/start cookies: $(curl -sS -o /dev/null -D - "$origin/auth/start?next=/" | cookies)"
echo "    signing-key file: $(ls "$serverhome/.local/share/dispatch/signing-key" 2>&1 | normalise)"
stop nats-app-insecure
