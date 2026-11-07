package agentloop

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A Final schema outside the strict structured-output subset is a request the loop will not
// make: the model call that would have carried a schema the provider cannot enforce is never
// made, and the error names the JSON path of the node at fault and what is wrong with it, so the
// caller fixes the schema.
func TestAFinalSchemaOutsideTheStrictSubsetIsRefused(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	for _, c := range []struct {
		name  string
		final string
		want  string
	}{{
		name: "a property left out of required",
		// The daemon's own reply form, as it was on the run that proved this: the
		// schema rode in output_config.format and bound nothing.
		final: `{"type":"object","properties":{"kind":{"type":"string"},"needs":{"type":"string"}},` +
			`"required":["kind"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.needs: optional property (not in required)",
	}, {
		name: "a type union",
		final: `{"type":"object","properties":{"needs":{"type":["string","null"]}},` +
			`"required":["needs"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.needs: a type union (type is an array)",
	}, {
		name: "an anyOf union",
		final: `{"type":"object","properties":{"needs":{"anyOf":[{"type":"string"},{"type":"null"}]}},` +
			`"required":["needs"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.needs: an anyOf union (a node of a Final schema takes one type)",
	}, {
		name:  "a root with no additionalProperties false",
		final: `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`,
		want:  "agentloop: the request's final schema: the root: no additionalProperties: false",
	}, {
		name: "an object that takes properties it does not name",
		final: `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],` +
			`"additionalProperties":true}`,
		want: "agentloop: the request's final schema: the root: additionalProperties is true, want false",
	}, {
		name: "a nested object with an optional property",
		final: `{"type":"object","properties":{"seen":{"type":"object",` +
			`"properties":{"at":{"type":"string"},"by":{"type":"string"}},"required":["at"],"additionalProperties":false}},` +
			`"required":["seen"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.seen.properties.by: optional property (not in required)",
	}, {
		name: "an array whose items take properties they do not name",
		final: `{"type":"object","properties":{"steps":{"type":"array","items":` +
			`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}},` +
			`"required":["steps"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.steps.items: no additionalProperties: false",
	}, {
		name: "an array whose items are a union",
		final: `{"type":"object","properties":{"steps":{"type":"array","items":{"type":["string","null"]}}},` +
			`"required":["steps"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.steps.items: a type union (type is an array)",
	}, {
		name: "an array with no items schema",
		final: `{"type":"object","properties":{"steps":{"type":"array"}},` +
			`"required":["steps"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.steps: an array with no items schema",
	}, {
		name: "a definition with an optional property",
		final: `{"type":"object","properties":{"step":{"$ref":"#/$defs/step"}},"required":["step"],` +
			`"additionalProperties":false,"$defs":{"step":{"type":"object",` +
			`"properties":{"name":{"type":"string"},"note":{"type":"string"}},"required":["name"],"additionalProperties":false}}}`,
		// The path is the one the caller writes the schema in, through the $ref rather
		// than to the definition it names.
		want: "agentloop: the request's final schema: properties.step.properties.note: optional property (not in required)",
	}, {
		name: "a definition no $ref names",
		final: `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],` +
			`"additionalProperties":false,"definitions":{"step":{"type":"object",` +
			`"properties":{"name":{"type":"string"}},"required":["name"]}}}`,
		want: "agentloop: the request's final schema: definitions.step: no additionalProperties: false",
	}, {
		name: "a recursive schema",
		final: `{"type":"object","properties":{"node":{"$ref":"#/$defs/node"}},"required":["node"],` +
			`"additionalProperties":false,"$defs":{"node":{"type":"object",` +
			`"properties":{"next":{"$ref":"#/$defs/node"}},"required":["next"],"additionalProperties":false}}}`,
		want: `agentloop: the request's final schema: properties.node.properties.next: a recursive $ref, "#/$defs/node"`,
	}, {
		name: "a $ref that names nothing",
		final: `{"type":"object","properties":{"step":{"$ref":"#/$defs/step"}},"required":["step"],` +
			`"additionalProperties":false}`,
		want: `agentloop: the request's final schema: properties.step: $ref "#/$defs/step" names nothing in this schema`,
	}, {
		name: "a constraint neither provider compiles",
		final: `{"type":"object","properties":{"answer":{"type":"string","minLength":1}},` +
			`"required":["answer"],"additionalProperties":false}`,
		want: `agentloop: the request's final schema: properties.answer: unsupported keyword "minLength"`,
	}, {
		name: "an array constraint the SDK strips before the call",
		final: `{"type":"object","properties":{"steps":{"type":"array","minItems":2,"items":{"type":"string"}}},` +
			`"required":["steps"],"additionalProperties":false}`,
		want: `agentloop: the request's final schema: properties.steps: unsupported keyword "minItems"`,
	}, {
		name: "a format one of the providers does not know",
		final: `{"type":"object","properties":{"home":{"type":"string","format":"uri"}},` +
			`"required":["home"],"additionalProperties":false}`,
		want: "agentloop: the request's final schema: properties.home: unsupported format uri",
	}, {
		name:  "a root that is not an object",
		final: `{"type":"array","items":{"type":"string"}}`,
		want:  "agentloop: the request's final schema: the root: the schema itself is not an object",
	}, {
		name: "a root that is one $ref, as a schema generator writes it",
		final: `{"$ref":"#/$defs/reply","$defs":{"reply":{"type":"object",` +
			`"properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}}`,
		want: "agentloop: the request's final schema: the root: the schema itself is a $ref, not an object",
	}} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, s, AnthropicMessages, 1)
			f.req.Final = json.RawMessage(c.final)
			result, err := Run(context.Background(), f.req)
			if err == nil {
				t.Fatalf("the request ran, with result %+v", result)
			}
			if err.Error() != c.want {
				t.Errorf("the error is\n%q\nwant\n%q", err, c.want)
			}
			if !reflect.DeepEqual(result, Result{}) {
				t.Errorf("the result is %+v, want nothing", result)
			}
		})
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d model calls were made, want none: a schema the provider cannot enforce is never sent", n)
	}
}

