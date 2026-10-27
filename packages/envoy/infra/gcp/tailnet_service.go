package gcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
)

// The parts of a Tailscale Service host: two or more pods, each its own tailnet node registered
// under its pod's name, all hosting one Tailscale Service (svc:<name>), so the Service's name
// answers while any one pod serves. Each pod runs Tailscale's containerboot in a container of its
// own (the tailscale container) and, beside it, an advertiser: a script on the same Tailscale
// image that advertises the Service only when this pod should host it, writes the ready file the
// tailscale container's readiness probe reads, and takes the node out of the Service again when
// it should not. The tailscale container's preStop hook takes the node out of the Service and
// off the tailnet before the pod goes. Every Tailscale fact below is cited from tailscale.com at
// v1.104.1.
//
// The pod's containers reach tailscaled through its LocalAPI socket on the state volume
// (TS_SOCKET; containerboot passes it to tailscaled as --socket, cmd/containerboot/tailscaled.go:70,
// and defaults it to /tmp/tailscaled.sock, settings.go:125, which no other container sees). Every
// container that calls it runs as the pod's one uid: tailscaled grants a LocalAPI caller of its own
// uid write access (ipn/ipnauth/ipnauth.go:191-194), which `tailscale serve advertise`, `serve
// drain`, `cert` and `logout` need.

// tailnetServicePaths is where a Tailscale Service host's containers find the files they share:
// everything lives on the pod's state volume, an emptyDir mounted at mount in every container
// that touches it. The scripts, the environment and the probes are rendered from one, so the
// Deployment and a test that runs the scripts spell every file the same way; a test roots one in
// its own directory.
type tailnetServicePaths struct {
	mount string // the state volume's mount point, the same in every container of the pod
}

// stateDir is tailscaled's state directory (TS_STATE_DIR and TS_LOGS_DIR): a subdirectory of the
// mount and never the mount point itself. tailscaled chmods a state directory named `tailscale`
// to 0700 before it opens its state (paths/paths_unix.go:74-90) and, when the chmod fails, falls
// back to an in-memory store. A volume's mount point is the kubelet's (root), so the pod's uid
// cannot chmod it; a subdirectory the pod's uid creates is its own. That keeps the node's state
// (machine key, node key, certificate cache) on the emptyDir across a restart of the tailscale
// container, rather than having the restarted container register as a new node. TS_LOGS_DIR
// counts only when it is already a directory (logpolicy/logpolicy.go:210-216), so the consumer
// creates this directory before tailscaled starts, from an init container running as the pod's
// uid (the emptyDir itself is world-writable).
func (p tailnetServicePaths) stateDir() string { return p.mount + "/tailscale" }

// socket is tailscaled's LocalAPI socket, TS_SOCKET.
func (p tailnetServicePaths) socket() string { return p.mount + "/tailscaled.sock" }

// ready is the ready file: the advertiser writes it, holding the node id, once this pod's current
// node hosts the Service as tailnetAdvertiserScript says, and removes it as soon as that stops
// being true. The tailscale container's readiness probe passes only while it exists.
func (p tailnetServicePaths) ready() string { return p.mount + "/service-ready" }

// draining is written by the tailscale container's preStop hook, `<node id> <unix time>`: the node
// it drains and when it began, so the advertiser does not advertise that node again while the hook
// waits. It holds only for the pod's grace period: past that the kubelet has killed the container
// the hook ran in, so its drain is over, and the advertiser removes it.
func (p tailnetServicePaths) draining() string { return p.mount + "/draining" }

// nodeID is where the advertiser writes this pod's own node id, the Self.ID its own tailscaled
// reports (ipn/ipnstate.PeerStatus.ID, a tailcfg.StableNodeID), for the preStop hook to name in
// the draining file and for a consumer's own scripts (an advertise gate) to record.
func (p tailnetServicePaths) nodeID() string { return p.mount + "/node-id" }

// advertiseFlag is an advertise gate's flag, `<node id> <why>`: with a certificate check, the
// advertiser advertises only the node it names. The consumer's gate writes it, once this pod may
// host the Service, and puts a new node id first when the node changes.
func (p tailnetServicePaths) advertiseFlag() string { return p.mount + "/advertise-service" }

