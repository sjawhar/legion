// Package config: this file holds the `capabilities` block (Capabilities) and its reading from
// the `legion.yaml` mapping (readCapabilities), with the prose helper a decision's error names the
// open choices with (orList).
package config

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
)

// Capabilities is the `capabilities` block: the deployment capabilities the operator has decided
// (`capabilities.decided`, each with the reason), which the daemon's report shows as decided
// rather than open (capabilities.Deployment). A decision records why the deployment leaves a
// capability out; it closes nothing.
type Capabilities struct {
	Decided map[capabilities.Name]string
}

// readCapabilities reads `capabilities`: a mapping whose one member, `decided`, maps a deployment
// capability (capabilities.Decidable) to the reason the operator decided it. A name the daemon
// does not measure from its deployment is refused naming the ones it does — nothing else can be
// decided here, since every other row is the image's, a live check's, or a ruling's — and so is a
// blank reason: the reason is what the report shows in the gap's place, and a blank one records
// nothing.
func readCapabilities(value *yaml.Node, key string) (*Capabilities, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := members(value, key, "decided")
	if err != nil {
		return nil, err
	}
	block := &Capabilities{}
	decided, decidedKey := fields["decided"], key+".decided"
	if decided == nil {
		return block, nil
	}
	if decided.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of capability to reason", decidedKey)
	}
	names := capabilities.Decidable()
	for i := 0; i+1 < len(decided.Content); i += 2 {
		name, reason := capabilities.Name(decided.Content[i].Value), decided.Content[i+1]
		field := decidedKey + "." + string(name)
		if !slices.Contains(names, name) {
			return nil, fmt.Errorf("unknown key %s: a decision may name %s", field, orList(names))
		}
		if _, twice := block.Decided[name]; twice {
			return nil, fmt.Errorf("%s names %s twice", decidedKey, name)
		}
		read, err := readNonEmptyString(reason, field)
		if err != nil {
			return nil, err
		}
		if read == nil {
			return nil, fmt.Errorf("%s must not be empty", field)
		}
		if block.Decided == nil {
			block.Decided = map[capabilities.Name]string{}
		}
		block.Decided[name] = *read
	}
	return block, nil
}

// orList is names as prose: "a, b or c".
func orList(names []capabilities.Name) string {
	words := make([]string, len(names))
	for i, name := range names {
		words[i] = string(name)
	}
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}
