package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	tlsProbeTimeout = 20 * time.Second
	tlsDetailLimit  = 300
)

type tlsSite struct {
	name, url, root string
	control         bool
}

var (
	emSignTLSSite  = tlsSite{name: "emSign", url: "https://dv-g3b-emudhra.we-have-a-problem.com/", root: "/usr/share/legion/tls-roots/emsign-ecc-root-ca-g3.pem"}
	aaaTLSSite     = tlsSite{name: "AAA", url: "https://dv-g3b-aaa-emudhra.we-have-a-problem.com/", root: "/usr/share/legion/tls-roots/aaa-certificate-services.pem"}
	controlTLSSite = tlsSite{name: "control", url: "https://example.com/", control: true}
	tlsSites       = []tlsSite{emSignTLSSite, aaaTLSSite, controlTLSSite}
)

type tlsStatus string

const (
	tlsCertificateRefused tlsStatus = "refused"
	tlsConnectionAccepted tlsStatus = "accepted"
	tlsNetworkUnavailable tlsStatus = "unavailable"
)

type tlsOutcome struct {
	status tlsStatus
	detail string
}

type tlsClient interface {
	name() string
	probe(context.Context, tlsSite) tlsOutcome
}

type tlsObservation struct {
	client string
	site   tlsSite
	tlsOutcome
}

// The root paths and fingerprints below are CA-published records from
// repository.emsign.com/certs/emSignECCRootCAG3.crt (86:A1:EC:BA:08:9C:4A:8D:3B:BE:27:34:C6:12:BA:34:1D:81:3E:04:3C:F9:E8:A8:62:CD:5C:57:A3:6B:BE:6B)
// and crt.sectigo.com/AAACertificateServices.crt
// (D7:A7:A0:FB:5D:7E:27:31:D7:71:E9:48:4E:BC:DE:F7:1D:5F:0C:3E:0A:29:48:78:2B:C8:3E:E0:EA:69:9E:F4).
func compromisedTLSSites() []tlsSite {
	var sites []tlsSite
	for _, site := range tlsSites {
		if site.root != "" {
			sites = append(sites, site)
		}
	}
	return sites
}