// tailnetServiceHost is one Tailscale Service host's configuration: what the tailscale container's
// environment, the advertiser, the preStop hook and the probes are rendered from.
type tailnetServiceHost struct {
	paths tailnetServicePaths
	// name is the Tailscale Service's name without its `svc:` prefix.
	name string
	// authKeySecret and authKeyKey name the Secret and key holding the nodes' auth key, TS_AUTHKEY.
	authKeySecret, authKeyKey string
	// tags are the tags every node advertises; the Service's auto-approval names them.
	tags []string
	// serveConfig is the path of the serve config (tailnetServeHTTPS or tailnetServeTCP) as the
	// tailscale container mounts it, TS_SERVE_CONFIG.
	serveConfig string
	// localAddrPort is containerboot's local HTTP address, which serves /healthz
	// (TS_LOCAL_ADDR_PORT); empty means tailnetLocalAddrPort.
	localAddrPort string
	// socks5Server, when set, is the address tailscaled's userspace SOCKS5 proxy binds,
	// TS_SOCKS5_SERVER.
	socks5Server string
	// graceSeconds is the pod's terminationGracePeriodSeconds: the age past which the advertiser
	// counts a preStop hook's draining file as over. It must cover tailnetShutdownSeconds.
	graceSeconds int
	// certificate, when set, is the Service's DNS name, <name>.<tailnet DNS name>: the advertiser
	// then advertises only the node advertiseFlag names, orders the name's certificate once the
	// node is approved, and is ready only once tailscaled holds it. Empty for a Service that
	// terminates no TLS (TCP forwards).
	certificate string
	// health, when set, is a URL the advertiser GETs every tailnetHealthPeriodSeconds: it advertises
	// after tailnetHealthRise answers of HTTP 200 in a row, and drains after tailnetHealthFall
	// failures in a row.
	health string
}

// service is the Tailscale Service's name as tailscaled and the CLI name it, svc:<name>.
func (h tailnetServiceHost) service() string { return "svc:" + h.name }

func (h tailnetServiceHost) localAddr() string {
	if h.localAddrPort == "" {
		return tailnetLocalAddrPort
	}
	return h.localAddrPort
}

// tailnetLocalAddrPort is containerboot's local HTTP address by default: loopback only, since only
// the kubelet's exec probes, run inside the container, read /healthz.
const tailnetLocalAddrPort = "127.0.0.1:9002"

// tailnetServiceEnv is the tailscale container's environment, read by containerboot.
func tailnetServiceEnv(h tailnetServiceHost) corev1.EnvVarArray {
	value := func(name, v string) corev1.EnvVarArgs {
		return corev1.EnvVarArgs{Name: pulumi.String(name), Value: pulumi.String(v)}
	}
	env := corev1.EnvVarArray{
		corev1.EnvVarArgs{Name: pulumi.String("TS_AUTHKEY"), ValueFrom: &corev1.EnvVarSourceArgs{
			SecretKeyRef: &corev1.SecretKeySelectorArgs{Name: pulumi.String(h.authKeySecret), Key: pulumi.String(h.authKeyKey)},
		}},
		// The key logs in a node with no state once; a node with state keeps it.
		value("TS_AUTH_ONCE", "true"),
		value("TS_USERSPACE", "true"),
		// Where tailscaled keeps its node and certificate cache (tailnetServicePaths.stateDir says
		// why a subdirectory of the mount). containerboot passes it as --statedir
		// (cmd/containerboot/tailscaled.go:78-79).
		value("TS_STATE_DIR", h.paths.stateDir()),
		// tailscaled's log policy keeps its log id and disk buffer here; without it tailscaled, as
		// a non-root uid, fails to create /var/lib/tailscale and its fallback cache directory, logs
		// so at every start, and keeps neither.
		value("TS_LOGS_DIR", h.paths.stateDir()),
		// Explicitly empty: no Kubernetes Secret holds a node's state. Unset, containerboot running
		// in Kubernetes stores it in Secret `tailscale` (cmd/containerboot/settings.go:114-122),
		// which every pod would share. The pods are interchangeable; each keeps its own node on its
		// emptyDir, and a new pod is a new node.
		value("TS_KUBE_SECRET", ""),
		// The pod keeps the cluster's resolver, so the serve proxy's dial resolves in-cluster names
		// through cluster DNS and never to the Tailscale Service this very pod advertises.
		value("TS_ACCEPT_DNS", "false"),
		// Each pod registers under its own pod name, so no two collide and none takes the
		// Service's name, which belongs to the Tailscale Service and not to a node.
		corev1.EnvVarArgs{Name: pulumi.String("TS_HOSTNAME"), ValueFrom: &corev1.EnvVarSourceArgs{
			FieldRef: &corev1.ObjectFieldSelectorArgs{FieldPath: pulumi.String("metadata.name")},
		}},
		value("TS_EXTRA_ARGS", "--advertise-tags="+strings.Join(h.tags, ",")),
		value("TS_SERVE_CONFIG", h.serveConfig),
		// Off: a pod advertises the Service only when the advertiser does. containerboot advertises
		// every Service of its serve config by itself unless this is false; it defaults to true
		// (cmd/containerboot/main.go:104-107, settings.go:174).
		value("TS_EXPERIMENTAL_SERVICE_AUTO_ADVERTISEMENT", "false"),
		// /healthz on the local address, which the startup probe reads.
		value("TS_ENABLE_HEALTH_CHECK", "true"),
		value("TS_LOCAL_ADDR_PORT", h.localAddr()),
		// On the state volume, where the advertiser and the probes reach it.
		value("TS_SOCKET", h.paths.socket()),
	}
	if h.socks5Server != "" {
		env = append(env, value("TS_SOCKS5_SERVER", h.socks5Server))
	}
	return env
}

