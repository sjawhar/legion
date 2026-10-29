package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeTLSClient struct {
	clientName string
	outcomes   map[string]tlsOutcome
}

func (c fakeTLSClient) name() string { return c.clientName }

func (c fakeTLSClient) probe(_ context.Context, site tlsSite) tlsOutcome {
	return c.outcomes[site.name]
}

func passingImageTLSClients(string, []string, string) []tlsClient {
	outcomes := map[string]tlsOutcome{
		emSignTLSSite.name:  {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
		aaaTLSSite.name:     {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
		controlTLSSite.name: {status: tlsConnectionAccepted, detail: "HTTP 200"},
	}
	names := workerTLSClientNames()
	clients := make([]tlsClient, 0, len(names))
	for _, name := range names {
		clients = append(clients, fakeTLSClient{clientName: name, outcomes: outcomes})
	}
	return clients
}

func TestProbeTLSClientsDecisions(t *testing.T) {
	refused := tlsOutcome{status: tlsCertificateRefused, detail: "CERT_REVOKED"}
	accepted := tlsOutcome{status: tlsConnectionAccepted, detail: "HTTP 200"}
	unavailable := tlsOutcome{status: tlsNetworkUnavailable, detail: "dial tcp: network is unreachable"}

	for _, testCase := range []struct {
		name     string
		outcomes map[string]tlsOutcome
		want     string
	}{
		{
			name: "both compromised chains refused and control reachable",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  refused,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: accepted,
			},
		},
		{
			name: "compromised chain accepted",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  accepted,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: accepted,
			},
			want: "node accepts the emSign chain",
		},
		{
			name: "compromised chain neither refused nor accepted",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  unavailable,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: accepted,
			},
			want: "node cannot verify refusal of the emSign chain: dial tcp: network is unreachable",
		},
		{
			name: "control unreachable",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  refused,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: unavailable,
			},
			want: "network unavailable: node cannot reach https://example.com/: dial tcp: network is unreachable",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := probeTLSClients(context.Background(), []tlsClient{fakeTLSClient{clientName: "node", outcomes: testCase.outcomes}})
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("probeTLSClients() = %v, want success", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("probeTLSClients() = %v, want error containing %q", err, testCase.want)
			}
		})
	}
}

func TestTLSCertificateErrorRecognizesClientRefusals(t *testing.T) {
	for _, detail := range []string{
		"Error: certificate revoked",
		"invalid peer certificate: Other(OtherError(CertDistrusted))",
		"SSL validation failed: certificate verify failed",
		"curl: (60) SSL certificate problem",
		"x509: certificate signed by unknown authority",
	} {
		if !tlsCertificateError(detail) {
			t.Errorf("tlsCertificateError(%q) = false, want true", detail)
		}
	}
}

func TestTLSCommandDetailKeepsCertificateErrorTail(t *testing.T) {
	detail := trimTLSDetail(tlsCommandMessage(nil, strings.Repeat("x", tlsDetailLimit)+" invalid peer certificate"))
	if !tlsCertificateError(detail) {
		t.Fatalf("detail = %q, dropped the certificate refusal", detail)
	}
}

func TestTLSCommandClientClassifiesApplicationErrorBeforeTruncatingDetail(t *testing.T) {
	response := strings.Repeat("x", tlsDetailLimit) + " invalid XML received " + strings.Repeat("y", tlsDetailLimit)
	client := tlsCommandClient{
		clientName:      "aws",
		timeout:         tlsDefaultProbeTimeout,
		applicationSeen: awsResponseSeen,
		command: func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", `printf %s "$1" >&2; exit 255`, "sh", response}, nil
		},
	}

	outcome := client.probe(context.Background(), controlTLSSite)

	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("client.probe() = %+v, want an accepted application response", outcome)
	}
}

