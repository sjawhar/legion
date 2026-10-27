package gcp

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	rbacv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/rbac/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
)

// Dispatch's front door is a Tailscale Service host (tailnet_service.go): frontDoorReplicas pods
// of Deployment <hostname>-tailnet, each its own tailnet node registered under its pod's name, all
// hosting the Tailscale Service svc:<hostname>. Each terminates TLS for <hostname>.<tailnetDnsName>
// on 443 and proxies to the app's Service, and each runs tailscaled's SOCKS5 proxy on 1055, which
// the app and the listener reach the NATS hub through at Service <hostname>-tailnet. So the name
// every reader reaches Dispatch at, and the hub, answer while any one front-door pod serves.
//
// Every pod holds the Service name's certificate, and Let's Encrypt issues at most 5 certificates
// for one exact set of names in 7 days, from every account together
// (letsencrypt.org/docs/rate-limits). A pod's tailscaled caches the certificate under its state
// directory and returns the cached one until two thirds of its life (feature/acme/cert.go:121-136
// at v1.104.1), but a new pod starts with an empty emptyDir, so without more each roll would order
// one per pod. Three scripts keep a new pod from ordering:
//
//   - cert-seed, an init container, copies the certificate published in Secret
//     <hostname>-tailnet-tls into the cache before tailscaled starts (dispatchCertSeedScript);
//   - the claim gate, in cert-publish, lets the pod advertise the Service only with a certificate
//     that is not due, cached or published, or with one of dispatchCertificateClaimsPerWeek claims
//     on that Secret (dispatchCertClaimScript); the advertiser advertises only the node the gate's
//     flag names (tailnetServiceHost.certificate);
//   - cert-publish's loop writes the pod's own cached certificate into that Secret when it is the
//     later of the two (dispatchCertPublishScript), so the next pod finds it.
//
// cert-publish runs on frontDoorToolsImage, for its shell, curl, openssl, base64 and GNU date: the
// Tailscale image is busybox and has neither curl nor openssl. The certificate never touches the
// app's own pods, which never join the tailnet.

const (
	// dispatchFrontDoorStateMount is the front-door pod's state volume, an emptyDir mounted here
	// in every container that touches it: tailscaled's state directory, its socket, the ready
	// file and the gate's files (tailnetServicePaths).
	dispatchFrontDoorStateMount = "/var/lib/frontdoor"
	// dispatchFrontDoorTLSMount is Secret <hostname>-tailnet-tls, mounted read-only into cert-seed
	// and cert-publish.
	dispatchFrontDoorTLSMount = "/etc/frontdoor-tls"
	// dispatchServiceAccountTokenDir is where the kubelet projects a pod's own ServiceAccount token
	// and the cluster's certificate authority: how cert-publish authenticates to the API server.
	dispatchServiceAccountTokenDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	// dispatchServeMount is where the tailscale container mounts ConfigMap <hostname>-serve, and
	// dispatchServeConfig the serve config in it, TS_SERVE_CONFIG.
	dispatchServeMount  = "/etc/tailscale"
	dispatchServeConfig = dispatchServeMount + "/serve.json"

	// dispatchFrontDoorUID is the one uid every container of a front-door pod runs as: the envoy
	// image's own user (`envoy`, uid 999 in its /etc/passwd), which cert-publish, on
	// frontDoorToolsImage, runs as. tailscaled writes its certificate cache owner-only, so cert-seed,
	// tailscaled, the advertiser and cert-publish must share it (tailnetPodSecurity).
	dispatchFrontDoorUID = 999

	// dispatchFrontDoorGraceSeconds is the front-door pod's terminationGracePeriodSeconds: the
	// tailscale container's preStop hook and then containerboot's own shutdown,
	// tailnetShutdownSeconds, with 5 s left for tailscaled to exit on its SIGTERM.
	dispatchFrontDoorGraceSeconds = 60

	// The certificate budget the claim gate keeps (dispatchCertClaimScript): at most
	// dispatchCertificateClaimsPerWeek claims in any dispatchCertificateClaimWindow, under the 5
	// Let's Encrypt allows, so an operator's own order or a retried apply still fits; a cached or
	// published certificate with more than dispatchCertNotDueSeconds left buys a pod its
	// advertisement with no claim. tailscaled renews a 90-day certificate at two thirds of its
	// life, 30 days before it ends, so a certificate with more than 31 days left is one no pod
	// renews yet.
	dispatchCertificateClaimsAnnotation = "legion.dev/certificate-claims"
	dispatchCertificateClaimsPerWeek    = 3
	dispatchCertificateClaimWindow      = 7 * 24 * 60 * 60
	dispatchCertNotDueSeconds           = 31 * 24 * 60 * 60
	// dispatchFrontDoorNodesAnnotation is the node registry on the same Secret: `<unix
	// time>/<pod>/<node id>` entries, written by each pod's own gate with the pod's own
	// ServiceAccount token, in the same write that lets it advertise. It binds a pod to the node
	// its own tailscaled reports, so a reader of the Service's hosts can tell the front door's
	// nodes from any other. Entries are kept dispatchFrontDoorNodesKeepSeconds, longer than a claim
	// counts, so a pod gated before its claim lapsed stays named.
	dispatchFrontDoorNodesAnnotation  = "legion.dev/front-door-nodes"
	dispatchFrontDoorNodesKeepSeconds = 30 * 24 * 60 * 60
)

