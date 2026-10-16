package pulumitest

import (
	"slices"
	"sync"
	"testing"

	"github.com/blang/semver"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/integration"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"

	"github.com/sjawhar/envoy/infra/provider"
)

// Registration is one resource the mock engine was asked to register.
type Registration struct {
	Type, Name string
	Inputs     resource.PropertyMap
	RPC        *pulumirpc.RegisterResourceRequest
}

// Mocks is a mock engine, for pulumi.WithMocks and integration.WithMocks, that records every
// registration. A resource's state is its inputs, less any that is unknown, plus what Computed
// adds. Preview reads a missing output as unknown, and the SDK fails to unmarshal a secret-wrapped
// unknown in a mock's state ("malformed RPC secret: missing value for ...").
type Mocks struct {
	// Computed, when not nil, returns what a resource's provider would compute: its id, "" for
	// <name>-id, and the outputs it adds to the resource's state.
	Computed func(args pulumi.MockResourceArgs) (id string, outputs resource.PropertyMap)

	mu         sync.Mutex
	registered []Registration
}

func (m *Mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered = append(m.registered, Registration{
		Type: args.TypeToken, Name: args.Name, Inputs: args.Inputs, RPC: args.RegisterRPC,
	})
	state := resource.PropertyMap{}
	for k, v := range args.Inputs {
		if !v.ContainsUnknowns() {
			state[k] = v
		}
	}
	id := args.Name + "-id"
	if m.Computed != nil {
		computedID, outputs := m.Computed(args)
		if computedID != "" {
			id = computedID
		}
		for k, v := range outputs {
			state[k] = v
		}
	}
	return id, state, nil
}

// Call answers a function call with its arguments.
func (m *Mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

// One returns the single registration of type typ, failing the test when there is none or more.
func (m *Mocks) One(t testing.TB, typ string) Registration {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []Registration
	for _, r := range m.registered {
		if r.Type == typ {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d registrations of %s, want 1", len(found), typ)
	}
	return found[0]
}

// Registrations returns every registration, in order.
func (m *Mocks) Registrations() []Registration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.registered)
}

// Types lists the type of every registration, in order.
func (m *Mocks) Types() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	types := make([]string, 0, len(m.registered))
	for _, r := range m.registered {
		types = append(types, r.Type)
	}
	return types
}

// Run runs program on m, as project "p" and stack "s", and returns its error.
func (m *Mocks) Run(program pulumi.RunFunc) error {
	return pulumi.RunErr(program, pulumi.WithMocks("p", "s", m))
}

// Construct serves the envoy-infra plugin in process with m behind it and constructs the
// component of type typ named name, in project "p" and stack "s", from inputs with providers, as
// a consumer's program does. It returns Construct's response and error.
func Construct(t testing.TB, m *Mocks, typ, name string, inputs map[string]property.Value, dryRun bool,
	providers map[tokens.Package]p.ProviderReference,
) (p.ConstructResponse, error) {
	t.Helper()
	prov, err := provider.New()
	if err != nil {
		t.Fatalf("provider.New: %v", err)
	}
	server, err := integration.NewServer(t.Context(), "envoy-infra", semver.MustParse("0.0.1"),
		integration.WithProvider(prov), integration.WithMocks(m))
	if err != nil {
		t.Fatalf("serve the plugin: %v", err)
	}
	return server.Construct(p.ConstructRequest{
		Urn:       resource.NewURN("s", "p", "", tokens.Type(typ), name),
		DryRun:    dryRun,
		Inputs:    property.NewMap(inputs),
		Providers: providers,
	})
}

// Plain returns v's value with any secret wrapper removed, and whether there was one.
func Plain(v resource.PropertyValue) (resource.PropertyValue, bool) {
	if v.IsSecret() {
		return v.SecretValue().Element, true
	}
	return v, false
}
