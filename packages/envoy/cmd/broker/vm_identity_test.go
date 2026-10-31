// vm_identity_test.go pins how the broker boots with Google VM identity tokens: the Google issuer
// is discovered, and named in one boot line, only when BROKER_GOOGLE_OIDC_AUDIENCE is set; the VM
// services reach the request machine, the policy loader and the enrollment service; and an audience
// with no VM service refuses the boot.
package main

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// exampleVMAccount is the one VM service account these rows bind, a placeholder: no real account,
// project or instance name appears in a test.
const exampleVMAccount = "example-fleet@example-project.iam.gserviceaccount.com"

// vmConfig loads the broker's configuration with the cluster's issuer and, when audience is set,
// one VM service: example-fleet, run by exampleVMAccount in example-project as example-vm-*.
func vmConfig(t *testing.T, audience string) config.Config {
	t.Helper()
	env := map[string]string{
		"BROKER_DATABASE_URL":        "postgres://unused",
		"BROKER_PUBLIC_URL":          "https://secrets.internal.example",
		"BROKER_MACHINE_LOGIN_URL":   "https://dispatch.example/credentials/machine",
		"BROKER_SECRETS_PREFIX":      testSecretsPrefix,
		"BROKER_SECRETS_KMS_KEY_ARN": testSecretsKMSKeyARN,
		"BROKER_K8S_OIDC_ISSUER":     "https://cluster.example",
		"BROKER_K8S_OIDC_AUDIENCE":   "example-broker-pod",
		"BROKER_SERVICES":            "example-service=system:serviceaccount:example:example-sa",
	}
	if audience != "" {
		env["BROKER_GOOGLE_OIDC_AUDIENCE"] = audience
		env["BROKER_VM_SERVICES"] = "example-fleet=" + exampleVMAccount
		env["BROKER_VM_PROJECTS"] = exampleVMAccount + "=example-project"
		env["BROKER_VM_INSTANCES"] = exampleVMAccount + "=example-vm-*"
	}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// discovery is a discoverFunc that records each issuer and audience asked for and answers a
// verifier for every configured pair, reading nothing, and nil for an unconfigured one, as
// oidc.Discover does.
type discovery struct{ asked [][2]string }

func (d *discovery) discover(_ context.Context, issuer, audience string, _ time.Duration) (*oidc.Verifier, error) {
	d.asked = append(d.asked, [2]string{issuer, audience})
	if issuer == "" {
		return nil, nil
	}
	return &oidc.Verifier{}, nil
}

// captureLog sends slog's default logger to a buffer until t ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logged
}

// TestWithNoGoogleAudienceTheBrokerDiscoversOneIssuer: with no Google audience the broker
// discovers the cluster's issuer alone, holds no VM verifier, and logs nothing about Google.
func TestWithNoGoogleAudienceTheBrokerDiscoversOneIssuer(t *testing.T) {
	logged := captureLog(t)
	d := &discovery{}
	pod, vm, err := discoverVerifiers(context.Background(), vmConfig(t, ""), d.discover)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{"https://cluster.example", "example-broker-pod"}}; !slices.Equal(d.asked, want) {
		t.Fatalf("discovered %v, want the cluster's issuer alone, %v", d.asked, want)
	}
	if pod == nil || vm != nil {
		t.Fatalf("verifiers = pod %v, vm %v; want a pod verifier and no VM verifier", pod, vm)
	}
	if strings.Contains(strings.ToLower(logged.String()), "google") {
		t.Fatalf("log with no Google audience names Google:\n%s", logged)
	}
}

