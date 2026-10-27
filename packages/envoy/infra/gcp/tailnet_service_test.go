package gcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"
)

// These tests sit inside the package because the pieces they run are unexported; they reach the
// shell as gcp_test's shellCommand does, by /bin/sh or the words of ENVOY_TEST_SHELL, so the
// scripts run under both dash and bash. A missing shell or tool fails the test rather than
// skipping it.

// tailnetTestShell is the command that runs a script under test, with args after it.
func tailnetTestShell(ctx context.Context, args ...string) *exec.Cmd {
	shell := strings.Fields(os.Getenv("ENVOY_TEST_SHELL"))
	if len(shell) == 0 {
		shell = []string{"/bin/sh"}
	}
	return exec.CommandContext(ctx, shell[0], append(shell[1:], args...)...)
}

// The stand-ins every script test puts first on PATH, all logging to FAKE/calls, each line
// starting with the fake clock in FAKE/clock:
//   - tailscale answers `status --json --peers=false` as the indented JSON the CLI prints, for the
//     node in FAKE/self-id (failing without one), with the self node's service-host capability
//     listing another Service always and SVC only while FAKE/approved and FAKE/advertised both
//     exist; a second capability names SVC too, which must not count. It keeps the advertisement
//     in FAKE/advertised, answers a certificate order for CERT by FAKE/cert-mode (ok, fail, or by
//     default `invalid domain`), and logs out by forgetting the node.
//   - wget answers with the status code in FAKE/health (none, or "down", is a refused connection).
//   - sleep moves the clock on, runs FAKE/hook with $now and $d set, and ends the script once the
//     clock reaches FAKE/limit.
//   - date prints the fake clock for +%s; timeout logs its bound and runs the rest.
const fakeTailnetTailscale = `#!/bin/sh
d=FAKE
a="$*"
a=${a#--socket=* }
r=
[ -e READY ] && r=" [ready]"
echo "$(cat $d/clock) tailscale $*$r" >>$d/calls
case "$a" in
"status --json --peers=false")
  [ -s $d/self-id ] || exit 1
  printf '{\n  "Version": "1.104.1",\n  "BackendState": "Running",\n  "Self": {\n    "ID": "%s",\n    "HostName": "example-host-a",\n' "$(cat $d/self-id)"
  printf '    "Capabilities": [\n      "example-cap",\n      "service-host"\n    ],\n'
  printf '    "CapMap": {\n      "example-cap": [\n        {\n          "SVC": []\n        }\n      ],\n'
  printf '      "service-host": [\n        {\n          "SVC-two": [\n            "100.100.100.1"\n          ]'
  if [ -e $d/approved ] && [ -e $d/advertised ]; then
    printf ',\n          "SVC": [\n            "100.100.100.2",\n            "fd7a:115c:a1e0::2"\n          ]'
  fi
  printf '\n        }\n      ]\n    },\n    "Online": true\n  }\n}\n' ;;
"debug prefs")
  if [ -e $d/advertised ]; then printf '{\n\t"AdvertiseServices": [\n\t\t"SVC"\n\t]\n}\n'; else printf '{\n\t"AdvertiseServices": null\n}\n'; fi ;;
"serve advertise SVC") touch $d/advertised ;;
"serve drain SVC") rm -f $d/advertised ;;
"cert --cert-file=- --key-file=- CERT")
  case "$(cat $d/cert-mode 2>/dev/null)" in
  ok) echo "-----BEGIN CERTIFICATE-----" ;;
  fail) echo "acme: authorization for CERT failed" >&2; exit 1 ;;
  *) echo '500 Internal Server Error: invalid domain "CERT"; must be one of ["example-host-a.example-tailnet.ts.net"]' >&2; exit 1 ;;
  esac ;;
logout) rm -f $d/self-id $d/advertised ;;
*) echo "unexpected: tailscale $*" >&2; exit 2 ;;
esac
`

const fakeTailnetWget = `#!/bin/sh
d=FAKE
code=$(cat $d/health 2>/dev/null)
echo "$(cat $d/clock) wget $* -> ${code:-down}" >>$d/calls
case "$code" in
''|down) echo "wget: can't connect to remote host: Connection refused" >&2; exit 1 ;;
esac
echo "Connecting to 127.0.0.1:8222 (127.0.0.1:8222)" >&2
echo "  HTTP/1.1 $code Example" >&2
case "$code" in
2??) exit 0 ;;
*) echo "wget: server returned error: HTTP/1.1 $code Example" >&2; exit 1 ;;
esac
`

const fakeTailnetSleep = `#!/bin/sh
d=FAKE
now=$(( $(cat $d/clock) + ${1%%.*} ))
echo $now >$d/clock.$$ && mv $d/clock.$$ $d/clock
r=no
[ -e READY ] && r=yes
echo "$now sleep $1 ready=$r" >>$d/calls
[ -f $d/hook ] && . $d/hook
[ "$now" -lt "$(cat $d/limit)" ] || kill -TERM $PPID
exit 0
`

const fakeTailnetDate = `#!/bin/sh
case "$*" in *+%s*) cat FAKE/clock ;; *) exec REALDATE "$@" ;; esac
`

const fakeTailnetTimeout = `#!/bin/sh
echo "$(cat FAKE/clock) timeout $1" >>FAKE/calls
shift
exec "$@"
`

