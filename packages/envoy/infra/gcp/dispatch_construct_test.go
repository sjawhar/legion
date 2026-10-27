package gcp_test

import (
	"math"
	"strings"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const dispatchConstructURN = "urn:pulumi:stack::project::envoy-infra:gcp:Dispatch::example-dispatch"

func dispatchConstructInputs() map[string]property.Value {
	return map[string]property.Value{
		"region":             property.New("us-central1"),
		"hostname":           property.New("example-dispatch"),
		"tailnetDnsName":     property.New("example-tailnet.ts.net"),
		"tags":               property.New(property.NewArray([]property.Value{property.New("tag:example-dispatch")})),
		"envoyImage":         property.New(dispatchEnvoyImage),
		"tailscaleImage":     property.New(dispatchTailscaleImage),
		"cloudSqlProxyImage": property.New(dispatchProxyImage),
		"signInClientId":     property.New("example-client-id"),
		"signInDomain":       property.New("example.com"),
	}
}

// setConstructApp sets the four GitHub App inputs, which come together.
func setConstructApp(inputs map[string]property.Value) {
	inputs["appClientId"] = property.New("example-app-client-id")
	inputs["appId"] = property.New("123456")
	inputs["appPrivateKeySecretName"] = property.New("example-app-key")
	inputs["appPrivateKeySecretKey"] = property.New("private-key.b64")
}

func constructDispatch(t *testing.T, inputs map[string]property.Value, dryRun bool) (*pulumitest.Mocks, p.ConstructResponse, error) {
	t.Helper()
	m := newMocks()
	response, err := pulumitest.Construct(t, m, dispatchType, "example-dispatch", inputs, dryRun, map[tokens.Package]p.ProviderReference{
		"gcp":    {Urn: resource.URN("urn:pulumi:s::p::" + gcpProviderType + "::gcp-example"), ID: "gcp-example-id"},
		"random": {Urn: resource.URN("urn:pulumi:s::p::" + randomProviderType + "::random-example"), ID: "random-example-id"},
	})
	return m, response, err
}

func TestConstructTakesDispatchInputsFromAnotherResource(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["agentSecretsUrl"] = property.New("https://example-broker.internal.example")
	inputs["agentSecretsAudience"] = property.New("example-broker-audience")
	setConstructApp(inputs)
	for key, value := range map[string]property.Value{
		"tailnetAuthKey":     property.New("tskey-auth-example-0123456789").WithSecret(true),
		"natsUrl":            property.New("nats://example-hub.example-tailnet.ts.net:4222"),
		"natsSeedSecret":     property.New("projects/example-project/secrets/example-nats-seed"),
		"signInClientSecret": property.New("example-client-secret").WithSecret(true),
	} {
		inputs[key] = value.WithDependencies([]resource.URN{upstream})
	}
	m, response, err := constructDispatch(t, inputs, false)
	require.NoError(t, err)
	require.NotEmpty(t, m.Registrations())
	url, ok := response.State.GetOk("url")
	require.True(t, ok)
	require.Equal(t, "https://example-dispatch.example-tailnet.ts.net", url.AsString())
}

func TestConstructAcceptsUnknownDispatchInputsUnderPreview(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["agentSecretsUrl"] = property.New("https://example-broker.internal.example")
	inputs["agentSecretsAudience"] = property.New("example-broker-audience")
	setConstructApp(inputs)
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
		inputs[key] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
	}
	_, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
}

func TestConstructRefusesDispatchPlainInputFromAnotherResource(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["hostname"] = property.New("example-dispatch").WithDependencies([]resource.URN{upstream})
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
		inputs[key] = property.New("example").WithDependencies([]resource.URN{upstream})
	}
	m, _, err := constructDispatch(t, inputs, false)
	require.ErrorContains(t, err, "hostname")
	require.Empty(t, m.Types())
}

// replicas is a plain *int: Construct refuses another resource's output there, as it does for
// every plain input, before anything registers.
func TestConstructRefusesDispatchReplicasFromAnotherResource(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["replicas"] = property.New(3.0).WithDependencies([]resource.URN{upstream})
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
		inputs[key] = property.New("example").WithDependencies([]resource.URN{upstream})
	}
	m, _, err := constructDispatch(t, inputs, false)
	require.ErrorContains(t, err, "replicas")
	require.Empty(t, m.Types())
}

// pprofPort is a plain *int, refused from another resource's output as replicas is.
func TestConstructRefusesDispatchPprofPortFromAnotherResource(t *testing.T) {
	inputs := dispatchConstructInputs()
	inputs["pprofPort"] = property.New(6060.0).WithDependencies([]resource.URN{upstream})
	for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
		inputs[key] = property.New("example").WithDependencies([]resource.URN{upstream})
	}
	m, _, err := constructDispatch(t, inputs, false)
	require.ErrorContains(t, err, "pprofPort")
	require.Empty(t, m.Types())
}

