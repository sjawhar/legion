#!/usr/bin/env bash
# INFRA-113 Docker-free production-like rig.
#
# Usage: run.sh <envoy-package-dir> <listener-binary-source-dir> [workdir]
#   $1  the packages/envoy whose infra module renders the authorization (the grant under test)
#   $2  the packages/envoy the listener binary is built from (same dir normally; an
#       origin/main checkout for the negative control)
#   $3  scratch dir, default a fresh mktemp -d
#
# It renders the hub's authorization, runs a real nats-server, runs the real listener, subscribes
# a credential-free core-NATS client, and publishes three topics through POST /v1/messages/publish.
# Everything it writes stays under the scratch dir. It kills both servers on exit.
set -euo pipefail

ENVOY_INFRA_SRC=$(cd -- "$1" && pwd)
ENVOY_LISTENER_SRC=$(cd -- "$2" && pwd)
WORK=${3:-$(mktemp -d /tmp/infra113-rig.XXXXXX)}
mkdir -p "$WORK/mod/sub" "$WORK/store"
cd "$WORK"

cat > mod/go.mod <<EOF
module rig

go 1.27.1

require (
	github.com/nats-io/nats.go v1.50.0
	github.com/nats-io/nkeys v0.4.15
	github.com/sjawhar/envoy/infra v0.0.0
)

replace github.com/sjawhar/envoy/infra => $ENVOY_INFRA_SRC/infra

replace github.com/sjawhar/envoy => $ENVOY_INFRA_SRC
EOF

cat > mod/main.go <<'EOF'
// Command rig renders the hub's nats-server authorization and the listener's environment.
// Arguments: <outdir> <machineID> <listenerPort> <natsURL>.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/nats-io/nkeys"
	"github.com/sjawhar/envoy/infra/core"
)

func user() (seed, public string, err error) {
	u, err := nkeys.CreateUser()
	if err != nil {
		return "", "", err
	}
	raw, err := u.Seed()
	if err != nil {
		return "", "", err
	}
	if public, err = u.PublicKey(); err != nil {
		return "", "", err
	}
	return string(raw), public, nil
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: rig <outdir> <machineID> <listenerPort> <natsURL>")
		os.Exit(2)
	}
	out, machineID, natsURL := os.Args[1], os.Args[2], os.Args[4]
	port, err := strconv.Atoi(os.Args[3])
	check(err)

	seed, public, err := user()
	check(err)
	_, sessionKey, err := user()
	check(err)

	listener, session := core.ListenerPermissions(machineID), core.SessionPermissions()
	config, err := core.Authorization{Users: []core.User{
		{Name: machineID, NKey: public, Publish: &listener.Publish, Subscribe: &listener.Subscribe, HasSeed: true},
		{Name: "example-sessions", NKey: sessionKey, Publish: &session.Publish, Subscribe: &session.Subscribe},
	}, NoAuthUser: "example-sessions"}.Render(nil)
	check(err)

	env, err := core.ListenerConfig{MachineID: machineID, Port: port}.Env([]string{natsURL})
	check(err)
	env = append(env, core.EnvNATSSeed+"="+seed)

	check(os.WriteFile(out+"/nats.conf", []byte(config), 0o600))
	envText := ""
	for _, kv := range env {
		envText += kv + "\n"
	}
	check(os.WriteFile(out+"/listener.env", []byte(envText), 0o600))
	fmt.Print(envText)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "rig:", err)
		os.Exit(1)
	}
}
EOF

cat > mod/sub/main.go <<'EOF'
// Command sub subscribes as the credential-free session user and prints what it receives.
// Arguments: <natsURL> <subject>...
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: sub <natsURL> <subject>...")
		os.Exit(2)
	}
	nc, err := natsgo.Connect(os.Args[1], natsgo.Name("rig-subscriber"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer nc.Close()
	for _, subject := range os.Args[2:] {
		if _, err := nc.Subscribe(subject, func(m *natsgo.Msg) {
			fmt.Printf("%s RECEIVED subject=%s bytes=%d body=%s\n",
				time.Now().UTC().Format(time.RFC3339Nano), m.Subject, len(m.Data), string(m.Data))
		}); err != nil {
			fmt.Fprintln(os.Stderr, "subscribe", subject, ":", err)
			os.Exit(1)
		}
		fmt.Printf("%s SUBSCRIBED subject=%s\n", time.Now().UTC().Format(time.RFC3339Nano), subject)
	}
	if err := nc.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "flush:", err)
		os.Exit(1)
	}
	fmt.Println("READY")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}
