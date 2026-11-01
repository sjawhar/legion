package pulumitest

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"
)

// Mocks records each registration as given and answers with the inputs as state, less any that is
// unknown, plus what Computed adds, and the id Computed returns or <name>-id.
func TestMocksRecordsAndAnswersAsTheProviderWould(t *testing.T) {
	const thing, other = "example:index:Thing", "example:index:Other"
	m := &Mocks{Computed: func(args pulumi.MockResourceArgs) (string, resource.PropertyMap) {
		if args.TypeToken != thing {
			return "", nil
		}
		return "arn:example:" + args.Name, resource.PropertyMap{"arn": resource.NewProperty("arn:example:" + args.Name)}
	}}
	inputs := resource.PropertyMap{
		"known":       resource.NewProperty("a"),
		"later":       resource.MakeComputed(resource.NewProperty("")),
		"secretLater": resource.MakeSecret(resource.MakeComputed(resource.NewProperty(""))),
		"secret":      resource.MakeSecret(resource.NewProperty("example-secret")),
	}

	id, state, err := m.NewResource(pulumi.MockResourceArgs{TypeToken: thing, Name: "a", Inputs: inputs})
	require.NoError(t, err)
	require.Equal(t, "arn:example:a", id)
	require.Equal(t, resource.PropertyMap{
		"known":  resource.NewProperty("a"),
		"secret": resource.MakeSecret(resource.NewProperty("example-secret")),
		"arn":    resource.NewProperty("arn:example:a"),
	}, state, "unknown inputs are left out: preview reads a missing output as unknown")

	id, state, err = m.NewResource(pulumi.MockResourceArgs{TypeToken: other, Name: "b", Inputs: resource.PropertyMap{"known": resource.NewProperty("b")}})
	require.NoError(t, err)
	require.Equal(t, "b-id", id)
	require.Equal(t, resource.PropertyMap{"known": resource.NewProperty("b")}, state)

	require.Equal(t, []string{thing, other}, m.Types())
	all := m.Registrations()
	require.Len(t, all, 2)
	require.Equal(t, []string{"a", "b"}, []string{all[0].Name, all[1].Name}, "every registration, in order")
	all[0].Name = "changed"
	require.Equal(t, "a", m.Registrations()[0].Name, "the list is a copy")
	a := m.One(t, thing)
	require.Equal(t, "a", a.Name)
	require.Equal(t, inputs, a.Inputs, "a registration's inputs are recorded as given, unknowns included")

	args := resource.PropertyMap{"x": resource.NewProperty("y")}
	result, err := m.Call(pulumi.MockCallArgs{Token: "example:index:fn", Args: args})
	require.NoError(t, err)
	require.Equal(t, args, result, "a call answers with its arguments")
}

// Without Computed, a resource's state is its known inputs and its id <name>-id.
func TestMocksWithoutComputed(t *testing.T) {
	m := &Mocks{}
	id, state, err := m.NewResource(pulumi.MockResourceArgs{TypeToken: "example:index:Thing", Name: "a", Inputs: resource.PropertyMap{"known": resource.NewProperty("a")}})
	require.NoError(t, err)
	require.Equal(t, "a-id", id)
	require.Equal(t, resource.PropertyMap{"known": resource.NewProperty("a")}, state)
}

func TestPlain(t *testing.T) {
	v, secret := Plain(resource.MakeSecret(resource.NewProperty("example-secret")))
	require.True(t, secret)
	require.Equal(t, resource.NewProperty("example-secret"), v)
	v, secret = Plain(resource.NewProperty("a"))
	require.False(t, secret)
	require.Equal(t, resource.NewProperty("a"), v)
}