// tailnetRigStart is the clock every rig starts at.
const tailnetRigStart = 1000

const (
	tailnetRigService     = "example"
	tailnetRigCertificate = "example.example-tailnet.ts.net"
	tailnetRigHealth      = "http://127.0.0.1:8222/healthz"
	tailnetRigGrace       = 60
	tailnetRigNode        = "nAAAACNTRL"
)

// tailnetRig is one pod's state volume, rooted in a test directory, with the stand-ins first on
// PATH and the host the scripts are rendered from.
type tailnetRig struct {
	fake string
	host tailnetServiceHost
}

// newTailnetRig is a rig for a host with a health URL (health) or a certificate (certificate),
// whose tailscaled runs as node selfID ("" for none).
func newTailnetRig(t *testing.T, selfID string, configure func(*tailnetServiceHost)) *tailnetRig {
	t.Helper()
	shell := strings.Fields(os.Getenv("ENVOY_TEST_SHELL"))
	if len(shell) == 0 {
		shell = []string{"/bin/sh"}
	}
	for _, tool := range append(shell[:1], "cat", "grep", "sed", "awk", "head", "tail", "tr", "mv", "rm", "touch") {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH; the Tailscale image carries it, and this test fails rather than skips", tool)
		}
	}
	realDate, err := exec.LookPath("date")
	require.NoError(t, err, "date is not on PATH")
	dir := t.TempDir()
	r := &tailnetRig{
		fake: filepath.Join(dir, "fake"),
		host: tailnetServiceHost{
			paths:         tailnetServicePaths{mount: filepath.Join(dir, "state")},
			name:          tailnetRigService,
			authKeySecret: "example-auth-key",
			authKeyKey:    "TS_AUTHKEY",
			tags:          []string{"tag:example"},
			serveConfig:   "/etc/tailscale/serve.json",
			graceSeconds:  tailnetRigGrace,
		},
	}
	configure(&r.host)
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{r.fake, r.host.paths.mount, bin} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	fill := strings.NewReplacer("FAKE", r.fake, "READY", r.host.paths.ready(), "REALDATE", realDate,
		"SVC", r.host.service(), "CERT", tailnetRigCertificate)
	for name, body := range map[string]string{
		"tailscale": fakeTailnetTailscale, "wget": fakeTailnetWget, "sleep": fakeTailnetSleep,
		"date": fakeTailnetDate, "timeout": fakeTailnetTimeout,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(fill.Replace(body)), 0o755))
	}
	r.set(t, "clock", strconv.Itoa(tailnetRigStart))
	r.set(t, "limit", strconv.Itoa(1<<30))
	if selfID != "" {
		r.set(t, "self-id", selfID)
	}
	return r
}

func tailnetWithHealth(h *tailnetServiceHost)      { h.health = tailnetRigHealth }
func tailnetWithCertificate(h *tailnetServiceHost) { h.certificate = tailnetRigCertificate }

// set writes one of the stand-ins' files.
func (r *tailnetRig) set(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(r.fake, name), []byte(content+"\n"), 0o644))
}

// hook has every stand-in sleep run body, with $now the clock it moved to and $d the stand-ins'
// directory.
func (r *tailnetRig) hook(t *testing.T, body string) { r.set(t, "hook", body) }

// flag writes the advertise gate's flag naming node, as a consumer's gate writes it.
func (r *tailnetRig) flag(t *testing.T, node string) {
	t.Helper()
	require.NoError(t, os.WriteFile(r.host.paths.advertiseFlag(), []byte(node+" claim 1/example-host-a\n"), 0o644))
}

func (r *tailnetRig) env() []string {
	return append(os.Environ(), "PATH="+filepath.Join(filepath.Dir(r.fake), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+r.fake)
}

type tailnetEvent struct {
	at   int
	what string
}

func (r *tailnetRig) events(t *testing.T) []tailnetEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.fake, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []tailnetEvent
	if len(bytes.TrimSpace(b)) == 0 {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		at, what, ok := strings.Cut(line, " ")
		n, err := strconv.Atoi(at)
		if !ok || err != nil {
			t.Fatalf("a stand-in logged a line with no clock: %q", line)
		}
		out = append(out, tailnetEvent{n, what})
	}
	return out
}

// run runs the advertiser until the clock has moved seconds on, and returns the calls it made and
// what it printed.
func (r *tailnetRig) run(t *testing.T, seconds int) ([]tailnetEvent, string) {
	t.Helper()
	clock, err := os.ReadFile(filepath.Join(r.fake, "clock"))
	require.NoError(t, err)
	now, _ := strconv.Atoi(strings.TrimSpace(string(clock)))
	r.set(t, "limit", strconv.Itoa(now+seconds))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := tailnetTestShell(ctx, "-c", tailnetAdvertiserScript(r.host))
	cmd.Env = r.env()
	out, _ := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the advertiser did not stop at its clock limit:\n%s", out)
	}
	return r.events(t), string(out)
}

// tailnetIndexOf is the first event at or after from that contains substr, or -1.
func tailnetIndexOf(ev []tailnetEvent, from int, substr string) int {
	for i := max(from, 0); i < len(ev); i++ {
		if strings.Contains(ev[i].what, substr) {
			return i
		}
	}
	return -1
}