// dispatchCertPaths is where the certificate scripts find what they read and write: the front-door
// pod's state volume (tailscaled's state directory, the node id file, the advertise flag), the TLS
// Secret as mounted, the projected token directory and the TLS Secret's URL on the API server. The
// Deployment and the tests render the scripts from one, so both spell every file the same way; a
// test roots one in its own directories.
type dispatchCertPaths struct {
	service   tailnetServicePaths
	secret    string // Secret <hostname>-tailnet-tls, mounted read-only
	tokens    string // the projected ServiceAccount token directory
	secretURL string // Secret <hostname>-tailnet-tls on the API server
}

// cache is tailscaled's certificate cache, <TS_STATE_DIR>/certs: tailscaled reads and writes
// <domain>.crt, <domain>.key and acme-account.key.pem there (feature/acme/certstore.go:76-77,116,
// 156-161 at v1.104.1).
func (p dispatchCertPaths) cache() string { return p.service.stateDir() + "/certs" }

// dispatchFrontDoorPaths is a front-door pod's dispatchCertPaths, as its containers mount their
// volumes, for the TLS Secret tlsSecret in namespace.
func dispatchFrontDoorPaths(namespace, tlsSecret string) dispatchCertPaths {
	return dispatchCertPaths{
		service:   tailnetServicePaths{mount: dispatchFrontDoorStateMount},
		secret:    dispatchFrontDoorTLSMount,
		tokens:    dispatchServiceAccountTokenDir,
		secretURL: fmt.Sprintf("https://kubernetes.default.svc/api/v1/namespaces/%s/secrets/%s", namespace, tlsSecret),
	}
}

// dispatchScriptVars renders shell variable assignments, each value quoted, ahead of a script body
// that reads them, as tailnetAdvertiserScript does.
func dispatchScriptVars(vars [][2]string) string {
	var b strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&b, "%s=%s\n", v[0], core.ShellQuote(v[1]))
	}
	return b.String()
}

// dispatchCertSeedScript is cert-seed's program, for /bin/sh -c on the Tailscale image's busybox
// shell, as the pod's uid, before tailscaled starts: it seeds tailscaled's certificate cache from
// the published Secret, so a fresh pod presents the certificate its predecessors already hold
// instead of ordering one. With a state directory tailscaled reads <state>/certs/<domain>.crt and
// .key and the ACME account key before it orders (feature/acme/certstore.go), and the directory
// must be 0700, searchable only by the pod's uid. An absent or empty Secret is no error: the pod
// starts with no cached certificate, and the claim gate decides whether it may order one. Its
// mkdir also creates the state directory itself, a subdirectory of the state volume
// (tailnetServicePaths.stateDir says why), so the pod's uid owns it.
func dispatchCertSeedScript(p dispatchCertPaths, fqdn string) string {
	return dispatchScriptVars([][2]string{{"cache", p.cache()}, {"secret", p.secret}, {"fqdn", fqdn}}) + dispatchCertSeedBody
}

const dispatchCertSeedBody = `set -eu
mkdir -p "$cache"
chmod 0700 "$cache"
if [ -s "$secret/tls.crt" ] && [ -s "$secret/tls.key" ] && [ -s "$secret/acme-account.key.pem" ]; then
  cp "$secret/tls.crt" "$cache/$fqdn.crt"
  chmod 0644 "$cache/$fqdn.crt"
  cp "$secret/tls.key" "$cache/$fqdn.key"
  chmod 0600 "$cache/$fqdn.key"
  cp "$secret/acme-account.key.pem" "$cache/acme-account.key.pem"
  chmod 0600 "$cache/acme-account.key.pem"
  echo "seeded the cached certificate for $fqdn from $secret"
else
  echo "no published certificate in $secret; the claim gate decides whether this pod orders one"
fi
`