func TestImageTLSClientsCoverTheWorkerInventory(t *testing.T) {
	clients := imageTLSClients("omp", nil, "")
	want := []struct {
		name    string
		timeout time.Duration
	}{
		{"node", tlsDefaultProbeTimeout},
		{"bun", tlsDefaultProbeTimeout},
		{"omp", tlsDefaultProbeTimeout},
		{"codegraph-node", tlsDefaultProbeTimeout},
		{"uv", tlsDefaultProbeTimeout},
		{"aws", tlsDefaultProbeTimeout},
		{"curl", tlsDefaultProbeTimeout},
		{"python3", tlsDefaultProbeTimeout},
		{"gh", tlsDefaultProbeTimeout},
		{"chromium", tlsChromiumProbeTimeout},
	}
	if len(workerTLSClients) != len(want) || len(clients) != len(want) {
		t.Fatalf("inventory has %d specifications and %d clients, want %d", len(workerTLSClients), len(clients), len(want))
	}
	for index, expected := range want {
		spec := workerTLSClients[index]
		if spec.name != expected.name || spec.timeout != expected.timeout || clients[index].name() != expected.name {
			t.Errorf("inventory client %d = (%q, %s, %q), want (%q, %s, %q)", index, spec.name, spec.timeout, clients[index].name(), expected.name, expected.timeout, expected.name)
		}
		var timeout time.Duration
		switch client := clients[index].(type) {
		case tlsCommandClient:
			timeout = client.timeout
		case tlsChromiumClient:
			timeout = client.timeout
		default:
			t.Errorf("inventory client %d = %T, want a command client", index, client)
			continue
		}
		if timeout != expected.timeout {
			t.Errorf("image client %s timeout = %s, want inventory timeout %s", expected.name, timeout, expected.timeout)
		}
	}
	if workerTLSClients[len(workerTLSClients)-1].timeout <= tlsDefaultProbeTimeout {
		t.Errorf("Chromium timeout %s must exceed the default %s", workerTLSClients[len(workerTLSClients)-1].timeout, tlsDefaultProbeTimeout)
	}
}

// Chromium resolves only the probed site, so its certificate verifier's AIA fetches fail at once
// instead of waiting on the CA's repository host.
func TestImageChromiumResolvesOnlyTheProbedSite(t *testing.T) {
	var chromium tlsChromiumClient
	found := false
	for _, client := range imageTLSClients("omp", nil, "") {
		if client.name() == "chromium" {
			chromium, found = client.(tlsChromiumClient)
		}
	}
	if !found {
		t.Fatal("imageTLSClients() has no chromium client of type tlsChromiumClient")
	}
	for site, host := range map[tlsSite]string{
		emSignTLSSite:  "dv-g3b-emudhra.we-have-a-problem.com",
		aaaTLSSite:     "dv-g3b-aaa-emudhra.we-have-a-problem.com",
		controlTLSSite: "example.com",
	} {
		program, args, _ := chromium.command(site)
		want := []string{"--headless", "--no-sandbox", "--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE " + host, "--dump-dom", site.url}
		if program != "chromium" || !slices.Equal(args, want) {
			t.Errorf("chromium command for %s = %s %q, want chromium %q", site.name, program, args, want)
		}
	}
}

func TestTLSPositiveControlsRequireTheExpectedChain(t *testing.T) {
	for _, testCase := range []struct {
		name, want         string
		outcome, isolation tlsOutcome
	}{
		{name: "accepts pinned root", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsCertificateRefused}},
		{name: "refuses pinned root", outcome: tlsOutcome{status: tlsCertificateRefused, detail: "certificate verify failed"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "cannot reach site", outcome: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "cannot reach isolation endpoint", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, want: "isolation endpoint"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := verifyTLSPositiveControls([]tlsSite{emSignTLSSite}, func(site tlsSite) tlsOutcome {
				if site.url == isolationTLSSite.url {
					return testCase.isolation
				}
				return testCase.outcome
			})
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("verifyTLSPositiveControls() = %v, want success", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("verifyTLSPositiveControls() = %v, want error containing %q", err, testCase.want)
			}
		})
	}
}