// tailnetCount is the number of events in ev[from:to] that contain substr.
func tailnetCount(ev []tailnetEvent, from, to int, substr string) int {
	n := 0
	for i := max(from, 0); i < min(to, len(ev)); i++ {
		if strings.Contains(ev[i].what, substr) {
			n++
		}
	}
	return n
}

// tailnetReadyWhile is the first sleep in ev[from:to] that saw the ready file, or -1.
func tailnetReadyWhile(ev []tailnetEvent, from, to int) int {
	for i := max(from, 0); i < min(to, len(ev)); i++ {
		if strings.HasPrefix(ev[i].what, "sleep ") && strings.HasSuffix(ev[i].what, "ready=yes") {
			return i
		}
	}
	return -1
}

func tailnetFileHolds(path, want string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) == want
}

func tailnetExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func tailnetDump(ev []tailnetEvent) string {
	var b strings.Builder
	for _, e := range ev {
		fmt.Fprintf(&b, "%d %s\n", e.at, e.what)
	}
	return b.String()
}

const (
	tailnetAdvertiseCall = "serve advertise svc:" + tailnetRigService
	tailnetDrainCall     = "serve drain svc:" + tailnetRigService
	tailnetOrderCall     = " cert --cert-file=- --key-file=- " + tailnetRigCertificate
)

// With a health URL the advertiser advertises only after tailnetHealthRise answers of HTTP 200 in
// a row, each GET given at least 6 s. The negative controls: a URL that answers 204, or refuses
// the connection, is never healthy, and the node is never advertised.
func TestTailnetServiceAdvertisesAfterTwoHealthyAnswers(t *testing.T) {
	for _, code := range []string{"204", "down"} {
		r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
		r.set(t, "health", code)
		ev, out := r.run(t, 120)
		if tailnetIndexOf(ev, 0, "serve ") >= 0 || tailnetExists(r.host.paths.ready()) {
			t.Errorf("health %s: the advertiser advertised or became ready:\n%s\n%s", code, tailnetDump(ev), out)
		}
		if tailnetCount(ev, 0, len(ev), "wget ") < 10 {
			t.Errorf("health %s: want a GET every 5 s:\n%s", code, tailnetDump(ev))
		}
	}

	r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "health", "503")
	r.hook(t, `if [ "$now" -ge 1020 ] && [ ! -e $d/healed ]; then echo 200 >$d/health; touch $d/healed; echo "$now healed" >>$d/calls; fi`)
	ev, out := r.run(t, 120)
	healed := tailnetIndexOf(ev, 0, "healed")
	advertise := tailnetIndexOf(ev, 0, tailnetAdvertiseCall)
	switch {
	case healed < 0 || advertise < 0:
		t.Fatalf("healed at %d, advertised at %d:\n%s\n%s", healed, advertise, tailnetDump(ev), out)
	case advertise < healed:
		t.Fatalf("advertised at %d, before the URL answered 200 at %d:\n%s", advertise, healed, tailnetDump(ev))
	}
	if n := tailnetCount(ev, healed, advertise, "-> 200"); n != tailnetHealthRise {
		t.Errorf("advertised after %d answers of 200, want %d:\n%s", n, tailnetHealthRise, tailnetDump(ev))
	}
	for _, e := range ev {
		if strings.Contains(e.what, "wget ") && !strings.Contains(e.what, fmt.Sprintf(" -T %d %s ", tailnetHealthTimeoutSeconds, tailnetRigHealth)) {
			t.Errorf("a GET without the %d s timeout: %s", tailnetHealthTimeoutSeconds, e.what)
		}
	}
	require.GreaterOrEqual(t, tailnetHealthTimeoutSeconds, 6)
}

// The ready file appears only once the node's service-host capability lists the Service: every
// moment before, with the node advertised and healthy, it is absent. The capability's other
// Service and another capability naming this Service count for nothing.
func TestTailnetServiceIsReadyOnlyOnceApproved(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "health", "200")
	r.hook(t, `if [ "$now" -ge 1100 ] && [ ! -e $d/approved ]; then touch $d/approved; echo "$now approved" >>$d/calls; fi`)
	ev, out := r.run(t, 200)
	approved := tailnetIndexOf(ev, 0, "approved")
	if tailnetIndexOf(ev, 0, tailnetAdvertiseCall) < 0 || approved < 0 {
		t.Fatalf("never advertised or approved:\n%s\n%s", tailnetDump(ev), out)
	}
	if i := tailnetReadyWhile(ev, 0, approved+1); i >= 0 {
		t.Errorf("ready at %d, before the approval at %d:\n%s", ev[i].at, ev[approved].at, tailnetDump(ev))
	}
	if tailnetReadyWhile(ev, approved, len(ev)) < 0 || !tailnetFileHolds(r.host.paths.ready(), tailnetRigNode) {
		t.Errorf("not ready after the approval:\n%s\n%s", tailnetDump(ev), out)
	}
	if !tailnetFileHolds(r.host.paths.nodeID(), tailnetRigNode) {
		t.Errorf("the node id file does not hold %s", tailnetRigNode)
	}
}