// tailnetTCPForward is one port of a TCP-forwarding Service: connections to port on the Service's
// addresses are forwarded to target, host:port.
type tailnetTCPForward struct {
	port   uint16
	target string
}

// The serve config's JSON, as ipn.ServeConfig reads it (ipn/serve.go:38-49 ServiceConfig, :125-127
// WebServerConfig, :131-165 TCPPortHandler, :168 HTTPHandler): one entry under Services, keyed by
// svc:<name>. Maps encode with sorted keys and the structs in field order, so the same arguments
// always render the same bytes, and a ConfigMap built from them changes only when they do.
type (
	tailnetServeConfig struct {
		Services map[string]tailnetServeService `json:"Services"`
	}
	tailnetServeService struct {
		TCP map[string]tailnetServePort `json:"TCP,omitempty"`
		Web map[string]tailnetServeWeb  `json:"Web,omitempty"`
	}
	tailnetServePort struct {
		HTTPS      bool   `json:"HTTPS,omitempty"`
		TCPForward string `json:"TCPForward,omitempty"`
	}
	tailnetServeWeb struct {
		Handlers map[string]tailnetServeHandler `json:"Handlers"`
	}
	tailnetServeHandler struct {
		Proxy string `json:"Proxy"`
	}
)

// tailnetServeHTTPS is the serve config of a Service that terminates TLS on port 443 with its own
// name's certificate and proxies every request to proxy, an http:// URL. The Web key is the
// Service's DNS name and port, the SNI name tailscaled matches.
func tailnetServeHTTPS(name, tailnetDNSName, proxy string) string {
	return tailnetServeJSON(name, tailnetServeService{
		TCP: map[string]tailnetServePort{"443": {HTTPS: true}},
		Web: map[string]tailnetServeWeb{core.TailnetHostName(name, tailnetDNSName) + ":443": {
			Handlers: map[string]tailnetServeHandler{"/": {Proxy: proxy}},
		}},
	})
}

// tailnetServeTCP is the serve config of a Service that forwards each port's connections as they
// are: no TLS termination, so no certificate.
func tailnetServeTCP(name string, forwards []tailnetTCPForward) string {
	ports := make(map[string]tailnetServePort, len(forwards))
	for _, f := range forwards {
		ports[strconv.Itoa(int(f.port))] = tailnetServePort{TCPForward: f.target}
	}
	return tailnetServeJSON(name, tailnetServeService{TCP: ports})
}

func tailnetServeJSON(name string, service tailnetServeService) string {
	var b strings.Builder
	e := json.NewEncoder(&b)
	// A proxy URL keeps its & and its < > as written, not \u0026.
	e.SetEscapeHTML(false)
	// Strings, booleans and string-keyed maps always encode.
	_ = e.Encode(tailnetServeConfig{Services: map[string]tailnetServeService{"svc:" + name: service}})
	return strings.TrimSuffix(b.String(), "\n")
}

// Recovery from a stuck advertisement. A second `tailscale serve advertise` of a Service already
// in the node's preferences returns without a change (cmd/tailscale/cli/serve_v2.go:569-572), and
// the control server learns a node's Services from Hostinfo.ServicesHash, which tailscaled sends
// again only when that hash changes (ipn/ipnlocal/serve.go:1708-1711). The hash covers the serve
// config's Services as well as the advertised ones (serve.go:523-545), so a node names the Service
// to the control server from its registration on. A node that did so while the control server
// could not approve it (the Service did not exist yet) was never approved: it waited, advertised,
// for as long as anyone watched, until the hash moved. `tailscale serve drain` takes the Service
// out of the preferences (serve_v2.go:583-603), and a later advertise puts it back: two changes
// the control server sees.
//
// A host approved normally was approved within seconds of advertising; the stuck ones waited
// minutes or for ever. tailnetAdvertiseStuckSeconds is 120 s, many times a normal approval, so a
// slow approval is not drained away, and shorter than the minutes a stuck host took to recover by
// itself. Each recovery doubles the bound up to tailnetAdvertiseStuckMaxSeconds, so a Service that
// is absent for a long time costs one drain every 16 minutes and not one every two. The pause
// between drain and advertise, tailnetDrainPauseSeconds, lets the drained preferences reach the
// control server as an update of their own before the advertisement changes them back: two edits
// close together could reach it as one Hostinfo with the old hash, which changes nothing.
const (
	tailnetAdvertiseStuckSeconds    = 120
	tailnetAdvertiseStuckMaxSeconds = 960
	tailnetDrainPauseSeconds        = 15
)