// dispatchCertPublishScript is cert-publish's publishing loop: every 60 s it compares tailscaled's
// own cached certificate with the one in Secret <hostname>-tailnet-tls and sends one merge patch
// when its own is the later of the two. It asks Tailscale for nothing: tailscaled renews the files
// in its cache by itself.
//
// The rule is an order on notAfter, not a difference, because each front-door pod holds a
// certificate of its own: "differs" is symmetric, so two pods holding different certificates
// would overwrite each other every 60 s for ever. "Later than" has one certificate win and quiets
// the others until a renewal moves their own notAfter past it. The comparison is -le and not -lt:
// two certificates that expire at the same instant must converge too, and a strict comparison
// would have both pods write for ever on the tie.
//
// An order means something only between two certificates for the same name, so notafter checks
// the name as well as the date and prints nothing for a certificate that is not for the Service's
// name: a published certificate for another name is no version of this pod's, and taking it for no
// certificate is what lets the loop replace it. The name is read out of the certificate's own
// subjectAltName and matched whole, never asked of a tool: `openssl x509 -checkhost` reports its
// verdict in its output and not its exit status on the OpenSSL 3.0 releases images carry, so a
// check on that status would pass wherever it is written and do nothing where it runs. `-ext
// subjectAltName` prints one `DNS:` entry per name, comma-separated; a wildcard entry is no match,
// and a file openssl cannot read prints none and reads as no certificate.
//
// A failed patch is logged and the loop goes on: the next pass retries, and the cached
// certificate is still good. So set -u without -e, and neither a cached certificate openssl
// cannot parse (the pass is skipped) nor a published one it cannot parse (taken for none) wedges
// it. A cache this process cannot read is the one exit: an identity that cannot read tailscaled's
// owner-only cache now never will, and skipping every pass in silence would leave the Secret empty
// with nothing saying why, so the container exits 1 naming the path and its uid, and the pod's
// restarts show it. An empty cache is normal until tailscaled's first order and is said once.
func dispatchCertPublishScript(p dispatchCertPaths, fqdn string) string {
	return dispatchScriptVars([][2]string{
		{"state", p.service.stateDir()}, {"cache", p.cache()}, {"secret", p.secret}, {"fqdn", fqdn}, {"tokens", p.tokens}, {"url", p.secretURL},
	}) + dispatchCertPublishBody
}

const dispatchCertPublishBody = `set -u
crt=$cache/$fqdn.crt
key=$cache/$fqdn.key
account=$cache/acme-account.key.pem
waiting=
# ournames succeeds only when the PEM certificate $1's subjectAltName names $fqdn exactly.
ournames() {
  openssl x509 -noout -ext subjectAltName -in "$1" 2>/dev/null |
    tr ',' '\n' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -Fqx "DNS:$fqdn"
}
# notafter prints a PEM certificate's notAfter as seconds since the epoch, and nothing at all
# unless it is a certificate for $fqdn.
notafter() {
  ournames "$1" || return 0
  enddate=$(openssl x509 -noout -enddate -in "$1" 2>/dev/null) || return 0
  date -u -d "${enddate#notAfter=}" +%s 2>/dev/null
}
# unreadable ends the container, naming what it cannot read.
unreadable() {
  echo "$1 is not readable by uid $(id -u): this container's identity cannot read tailscaled's certificate cache, so the Secret will never be written" >&2
  exit 1
}
publish() {
  for dir in "$state" "$cache"; do
    if [ -e "$dir" ] && ! { [ -r "$dir" ] && [ -x "$dir" ]; }; then unreadable "$dir"; fi
  done
  for file in "$crt" "$key" "$account"; do
    if [ -e "$file" ] && [ ! -r "$file" ]; then unreadable "$file"; fi
  done
  if ! { [ -s "$crt" ] && [ -s "$key" ] && [ -s "$account" ]; }; then
    if [ -z "$waiting" ]; then
      echo "waiting for tailscaled to cache a certificate under $cache"
      waiting=1
    fi
    return 0
  fi
  mine=$(notafter "$crt")
  [ -n "$mine" ] || return 0
  theirs=
  if [ -s "$secret/tls.crt" ]; then theirs=$(notafter "$secret/tls.crt"); fi
  if [ -n "$theirs" ] && [ "$mine" -le "$theirs" ]; then return 0; fi
  body=$(printf '{"data":{"tls.crt":"%s","tls.key":"%s","acme-account.key.pem":"%s"}}' \
    "$(base64 <"$crt" | tr -d '\n')" "$(base64 <"$key" | tr -d '\n')" "$(base64 <"$account" | tr -d '\n')")
  if curl -sS -f -X PATCH "$url" \
    --cacert "$tokens/ca.crt" \
    -H "Authorization: Bearer $(cat "$tokens/token")" \
    -H 'Content-Type: application/merge-patch+json' \
    --data-binary "$body" >/dev/null; then
    echo "published a later certificate for $fqdn"
  else
    echo "publishing the certificate for $fqdn failed; retrying in 60s" >&2
  fi
}
while true; do
  publish
  sleep 60
done
`