func verifyTLSPositiveControls(sites []tlsSite, probe func(tlsSite) tlsOutcome) error {
	var failures []string
	for _, site := range sites {
		if outcome := probe(site); outcome.status != tlsConnectionAccepted {
			failures = append(failures, fmt.Sprintf("%s positive control at %s did not accept its pinned root: %s", site.name, site.url, outcome.detail))
		}
		negative := controlTLSSite
		negative.root = site.root
		if outcome := probe(negative); outcome.status == tlsConnectionAccepted {
			failures = append(failures, fmt.Sprintf("%s positive control is not isolated: its pinned root accepted %s", site.name, negative.url))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("worker TLS probe cannot verify test chains: %s", strings.Join(failures, "; "))
	}
	return nil
}

func probeTLSPositiveControls(ctx context.Context, env []string, workDir string) error {
	return verifyTLSPositiveControls(compromisedTLSSites(), func(site tlsSite) tlsOutcome {
		client := tlsCommandClient{clientName: "positive-control", env: env, workDir: workDir, command: func(site tlsSite) (string, []string, []string) {
			return "curl", []string{"--cacert", site.root, "--capath", "/nonexistent", "--fail", "--silent", "--show-error", site.url}, nil
		}}
		return client.probe(ctx, site)
	})
}

var newTLSPositiveControls = probeTLSPositiveControls

// probeTLSClients holds every TLS client the worker image carries to the two compromised chains and
// a reachable control. A chain is safe only when its client refuses it for a certificate error; a
// network failure is neither a refusal nor an acceptance.
func probeTLSClients(ctx context.Context, clients []tlsClient) ([]tlsObservation, error) {
	var observations []tlsObservation
	var failures []string
	for _, client := range clients {
		for _, site := range tlsSites {
			outcome := client.probe(ctx, site)
			observation := tlsObservation{client: client.name(), site: site, tlsOutcome: outcome}
			observations = append(observations, observation)
			switch {
			case site.control && outcome.status != tlsConnectionAccepted:
				failures = append(failures, fmt.Sprintf("network unavailable: %s cannot reach %s: %s", client.name(), site.url, outcome.detail))
			case !site.control && outcome.status == tlsConnectionAccepted:
				failures = append(failures, fmt.Sprintf("%s accepts the %s chain: %s", client.name(), site.name, outcome.detail))
			case !site.control && outcome.status != tlsCertificateRefused:
				failures = append(failures, fmt.Sprintf("%s cannot verify refusal of the %s chain: %s", client.name(), site.name, outcome.detail))
			}
		}
	}
	if len(failures) > 0 {
		return observations, fmt.Errorf("worker TLS probe failed: %s", strings.Join(failures, "; "))
	}
	return observations, nil
}

type tlsCommandClient struct {
	clientName      string
	env             []string
	workDir         string
	command         func(tlsSite) (string, []string, []string)
	applicationSeen func(string) bool
}

func (c tlsCommandClient) name() string { return c.clientName }

func (c tlsCommandClient) probe(ctx context.Context, site tlsSite) tlsOutcome {
	program, args, extraEnv := c.command(site)
	commandCtx, cancel := context.WithTimeout(ctx, tlsProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, program, args...)
	cmd.Env = tlsEnvironment(c.env, append(extraEnv, "LEGION_TLS_URL="+site.url))
	cmd.Dir = c.workDir
	output, err := cmd.CombinedOutput()
	message := tlsCommandMessage(err, string(output))
	detail := trimTLSDetail(message)
	switch {
	case tlsCertificateError(message):
		return tlsOutcome{status: tlsCertificateRefused, detail: detail}
	case err == nil || c.applicationSeen != nil && c.applicationSeen(message):
		return tlsOutcome{status: tlsConnectionAccepted, detail: detail}
	default:
		return tlsOutcome{status: tlsNetworkUnavailable, detail: detail}
	}
}

func tlsEnvironment(base, additions []string) []string {
	out := make([]string, 0, len(base)+len(additions))
	for _, pair := range base {
		name, _, ok := strings.Cut(pair, "=")
		if !ok || !tlsEnvironmentOverrides(name, additions) {
			out = append(out, pair)
		}
	}
	return append(out, additions...)
}

func tlsEnvironmentOverrides(name string, additions []string) bool {
	for _, pair := range additions {
		if added, _, ok := strings.Cut(pair, "="); ok && added == name {
			return true
		}
	}
	return false
}

func tlsCommandMessage(err error, output string) string {
	var parts []string
	if err != nil {
		parts = append(parts, err.Error())
	}
	if text := strings.Join(strings.Fields(output), " "); text != "" {
		parts = append(parts, text)
	}
	if message := strings.Join(parts, ": "); message != "" {
		return message
	}
	return "the command returned no output"
}

func trimTLSDetail(detail string) string {
	if len(detail) > tlsDetailLimit {
		head := tlsDetailLimit / 3
		return detail[:head] + "…" + detail[len(detail)-(tlsDetailLimit-head-1):]
	}
	return detail
}

func tlsCertificateError(detail string) bool {
	lower := strings.ToLower(detail)
	for _, marker := range []string{
		"cert_revoked",
		"certificate revoked",
		"certificate verify failed",
		"invalid peer certificate",
		"ssl certificate problem",
		"x509:",
		"failed to verify certificate",
		"net::err_cert",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

const tlsFetchProgram = `const response = await fetch(process.env.LEGION_TLS_URL); if (!response.ok) throw new Error("HTTP " + response.status);`

type tlsClientSpec struct {
	name  string
	build func(invocation string, env []string, workDir string) tlsClient
}

func tlsCommandClientSpec(name, program string, args func(tlsSite) []string, applicationSeen func(string) bool, extraEnv ...string) tlsClientSpec {
	return tlsClientSpec{
		name: name,
		build: func(_ string, env []string, workDir string) tlsClient {
			return tlsCommandClient{
				clientName: name, env: env, workDir: workDir, applicationSeen: applicationSeen,
				command: func(site tlsSite) (string, []string, []string) {
					return program, args(site), extraEnv
				},
			}
		},
	}
}

// workerTLSClients is the complete TLS client inventory the worker image exposes. The probe and
// its fakes derive their client names from this list, so a client cannot silently fall out of tests.
var workerTLSClients = []tlsClientSpec{
	tlsCommandClientSpec("node", "node", func(tlsSite) []string { return []string{"--input-type=module", "--eval", tlsFetchProgram} }, nil),
	tlsCommandClientSpec("bun", "bun", func(tlsSite) []string { return []string{"--eval", tlsFetchProgram} }, nil),
	{
		name: "omp",
		build: func(invocation string, env []string, workDir string) tlsClient {
			return tlsCommandClient{
				clientName: "omp", env: env, workDir: workDir,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", "exec " + invocation + " --eval \"$1\"", "sh", tlsFetchProgram}, []string{"BUN_BE_BUN=1"}
				},
			}
		},
	},
	tlsCommandClientSpec("codegraph-node", "/opt/codegraph/node", func(tlsSite) []string { return []string{"--input-type=module", "--eval", tlsFetchProgram} }, nil),
	tlsCommandClientSpec("uv", "uv", func(site tlsSite) []string {
		return []string{"pip", "install", "--system", "--break-system-packages", "--dry-run", "--no-deps", "--no-cache", "--index-url", site.url, "legion-tls-probe-not-a-package"}
	}, uvResponseSeen),
	tlsCommandClientSpec("aws", "aws", func(site tlsSite) []string {
		return []string{"sts", "get-caller-identity", "--no-sign-request", "--region", "us-east-1", "--endpoint-url", site.url}
	}, awsResponseSeen),
	tlsCommandClientSpec("curl", "curl", func(site tlsSite) []string { return []string{"--fail", "--silent", "--show-error", site.url} }, nil),
	tlsCommandClientSpec("python3", "python3", func(tlsSite) []string {
		return []string{"-c", "import os, urllib.request; urllib.request.urlopen(os.environ['LEGION_TLS_URL']).read(1)"}
	}, nil),
	tlsCommandClientSpec("gh", "/usr/local/bin/gh", func(site tlsSite) []string { return []string{"api", site.url} }, ghResponseSeen, "GH_TOKEN=legion-tls-probe"),
	tlsCommandClientSpec("chromium", "chromium", func(site tlsSite) []string {
		return []string{"--headless", "--no-sandbox", "--dump-dom", site.url}
	}, nil),
}

// imageTLSClients returns the image's TLS-capable programs. The gh wrapper manages worker tokens,
// so the probe runs the real gh binary it wraps; no probe uses an operator token.
func imageTLSClients(invocation string, env []string, workDir string) []tlsClient {
	clients := make([]tlsClient, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		clients = append(clients, spec.build(invocation, env, workDir))
	}
	return clients
}

func workerTLSClientNames() []string {
	names := make([]string, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		names = append(names, spec.name)
	}
	return names
}

// newImageTLSClients is a variable so the command's image-environment tests can use fake clients.
var newImageTLSClients = imageTLSClients

func uvResponseSeen(detail string) bool {
	return strings.Contains(detail, "No solution found when resolving dependencies")
}

func awsResponseSeen(detail string) bool {
	for _, marker := range []string{"An error occurred", "InvalidResponse", "invalid XML", "ResponseParserError"} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func ghResponseSeen(detail string) bool {
	return strings.Contains(detail, "HTTP ")
}