// tailnetCertOrderAttempts bounds the certificate orders a pod makes after its node is approved:
// Let's Encrypt issues at most 5 certificates for one exact set of names in 7 days, from every
// account together (letsencrypt.org/docs/rate-limits), and every failed order of a live name
// draws on that budget. Two hosts ordering one name at once contend for its one challenge record,
// so the orders are spaced with a jittered backoff.
const tailnetCertOrderAttempts = 6

// The health gate of a host with a health URL: a GET every tailnetHealthPeriodSeconds, each given
// tailnetHealthTimeoutSeconds (wget -T, longer than the period, so a slow answer is waited for and
// not counted as a failure while it is still coming), healthy only on HTTP 200. The host is
// advertised after tailnetHealthRise healthy answers in a row and drained after tailnetHealthFall
// failures in a row, so one slow answer neither drains a serving host nor advertises a flapping
// one.
const (
	tailnetHealthPeriodSeconds  = 5
	tailnetHealthTimeoutSeconds = 6
	tailnetHealthRise           = 2
	tailnetHealthFall           = 3
)

// tailnetAdvertiserScript is the advertiser's whole program, for /bin/sh -c on the Tailscale
// image's busybox shell, as the pod's uid with the state volume mounted: one cycle per node this
// pod's tailscaled runs as, each ending in the ready file or in a fresh cycle.
//
// A cycle reads the node id from tailscaled (the first string "ID" of `tailscale status --json
// --peers=false` is Self's: ipnstate.Status lists no ID before Self, and PeerStatus.ID is its first
// field, ipn/ipnstate/ipnstate.go:50,239) and writes it to the node id file. It waits until the
// node may advertise: no preStop hook drains it, the advertise gate's flag names it (with a
// certificate), and the health URL answered tailnetHealthRise times in a row (with a health URL).
// It then advertises the Service, which containerboot is told not to do.
//
// It then watches every 5 s. The node is approved once its service-host capability lists the
// Service: the control server sends it in the self node's CapMap, as tailcfg.ServiceIPMappings
// (tailcfg/nodecap/nodecap.go:267-274, tailcfg/tailcfg.go:3111-3127), and `tailscale status
// --json` reports the self node's CapMap whole (ipn/ipnlocal/local.go:1565-1573; ipnstate
// PeerStatus.CapMap, ipn/ipnstate/ipnstate.go:312-313). The CLI itself reads it there to tell a
// Service waiting for approval from one that is available (cmd/tailscale/cli/serve_v2.go:1088-1092).
// A node unapproved past the stuck bound is drained and advertised again (tailnetAdvertiseStuckSeconds
// says why and how long).
//
// With a certificate, once approved it asks for the Service name's certificate, which tailscaled
// otherwise orders only on a client's first TLS handshake. Each failed order counts, with a
// jittered backoff, up to tailnetCertOrderAttempts; a pod whose orders are spent drains the
// Service and stays unready. An `invalid domain` answer is tailscaled refusing the name locally,
// before any order (feature/acme/cert.go:699-727), because the name is not yet among the node's
// certificate domains: it costs nothing and is not counted. The certificate goes to stdout, which
// is discarded: tailscaled's cache is the only copy written.
//
// The ready file is written once the node is approved and, with a certificate, tailscaled holds
// it. The watch removes it, and a new cycle starts, when tailscaled no longer runs as the same
// logged-in node (a re-logged-in node gets a fresh cycle under its new id), when the node may no
// longer advertise, when the node no longer advertises the Service (containerboot takes every
// Service out of the preferences whenever the tailscale container stops,
// cmd/containerboot/main.go:491-494, and a restarted container keeps its node), or, with a health
// URL, after tailnetHealthFall failures in a row, when the advertiser first drains the Service.
// A node whose approval is withdrawn loses the ready file and is watched as unapproved. No pod is
// ever deleted for any of it.
func tailnetAdvertiserScript(h tailnetServiceHost) string {
	flag := ""
	if h.certificate != "" {
		flag = h.paths.advertiseFlag()
	}
	vars := []struct {
		name, value string
	}{
		{"socket", h.paths.socket()},
		{"ready_file", h.paths.ready()},
		{"draining_file", h.paths.draining()},
		{"node_file", h.paths.nodeID()},
		{"flag_file", flag},
		{"service", h.service()},
		{"certificate", h.certificate},
		{"health", h.health},
		{"grace", strconv.Itoa(h.graceSeconds)},
		{"stuck_first", strconv.Itoa(tailnetAdvertiseStuckSeconds)},
		{"stuck_max", strconv.Itoa(tailnetAdvertiseStuckMaxSeconds)},
		{"pause", strconv.Itoa(tailnetDrainPauseSeconds)},
		{"attempts", strconv.Itoa(tailnetCertOrderAttempts)},
		{"period", strconv.Itoa(tailnetHealthPeriodSeconds)},
		{"health_timeout", strconv.Itoa(tailnetHealthTimeoutSeconds)},
		{"rise", strconv.Itoa(tailnetHealthRise)},
		{"fall", strconv.Itoa(tailnetHealthFall)},
	}
	var b strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&b, "%s=%s\n", v.name, core.ShellQuote(v.value))
	}
	return b.String() + tailnetAdvertiserBody
}