// A ready host whose health URL fails tailnetHealthFall times in a row drains the Service and
// loses the ready file, and not after fewer failures; once the URL answers 200 again it is
// advertised again after tailnetHealthRise healthy answers, and ready again.
func TestTailnetServiceDrainsAfterThreeFailuresAndAdvertisesAgain(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "health", "200")
	r.set(t, "approved", "")
	r.hook(t, fmt.Sprintf(`if [ -e %[1]s ] && [ ! -e $d/broken ]; then echo 503 >$d/health; echo $now >$d/broken; echo "$now broken" >>$d/calls; fi
if [ -e $d/broken ] && [ ! -e $d/mended ] && [ "$now" -ge $(($(cat $d/broken) + 60)) ]; then echo 200 >$d/health; touch $d/mended; echo "$now mended" >>$d/calls; fi`,
		r.host.paths.ready()))
	ev, out := r.run(t, 300)
	broken := tailnetIndexOf(ev, 0, "broken")
	drain := tailnetIndexOf(ev, broken, tailnetDrainCall)
	mended := tailnetIndexOf(ev, broken, "mended")
	again := tailnetIndexOf(ev, mended, tailnetAdvertiseCall)
	switch {
	case broken < 0 || drain < 0 || mended < 0 || again < 0:
		t.Fatalf("broken %d, drained %d, mended %d, advertised again %d:\n%s\n%s", broken, drain, mended, again, tailnetDump(ev), out)
	case drain > mended:
		t.Fatalf("drained at %d, after the URL was mended at %d:\n%s", drain, mended, tailnetDump(ev))
	}
	if n := tailnetCount(ev, broken, drain, "-> 503"); n != tailnetHealthFall {
		t.Errorf("drained after %d failures, want %d:\n%s", n, tailnetHealthFall, tailnetDump(ev))
	}
	if i := tailnetReadyWhile(ev, drain, again); i >= 0 {
		t.Errorf("ready at %d, between the drain and the new advertisement:\n%s", ev[i].at, tailnetDump(ev))
	}
	if n := tailnetCount(ev, mended, again, "-> 200"); n != tailnetHealthRise {
		t.Errorf("advertised again after %d answers of 200, want %d:\n%s", n, tailnetHealthRise, tailnetDump(ev))
	}
	if tailnetReadyWhile(ev, again, len(ev)) < 0 || !tailnetFileHolds(r.host.paths.ready(), tailnetRigNode) {
		t.Errorf("not ready again after the recovery:\n%s\n%s", tailnetDump(ev), out)
	}
}

// While the node stays unapproved the advertiser leaves the advertisement alone up to
// tailnetAdvertiseStuckSeconds after it advertised (the negative control), then drains, pauses
// tailnetDrainPauseSeconds and advertises again, logging the attempt, and waits twice as long
// before the next. It never writes the ready file.
func TestTailnetServiceRecoversAStuckAdvertisement(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "health", "200")
	ev, out := r.run(t, tailnetAdvertiseStuckSeconds)
	if tailnetIndexOf(ev, 0, tailnetAdvertiseCall) < 0 || tailnetIndexOf(ev, 0, "serve drain") >= 0 {
		t.Errorf("within the bound: want an advertisement and no drain:\n%s\n%s", tailnetDump(ev), out)
	}

	r = newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "health", "200")
	ev, out = r.run(t, 4*tailnetAdvertiseStuckSeconds+2*tailnetDrainPauseSeconds+60)
	advertise := tailnetIndexOf(ev, 0, tailnetAdvertiseCall)
	drain := tailnetIndexOf(ev, advertise, tailnetDrainCall)
	again := tailnetIndexOf(ev, drain, tailnetAdvertiseCall)
	next := tailnetIndexOf(ev, again, tailnetDrainCall)
	switch {
	case advertise < 0 || drain < 0 || again < 0 || next < 0:
		t.Fatalf("want advertise, drain, advertise, drain: %d %d %d %d:\n%s\n%s", advertise, drain, again, next, tailnetDump(ev), out)
	case ev[drain].at-ev[advertise].at <= tailnetAdvertiseStuckSeconds:
		t.Errorf("drained %ds after advertising, want more than %d", ev[drain].at-ev[advertise].at, tailnetAdvertiseStuckSeconds)
	case ev[again].at-ev[drain].at < tailnetDrainPauseSeconds:
		t.Errorf("advertised again %ds after the drain, want at least %d", ev[again].at-ev[drain].at, tailnetDrainPauseSeconds)
	case ev[next].at-ev[again].at <= 2*tailnetAdvertiseStuckSeconds:
		t.Errorf("the second recovery came %ds after the first, want more than %d", ev[next].at-ev[again].at, 2*tailnetAdvertiseStuckSeconds)
	}
	if logged := strings.Count(out, ": drained and advertised again;"); logged != tailnetCount(ev, 0, len(ev), "serve drain") || !strings.Contains(out, "| recovery 2: not approved") {
		t.Errorf("want one log line per recovery:\n%s", out)
	}
	if tailnetExists(r.host.paths.ready()) || tailnetReadyWhile(ev, 0, len(ev)) >= 0 {
		t.Error("an unapproved host wrote the ready file")
	}
}

