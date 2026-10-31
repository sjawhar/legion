package provider

import (
	"context"
	"errors"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	presource "github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"
)

// exampleSchema puts an integer everywhere the walk reaches one: a component's own input, a
// property of an object type it refers to, an element of an array and a value of a map.
const exampleSchema = `{
	"resources": {"example:index:Component": {"inputProperties": {
		"count": {"type": "integer"},
		"item": {"$ref": "#/types/example:index:Item"},
		"items": {"type": "array", "items": {"$ref": "#/types/example:index:Item"}},
		"sizes": {"type": "object", "additionalProperties": {"type": "integer"}}
	}}},
	"types": {"example:index:Item": {"properties": {"size": {"type": "integer"}, "name": {"type": "string"}}}}
}`

// fakeProvider serves schema and records whether its own Construct ran.
func fakeProvider(schema string, schemaErr error, constructed *bool) p.Provider {
	return p.Provider{
		GetSchema: func(context.Context, p.GetSchemaRequest) (p.GetSchemaResponse, error) {
			return p.GetSchemaResponse{Schema: schema}, schemaErr
		},
		Construct: func(context.Context, p.ConstructRequest) (p.ConstructResponse, error) {
			*constructed = true
			return p.ConstructResponse{}, nil
		},
	}
}

const exampleURN = presource.URN("urn:pulumi:s::p::example:index:Component::example")

// The wrapper refuses a number that is not a whole number within int64's range wherever the
// schema types it integer, naming the component, its name and the input's path, and never runs
// the component's own Construct then. A whole number, an unknown, a null and a key the schema does
// not list reach it. A value inside a secret container is not quoted.
func TestRefuseFractionalIntegersWalksTheSchema(t *testing.T) {
	item := func(size float64) property.Value {
		return property.New(property.NewMap(map[string]property.Value{"size": property.New(size), "name": property.New("example")}))
	}
	for _, row := range []struct {
		name   string
		inputs map[string]property.Value
		want   string
	}{
		{"whole numbers everywhere", map[string]property.Value{
			"count": property.New(3.0), "item": item(1), "items": property.New(property.NewArray([]property.Value{item(2)})),
			"sizes": property.New(property.NewMap(map[string]property.Value{"a": property.New(4.0)})),
		}, ""},
		{"an unknown, a null and an unlisted key", map[string]property.Value{
			"count": property.New(property.Computed), "item": property.New(property.Null), "other": property.New(2.5),
		}, ""},
		{"a top-level fraction", map[string]property.Value{"count": property.New(2.5)}, "count 2.5 must be a whole number"},
		{"a fraction in a referred object", map[string]property.Value{"item": item(1.5)}, "item.size 1.5 must be a whole number"},
		{"a fraction in an array element", map[string]property.Value{
			"items": property.New(property.NewArray([]property.Value{item(1), item(0.25)})),
		}, "items[1].size 0.25 must be a whole number"},
		{"a fraction in a map value", map[string]property.Value{
			"sizes": property.New(property.NewMap(map[string]property.Value{"a": property.New(4.5)})),
		}, `sizes["a"] 4.5 must be a whole number`},
		{"a whole number past int64", map[string]property.Value{"count": property.New(9223372036854775808.0)}, "count 9.223372036854776e+18 is outside the 64-bit integer range"},
		{"a secret fraction", map[string]property.Value{"count": property.New(2.5).WithSecret(true)}, "count must be a whole number within the 64-bit integer range (not echoed, as it is secret)"},
		{"a fraction inside a secret object", map[string]property.Value{"item": item(1.5).WithSecret(true)}, "item.size must be a whole number within the 64-bit integer range (not echoed, as it is secret)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var constructed bool
			prov := refuseFractionalIntegers(fakeProvider(exampleSchema, nil, &constructed))
			_, err := prov.Construct(t.Context(), p.ConstructRequest{Urn: exampleURN, Inputs: property.NewMap(row.inputs)})
			if row.want == "" {
				require.NoError(t, err)
				require.True(t, constructed)
				return
			}
			require.EqualError(t, err, "example:index:Component example: "+row.want)
			require.False(t, constructed)
		})
	}
}

// A schema the wrapper cannot read refuses every Construct rather than skipping the check.
func TestRefuseFractionalIntegersRefusesWithoutASchema(t *testing.T) {
	for name, prov := range map[string]func(*bool) p.Provider{
		"GetSchema fails":   func(c *bool) p.Provider { return fakeProvider("", errors.New("example failure"), c) },
		"schema is no JSON": func(c *bool) p.Provider { return fakeProvider("{", nil, c) },
	} {
		t.Run(name, func(t *testing.T) {
			var constructed bool
			_, err := refuseFractionalIntegers(prov(&constructed)).Construct(t.Context(), p.ConstructRequest{Urn: exampleURN})
			require.ErrorContains(t, err, "to check integer inputs")
			require.False(t, constructed)
		})
	}
}

// The schema is read once and cached, so it is read without the first request's cancellation: a
// GetSchema that honours its context, as a real one may, still answers when that first request's
// context is already cancelled, and so does every Construct after it.
func TestRefuseFractionalIntegersSurvivesACancelledFirstRequest(t *testing.T) {
	var constructed bool
	fake := fakeProvider(exampleSchema, nil, &constructed)
	fake.GetSchema = func(ctx context.Context, _ p.GetSchemaRequest) (p.GetSchemaResponse, error) {
		if err := ctx.Err(); err != nil {
			return p.GetSchemaResponse{}, err
		}
		return p.GetSchemaResponse{Schema: exampleSchema}, nil
	}
	prov := refuseFractionalIntegers(fake)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	req := p.ConstructRequest{Urn: exampleURN, Inputs: property.NewMap(map[string]property.Value{"count": property.New(2.5)})}
	_, err := prov.Construct(cancelled, req)
	require.EqualError(t, err, "example:index:Component example: count 2.5 must be a whole number")
	_, err = prov.Construct(t.Context(), p.ConstructRequest{Urn: exampleURN, Inputs: property.NewMap(map[string]property.Value{"count": property.New(3.0)})})
	require.NoError(t, err)
	require.True(t, constructed)
}