// tailnetAdvertiserBody is tailnetAdvertiserScript after its variables. approved reads the
// service-host capability from the indented JSON `tailscale status --json` prints
// (cmd/tailscale/cli/status.go:96, json.MarshalIndent with two spaces): the lines after
// `"service-host": [` down to the `]` at that line's own indent hold the mappings, and the Service
// is approved when one of them is the key `"svc:<name>": [`.
const tailnetAdvertiserBody = `set -u
at() { date -u +%Y-%m-%dT%H:%M:%SZ; }
now() { date -u +%s; }
log() { echo "$(at) $1 | $2 | $3"; }
ts() { tailscale --socket="$socket" "$@"; }
err=${TMPDIR:-/tmp}/tailnet-advertiser.err
health_out=${TMPDIR:-/tmp}/tailnet-advertiser.health
# follow sets id to Self.ID and st to the status it read, and keeps the node id file on id. It
# fails, and writes nothing, while tailscaled does not answer as a logged-in node, or names an id
# that is not alphanumeric, as no StableNodeID is.
follow() {
  id=
  st=$(ts status --json --peers=false 2>/dev/null) || return 1
  printf '%s\n' "$st" | grep -q '^[[:space:]]*"BackendState": "Running",*$' || return 1
  id=$(printf '%s\n' "$st" | sed -n 's/^[[:space:]]*"ID": "\([^"]*\)",*$/\1/p' | head -n 1)
  case "$id" in
    '') return 1 ;;
    *[!A-Za-z0-9]*) log "node id" "tailscale status --json --peers=false" "$id is not alphanumeric: not written"; id=; return 1 ;;
  esac
  was=$(cat "$node_file" 2>/dev/null)
  if [ "$id" != "$was" ]; then
    printf '%s\n' "$id" >"$node_file.tmp" && mv "$node_file.tmp" "$node_file"
    log "node id" "tailscale status --json --peers=false" "$id, was ${was:-none}: written to $node_file"
  fi
}
# draining: the draining file names this cycle's node and is younger than the grace period. One
# older than that is from a termination that is over (the kubelet has killed the container whose
# preStop hook wrote it), and is removed: else a tailscale container that came back as the same
# node, after a logout that failed, would never be advertised again. A time that is not a number
# counts as over.
draining() {
  [ -e "$draining_file" ] && read -r drained began _ <"$draining_file" || return 1
  case "$began" in ''|*[!0-9]*) began=0 ;; esac
  if [ $(($(now) - began)) -gt "$grace" ]; then
    rm -f "$draining_file"
    log "service advertisement" "rm -f $draining_file" "the preStop hook's drain of node ${drained:-none}, begun at $began, is more than ${grace}s old: over"
    return 1
  fi
  [ "$drained" = "$node" ]
}
# may: no preStop hook drains this cycle's node and, with an advertise gate, its flag names it.
may() {
  if [ -n "$flag_file" ]; then
    [ -e "$flag_file" ] && read -r flagged _ <"$flag_file" && [ "$flagged" = "$node" ] || return 1
  fi
  ! draining
}
# still: tailscaled still runs as this cycle's node, and it may advertise.
still() { follow && [ "$id" = "$node" ] && may; }
advertising() { ts debug prefs 2>/dev/null | grep -q "\"$service\""; }
# approved: the status follow last read lists the Service in the self node's service-host
# capability.
approved() {
  printf '%s\n' "$st" | awk -v want="\"$service\": [" '
    !inside && /^ *"service-host": \[$/ { inside = 1; match($0, /^ */); indent = RLENGTH; next }
    inside {
      match($0, /^ */)
      if (RLENGTH <= indent) { inside = 0; next }
      if (index(substr($0, RLENGTH + 1), want) == 1) found = 1
    }
    END { exit !found }'
}
# healthy: one GET of the health URL answered HTTP 200 (the last status line wget -S prints, after
# any redirect). Each change of answer is logged once.
last_code=unknown
healthy() {
  wget -S -O /dev/null -T "$health_timeout" "$health" >"$health_out" 2>&1
  rc=$?
  code=$(sed -n 's/^[[:space:]]*HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$health_out" | tail -n 1)
  [ -n "$code" ] || code="no answer: $(tr '\n' ' ' <"$health_out")"
  if [ "$code" != "$last_code" ]; then
    log "health" "wget $health" "HTTP $code, was $last_code"
    last_code=$code
  fi
  [ "$rc" -eq 0 ] && [ "$code" = 200 ]
}
# unstick drains the Service and advertises it again once the node has been advertised and
# unapproved for more than stuck seconds, doubling stuck up to stuck_max. It returns 1 when the
# cycle must start again: the node changed or may no longer advertise during the pause, or the new
# advertisement failed.
unstick() {
  waited=$(($(now) - since))
  [ "$waited" -gt "$stuck" ] || return 0
  recoveries=$((recoveries + 1))
  stuck=$((stuck * 2))
  [ "$stuck" -le "$stuck_max" ] || stuck=$stuck_max
  recovery="recovery $recoveries: not approved ${waited}s after advertising"
  if ! ts serve drain "$service" >/dev/null 2>"$err"; then
    log "service advertisement" "tailscale serve drain $service" "$recovery: the drain failed: $(tr '\n' ' ' <"$err"); next recovery after ${stuck}s"
    return 0
  fi
  sleep "$pause"
  still || return 1
  if ! ts serve advertise "$service" >/dev/null 2>"$err"; then
    log "service advertisement" "tailscale serve drain $service; sleep $pause; tailscale serve advertise $service" "$recovery: drained, and the advertisement failed: $(tr '\n' ' ' <"$err"); advertising again"
    return 1
  fi
  since=$(now)
  log "service advertisement" "tailscale serve drain $service; sleep $pause; tailscale serve advertise $service" "$recovery: drained and advertised again; next recovery after ${stuck}s"
}
orders=0
while :; do
  rm -f "$ready_file"
  until follow; do sleep "$period"; done
  node=$id
  good=0
  while :; do
    if may; then
      [ -z "$health" ] && break
      if healthy; then good=$((good + 1)); else good=0; fi
      [ "$good" -ge "$rise" ] && break
    else
      good=0
    fi
    sleep "$period"
    follow && [ "$id" = "$node" ] || continue 2
  done
  [ -z "$flag_file" ] || log "service advertisement" "cat $flag_file" "$(cat "$flag_file")"
  until ts serve advertise "$service" >/dev/null 2>"$err"; do
    log "service advertisement" "tailscale serve advertise $service" "failed: $(tr '\n' ' ' <"$err"); again in 10s"
    sleep 10
    still || continue 2
  done
  log "service advertisement" "tailscale serve advertise $service" "advertised as node $node"
  since=$(now)
  stuck=$stuck_first
  recoveries=0
  bad=0
  held=
  while :; do
    if ! follow; then gone="tailscaled answers as no logged-in node"; break
    elif [ "$id" != "$node" ]; then gone="tailscaled is node $id now, not $node"; break
    elif ! may; then gone="node $node may not advertise: a preStop hook drains it${flag_file:+, or the advertise flag does not name it}"; break
    fi
    if [ -n "$health" ]; then
      if healthy; then bad=0; else bad=$((bad + 1)); fi
      if [ "$bad" -ge "$fall" ]; then
        ts serve drain "$service" >/dev/null 2>"$err" || log "service advertisement" "tailscale serve drain $service" "failed: $(tr '\n' ' ' <"$err")"
        gone="$bad health checks of $health failed in a row: $service drained"
        break
      fi
    fi
    if ! advertising; then gone="node $node no longer advertises $service"; break; fi
    if ! approved; then
      if [ -e "$ready_file" ]; then
        rm -f "$ready_file"
        log "readiness" "rm -f $ready_file" "node $node is no longer approved to host $service: not ready"
      fi
      if ! unstick; then gone="node $node changed, may no longer advertise, or was not advertised again during a recovery"; break; fi
      sleep "$period"
      continue
    fi
    if [ -n "$certificate" ] && [ "$held" != "$node" ]; then
      if ts cert --cert-file=- --key-file=- "$certificate" >/dev/null 2>"$err"; then
        held=$node
        log "service certificate" "tailscale cert $certificate" "in tailscaled's cache"
      else
        why=$(tr '\n' ' ' <"$err")
        if grep -q 'invalid domain' "$err"; then
          backoff=30
        else
          orders=$((orders + 1))
          if [ "$orders" -ge "$attempts" ]; then
            ts serve drain "$service" >/dev/null 2>&1
            log "service certificate" "tailscale cert $certificate" "$orders orders failed, the last: $why; no more from this pod: $service drained, not ready"
            while sleep 30; do follow; done
          fi
          backoff=$((60 * orders + ${RANDOM:-0} % 30))
        fi
        log "service certificate" "tailscale cert $certificate" "not yet: $why; again in ${backoff}s"
        sleep "$backoff"
        continue
      fi
    fi
    if [ ! -e "$ready_file" ]; then
      printf '%s\n' "$node" >"$ready_file.tmp" && mv "$ready_file.tmp" "$ready_file"
      log "readiness" "write $ready_file" "node $node is approved to host $service${certificate:+ and tailscaled holds its certificate}: ready"
    fi
    sleep "$period"
  done
  rm -f "$ready_file"
  log "readiness" "rm -f $ready_file" "$gone: not ready; advertising again once the node may"
done
`