// strictRun is a Final schema inside the subset — a nested object, an array of objects, an enum
// and a format — and the reply it asks for.
const (
	strictRunSchema = `{"type":"object","properties":{` +
		`"kind":{"type":"string","enum":["answered","not_answered"]},` +
		`"text":{"type":"string"},` +
		`"steps":{"type":"array","items":{"type":"object",` +
		`"properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}},` +
		`"seen":{"type":"object","properties":{"at":{"type":"string","format":"date-time"}},` +
		`"required":["at"],"additionalProperties":false}},` +
		`"required":["kind","text","steps","seen"],"additionalProperties":false}`
	strictRunAnswer = `{"kind":"answered","text":"plum","steps":[{"name":"read"}],"seen":{"at":"2026-11-07T00:00:00Z"}}`
)

// A Final schema inside the subset is a run like any other: the call is made, the schema rides to
// the provider as it was written, and the reply comes back as the object.
func TestAStrictFinalSchemaRuns(t *testing.T) {
	t.Parallel()
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 9, Output: 12}, anthropicText(strictRunAnswer)))
	f := newFixture(t, s, AnthropicMessages, 1)
	f.req.Final = json.RawMessage(strictRunSchema)
	result, err := Run(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fault != "" {
		t.Fatalf("fault %q: %s", result.Fault, result.Detail)
	}
	if !sameJSON(t, result.Value, []byte(strictRunAnswer)) {
		t.Errorf("the value is %s, want %s", result.Value, strictRunAnswer)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	var body struct {
		OutputConfig struct {
			Format struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if schema := body.OutputConfig.Format.Schema; !sameJSON(t, schema, []byte(strictRunSchema)) {
		t.Errorf("the call's output_config format schema is %s, want the request's %s", schema, strictRunSchema)
	}
}

// Every shape the subset allows is accepted, including the ones no caller in this repository
// writes yet: a definition reached through a $ref, a definition reached through two, an empty
// object, and the annotations that constrain nothing.
func TestTheStrictSubsetAcceptsWhatBothProvidersEnforce(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		final string
	}{{
		name: "a definition reached through a $ref",
		final: `{"type":"object","title":"A plan","properties":{"step":{"$ref":"#/$defs/step","description":"the one step"}},` +
			`"required":["step"],"additionalProperties":false,"$defs":{"step":{"type":"object",` +
			`"properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}}}`,
	}, {
		name: "the same definition named twice",
		final: `{"type":"object","properties":{"first":{"$ref":"#/$defs/step"},"second":{"$ref":"#/$defs/step"}},` +
			`"required":["first","second"],"additionalProperties":false,"$defs":{"step":{"type":"object",` +
			`"properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}}}`,
	}, {
		name: "an object that names no property",
		final: `{"type":"object","properties":{"free":{"type":"object","additionalProperties":false}},` +
			`"required":["free"],"additionalProperties":false}`,
	}, {
		name: "an enum of plain values of each kind",
		final: `{"type":"object","properties":{"level":{"type":"integer","enum":[1,2,3]},` +
			`"ok":{"type":"boolean"},"nothing":{"type":"null"},` +
			`"tags":{"type":"array","items":{"type":"string","enum":["red","green"]}}},` +
			`"required":["level","ok","nothing","tags"],"additionalProperties":false}`,
	}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := strictFinal(json.RawMessage(c.final)); err != nil {
				t.Errorf("the schema was refused: %v", err)
			}
		})
	}
}

// A schema that is no JSON object at all is refused before anything is read out of it.
func TestAFinalThatIsNoSchemaIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		final string
		want  string
	}{
		{name: "a fragment", final: `{"type":"object",`, want: "unexpected end of JSON input"},
		{name: "a string", final: `"an object"`, want: "the root: not a JSON object"},
		{name: "a boolean schema", final: `true`, want: "the root: not a JSON object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := strictFinal(json.RawMessage(c.final))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error is %v, want %q in it", err, c.want)
			}
		})
	}
}
