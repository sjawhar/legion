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
	name, url string
	control   bool
}

var (
	emSignTLSSite  = tlsSite{name: "emSign", url: "https://dv-g3b-emudhra.we-have-a-problem.com/"}
	aaaTLSSite     = tlsSite{name: "AAA", url: "https://dv-g3b-aaa-emudhra.we-have-a-problem.com/"}
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

func tlsCommandDetail(err error, output string) string {
	return trimTLSDetail(tlsCommandMessage(err, output))
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
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

const tlsFetchProgram = `const response = await fetch(process.env.LEGION_TLS_URL); if (!response.ok) throw new Error("HTTP " + response.status);`

// imageTLSClients returns the image's TLS-capable programs. The gh wrapper manages worker tokens,
// so the probe runs the real gh binary it wraps; no probe uses an operator token.
func imageTLSClients(invocation string, env []string, workDir string) []tlsClient {
	command := func(clientName, program string, args func(tlsSite) []string, applicationSeen func(string) bool, extraEnv ...string) tlsClient {
		return tlsCommandClient{
			clientName: clientName, env: env, workDir: workDir, applicationSeen: applicationSeen,
			command: func(site tlsSite) (string, []string, []string) {
				return program, args(site), extraEnv
			},
		}
	}
	return []tlsClient{
		command("node", "node", func(tlsSite) []string { return []string{"--input-type=module", "--eval", tlsFetchProgram} }, nil),
		command("bun", "bun", func(tlsSite) []string { return []string{"--eval", tlsFetchProgram} }, nil),
		command("omp", "sh", func(tlsSite) []string {
			return []string{"-c", "exec " + invocation + " --eval \"$1\"", "sh", tlsFetchProgram}
		}, nil, "BUN_BE_BUN=1"),
		command("uv", "uv", func(site tlsSite) []string {
			return []string{"pip", "install", "--system", "--break-system-packages", "--dry-run", "--no-deps", "--no-cache", "--index-url", site.url, "legion-tls-probe-not-a-package"}
		}, uvResponseSeen),
		command("aws", "aws", func(site tlsSite) []string {
			return []string{"sts", "get-caller-identity", "--no-sign-request", "--region", "us-east-1", "--endpoint-url", site.url}
		}, awsResponseSeen),
		command("curl", "curl", func(site tlsSite) []string { return []string{"--fail", "--silent", "--show-error", site.url} }, nil),
		command("python3", "python3", func(tlsSite) []string {
			return []string{"-c", "import os, urllib.request; urllib.request.urlopen(os.environ['LEGION_TLS_URL']).read(1)"}
		}, nil),
		command("gh", "/usr/local/bin/gh", func(site tlsSite) []string { return []string{"api", site.url} }, ghResponseSeen, "GH_TOKEN=legion-tls-probe"),
	}
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