// dispatchCertClaimScript is the claim gate: it runs in the background ahead of
// dispatchCertPublishScript in cert-publish, as the pod's uid, and writes the advertise flag
// (tailnetServicePaths.advertiseFlag) once the pod may host the Service, logging each reading. No
// pod advertises the Service by itself (TS_EXPERIMENTAL_SERVICE_AUTO_ADVERTISEMENT is false), and a
// node the Service has not approved cannot get its name among its certificate domains, so
// tailscaled refuses to order the name for it, locally and at no cost (feature/acme/cert.go:699-727
// at v1.104.1). The flag is so what lets a pod order: the advertiser advertises only the node it
// names, and orders the certificate once that node is approved.
//
// gate is one pass, run every 30 s for the pod's life; it returns 0 once the flag is written, and a
// read or a write that fails (a refused claim, a precondition another pod's write broke) is tried
// again on the next pass. A pod gets the flag in one of three ways:
//
//   - its cache holds the Service name's certificate with more than dispatchCertNotDueSeconds left
//     (cert-seed copied it in, or the pod ordered it before a container restart): no claim;
//   - the mounted Secret holds such a certificate (a pod started before a publisher wrote it): the
//     gate copies it into the cache, where tailscaled reads it on every lookup, and takes no claim;
//   - it takes one of dispatchCertificateClaimsPerWeek claims, `<unix time>/<pod name>` entries in
//     the Secret's dispatchCertificateClaimsAnnotation, of which one older than the claim window no
//     longer counts. A pod already holding one (a restarted container) takes no other.
//
// Each pass reads the Secret as a PartialObjectMetadata (the Accept header asks for
// as=PartialObjectMetadata;g=meta.k8s.io;v=v1), so it reads the resourceVersion and the two
// annotations and never the certificate's private key, which the mounted Secret already holds.
//
// Every write carries the resourceVersion the pass read as its precondition, so of two pods that
// read one version the API server answers the later 409, and two pods never take one claim. Every
// path that sets the flag first writes this pod's entry in the node registry
// (dispatchFrontDoorNodesAnnotation), in the same write as a claim when it takes one, and sets the
// flag only once that write is accepted: a pod the registry does not name never advertises. Its
// node id is the one the advertiser read from this pod's own tailscaled. Once the flag is set, each
// pass reads the registry and writes this pod's entry again when it is missing (a re-created
// Secret, or another pod's carry-forward dropping it past the keep window) or when the node id file
// names a node the registry does not (tailscaled re-registered under a new node id); that write
// also puts the new id first in the flag, which is what lets the advertiser advertise the new node.
//
// The pod writes the two annotations with merge patches under its own field manager; the
// component declares the Secret with no annotations and applies it server-side, and a server-side
// apply removes only the fields its own manager set, so no apply strips them.
func dispatchCertClaimScript(p dispatchCertPaths, fqdn string) string {
	return dispatchScriptVars([][2]string{
		{"cache", p.cache()}, {"secret", p.secret}, {"fqdn", fqdn}, {"tokens", p.tokens}, {"url", p.secretURL},
		{"flag_file", p.service.advertiseFlag()}, {"node_file", p.service.nodeID()},
		{"claims_max", strconv.Itoa(dispatchCertificateClaimsPerWeek)}, {"window", strconv.Itoa(dispatchCertificateClaimWindow)},
		{"not_due", strconv.Itoa(dispatchCertNotDueSeconds)}, {"keep", strconv.Itoa(dispatchFrontDoorNodesKeepSeconds)},
		{"claims_key", dispatchCertificateClaimsAnnotation}, {"nodes_key", dispatchFrontDoorNodesAnnotation},
	}) + dispatchCertClaimBody
}