// While the preStop hook's draining file names the node and is younger than the grace period,
// the advertiser does nothing: no GET, no advertisement, no drain. Once the file is older, it is
// removed and the node is advertised again, with no one deleting the pod.
func TestTailnetServiceStaysQuietWhileDraining(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "health", "200")
	r.set(t, "approved", "")
	require.NoError(t, os.WriteFile(r.host.paths.draining(), []byte(fmt.Sprintf("%s %d\n", tailnetRigNode, tailnetRigStart)), 0o644))
	ev, out := r.run(t, 2*tailnetRigGrace)
	over := tailnetRigStart + tailnetRigGrace
	for _, e := range ev {
		if e.at <= over && (strings.Contains(e.what, "wget ") || strings.Contains(e.what, " serve ")) {
			t.Errorf("at %d, while the draining file of %d was fresh (until %d): %s", e.at, tailnetRigStart, over, e.what)
		}
	}
	if i := tailnetReadyWhile(ev, 0, len(ev)); i >= 0 && ev[i].at <= over {
		t.Errorf("ready at %d while draining", ev[i].at)
	}
	advertise := tailnetIndexOf(ev, 0, tailnetAdvertiseCall)
	if advertise < 0 || tailnetExists(r.host.paths.draining()) || !strings.Contains(out, "| rm -f "+r.host.paths.draining()+" |") {
		t.Errorf("the node was not advertised again, or the draining file stayed, once it was over:\n%s\n%s", tailnetDump(ev), out)
	}
	if !tailnetFileHolds(r.host.paths.ready(), tailnetRigNode) {
		t.Errorf("not ready once the draining file was over:\n%s\n%s", tailnetDump(ev), out)
	}
}

// A ready pod whose tailscaled came back as a new node (the preStop hook logged the old one out
// when a probe killed the container) is not left ready on a stale file: the advertiser removes
// it, writes the new id, advertises the new node and is ready as it, with no one deleting the pod.
// The negative control: an id that is not alphanumeric is never written and never advertised.
func TestTailnetServiceFollowsANewNode(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "health", "200")
	r.set(t, "approved", "")
	r.hook(t, fmt.Sprintf(`if [ -e %s ] && [ ! -e $d/switched ]; then
  echo nBBBBCNTRL >$d/self-id; rm -f $d/advertised; touch $d/switched; echo "$now new node" >>$d/calls
fi`, r.host.paths.ready()))
	ev, out := r.run(t, 120)
	switched := tailnetIndexOf(ev, 0, "new node")
	advertise := tailnetIndexOf(ev, switched, tailnetAdvertiseCall)
	if switched < 0 || advertise < 0 {
		t.Fatalf("never switched (%d) or never advertised the new node (%d):\n%s\n%s", switched, advertise, tailnetDump(ev), out)
	}
	if i := tailnetReadyWhile(ev, switched+1, advertise); i >= 0 {
		t.Errorf("still ready at %d on the old node's file:\n%s", ev[i].at, tailnetDump(ev))
	}
	if !tailnetFileHolds(r.host.paths.ready(), "nBBBBCNTRL") || !tailnetFileHolds(r.host.paths.nodeID(), "nBBBBCNTRL") {
		t.Errorf("not ready as the new node, or its id not written:\n%s\n%s", tailnetDump(ev), out)
	}

	r = newTailnetRig(t, "nA}{x", tailnetWithHealth)
	r.set(t, "health", "200")
	ev, out = r.run(t, 60)
	if tailnetExists(r.host.paths.nodeID()) || tailnetIndexOf(ev, 0, "serve advertise") >= 0 || !strings.Contains(out, "not alphanumeric") {
		t.Errorf("an id that is not alphanumeric was written or advertised:\n%s\n%s", tailnetDump(ev), out)
	}
}

// With a certificate the advertiser advertises only once the advertise flag names this pod's
// current node and no preStop hook drains it, orders a certificate only after it has advertised,
// never for any name but the Service's, and writes the ready file once tailscaled holds it. The
// three refusals are the negative controls; the drained one runs only as long as its draining file
// holds.
func TestTailnetServiceWithACertificateAdvertisesOnlyTheFlaggedNode(t *testing.T) {
	for name, c := range map[string]struct{ flag, draining string }{
		"no flag":                 {},
		"a flag for another node": {flag: "nOLDCNTRL"},
		"this node, drained":      {flag: tailnetRigNode, draining: fmt.Sprintf("%s %d", tailnetRigNode, tailnetRigStart)},
	} {
		r := newTailnetRig(t, tailnetRigNode, tailnetWithCertificate)
		r.set(t, "cert-mode", "ok")
		r.set(t, "approved", "")
		if c.flag != "" {
			r.flag(t, c.flag)
		}
		seconds := 120
		if c.draining != "" {
			require.NoError(t, os.WriteFile(r.host.paths.draining(), []byte(c.draining+"\n"), 0o644))
			seconds = tailnetRigGrace
		}
		ev, out := r.run(t, seconds)
		if tailnetIndexOf(ev, 0, "serve ") >= 0 || tailnetIndexOf(ev, 0, " cert ") >= 0 || tailnetExists(r.host.paths.ready()) {
			t.Errorf("%s: the advertiser advertised, ordered or became ready:\n%s\n%s", name, tailnetDump(ev), out)
		}
		if !tailnetFileHolds(r.host.paths.nodeID(), tailnetRigNode) {
			t.Errorf("%s: the node id file does not hold %s, which the gate needs to record the node", name, tailnetRigNode)
		}
	}

	r := newTailnetRig(t, tailnetRigNode, tailnetWithCertificate)
	r.set(t, "cert-mode", "ok")
	r.set(t, "approved", "")
	r.flag(t, tailnetRigNode)
	ev, out := r.run(t, 60)
	advertise := tailnetIndexOf(ev, 0, tailnetAdvertiseCall)
	order := tailnetIndexOf(ev, 0, tailnetOrderCall)
	if advertise < 0 || order < advertise {
		t.Errorf("advertise at %d, the order at %d, want the advertisement first:\n%s", advertise, order, tailnetDump(ev))
	}
	for _, e := range ev {
		if what := strings.TrimSuffix(e.what, " [ready]"); strings.Contains(what, " cert ") && !strings.HasSuffix(what, tailnetOrderCall) {
			t.Errorf("the advertiser asked for a certificate other than the Service's: %s", e.what)
		}
	}
	if tailnetCount(ev, 0, len(ev), "wget ") != 0 {
		t.Error("a host with no health URL sent a GET")
	}
	if !tailnetFileHolds(r.host.paths.ready(), tailnetRigNode) {
		t.Errorf("no ready file naming %s after the certificate:\n%s\n%s", tailnetRigNode, tailnetDump(ev), out)
	}
}