func TestTLSCommandPastTheProbeDeadlineIsUnavailableAndFailsTheProbe(t *testing.T) {
	originalSites := tlsSites
	tlsSites = []tlsSite{emSignTLSSite, aaaTLSSite}
	t.Cleanup(func() { tlsSites = originalSites })

	client := tlsCommandClient{
		clientName: "curl",
		timeout:    tlsDefaultProbeTimeout,
		command: func(site tlsSite) (string, []string, []string) {
			if site.name == emSignTLSSite.name {
				return "sh", []string{"-c", "echo certificate verify failed >&2; exit 1"}, nil
			}
			return "sh", []string{"-c", "exec sleep 1"}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	observations, err := probeTLSClients(ctx, []tlsClient{client})
	if len(observations) != 2 {
		t.Fatalf("probeTLSClients() returned %d observations, want the client to reach both sites", len(observations))
	}
	if got := observations[1]; got.status != tlsNetworkUnavailable || got.detail != "probe deadline" {
		t.Fatalf("AAA outcome = %+v, want unavailable (probe deadline)", got.tlsOutcome)
	}
	if err == nil || !strings.Contains(err.Error(), "curl cannot verify refusal of the AAA chain: probe deadline") {
		t.Fatalf("probeTLSClients() = %v, want the deadline to name curl and AAA", err)
	}
}
func TestTLSPositiveControlsLineNamesEverySite(t *testing.T) {
	sites := []tlsSite{{name: "first"}, {name: "second"}, {name: "third"}}
	const want = "probe-image: TLS positive controls: first, second and third chains verified"

	if got := tlsPositiveControlsLine(sites); got != want {
		t.Fatalf("tlsPositiveControlsLine() = %q, want %q", got, want)
	}
}

func TestTLSCommandClientClassifiesOutputStreams(t *testing.T) {
	for _, testCase := range []struct {
		name, stdout, stderr, exit string
		want                       tlsStatus
	}{
		{name: "accepts a loaded body with certificate text", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", want: tlsConnectionAccepted},
		{name: "refuses a certificate failure", stderr: "net::ERR_CERT_AUTHORITY_INVALID", exit: "failure", want: tlsCertificateRefused},
		{name: "reports another failure as unavailable", stderr: "network is unreachable", exit: "failure", want: tlsNetworkUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := tlsCommandClient{
				clientName: "generic",
				timeout:    tlsDefaultProbeTimeout,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2; test "$3" != failure`, "sh", testCase.stdout, testCase.stderr, testCase.exit}, nil
				},
			}

			if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != testCase.want {
				t.Fatalf("client.probe() = %+v, want status %q", outcome, testCase.want)
			}
		})
	}
}

func TestTLSChromiumClientClassifiesNavigationResult(t *testing.T) {
	for _, testCase := range []struct {
		name, stdout, stderr string
		want                 tlsStatus
	}{
		{name: "accepts a loaded DOM with certificate text", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", want: tlsConnectionAccepted},
		{name: "refuses a certificate navigation failure", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", stderr: "Page load failed: net::ERR_CERT_AUTHORITY_INVALID", want: tlsCertificateRefused},
		{name: "refuses after failed AIA fetches", stderr: "[5:18:1028/173830.670117:ERROR:net/cert/internal/cert_issuer_source_aia.cc:138] AiaRequest::OnFetchCompleted got error -105\n[5:5:1028/173830.690777:ERROR:components/headless/command_handler/headless_command_handler.cc:403] Page load failed: net::ERR_CERT_AUTHORITY_INVALID\n", want: tlsCertificateRefused},
		{name: "reports another navigation failure as unavailable", stderr: "Page load failed: net::ERR_NAME_NOT_RESOLVED", want: tlsNetworkUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := tlsChromiumClient{tlsCommandClient: tlsCommandClient{
				clientName: "chromium",
				timeout:    tlsChromiumProbeTimeout,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2`, "sh", testCase.stdout, testCase.stderr}, nil
				},
			}}

			outcome := client.probe(context.Background(), controlTLSSite)
			if outcome.status != testCase.want {
				t.Fatalf("client.probe() = %+v, want status %q", outcome, testCase.want)
			}
			if strings.Contains(outcome.detail, "page body") {
				t.Fatalf("client.probe() detail = %q, must not read the DOM", outcome.detail)
			}
		})
	}
}

// Chromium's first process in a probe pod needs more time than the other clients,
// while each ordinary client keeps the short refusal bound.
func TestImageTLSChromiumClientHasColdStartHeadroom(t *testing.T) {
	clients := imageTLSClients("omp", nil, "")
	chromium, ok := clients[len(clients)-1].(tlsChromiumClient)
	if !ok {
		t.Fatalf("last image TLS client = %T, want tlsChromiumClient", clients[len(clients)-1])
	}
	chromium.command = func(tlsSite) (string, []string, []string) {
		return "sh", []string{"-c", "exec sleep 21"}, nil
	}

	outcome := chromium.probe(context.Background(), controlTLSSite)
	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("Chromium probe after a 21s cold start = %+v, want accepted", outcome)
	}
}