EOF

echo "== build"
(cd mod && go mod tidy >/dev/null && go build -o "$WORK/rig" . && go build -o "$WORK/subscriber" ./sub)
(cd "$ENVOY_LISTENER_SRC" && go build -o "$WORK/listener" ./cmd/listener)

read -r NATS MON LIS < <(python3 - <<'PY'
import socket
socks = [socket.socket() for _ in range(3)]
for s in socks:
    s.bind(("127.0.0.1", 0))
print(" ".join(str(s.getsockname()[1]) for s in socks))
for s in socks:
    s.close()
PY
)
echo "== ports nats=$NATS monitor=$MON listener=$LIS"

echo "== render authorization"
./rig "$WORK" example-host-a "$LIS" "nats://127.0.0.1:$NATS"

echo "== nats-server ${NATS_VERSION:=v2.15.1}"
nohup go run "github.com/nats-io/nats-server/v2@$NATS_VERSION" \
	-c "$WORK/nats.conf" -js --store_dir "$WORK/store" -p "$NATS" -m "$MON" \
	>"$WORK/nats.log" 2>&1 &
NATS_PID=$!
LISTENER_PID=""
SUB_PID=""
trap 'pkill -P $NATS_PID 2>/dev/null; kill $NATS_PID ${LISTENER_PID:-} ${SUB_PID:-} 2>/dev/null' EXIT
for _ in $(seq 100); do
	grep -q "Server is ready" "$WORK/nats.log" && break
	sleep 0.5
done
grep -q "Server is ready" "$WORK/nats.log" || { echo "nats-server did not start"; cat "$WORK/nats.log"; exit 1; }

echo "== listener"
nohup env -i PATH="$PATH" HOME="$WORK" $(tr '\n' ' ' < "$WORK/listener.env") \
	"$WORK/listener" >"$WORK/listener.log" 2>&1 &
LISTENER_PID=$!
for _ in $(seq 120); do
	[ "$(curl -s -m 3 "http://127.0.0.1:$LIS/healthz" | jq -r '.status // empty')" = healthy ] && break
	sleep 0.5
done
test "$(curl -s -m 3 "http://127.0.0.1:$LIS/healthz" | jq -r .status)" = healthy
echo "healthz: healthy"

echo "== subscriber (credential-free session user)"
nohup "$WORK/subscriber" "nats://127.0.0.1:$NATS" \
	notifications.team.deploy-squad.all notifications.github.probe.all >"$WORK/sub.log" 2>&1 &
SUB_PID=$!
for _ in $(seq 60); do grep -q READY "$WORK/sub.log" && break; sleep 0.5; done
grep -q READY "$WORK/sub.log" || { echo "subscriber did not start"; cat "$WORK/sub.log"; exit 1; }

# kv_messages reports how many messages the role-claim KV bucket holds, read from the server's own
# monitoring port. A publish outside notifications. lands there today; after the fix it is refused,
# so this count must not move.
kv_messages() {
	curl -s -m 5 "http://127.0.0.1:$MON/jsz?streams=1" |
		jq -r '[.account_details[]?.stream_detail[]? | select(.name == "KV_envoy_roles") | .state.messages] | add // 0'
}

echo "== KV_envoy_roles before: $(kv_messages) message(s)"

echo "== publish"
for T in notifications.team.deploy-squad.all notifications.github.probe.all '$KV.envoy_roles.example-role'; do
	printf '%s ' "topic=$T"
	curl -sS -o "$WORK/body.json" -w 'http=%{http_code} time_total=%{time_total}s ' -m 60 \
		-X POST "http://127.0.0.1:$LIS/v1/messages/publish" \
		-H 'Content-Type: application/json' \
		--data "$(jq -nc --arg t "$T" '{topic:$t,message:("INFRA-113 rig probe to "+$t),source:"agent"}')"
	echo "body=$(cat "$WORK/body.json")"
done

sleep 1
echo "== KV_envoy_roles after: $(kv_messages) message(s)"
echo "== subscriber received"
grep RECEIVED "$WORK/sub.log" || echo "(nothing)"
echo "== nats-server violations"
grep -E "Violation" "$WORK/nats.log" || echo "(none)"
echo "== workdir $WORK"