// The tailscale container's preStop hook and its budget. A replaced pod's node must leave the
// Service's hosts and the tailnet, not stay behind as an offline host approved automatically. Nothing
// on the shutdown path does that for a node with a state directory: containerboot only
// unadvertises the Services, waits up to 20 s and sends tailscaled SIGTERM, all inside
// tailnetContainerbootShutdownSeconds (cmd/containerboot/main.go:483-508, its 25 s bound at :488;
// kube/services/services.go:59-99), and tailscaled logs itself out on shutdown only when it
// registered with LoginEphemeral (ipn/ipnlocal/local.go:1344-1355), which it sets only for
// --state=mem: (cmd/tailscaled/tailscaled.go:456-458). So the hook does it, in this order:
//
//  1. write the node id and the time, `<node id> <unix time>`, into the draining file and remove
//     the ready file, so the advertiser neither advertises this node again nor calls the pod ready
//     while the hook waits;
//  2. `tailscale serve drain svc:<name>`, which takes the Service out of the node's preferences
//     (cmd/tailscale/cli/serve_v2.go:278-282,583-603), bounded by tailnetPreStopDrainSeconds;
//  3. wait tailnetPreStopWaitSeconds for clients to stop choosing this host: containerboot's own
//     figure for the same wait, "15s is the approximate maximum time it should take for control to
//     choose a new primary, and 5s is for buffer" (kube/services/services.go:82-98);
//  4. `tailscale logout`, bounded by tailnetPreStopLogoutSeconds, tailscaled's own bound for the
//     same logout on its shutdown path (local.go:1346). It is the call tailscaled itself makes so
//     an ephemeral node leaves the tailnet on shutdown (local.go:1344-1355).
//
// Logging out leaves tailscaled's running state, so containerboot exits by its own shutdown path,
// with nothing left to unadvertise. The pod's grace period must hold the hook and then
// containerboot's shutdown: tailnetShutdownSeconds, 55 s, which in a 60 s grace period leaves 5 s
// for tailscaled to exit on its SIGTERM before the kubelet's SIGKILL.
//
// The draining file outlives none of it. The grace period counts from the start of the
// termination, before the hook writes the file, and the kubelet kills the container at its end,
// so a grace period after the file's time the hook's container is gone and its drain over; the
// advertiser then ignores the file and removes it. Nothing else removes it, and it is on the
// state emptyDir, which a restarted container finds again: without the bound, a container that
// comes back as the node the file names (its logout failed: LocalBackend.Logout returns on the
// control client's error before it deletes the profile, ipn/ipnlocal/local.go:7170-7178) would
// never be advertised again.
//
// Why a logout in the hook and not --state=mem:. With a state directory a tailscale container
// that crashes or is OOM-killed comes back as the same node, still approved, and the advertiser
// only advertises it again; with --state=mem: every restart registers a new node, and one killed
// with no clean shutdown also leaves its old node behind with no logout at all. The hook logs out
// exactly when the kubelet stops the container on purpose: pod deletion, and a failed startup or
// liveness probe.
const (
	tailnetPreStopDrainSeconds  = 5
	tailnetPreStopWaitSeconds   = 20
	tailnetPreStopLogoutSeconds = 5
	// tailnetContainerbootShutdownSeconds is containerboot's own bound on its shutdown after the
	// hook: context.WithTimeout(..., 25*time.Second) at cmd/containerboot/main.go:488.
	tailnetContainerbootShutdownSeconds = 25
	// tailnetPreStopSeconds is the hook's longest run.
	tailnetPreStopSeconds = tailnetPreStopDrainSeconds + tailnetPreStopWaitSeconds + tailnetPreStopLogoutSeconds
	// tailnetShutdownSeconds is the longest a tailscale container takes to stop once the kubelet
	// begins: the hook, then containerboot's shutdown. A pod's terminationGracePeriodSeconds must be
	// at least this.
	tailnetShutdownSeconds = tailnetPreStopSeconds + tailnetContainerbootShutdownSeconds
)