// TestAGoogleAudienceDiscoversGooglesIssuer: with BROKER_GOOGLE_OIDC_AUDIENCE set the broker also
// discovers config.GoogleOIDCIssuer for that audience, verifies VM tokens with it
// (enroll.GoogleVMVerifier), and logs one line naming the issuer, the audience and the VM services.
func TestAGoogleAudienceDiscoversGooglesIssuer(t *testing.T) {
	logged := captureLog(t)
	d := &discovery{}
	_, vm, err := discoverVerifiers(context.Background(), vmConfig(t, "example-vm-audience"), d.discover)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"https://cluster.example", "example-broker-pod"}, {config.GoogleOIDCIssuer, "example-vm-audience"}}
	if !slices.Equal(d.asked, want) {
		t.Fatalf("discovered %v, want %v", d.asked, want)
	}
	if google, ok := vm.(enroll.GoogleVMVerifier); !ok || google.Verifier == nil {
		t.Fatalf("VM verifier = %#v, want an enroll.GoogleVMVerifier", vm)
	}
	lines := strings.Count(logged.String(), "google vm identity")
	wantLine := `msg="google vm identity" issuer=https://accounts.google.com audience=example-vm-audience services=example-fleet`
	if lines != 1 || !strings.Contains(logged.String(), wantLine) {
		t.Fatalf("log = %q, want one line holding %q", logged, wantLine)
	}
}

// TestTheRequestMachineCarriesTheVMServices pins the one place the broker hands
// BROKER_VM_SERVICES to the request decisions, without which no VM is ever its service's.
func TestTheRequestMachineCarriesTheVMServices(t *testing.T) {
	m := newRequestMachine(vmConfig(t, "example-vm-audience"), nil, nil, nil, nil)
	if want := map[string]string{exampleVMAccount: "example-fleet"}; !maps.Equal(m.VMServicesByAccount, want) {
		t.Fatalf("request Machine's VMServicesByAccount = %v, want %v", m.VMServicesByAccount, want)
	}
	if want := map[string]string{"system:serviceaccount:example:example-sa": "example-service"}; !maps.Equal(m.ServicesByAccount, want) {
		t.Fatalf("request Machine's ServicesByAccount = %v, want %v", m.ServicesByAccount, want)
	}
}

// TestThePolicyServesTheVMServicesSecrets: the policy loader's services are BROKER_SERVICES' and
// BROKER_VM_SERVICES', sorted and each once, so a secret a VM service owns is that service's and not
// refused as owner-tag-malformed.
func TestThePolicyServesTheVMServicesSecrets(t *testing.T) {
	if got, want := serviceNames(vmConfig(t, "example-vm-audience")), []string{"example-fleet", "example-service"}; !slices.Equal(got, want) {
		t.Fatalf("serviceNames = %v, want %v", got, want)
	}
	if got, want := serviceNames(vmConfig(t, "")), []string{"example-service"}; !slices.Equal(got, want) {
		t.Fatalf("serviceNames with no VM service = %v, want %v", got, want)
	}
}

// TestAGoogleAudienceWithNoVMServicesIsRefusedAtBoot drives the real binary: an audience with no
// VM service to bind refuses the boot, naming both settings, before the broker reaches its
// database or binds.
func TestAGoogleAudienceWithNoVMServicesIsRefusedAtBoot(t *testing.T) {
	out := runRefusedBroker(t, buildBroker(t),
		"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
		"BROKER_PUBLIC_URL=https://secrets.internal.example",
		"BROKER_GOOGLE_OIDC_AUDIENCE=example-vm-audience",
	)
	if !strings.Contains(out, "BROKER_GOOGLE_OIDC_AUDIENCE needs BROKER_VM_SERVICES") {
		t.Fatalf("broker refused, but not naming the audience's missing VM services: %s", out)
	}
}

// TestWithNoGoogleAudienceTheBootLogHoldsNoGoogleLine drives the real binary with the cluster's
// issuer and no Google audience: it boots, and its boot log names neither Google's issuer nor a VM
// identity line.
func TestWithNoGoogleAudienceTheBootLogHoldsNoGoogleLine(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	issuer.PublishKey(t, "signing-key")
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=example-broker-pod",
	}})
	for _, never := range []string{config.GoogleOIDCIssuer, "google vm identity"} {
		if strings.Contains(broker.Boot, never) {
			t.Fatalf("boot log with no Google audience holds %q:\n%s", never, broker.Boot)
		}
	}
}