const dispatchCertClaimBody = `(
  at() { date -u +%Y-%m-%dT%H:%M:%SZ; }
  log() { echo "$(at) $1 $2 | $3 | $4"; }
  me=${HOSTNAME:-$(hostname)}
  # left prints the seconds until a PEM certificate's notAfter, and fails unless its
  # subjectAltName names $fqdn exactly.
  left() {
    openssl x509 -noout -ext subjectAltName -in "$1" 2>/dev/null |
      tr ',' '\n' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -Fqx "DNS:$fqdn" || return 1
    end=$(openssl x509 -noout -enddate -in "$1" 2>/dev/null) || return 1
    end=$(date -u -d "${end#notAfter=}" +%s 2>/dev/null) || return 1
    echo $((end - $(date -u +%s)))
  }
  # annotation prints the value of the Secret's annotation $1 as the pass read it, or nothing.
  annotation() { printf '%s' "$body" | sed -n "s|.*\"$1\": *\"\([^\"]*\)\".*|\1|p"; }
  # patch writes the annotations $1 (JSON members) on the Secret, with the resourceVersion the pass
  # read as the precondition, so a write after another pod's fails with 409 and writes nothing.
  patch() {
    curl -sS -f -X PATCH "$url" --cacert "$tokens/ca.crt" -H "Authorization: Bearer $(cat "$tokens/token")" \
      -H 'Content-Type: application/merge-patch+json' \
      --data-binary "{\"metadata\":{\"resourceVersion\":\"$rv\",\"annotations\":{$1}}}" >/dev/null 2>&1
  }
  gate() {
    if [ ! -s "$node_file" ]; then
      log happened "certificate budget" "cat $node_file" "this pod's node id is not read yet: not advertising; again in 30s"
      return 1
    fi
    id=$(cat "$node_file")
    # A StableNodeID is alphanumeric; anything else would be written into the merge patch below
    # unescaped, so it is not recorded at all.
    case "$id" in ''|*[!A-Za-z0-9]*)
      log happened "certificate budget" "cat $node_file" "the node id is not alphanumeric: not recording it, not advertising; again in 30s"
      return 1 ;;
    esac
    cached=$cache/$fqdn.crt
    why=
    # A flag holds the node id it was set for, then why. A pod advertising already takes no claim
    # and copies nothing: the registry is read, and this pod's entry under its current node id
    # written again when it is missing.
    flagged=
    if [ -e "$flag_file" ]; then
      read -r flagged prev <"$flag_file" || :
      why=${prev:-flag set}
    elif l=$(left "$cached") && [ "$l" -gt "$not_due" ]; then
      why="cached certificate, ${l}s left"
      log happened "certificate budget" "openssl x509 -enddate -in $cached" "the cache holds the certificate with ${l}s left: advertise, no claim"
    elif l=$(left "$secret/tls.crt") && [ "$l" -gt "$not_due" ] && [ -s "$secret/tls.key" ]; then
      cp "$secret/tls.crt" "$cached.tmp" && cp "$secret/tls.key" "$cache/$fqdn.key.tmp" && chmod 0644 "$cached.tmp" &&
        chmod 0600 "$cache/$fqdn.key.tmp" && mv "$cache/$fqdn.key.tmp" "$cache/$fqdn.key" && mv "$cached.tmp" "$cached" || return 1
      if [ ! -s "$cache/acme-account.key.pem" ] && [ -s "$secret/acme-account.key.pem" ]; then
        cp "$secret/acme-account.key.pem" "$cache/acme-account.key.pem" && chmod 0600 "$cache/acme-account.key.pem"
      fi
      why="published certificate, ${l}s left"
      log happened "certificate budget" "openssl x509 -enddate -in $secret/tls.crt" "the Secret holds the certificate with ${l}s left: copied into the cache, advertise, no claim"
    fi
    if ! body=$(curl -sS -f --cacert "$tokens/ca.crt" -H "Authorization: Bearer $(cat "$tokens/token")" \
      -H 'Accept: application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1' "$url" 2>/dev/null); then
      log happened "certificate budget" "GET $url" "the claim ledger could not be read: not advertising; again in 30s"
      return 1
    fi
    body=$(printf '%s' "$body" | tr -d '\n')
    rv=$(printf '%s' "$body" | sed -n 's/.*"resourceVersion": *"\([0-9]*\)".*/\1/p')
    claims=$(annotation "$claims_key")
    nodes=$(annotation "$nodes_key")
    now=$(date -u +%s)
    if [ -z "$rv" ]; then
      log happened "certificate budget" "GET $url" "no resourceVersion read: not advertising; again in 30s"
      return 1
    fi
    # The registry carried forward: every entry younger than the keep window but this pod's entry
    # for its current node id, then that one, fresh. This pod's entries for older node ids stay, so
    # a node it was before a re-registration is still named as the front door's.
    reg=
    for e in $nodes; do
      t=${e%%/*}
      rest=${e#*/}
      case "$t" in ''|*[!0-9]*) continue ;; esac
      [ $((now - t)) -lt "$keep" ] || continue
      [ "$rest" = "$me/$id" ] && continue
      reg="$reg $e"
    done
    reg="${reg# } $now/$me/$id"
    reg=${reg# }
    live=
    count=0
    mine=
    for c in $claims; do
      t=${c%%/*}
      case "$t" in ''|*[!0-9]*) continue ;; esac
      [ $((now - t)) -lt "$window" ] || continue
      live="$live $c"
      count=$((count + 1))
      [ "${c#*/}" = "$me" ] && mine=$c
    done
    live=${live# }
    newclaims=$live
    if [ -e "$flag_file" ]; then
      case " $nodes " in *"/$me/$id "*) return 0 ;; esac
      log happened "certificate budget" "GET $url" "this pod advertises (its flag names node ${flagged:-none}), and the registry lacks $me/$id: recording it again"
    fi
    if [ -z "$why" ]; then
      log happened "certificate budget" "GET $url" "claims in the last ${window}s: $count of $claims_max:${live:- none}"
      if [ -n "$mine" ]; then
        why="claim $mine"
        log happened "certificate budget" "GET $url" "this pod holds claim $mine: advertise"
      elif [ "$count" -ge "$claims_max" ]; then
        log happened "certificate budget" "GET $url" "no claim left: not advertising, so this pod orders no certificate for $fqdn; again in 30s"
        return 1
      else
        newclaims="${live:+$live }$now/$me"
        why="claim $now/$me"
      fi
    fi
    log asked "certificate budget" "PATCH $url (resourceVersion $rv)" "$why; node $me=$id"
    if patch "\"$claims_key\":\"$newclaims\",\"$nodes_key\":\"$reg\""; then
      echo "$id $why" >"$flag_file"
      case "$why" in
        "claim $now/$me") log happened "certificate budget" "PATCH $url" "claim $now/$me taken, node $id recorded: advertise" ;;
        *) log happened "certificate budget" "PATCH $url" "node $id recorded: advertise ($why)" ;;
      esac
      return 0
    fi
    log happened "certificate budget" "PATCH $url" "the write was refused (another pod's write, or the API): again in 30s"
    return 1
  }
  while :; do gate; sleep 30; done
) &
`