// tailnetPreStopScript is the tailscale container's preStop hook, the steps above, for /bin/sh -c.
// Each line runs whatever the one before it did: a failed drain must not keep the node from
// logging out. With no node id file (tailscaled never ran as a node) it writes no draining file:
// there is no node to keep from being advertised.
func tailnetPreStopScript(h tailnetServiceHost) string {
	q := core.ShellQuote
	socket, draining := q("--socket="+h.paths.socket()), q(h.paths.draining())
	return fmt.Sprintf(`read -r id _ 2>/dev/null <%[1]s && printf '%%s %%s\n' "$id" "$(date -u +%%s)" >%[2]s.tmp && mv %[2]s.tmp %[2]s; rm -f %[3]s
timeout %[4]d tailscale %[5]s serve drain %[6]s
sleep %[7]d
timeout %[8]d tailscale %[5]s logout
`, q(h.paths.nodeID()), draining, q(h.paths.ready()), tailnetPreStopDrainSeconds, socket, q(h.service()),
		tailnetPreStopWaitSeconds, tailnetPreStopLogoutSeconds)
}

// tailnetPreStop is the tailscale container's lifecycle: tailnetPreStopScript as its preStop hook.
func tailnetPreStop(h tailnetServiceHost) *corev1.LifecycleArgs {
	return &corev1.LifecycleArgs{PreStop: &corev1.LifecycleHandlerArgs{Exec: &corev1.ExecActionArgs{
		Command: pulumi.StringArray{pulumi.String("/bin/sh"), pulumi.String("-c"), pulumi.String(tailnetPreStopScript(h))},
	}}}
}