// With a certificate the ready file appears only once the node is approved and tailscaled holds
// the certificate: no order is made before the approval, and no ready file before the order
// succeeds. An approved node whose name tailscaled still refuses (`invalid domain`) orders again
// without counting the refusals, and is never ready.
func TestTailnetServiceWithACertificateIsReadyOnlyApprovedAndHoldingIt(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithCertificate)
	r.set(t, "cert-mode", "ok")
	r.flag(t, tailnetRigNode)
	r.hook(t, `if [ "$now" -ge 1090 ] && [ ! -e $d/approved ]; then touch $d/approved; echo "$now approved" >>$d/calls; fi`)
	ev, out := r.run(t, 200)
	approved := tailnetIndexOf(ev, 0, "approved")
	order := tailnetIndexOf(ev, 0, tailnetOrderCall)
	switch {
	case approved < 0 || order < 0:
		t.Fatalf("approved at %d, ordered at %d:\n%s\n%s", approved, order, tailnetDump(ev), out)
	case order < approved:
		t.Errorf("ordered at %d, before the approval at %d:\n%s", ev[order].at, ev[approved].at, tailnetDump(ev))
	}
	if i := tailnetReadyWhile(ev, 0, order); i >= 0 {
		t.Errorf("ready at %d, before the certificate at %d:\n%s", ev[i].at, ev[order].at, tailnetDump(ev))
	}
	if tailnetReadyWhile(ev, order, len(ev)) < 0 || !tailnetFileHolds(r.host.paths.ready(), tailnetRigNode) {
		t.Errorf("not ready after the certificate:\n%s\n%s", tailnetDump(ev), out)
	}

	r = newTailnetRig(t, tailnetRigNode, tailnetWithCertificate)
	r.set(t, "approved", "")
	r.flag(t, tailnetRigNode)
	ev, out = r.run(t, 600)
	if n := tailnetCount(ev, 0, len(ev), tailnetOrderCall); n <= tailnetCertOrderAttempts {
		t.Errorf("%d refused orders, want them retried past %d, uncounted:\n%s", n, tailnetCertOrderAttempts, tailnetDump(ev))
	}
	if tailnetExists(r.host.paths.ready()) || tailnetReadyWhile(ev, 0, len(ev)) >= 0 || tailnetIndexOf(ev, 0, "serve drain") >= 0 || strings.Contains(out, "no more from this pod") {
		t.Errorf("a refused name made the pod ready, or spent its orders:\n%s\n%s", tailnetDump(ev), out)
	}
}

// Orders that fail for a reason other than `invalid domain` are bounded: tailnetCertOrderAttempts
// of them, then the pod drains the Service, orders no more and never becomes ready.
func TestTailnetServiceWithACertificateBoundsItsOrders(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithCertificate)
	r.set(t, "cert-mode", "fail")
	r.set(t, "approved", "")
	r.flag(t, tailnetRigNode)
	ev, out := r.run(t, 4000)
	orders, last := 0, -1
	for i, e := range ev {
		if strings.Contains(e.what, tailnetOrderCall) {
			orders, last = orders+1, i
		}
	}
	if orders != tailnetCertOrderAttempts || tailnetIndexOf(ev, last, tailnetDrainCall) < 0 || tailnetExists(r.host.paths.ready()) ||
		!strings.Contains(out, "no more from this pod") {
		t.Errorf("%d orders (want %d), then a drain at %d:\n%s\n%s", orders, tailnetCertOrderAttempts, tailnetIndexOf(ev, last, tailnetDrainCall), tailnetDump(ev), out)
	}
}

