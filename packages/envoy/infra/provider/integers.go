package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/property"

	"github.com/sjawhar/envoy/internal/bus"
)

// schemaProperty is the part of a schema property refuseFractionalIntegers reads: its type, the
// object type it refers to, and the element type of an array or a map.
type schemaProperty struct {
	Type                 string          `json:"type"`
	Ref                  string          `json:"$ref"`
	Items                *schemaProperty `json:"items"`
	AdditionalProperties *schemaProperty `json:"additionalProperties"`
}

// integerSchema is the plugin's schema as refuseFractionalIntegers walks it: each component's
// inputs, by token, and each object type's properties, by $ref.
type integerSchema struct {
	Resources map[string]struct {
		InputProperties map[string]schemaProperty `json:"inputProperties"`
	} `json:"resources"`
	Types map[string]struct {
		Properties map[string]schemaProperty `json:"properties"`
	} `json:"types"`
}

// refuseFractionalIntegers wraps prov's Construct so a number in an input the schema types
// integer, at any depth, is refused unless it is a whole number within int64's range. Pulumi
// decodes such an input into a Go int by conversion (SDK v3.265.0 go/pulumi/rpc.go,
// unmarshalOutput: SetInt(int64(v))), so 2.5 would become 2 with no error, and a value past the
// range would become whatever the conversion gives; a typed SDK in another language, or Pulumi
// YAML, sends any number the schema's integer allows in JSON. The refusal names the component,
// its name and the input's path, and quotes the value unless it is secret. The schema is read from
// prov itself, once, on the first Construct, so every integer the components declare is covered
// without a list to keep.
func refuseFractionalIntegers(prov p.Provider) p.Provider {
	getSchema, construct := prov.GetSchema, prov.Construct
	var once sync.Once
	var schema integerSchema
	var schemaErr error
	prov.Construct = func(ctx context.Context, req p.ConstructRequest) (p.ConstructResponse, error) {
		once.Do(func() {
			// The result is cached for every later Construct, so the read keeps the request's
			// values (the run info the schema is named from) but not its cancellation: a first
			// request cancelled mid-read must not fail every Construct after it.
			resp, err := getSchema(context.WithoutCancel(ctx), p.GetSchemaRequest{})
			if err != nil {
				schemaErr = fmt.Errorf("reading the schema to check integer inputs: %w", err)
				return
			}
			if err := json.Unmarshal([]byte(resp.Schema), &schema); err != nil {
				schemaErr = fmt.Errorf("decoding the schema to check integer inputs: %w", err)
			}
		})
		if schemaErr != nil {
			return p.ConstructResponse{}, schemaErr
		}
		typ, name := string(req.Urn.Type()), req.Urn.Name()
		// A name holding an nkey seed is the component's own to refuse, first and without quoting
		// it, so the check, whose refusal quotes the name, leaves that request to the component.
		if bus.HoldsSeed(name) {
			return construct(ctx, req)
		}
		if refusal := schema.fractional(schema.Resources[typ].InputProperties, req.Inputs, "", false); refusal != "" {
			return p.ConstructResponse{}, fmt.Errorf("%s %s: %s", typ, name, refusal)
		}
		return construct(ctx, req)
	}
	return prov
}

// fractional is the refusal of the first integer in inputs, whose properties are props, that is
// not a whole number within int64's range, or "" when there is none. A key the schema does not
// list is left to the decoder. secret is whether a container holding inputs is secret: a secret
// flag marks the value it is set on and not the values inside it, so it is carried down.
func (s integerSchema) fractional(props map[string]schemaProperty, inputs property.Map, prefix string, secret bool) string {
	for key, value := range inputs.AllStable {
		spec, ok := props[key]
		if !ok {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if refusal := s.fractionalValue(spec, value, path, secret); refusal != "" {
			return refusal
		}
	}
	return ""
}

func (s integerSchema) fractionalValue(spec schemaProperty, value property.Value, path string, secret bool) string {
	secret = secret || value.Secret()
	switch {
	case value.IsComputed() || value.IsNull():
		return ""
	case spec.Type == "integer" && value.IsNumber():
		n := value.AsNumber()
		whole := n == math.Trunc(n)
		// -2^63 and 2^63 are exact as float64; int64 holds the first and not the second.
		inRange := n >= math.MinInt64 && n < math.MaxInt64
		switch {
		case whole && inRange:
			return ""
		case secret:
			return path + " must be a whole number within the 64-bit integer range (not echoed, as it is secret)"
		case whole || math.IsInf(n, 0):
			return fmt.Sprintf("%s %s is outside the 64-bit integer range", path, strconv.FormatFloat(n, 'g', -1, 64))
		}
		return fmt.Sprintf("%s %s must be a whole number", path, strconv.FormatFloat(n, 'g', -1, 64))
	case spec.Ref != "" && value.IsMap():
		ref := strings.TrimPrefix(spec.Ref, "#/types/")
		return s.fractional(s.Types[ref].Properties, value.AsMap(), path, secret)
	case spec.Type == "array" && spec.Items != nil && value.IsArray():
		for i, item := range value.AsArray().All {
			if refusal := s.fractionalValue(*spec.Items, item, fmt.Sprintf("%s[%d]", path, i), secret); refusal != "" {
				return refusal
			}
		}
	case spec.Type == "object" && spec.AdditionalProperties != nil && value.IsMap():
		for key, item := range value.AsMap().AllStable {
			if refusal := s.fractionalValue(*spec.AdditionalProperties, item, fmt.Sprintf("%s[%q]", path, key), secret); refusal != "" {
				return refusal
			}
		}
	}
	return ""
}