// tailnetServiceStartup is the tailscale container's startup probe: containerboot's /healthz on
// its local address, which answers 200 once the node has its tailnet addresses, given 3 minutes
// (5 s × 36) for a first login.
func tailnetServiceStartup(h tailnetServiceHost) *corev1.ProbeArgs {
	return &corev1.ProbeArgs{
		Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{
			pulumi.String("wget"), pulumi.String("-qO-"), pulumi.String("http://" + h.localAddr() + "/healthz"),
		}},
		PeriodSeconds:    pulumi.Int(5),
		FailureThreshold: pulumi.Int(36),
	}
}

// tailnetServiceReadiness is the tailscale container's readiness probe: ready only while the
// advertiser's ready file exists. The probe is on the container that serves the Service because
// the kubelet takes a container's readiness away the moment it stops and gives it back only
// through its probes after a restart, so a restarted tailscaled is not ready until the advertiser
// has confirmed its node again. With failureThreshold 1 the ready file's removal takes readiness
// away at the next probe, and the rolling update and the PodDisruptionBudget count only pods that
// serve.
func tailnetServiceReadiness(h tailnetServiceHost) *corev1.ProbeArgs {
	return &corev1.ProbeArgs{
		Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{
			pulumi.String("/bin/sh"), pulumi.String("-c"), pulumi.String("test -e " + core.ShellQuote(h.paths.ready())),
		}},
		PeriodSeconds:    pulumi.Int(5),
		FailureThreshold: pulumi.Int(1),
	}
}

// tailnetServiceLiveness is the tailscale container's liveness probe, which fails only when
// tailscaled's LocalAPI does not answer at all: a node logged out, waiting for login or unapproved
// is alive, and a restart would not help it. `tailscale version --daemon` asks the LocalAPI for
// its status without peers and returns that call's error alone (cmd/tailscale/cli/version.go:49-54);
// the client fails only on a transport error or a status other than 200 (client/local/local.go:801-811,
// get200), and the LocalAPI's status handler answers 200 with the status in every backend state,
// NeedsLogin included, for a caller allowed to read (ipn/localapi/localapi.go:844-859), which the
// pod's own uid is. --socket names the pod's socket (a root flag, cmd/tailscale/cli/cli.go:255). One
// minute (10 s × 6) of no answer restarts the container.
func tailnetServiceLiveness(h tailnetServiceHost) *corev1.ProbeArgs {
	return &corev1.ProbeArgs{
		Exec: &corev1.ExecActionArgs{Command: pulumi.StringArray{
			pulumi.String("tailscale"), pulumi.String("--socket=" + h.paths.socket()), pulumi.String("version"), pulumi.String("--daemon"),
		}},
		PeriodSeconds:    pulumi.Int(10),
		TimeoutSeconds:   pulumi.Int(5),
		FailureThreshold: pulumi.Int(6),
	}
}

// tailnetPodSecurity is a Tailscale Service host pod's security context: one uid for every
// container, set here and overridden by none. tailscaled writes its state and certificate cache
// owner-only (feature/acme/certstore.go:77,132,161), and grants LocalAPI write access to a caller
// of its own uid, so the advertiser, the preStop hook and any container that reads the cache must
// run as the uid tailscaled runs as. The Tailscale image's containers default to root otherwise.
func tailnetPodSecurity(uid int) *corev1.PodSecurityContextArgs {
	return &corev1.PodSecurityContextArgs{
		RunAsUser:      pulumi.Int(uid),
		RunAsGroup:     pulumi.Int(uid),
		RunAsNonRoot:   pulumi.Bool(true),
		SeccompProfile: &corev1.SeccompProfileArgs{Type: pulumi.String("RuntimeDefault")},
	}
}

// tailnetContainerSecurity is every container's security context in such a pod: no privilege
// escalation and every Linux capability dropped. tailscaled runs in userspace (TS_USERSPACE), so
// it needs no capability and no tun device.
func tailnetContainerSecurity() *corev1.SecurityContextArgs {
	return &corev1.SecurityContextArgs{
		AllowPrivilegeEscalation: pulumi.Bool(false),
		Capabilities:             &corev1.CapabilitiesArgs{Drop: pulumi.StringArray{pulumi.String("ALL")}},
	}
}