// The preStop hook marks the node as draining and takes the ready file away first, then drains
// the Service, waits tailnetPreStopWaitSeconds and logs the node out, each bounded and each
// through the pod's own socket.
func TestTailnetServicePreStopDrainsThenLogsOut(t *testing.T) {
	r := newTailnetRig(t, tailnetRigNode, tailnetWithHealth)
	r.set(t, "advertised", "")
	for _, f := range []string{r.host.paths.nodeID(), r.host.paths.ready()} {
		require.NoError(t, os.WriteFile(f, []byte(tailnetRigNode+"\n"), 0o644))
	}
	cmd := tailnetTestShell(context.Background(), "-c", tailnetPreStopScript(r.host))
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	ev := r.events(t)
	socket := "tailscale --socket=" + r.host.paths.socket() + " "
	at := 0
	for _, want := range []string{
		fmt.Sprintf("timeout %d", tailnetPreStopDrainSeconds), socket + tailnetDrainCall,
		fmt.Sprintf("sleep %d ", tailnetPreStopWaitSeconds),
		fmt.Sprintf("timeout %d", tailnetPreStopLogoutSeconds), socket + "logout",
	} {
		i := tailnetIndexOf(ev, at, want)
		require.GreaterOrEqual(t, i, 0, "no %q after event %d:\n%s", want, at, tailnetDump(ev))
		at = i + 1
	}
	require.False(t, tailnetExists(r.host.paths.ready()), "the ready file stayed")
	require.True(t, tailnetFileHolds(r.host.paths.draining(), fmt.Sprintf("%s %d", tailnetRigNode, tailnetRigStart)),
		"the draining file does not name the node and the time it began")

	// With no node id (tailscaled never ran as a node) there is nothing to keep from being
	// advertised: no draining file, and the drain and logout still run.
	r = newTailnetRig(t, "", tailnetWithHealth)
	cmd = tailnetTestShell(context.Background(), "-c", tailnetPreStopScript(r.host))
	cmd.Env = r.env()
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.False(t, tailnetExists(r.host.paths.draining()))
	require.GreaterOrEqual(t, tailnetIndexOf(r.events(t), 0, "logout"), 0)
}

// The hook and containerboot's own shutdown fit a 60 s grace period, with time left for
// tailscaled to exit on its SIGTERM.
func TestTailnetServiceShutdownFitsTheGracePeriod(t *testing.T) {
	require.Equal(t, tailnetPreStopDrainSeconds+tailnetPreStopWaitSeconds+tailnetPreStopLogoutSeconds, tailnetPreStopSeconds)
	require.Equal(t, tailnetPreStopSeconds+tailnetContainerbootShutdownSeconds, tailnetShutdownSeconds)
	require.LessOrEqual(t, tailnetShutdownSeconds, 60)
	require.Equal(t, 25, tailnetContainerbootShutdownSeconds, "containerboot's bound at cmd/containerboot/main.go:488")
}

func tailnetEnvValue(t *testing.T, env map[string]corev1.EnvVarArgs, name string) string {
	t.Helper()
	e, ok := env[name]
	require.True(t, ok, "no %s", name)
	v, ok := e.Value.(pulumi.String)
	require.True(t, ok, "%s has no plain value", name)
	return string(v)
}

func tailnetEnvOf(t *testing.T, h tailnetServiceHost) map[string]corev1.EnvVarArgs {
	t.Helper()
	env := map[string]corev1.EnvVarArgs{}
	for _, in := range tailnetServiceEnv(h) {
		e, ok := in.(corev1.EnvVarArgs)
		require.True(t, ok)
		name := string(e.Name.(pulumi.String))
		_, dup := env[name]
		require.False(t, dup, "%s twice", name)
		env[name] = e
	}
	return env
}

// The tailscale container's environment: state on a subdirectory of the state volume, never its
// mount point (tailnetServicePaths.stateDir says why), no Kubernetes Secret for state, no
// automatic advertisement, a pod-named node, the socket on the state volume, and a SOCKS5 server
// only when one is asked for.
func TestTailnetServiceEnv(t *testing.T) {
	h := tailnetServiceHost{
		paths: tailnetServicePaths{mount: "/var/lib/example"}, name: tailnetRigService,
		authKeySecret: "example-auth-key", authKeyKey: "TS_AUTHKEY", tags: []string{"tag:example", "tag:example-two"},
		serveConfig: "/etc/tailscale/serve.json", graceSeconds: tailnetRigGrace,
	}
	env := tailnetEnvOf(t, h)
	state := tailnetEnvValue(t, env, "TS_STATE_DIR")
	require.NotEqual(t, filepath.Clean(h.paths.mount), filepath.Clean(state), "TS_STATE_DIR is the mount point")
	require.Equal(t, h.paths.mount, filepath.Dir(state), "TS_STATE_DIR is not a directory of the state volume")
	require.Equal(t, state, tailnetEnvValue(t, env, "TS_LOGS_DIR"))
	require.Equal(t, "", tailnetEnvValue(t, env, "TS_KUBE_SECRET"))
	require.Equal(t, "false", tailnetEnvValue(t, env, "TS_EXPERIMENTAL_SERVICE_AUTO_ADVERTISEMENT"))
	for name, want := range map[string]string{
		"TS_AUTH_ONCE": "true", "TS_USERSPACE": "true", "TS_ACCEPT_DNS": "false", "TS_ENABLE_HEALTH_CHECK": "true",
		"TS_EXTRA_ARGS": "--advertise-tags=tag:example,tag:example-two", "TS_SERVE_CONFIG": "/etc/tailscale/serve.json",
		"TS_LOCAL_ADDR_PORT": "127.0.0.1:9002", "TS_SOCKET": "/var/lib/example/tailscaled.sock",
	} {
		require.Equal(t, want, tailnetEnvValue(t, env, name), name)
	}
	host := env["TS_HOSTNAME"].ValueFrom.(*corev1.EnvVarSourceArgs).FieldRef.(*corev1.ObjectFieldSelectorArgs)
	require.Equal(t, pulumi.String("metadata.name"), host.FieldPath)
	key := env["TS_AUTHKEY"].ValueFrom.(*corev1.EnvVarSourceArgs).SecretKeyRef.(*corev1.SecretKeySelectorArgs)
	require.Equal(t, pulumi.String("example-auth-key"), key.Name)
	require.Equal(t, pulumi.String("TS_AUTHKEY"), key.Key)
	_, socks := env["TS_SOCKS5_SERVER"]
	require.False(t, socks, "a SOCKS5 server nobody asked for")

	h.socks5Server, h.localAddrPort = "0.0.0.0:1055", "127.0.0.1:9012"
	env = tailnetEnvOf(t, h)
	require.Equal(t, "0.0.0.0:1055", tailnetEnvValue(t, env, "TS_SOCKS5_SERVER"))
	require.Equal(t, "127.0.0.1:9012", tailnetEnvValue(t, env, "TS_LOCAL_ADDR_PORT"))
}