// newDispatchFrontDoor registers the front door's own objects and renders its pod, for the
// Deployment newDispatchKubernetes registers as <hostname>-tailnet: ServiceAccount
// <hostname>-tailnet, Secret <hostname>-tailnet-tls with the Role that lets that account alone
// write it, and ConfigMap <hostname>-serve. It returns the pod and the options that Deployment
// registers with. tailnetService is checked once it resolves; authKeySecret names the Secret
// holding TS_AUTHKEY.
func newDispatchFrontDoor(ctx *pulumi.Context, dispatch string, s dispatchSettings, tailnetService pulumi.StringInput, authKeySecret string, metadata func(string) *metav1.ObjectMetaArgs, child []pulumi.ResourceOption) (corev1.PodSpecArgs, []pulumi.ResourceOption, error) {
	name := s.hostname + "-tailnet"
	tlsName := name + "-tls"
	serveName := s.hostname + "-serve"
	fqdn := core.TailnetHostName(s.hostname, s.tailnetDNSName)
	paths := dispatchFrontDoorPaths(s.namespace, tlsName)

	// The front door's pods run as an account of their own, so the one write they need, to the
	// TLS Secret, is granted to them and to neither the app nor the listener, and the account the
	// app's federation trusts (serviceAccountSubject) holds no such grant.
	if _, err := corev1.NewServiceAccount(ctx, name, &corev1.ServiceAccountArgs{Metadata: metadata(name)}, child...); err != nil {
		return corev1.PodSpecArgs{}, nil, err
	}
	// The certificate Secret, keys tls.crt, tls.key and acme-account.key.pem: declared with no data,
	// so it exists for the pods' volume from the first apply, and its contents ignored, so a later
	// preview does not plan to undo what cert-publish wrote. No pod template carries its checksum: a
	// renewed certificate must roll no pod.
	tlsSecret, err := corev1.NewSecret(ctx, tlsName, &corev1.SecretArgs{Metadata: metadata(tlsName)},
		append(append([]pulumi.ResourceOption{}, child...), pulumi.IgnoreChanges([]string{"data", "stringData"}))...)
	if err != nil {
		return corev1.PodSpecArgs{}, nil, err
	}
	// cert-publish writes that one Secret and nothing else: no create verb, since the component
	// creates it, and its name is the only one the rule names.
	if _, err := rbacv1.NewRole(ctx, tlsName, &rbacv1.RoleArgs{
		Metadata: metadata(tlsName),
		Rules: rbacv1.PolicyRuleArray{rbacv1.PolicyRuleArgs{
			ApiGroups: pulumi.StringArray{pulumi.String("")}, Resources: pulumi.StringArray{pulumi.String("secrets")},
			ResourceNames: pulumi.StringArray{pulumi.String(tlsName)},
			Verbs:         pulumi.StringArray{pulumi.String("get"), pulumi.String("update"), pulumi.String("patch")},
		}},
	}, child...); err != nil {
		return corev1.PodSpecArgs{}, nil, err
	}
	if _, err := rbacv1.NewRoleBinding(ctx, tlsName, &rbacv1.RoleBindingArgs{
		Metadata: metadata(tlsName),
		RoleRef:  rbacv1.RoleRefArgs{ApiGroup: pulumi.String("rbac.authorization.k8s.io"), Kind: pulumi.String("Role"), Name: pulumi.String(tlsName)},
		Subjects: rbacv1.SubjectArray{rbacv1.SubjectArgs{Kind: pulumi.String("ServiceAccount"), Name: pulumi.String(name), Namespace: pulumi.String(s.namespace)}},
	}, child...); err != nil {
		return corev1.PodSpecArgs{}, nil, err
	}

	host := tailnetServiceHost{
		paths: paths.service, name: s.hostname,
		authKeySecret: authKeySecret, authKeyKey: "TS_AUTHKEY",
		tags: s.tags, serveConfig: dispatchServeConfig,
		// Every front-door pod runs the SOCKS5 proxy the app and the listener reach the NATS hub
		// through, on every address, since its callers are pods of their own.
		socks5Server: "0.0.0.0:" + strconv.Itoa(dispatchSOCKS5Port),
		graceSeconds: dispatchFrontDoorGraceSeconds,
		certificate:  fqdn,
	}

	// The serve config is rendered from the resolved tailnetService, so a consumer passing its
	// Tailscale Service's name output orders the ConfigMap after that Service. The value is checked
	// first: the pods host svc:<hostname>, whose name the certificate and the URL carry, so any other
	// value is refused, naming the input and never the value.
	want := host.service()
	serveJSON := tailnetService.ToStringOutput().ApplyT(func(service string) (string, error) {
		if service != want {
			return "", fmt.Errorf("%s: tailnetService must be %s, the Tailscale Service the front door hosts, as Tailscale's API names it (not echoed)", dispatch, want)
		}
		return tailnetServeHTTPS(s.hostname, s.tailnetDNSName, "http://"+clusterLocal(s.hostname, s.namespace)+":8766"), nil
	}).(pulumi.StringOutput)
	serve, err := corev1.NewConfigMap(ctx, serveName, &corev1.ConfigMapArgs{Metadata: metadata(serveName), Data: pulumi.StringMap{"serve.json": serveJSON}}, child...)
	if err != nil {
		return corev1.PodSpecArgs{}, nil, err
	}

	shell := func(script string) (pulumi.StringArray, pulumi.StringArray) {
		return pulumi.StringArray{pulumi.String("/bin/sh"), pulumi.String("-c")}, pulumi.StringArray{pulumi.String(script)}
	}
	stateMount := corev1.VolumeMountArgs{Name: pulumi.String("state"), MountPath: pulumi.String(dispatchFrontDoorStateMount)}
	tlsMount := corev1.VolumeMountArgs{Name: pulumi.String("tls"), MountPath: pulumi.String(dispatchFrontDoorTLSMount), ReadOnly: pulumi.Bool(true)}
	seedCommand, seedArgs := shell(dispatchCertSeedScript(paths, fqdn))
	publishCommand, publishArgs := shell(dispatchCertClaimScript(paths, fqdn) + dispatchCertPublishScript(paths, fqdn))
	advertiserCommand, advertiserArgs := shell(tailnetAdvertiserScript(host))
	pod := corev1.PodSpecArgs{
		ServiceAccountName: pulumi.String(name), EnableServiceLinks: pulumi.Bool(false),
		SecurityContext:               tailnetPodSecurity(dispatchFrontDoorUID),
		TerminationGracePeriodSeconds: pulumi.Int(dispatchFrontDoorGraceSeconds),
		InitContainers: corev1.ContainerArray{
			// Runs to completion before tailscaled starts (not a sidecar), as the pod's uid, so the
			// state directory it creates and the cache it seeds are that uid's.
			corev1.ContainerArgs{
				Name: pulumi.String("cert-seed"), Image: pulumi.String(s.images.tailscale), Command: seedCommand, Args: seedArgs,
				VolumeMounts: corev1.VolumeMountArray{stateMount, tlsMount},
				Resources:    resources("100m", "128Mi"), SecurityContext: tailnetContainerSecurity(),
			},
		},
		Containers: corev1.ContainerArray{
			// tailscaled, which carries every tailnet request and the NATS traffic of the app and the
			// listener, so it takes the pod's larger share: with cert-publish's and the advertiser's
			// 100m and 128Mi the pod requests 700m and 768Mi, above Autopilot's 500m and 512Mi floor
			// for a pod with anti-affinity and inside its memory-per-CPU range.
			corev1.ContainerArgs{
				Name: pulumi.String("tailscale"), Image: pulumi.String(s.images.tailscale), Env: tailnetServiceEnv(host),
				VolumeMounts: corev1.VolumeMountArray{
					corev1.VolumeMountArgs{Name: pulumi.String("serve"), MountPath: pulumi.String(dispatchServeMount)},
					stateMount,
				},
				StartupProbe: tailnetServiceStartup(host), ReadinessProbe: tailnetServiceReadiness(host), LivenessProbe: tailnetServiceLiveness(host),
				Lifecycle: tailnetPreStop(host),
				Resources: resources("500m", "512Mi"), SecurityContext: tailnetContainerSecurity(),
			},
			// The claim gate in the background, then the publishing loop. HOSTNAME is the pod's name
			// from the API, not the container's hostname: the gate keys its claims and registry
			// entries on it, and a reader matches them against the pod names the API lists.
			corev1.ContainerArgs{
				Name: pulumi.String("cert-publish"), Image: pulumi.String(s.images.tools), Command: publishCommand, Args: publishArgs,
				Env: corev1.EnvVarArray{corev1.EnvVarArgs{Name: pulumi.String("HOSTNAME"), ValueFrom: &corev1.EnvVarSourceArgs{
					FieldRef: &corev1.ObjectFieldSelectorArgs{FieldPath: pulumi.String("metadata.name")},
				}}},
				VolumeMounts: corev1.VolumeMountArray{stateMount, tlsMount},
				Resources:    resources("100m", "128Mi"), SecurityContext: tailnetContainerSecurity(),
			},
			// The advertiser (tailnetAdvertiserScript, with the Service's name as its certificate):
			// it advertises the Service once the gate's flag names this pod's node, orders the name's
			// certificate once the node is approved, and writes the ready file the tailscale
			// container's readiness probe reads. It is named for that whole job, as the shared host
			// names it, and not for the certificate order alone. The Tailscale image, for its CLI.
			corev1.ContainerArgs{
				Name: pulumi.String("advertiser"), Image: pulumi.String(s.images.tailscale), Command: advertiserCommand, Args: advertiserArgs,
				VolumeMounts: corev1.VolumeMountArray{stateMount},
				Resources:    resources("100m", "128Mi"), SecurityContext: tailnetContainerSecurity(),
			},
		},
		Volumes: corev1.VolumeArray{
			corev1.VolumeArgs{Name: pulumi.String("state"), EmptyDir: &corev1.EmptyDirVolumeSourceArgs{}},
			corev1.VolumeArgs{Name: pulumi.String("serve"), ConfigMap: &corev1.ConfigMapVolumeSourceArgs{Name: pulumi.String(serveName)}},
			corev1.VolumeArgs{Name: pulumi.String("tls"), Secret: &corev1.SecretVolumeSourceArgs{SecretName: pulumi.String(tlsName), Optional: pulumi.Bool(false)}},
		},
	}
	// The pod names the ConfigMap and the TLS Secret by their constant names, and the Deployment
	// waits for both, so no pod waits on a missing volume and a ConfigMap the provider replaces
	// changes nothing in the Deployment's inputs. It also waits for whatever tailnetService came
	// from: on a fresh stack no pod exists, and so none advertises, before the Tailscale Service
	// does, since a node that names a Service before it exists is not approved when it appears
	// (tailnetAdvertiseStuckSeconds).
	serviceDeps := serveJSON.ApplyT(func(string) []pulumi.Resource { return nil }).(pulumi.ResourceArrayOutput)
	opts := []pulumi.ResourceOption{pulumi.DependsOn([]pulumi.Resource{serve, tlsSecret}), pulumi.DependsOnInputs(serviceDeps)}
	return pod, opts, nil
}