// appCpu and appMemory are plain strings, refused from another resource's output as replicas is.
func TestConstructRefusesDispatchAppResourcesFromAnotherResource(t *testing.T) {
	for _, input := range []string{"appCpu", "appMemory"} {
		t.Run(input, func(t *testing.T) {
			inputs := dispatchConstructInputs()
			inputs[input] = property.New("2").WithDependencies([]resource.URN{upstream})
			for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
				inputs[key] = property.New("example").WithDependencies([]resource.URN{upstream})
			}
			m, _, err := constructDispatch(t, inputs, false)
			require.ErrorContains(t, err, input)
			require.Empty(t, m.Types())
		})
	}
}

// appPriorityClassName is a plain string and appPdbMinAvailable a plain *int, each refused from
// another resource's output as replicas is.
func TestConstructRefusesDispatchAppDisruptionInputsFromAnotherResource(t *testing.T) {
	for input, value := range map[string]property.Value{
		"appPriorityClassName": property.New("example-priority"),
		"appPdbMinAvailable":   property.New(1.0),
	} {
		t.Run(input, func(t *testing.T) {
			inputs := dispatchConstructInputs()
			inputs[input] = value.WithDependencies([]resource.URN{upstream})
			for _, key := range []string{"tailnetAuthKey", "natsUrl", "natsSeedSecret", "signInClientSecret"} {
				inputs[key] = property.New("example").WithDependencies([]resource.URN{upstream})
			}
			m, _, err := constructDispatch(t, inputs, false)
			require.ErrorContains(t, err, input)
			require.Empty(t, m.Types())
		})
	}
}