// The serve configs, byte for byte: an HTTPS Service terminating TLS on 443 and proxying to an
// in-cluster URL, and a TCP Service forwarding two ports.
func TestTailnetServeConfigs(t *testing.T) {
	require.Equal(t,
		`{"Services":{"svc:example-web":{"TCP":{"443":{"HTTPS":true}},"Web":{"example-web.example-tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://example-web.example.svc.cluster.local:8080"}}}}}}}`,
		tailnetServeHTTPS("example-web", "example-tailnet.ts.net", "http://example-web.example.svc.cluster.local:8080"))
	require.Equal(t,
		`{"Services":{"svc:example-tcp":{"TCP":{"4222":{"TCPForward":"example-backend.example.svc.cluster.local:4222"},"9020":{"TCPForward":"127.0.0.1:9020"}}}}}`,
		tailnetServeTCP("example-tcp", []tailnetTCPForward{
			{port: 9020, target: "127.0.0.1:9020"},
			{port: 4222, target: "example-backend.example.svc.cluster.local:4222"},
		}))
	require.Contains(t, tailnetServeHTTPS("a", "example-tailnet.ts.net", "http://b/?x=1&y=2"), `"Proxy":"http://b/?x=1&y=2"`)
}

func tailnetProbeCommand(t *testing.T, p *corev1.ProbeArgs) []string {
	t.Helper()
	var out []string
	for _, w := range p.Exec.(*corev1.ExecActionArgs).Command.(pulumi.StringArray) {
		out = append(out, string(w.(pulumi.String)))
	}
	return out
}

// The tailscale container's probes: liveness asks tailscaled's LocalAPI through the pod's socket
// and nothing more; readiness passes only with the ready file, run here under the shell; startup
// reads containerboot's /healthz.
func TestTailnetServiceProbes(t *testing.T) {
	h := tailnetServiceHost{paths: tailnetServicePaths{mount: t.TempDir()}, name: tailnetRigService}

	live := tailnetServiceLiveness(h)
	require.Equal(t, []string{"tailscale", "--socket=" + h.paths.socket(), "version", "--daemon"}, tailnetProbeCommand(t, live))
	require.Equal(t, pulumi.Int(10), live.PeriodSeconds)
	require.Equal(t, pulumi.Int(5), live.TimeoutSeconds)
	require.Equal(t, pulumi.Int(6), live.FailureThreshold)

	ready := tailnetServiceReadiness(h)
	require.Equal(t, pulumi.Int(5), ready.PeriodSeconds)
	require.Equal(t, pulumi.Int(1), ready.FailureThreshold)
	command := tailnetProbeCommand(t, ready)
	require.Equal(t, []string{"/bin/sh", "-c"}, command[:2])
	require.Error(t, tailnetTestShell(context.Background(), "-c", command[2]).Run(), "ready with no ready file")
	require.NoError(t, os.WriteFile(h.paths.ready(), []byte(tailnetRigNode+"\n"), 0o644))
	require.NoError(t, tailnetTestShell(context.Background(), "-c", command[2]).Run(), "not ready with the ready file")

	require.Equal(t, []string{"wget", "-qO-", "http://127.0.0.1:9002/healthz"}, tailnetProbeCommand(t, tailnetServiceStartup(h)))

	stop := tailnetPreStop(h).PreStop.(*corev1.LifecycleHandlerArgs).Exec.(*corev1.ExecActionArgs).Command.(pulumi.StringArray)
	require.Equal(t, pulumi.StringArray{pulumi.String("/bin/sh"), pulumi.String("-c"), pulumi.String(tailnetPreStopScript(h))}, stop)
}

// Every container runs unprivileged as the pod's one uid.
func TestTailnetServiceSecurityContexts(t *testing.T) {
	pod := tailnetPodSecurity(999)
	require.Equal(t, pulumi.Int(999), pod.RunAsUser)
	require.Equal(t, pulumi.Int(999), pod.RunAsGroup)
	require.Equal(t, pulumi.Bool(true), pod.RunAsNonRoot)
	require.Equal(t, pulumi.String("RuntimeDefault"), pod.SeccompProfile.(*corev1.SeccompProfileArgs).Type)
	c := tailnetContainerSecurity()
	require.Equal(t, pulumi.Bool(false), c.AllowPrivilegeEscalation)
	require.Equal(t, pulumi.StringArray{pulumi.String("ALL")}, c.Capabilities.(*corev1.CapabilitiesArgs).Drop)
}
