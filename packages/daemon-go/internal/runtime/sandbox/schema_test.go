package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// crdFile is kubernetes-sigs/agent-sandbox's Sandbox CRD at tag v1.0.3, byte for byte
// (https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/v1.0.3/k8s/crds/agents.x-k8s.io_sandboxes.yaml,
// sha256 bc6d66863decb675ce6da2c835ee3bb06aaaf8d73ad03aae9fe971fde5e8acce): the schema the
// production cluster's API server prunes a Sandbox against.
const crdFile = "testdata/sandboxes.agents.x-k8s.io.yaml"

// sandboxSchema is the CRD's openAPIV3Schema for v1beta1.
func sandboxSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(crdFile)
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	versions, _ := crd["spec"].(map[string]any)["versions"].([]any)
	for _, item := range versions {
		version := item.(map[string]any)
		if version["name"] == sandboxGVR.Version {
			return version["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
		}
	}
	t.Fatalf("%s has no %s", crdFile, sandboxGVR.Version)
	return nil
}

// schemaViolations walks value against a structural schema, the way the API server's pruning and
// validation read one: every field of an object must be a declared property, or allowed by
// additionalProperties or x-kubernetes-preserve-unknown-fields, and every value must have its
// schema's type, and enum value when one is listed. The root's apiVersion, kind, and metadata are
// the API server's own, so they are not walked. An unknown field is the finding that matters: the
// API server drops it without a word.
func schemaViolations(value any, schema map[string]any, path string) []string {
	if path == "" {
		object, ok := value.(map[string]any)
		if !ok {
			return []string{"the manifest is not an object"}
		}
		rest := map[string]any{}
		for key, field := range object {
			if key != "apiVersion" && key != "kind" && key != "metadata" {
				rest[key] = field
			}
		}
		value = rest
	}
	if schema["x-kubernetes-int-or-string"] == true {
		switch v := value.(type) {
		case string:
			return nil
		case json.Number:
			if !strings.ContainsAny(v.String(), ".eE") {
				return nil
			}
		}
		return []string{fmt.Sprintf("%s: %v is neither an integer nor a string", path, value)}
	}
	if value == nil {
		if schema["nullable"] == true {
			return nil
		}
		return []string{path + ": null, which the schema does not allow"}
	}
	var found []string
	if enum, ok := schema["enum"].([]any); ok && !slices.Contains(enum, value) {
		found = append(found, fmt.Sprintf("%s: %v is not one of %v", path, value, enum))
	}
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return append(found, fmt.Sprintf("%s: %T, want an object", path, value))
		}
		properties, _ := schema["properties"].(map[string]any)
		additional := schema["additionalProperties"]
		preserve := schema["x-kubernetes-preserve-unknown-fields"] == true
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := path + "." + key
			if property, ok := properties[key].(map[string]any); ok {
				found = append(found, schemaViolations(object[key], property, child)...)
				continue
			}
			switch additional := additional.(type) {
			case map[string]any:
				found = append(found, schemaViolations(object[key], additional, child)...)
			case bool:
				if !additional {
					found = append(found, child+": not in the schema; the API server prunes it")
				}
			default:
				if !preserve {
					found = append(found, child+": not in the schema; the API server prunes it")
				}
			}
		}
		required, _ := schema["required"].([]any)
		for _, key := range required {
			if _, ok := object[key.(string)]; !ok {
				found = append(found, fmt.Sprintf("%s.%s: required and absent", path, key))
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return append(found, fmt.Sprintf("%s: %T, want an array", path, value))
		}
		itemSchema, _ := schema["items"].(map[string]any)
		for i, item := range items {
			found = append(found, schemaViolations(item, itemSchema, fmt.Sprintf("%s[%d]", path, i))...)
		}
	case "string":
		if _, ok := value.(string); !ok {
			found = append(found, fmt.Sprintf("%s: %T, want a string", path, value))
		}
	case "integer":
		if n, ok := value.(json.Number); !ok || strings.ContainsAny(n.String(), ".eE") {
			found = append(found, fmt.Sprintf("%s: %v, want an integer", path, value))
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			found = append(found, fmt.Sprintf("%s: %T, want a number", path, value))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			found = append(found, fmt.Sprintf("%s: %T, want a boolean", path, value))
		}
	case nil:
		if schema["x-kubernetes-preserve-unknown-fields"] != true {
			found = append(found, path+": the schema declares no type")
		}
	}
	return found
}

// wire is a manifest as the API server receives it: the JSON the client sends, decoded with
// numbers kept as numbers.
func wire(t *testing.T, manifest any) any {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// The walk finds what it exists to find: a field the schema does not declare, a value of the
// wrong type, and a value outside an enum. Without these, a clean walk of the real manifests
// would prove nothing.
func TestSchemaWalkFindsUnknownFieldsAndWrongTypes(t *testing.T) {
	schema := sandboxSchema(t)
	for name, tc := range map[string]struct {
		manifest string
		want     string
	}{
		"unknown field": {
			`{"spec":{"podTemplate":{"metadata":{},"spec":{"containers":[{"name":"x","imagePullPolicyy":"Always"}]}}}}`,
			".spec.podTemplate.spec.containers[0].imagePullPolicyy: not in the schema",
		},
		"unknown podTemplate metadata": {
			`{"spec":{"podTemplate":{"metadata":{"name":"x"},"spec":{"containers":[{"name":"x"}]}}}}`,
			".spec.podTemplate.metadata.name: not in the schema",
		},
		"wrong type": {
			`{"spec":{"podTemplate":{"metadata":{},"spec":{"containers":[{"name":"x"}],"terminationGracePeriodSeconds":"30"}}}}`,
			".spec.podTemplate.spec.terminationGracePeriodSeconds: 30, want an integer",
		},
		"enum": {
			`{"spec":{"operatingMode":"Paused","podTemplate":{"metadata":{},"spec":{"containers":[{"name":"x"}]}}}}`,
			".spec.operatingMode: Paused is not one of",
		},
		"missing required": {
			`{"spec":{"podTemplate":{"metadata":{},"spec":{}}}}`,
			".spec.podTemplate.spec.containers: required and absent",
		},
	} {
		t.Run(name, func(t *testing.T) {
			decoder := json.NewDecoder(strings.NewReader(tc.manifest))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				t.Fatal(err)
			}
			found := strings.Join(schemaViolations(value, schema, ""), "\n")
			if !strings.Contains(found, tc.want) {
				t.Fatalf("violations:\n%s\nwant one containing %q", found, tc.want)
			}
		})
	}
}