// tailnetAuthKey, signInClientSecret and natsSeedSecret are Inputs, which the component reads only
// once they resolve: a value holding an nkey seed is refused then, naming the input and never the
// value, so no child is registered with it, plainly or as a secret. Each arrives as another
// resource's output, as a consumer's does.
func TestConstructRefusesASeedInADispatchInput(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct {
		name, input string
		value       property.Value
	}{
		{"a seed inside the tailnet auth key", "tailnetAuthKey", property.New("tskey-auth-" + seed).WithSecret(true)},
		{"a lowercase seed as the sign-in client secret", "signInClientSecret", property.New(strings.ToLower(seed)).WithSecret(true)},
		{"a seed inside the NATS seed secret id", "natsSeedSecret", property.New("projects/example-project/secrets/" + seed)},
	} {
		t.Run(row.name, func(t *testing.T) {
			inputs := dispatchConstructInputs()
			inputs["tailnetAuthKey"] = property.New("tskey-auth-example-0123456789").WithSecret(true)
			inputs["natsUrl"] = property.New("nats://example-hub.example-tailnet.ts.net:4222")
			inputs["natsSeedSecret"] = property.New("projects/example-project/secrets/example-nats-seed")
			inputs["agentSecretsUrl"] = property.New("https://example-broker.internal.example")
			inputs["agentSecretsAudience"] = property.New("example-broker-audience")
			setConstructApp(inputs)
			inputs["signInClientSecret"] = property.New("example-client-secret").WithSecret(true)
			inputs[row.input] = row.value.WithDependencies([]resource.URN{upstream})
			m, _, err := constructDispatch(t, inputs, false)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.input+" holds an nkey seed (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			for _, r := range m.Registrations() {
				for _, s := range allStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}

// dispatchConstructBaseInputs is dispatchConstructInputs with the required Inputs set plainly.
func dispatchConstructBaseInputs() map[string]property.Value {
	inputs := dispatchConstructInputs()
	inputs["tailnetAuthKey"] = property.New("tskey-auth-example-0123456789").WithSecret(true)
	inputs["natsUrl"] = property.New("nats://example-hub.example-tailnet.ts.net:4222")
	inputs["natsSeedSecret"] = property.New("projects/example-project/secrets/example-nats-seed")
	inputs["signInClientSecret"] = property.New("example-client-secret").WithSecret(true)
	return inputs
}

// azureConstructForms are the Azure embedder's two credential forms as Construct inputs: the
// Inputs each sets, the endpoint among them, and the quota, a plain input, beside them.
var azureConstructForms = []struct {
	name   string
	inputs map[string]string
}{
	{"key", map[string]string{"azureEmbedEndpoint": azureEmbedEndpoint, "azureEmbedKeySecret": azureEmbedKeySecret}},
	{"federation", map[string]string{"azureEmbedEndpoint": azureEmbedEndpoint, "azureEmbedDeployment": "example-embed-deployment", "azureTenantId": azureTenantID, "azureClientId": azureClientID}},
}

// appEnv is the app's envoy-dispatch container's environment, as containerEnv indexes it.
func appEnv(t *testing.T, m *pulumitest.Mocks) map[string]any {
	t.Helper()
	return containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch"))
}

// The endpoint, the deployment and either credential are Inputs, typically an Azure resource's or
// a managed identity's outputs: Construct takes them from another resource and renders them as
// they resolve.
func TestConstructTakesAzureInputsFromAnotherResource(t *testing.T) {
	for _, form := range azureConstructForms {
		t.Run(form.name, func(t *testing.T) {
			inputs := dispatchConstructBaseInputs()
			inputs["azureEmbedTokensPerMinute"] = property.New(300000.0)
			for key, value := range form.inputs {
				inputs[key] = property.New(value).WithDependencies([]resource.URN{upstream})
			}
			m, _, err := constructDispatch(t, inputs, false)
			require.NoError(t, err)
			env := appEnv(t, m)
			require.Equal(t, "azure", env["DISPATCH_EMBEDDER"])
			require.Equal(t, azureEmbedEndpoint, env["DISPATCH_AZURE_EMBED_ENDPOINT"])
			require.Equal(t, "300000", env["DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE"])
			if form.name == "key" {
				require.Equal(t, "/var/run/secrets/azure-embed/key", env["DISPATCH_AZURE_EMBED_KEY_FILE"])
				require.Equal(t, azureEmbedKeySecret, m.Named(t, "gcp:secretmanager/secretIamMember:SecretIamMember", "example-dispatch-azure-embed-key-accessor").Inputs.Mappable()["secretId"])
				return
			}
			require.Equal(t, "example-embed-deployment", env["DISPATCH_AZURE_EMBED_DEPLOYMENT"])
			require.Equal(t, azureTenantID, env["DISPATCH_AZURE_TENANT_ID"])
			require.Equal(t, azureClientID, env["DISPATCH_AZURE_CLIENT_ID"])
			require.Equal(t, "/var/run/secrets/azure-federation/token", env["DISPATCH_AZURE_FEDERATED_TOKEN_FILE"])
		})
	}
}

// At preview the endpoint and the credential may not be known yet (the Azure resources are created
// in the same update): the resolver judges them by being set, so the component previews, with the
// unknown values rendered as unknown and the key form's accessor registered.
func TestConstructAcceptsUnknownAzureInputsUnderPreview(t *testing.T) {
	for _, form := range azureConstructForms {
		t.Run(form.name, func(t *testing.T) {
			inputs := dispatchConstructBaseInputs()
			inputs["azureEmbedTokensPerMinute"] = property.New(300000.0)
			for key := range form.inputs {
				inputs[key] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
			}
			m, _, err := constructDispatch(t, inputs, true)
			require.NoError(t, err)
			env := appEnv(t, m)
			require.Equal(t, "azure", env["DISPATCH_EMBEDDER"])
			require.Equal(t, "<unknown>", env["DISPATCH_AZURE_EMBED_ENDPOINT"])
			if form.name == "key" {
				m.Named(t, "gcp:secretmanager/secretIamMember:SecretIamMember", "example-dispatch-azure-embed-key-accessor")
				return
			}
			require.Equal(t, "<unknown>", env["DISPATCH_AZURE_TENANT_ID"])
			require.Equal(t, "<unknown>", env["DISPATCH_AZURE_CLIENT_ID"])
		})
	}
}

// A refused Azure configuration fails Construct before anything registers.
func TestConstructRefusesAnAzureMisconfigurationBeforeRegistering(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["azureEmbedEndpoint"] = property.New(azureEmbedEndpoint).WithDependencies([]resource.URN{upstream})
	inputs["azureTenantId"] = property.New(azureTenantID).WithDependencies([]resource.URN{upstream})
	inputs["azureEmbedTokensPerMinute"] = property.New(300000.0)
	m, _, err := constructDispatch(t, inputs, false)
	require.ErrorContains(t, err, "azureClientId is required when azureTenantId is set")
	require.Empty(t, m.Types())
}

// The endpoint, the deployment, the key's secret id, the tenant and the client are Inputs, read
// only once they resolve: a value holding an nkey seed is refused then, naming the input and never
// the value, so no child is registered with it.
func TestConstructRefusesASeedInAnAzureInput(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	for _, row := range []struct {
		name, form, input, value string
	}{
		{"a seed inside the Azure endpoint", "key", "azureEmbedEndpoint", "https://" + lower + ".services.ai.azure.com/models"},
		{"a seed as the Azure deployment", "federation", "azureEmbedDeployment", seed},
		{"a seed inside the Azure key secret id", "key", "azureEmbedKeySecret", "projects/example-project/secrets/" + seed},
		{"a seed as the Azure tenant", "federation", "azureTenantId", seed},
		{"a seed as the Azure client", "federation", "azureClientId", lower},
	} {
		t.Run(row.name, func(t *testing.T) {
			inputs := dispatchConstructBaseInputs()
			inputs["azureEmbedTokensPerMinute"] = property.New(300000.0)
			for _, form := range azureConstructForms {
				if form.name == row.form {
					for key, value := range form.inputs {
						inputs[key] = property.New(value)
					}
				}
			}
			inputs[row.input] = property.New(row.value).WithDependencies([]resource.URN{upstream})
			m, _, err := constructDispatch(t, inputs, false)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.input+" holds an nkey seed (not echoed)")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			for _, r := range m.Registrations() {
				for _, s := range allStrings(resource.NewProperty(r.Inputs)) {
					require.NotContains(t, strings.ToUpper(s), seed, "%s %s holds the seed", r.Type, r.Name)
				}
			}
		})
	}
}

// The issuer and subject an Azure federated credential trusts are outputs, so the Azure side reads
// them rather than rebuilding them: the cluster's token issuer, and the pods' ServiceAccount
// <hostname> in the component's namespace. Neither is secret, and both are there with no Azure
// input set.
func TestConstructOutputsTheServiceAccountIssuerAndSubject(t *testing.T) {
	for _, row := range []struct {
		name, namespace, subject string
	}{
		{"the default namespace", "", "system:serviceaccount:dispatch:example-dispatch"},
		{"a namespace given", "example-namespace", "system:serviceaccount:example-namespace:example-dispatch"},
	} {
		t.Run(row.name, func(t *testing.T) {
			inputs := dispatchConstructBaseInputs()
			if row.namespace != "" {
				inputs["namespace"] = property.New(row.namespace)
			}
			m, response, err := constructDispatch(t, inputs, false)
			require.NoError(t, err)
			issuer, ok := response.State.GetOk("serviceAccountIssuer")
			require.True(t, ok)
			require.False(t, issuer.Secret())
			require.Equal(t, azureIssuer, issuer.AsString())
			subject, ok := response.State.GetOk("serviceAccountSubject")
			require.True(t, ok)
			require.False(t, subject.Secret())
			require.Equal(t, row.subject, subject.AsString())

			pod := deploymentPodTemplate(t, m, "example-dispatch")["spec"].(map[string]any)
			require.Equal(t, "example-dispatch", pod["serviceAccountName"], "the subject names the pods' own ServiceAccount")
		})
	}
}

// Pulumi decodes an integer input into a Go int by conversion, so a typed SDK or YAML program
// sending 2.5 replicas would get 2 with no error. Each integer input of a gcp:Dispatch whose other
// inputs are all valid refuses a number that is not a whole number within int64's range, naming
// the component and the input, before anything registers, and never quotes a secret one
// (provider's TestConstructRefusesAFractionInEveryIntegerInput covers every component's).
func TestConstructRefusesAFractionalDispatchInteger(t *testing.T) {
	for _, input := range []string{"replicas", "pprofPort", "azureEmbedTokensPerMinute"} {
		for _, row := range []struct {
			value property.Value
			want  string
		}{
			{property.New(2.5), input + " 2.5 must be a whole number"},
			{property.New(-0.5), input + " -0.5 must be a whole number"},
			{property.New(1e19), input + " 1e+19 is outside the 64-bit integer range"},
			{property.New(math.Inf(1)), input + " +Inf is outside the 64-bit integer range"},
			{property.New(2.5).WithSecret(true), input + " must be a whole number within the 64-bit integer range (not echoed, as it is secret)"},
		} {
			t.Run(row.want, func(t *testing.T) {
				inputs := dispatchConstructBaseInputs()
				inputs[input] = row.value
				m, _, err := constructDispatch(t, inputs, false)
				require.EqualError(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
				require.Empty(t, m.Types())
			})
		}
	}
}

// A whole number sent as a float, as every JSON number is, still decodes: 3.0 replicas is 3.
func TestConstructTakesAWholeDispatchIntegerSentAsAFloat(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["replicas"] = property.New(3.0)
	m, _, err := constructDispatch(t, inputs, false)
	require.NoError(t, err)
	spec := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").Inputs.Mappable()["spec"].(map[string]any)
	require.EqualValues(t, 3, spec["replicas"])
}
