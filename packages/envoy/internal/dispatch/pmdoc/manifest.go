package pmdoc

import (
	"encoding/json"
	"fmt"
)

// ElementVersions is from which schema version a build reads an element (Since) and from which a
// build may create it (CreatableSince). The manifest leaves out a number that is 1. Two builds
// serve one database during a release, so an element is added in two steps: one release reads it
// and does not create it, and a later one creates it, once no live build lacks it.
type ElementVersions struct {
	Since          int `json:"since,omitempty"`
	CreatableSince int `json:"creatable_since,omitempty"`
}

// ElementKind is what an Element names.
type ElementKind string

const (
	NodeElement       ElementKind = "node"
	MarkElement       ElementKind = "mark"
	TypedBlockElement ElementKind = "typed block"
	AttributeElement  ElementKind = "attribute"
	ChoiceElement     ElementKind = "value"
)

// Element names one element of the manifest. Name is its own name. Block is the typed block of an
// attribute or of a choice, and Attribute is the enum attribute of a choice.
type Element struct {
	Kind      ElementKind
	Block     string
	Attribute string
	Name      string
}

// String names e as a refusal names it: `typed block "ask"`, `attribute "urgency" of typed block
// "ask"`, `value "low" of attribute "urgency" of typed block "ask"`.
func (e Element) String() string {
	switch e.Kind {
	case AttributeElement:
		return fmt.Sprintf("%s %q of typed block %q", e.Kind, e.Name, e.Block)
	case ChoiceElement:
		return fmt.Sprintf("%s %q of attribute %q of typed block %q", e.Kind, e.Name, e.Attribute, e.Block)
	default:
		return fmt.Sprintf("%s %q", e.Kind, e.Name)
	}
}

// Versions is e's since and creatable_since in this build's manifest, each 1 where the manifest
// says none, and false when the manifest does not declare e.
func (e Element) Versions() (ElementVersions, bool) {
	versions, declared := active.versions[e]
	return versions, declared
}

// compiledManifest is a manifest decoded, checked and indexed.
type compiledManifest struct {
	schema      BlockSchema
	types       map[string]BlockTypeSchema
	nodes       map[string]bool
	marks       map[string]bool
	versions    map[Element]ElementVersions
	uncreatable map[Element]bool
	creatable   int
}

// active is the manifest this build reads by: the embedded one, or another one a test installs
// (UseManifestForTest). schema, schemaTypes, nodeTypes and markTypes are its parts.
var (
	active      compiledManifest
	schema      BlockSchema
	schemaTypes map[string]BlockTypeSchema
	nodeTypes   map[string]bool
	markTypes   map[string]bool
)

func init() {
	compiled, err := compileManifest(blockSchemaJSON)
	if err != nil {
		panic(fmt.Sprintf("pmdoc: embedded block schema: %v", err))
	}
	compiled.install()
}

func (m compiledManifest) install() {
	active = m
	schema, schemaTypes, nodeTypes, markTypes = m.schema, m.types, m.nodes, m.marks
}

func compileManifest(raw []byte) (compiledManifest, error) {
	var value BlockSchema
	if err := json.Unmarshal(raw, &value); err != nil {
		return compiledManifest{}, fmt.Errorf("decode: %w", err)
	}
	if err := normalizeSchemaDefaults(&value); err != nil {
		return compiledManifest{}, fmt.Errorf("normalize: %w", err)
	}
	normalizeVersions(&value)
	if err := validateBlockSchema(value); err != nil {
		return compiledManifest{}, fmt.Errorf("invalid: %w", err)
	}
	compiled := compiledManifest{
		schema:      value,
		types:       make(map[string]BlockTypeSchema, len(value.Types)),
		nodes:       make(map[string]bool, len(value.Nodes)),
		marks:       make(map[string]bool, len(value.Marks)),
		versions:    make(map[Element]ElementVersions),
		uncreatable: make(map[Element]bool),
	}
	declare := func(element Element, versions ElementVersions) error {
		if _, duplicate := compiled.versions[element]; duplicate {
			return fmt.Errorf("invalid: %s is declared twice", element)
		}
		if err := validateElementVersions(element, versions, value.Version); err != nil {
			return fmt.Errorf("invalid: %w", err)
		}
		compiled.versions[element] = versions
		if versions.CreatableSince > value.Version {
			compiled.uncreatable[element] = true
		} else {
			compiled.creatable = max(compiled.creatable, versions.Since)
		}
		return nil
	}
	for _, node := range value.Nodes {
		if node.Name == "" {
			return compiledManifest{}, fmt.Errorf("invalid: a node has no name")
		}
		if err := declare(Element{Kind: NodeElement, Name: node.Name}, node.ElementVersions); err != nil {
			return compiledManifest{}, err
		}
		compiled.nodes[node.Name] = true
	}
	for _, mark := range value.Marks {
		if mark.Name == "" {
			return compiledManifest{}, fmt.Errorf("invalid: a mark has no name")
		}
		if err := declare(Element{Kind: MarkElement, Name: mark.Name}, mark.ElementVersions); err != nil {
			return compiledManifest{}, err
		}
		compiled.marks[mark.Name] = true
	}
	for _, typ := range value.Types {
		if compiled.nodes[typ.Name] {
			return compiledManifest{}, fmt.Errorf("invalid: type %q is also a node", typ.Name)
		}
		if err := declare(Element{Kind: TypedBlockElement, Name: typ.Name}, typ.ElementVersions); err != nil {
			return compiledManifest{}, err
		}
		compiled.types[typ.Name] = typ
		for name, attr := range typ.Attributes {
			if err := declare(Element{Kind: AttributeElement, Block: typ.Name, Name: name}, attr.ElementVersions); err != nil {
				return compiledManifest{}, err
			}
			for choice := range attr.ChoiceVersions {
				if !containsString(attr.Choices, choice) {
					return compiledManifest{}, fmt.Errorf("invalid: type %q attribute %q gives versions to %q, which is not one of its choices", typ.Name, name, choice)
				}
			}
			for _, choice := range attr.Choices {
				if err := declare(Element{Kind: ChoiceElement, Block: typ.Name, Attribute: name, Name: choice}, attr.ChoiceVersions[choice]); err != nil {
					return compiledManifest{}, err
				}
			}
			// A write gives a new block an attribute's default (defaultAttributes), so a default
			// value a build may not create would be created with every block that takes it.
			if fallback, ok := attr.Default.(string); ok && attr.Kind == "enum" && attr.ChoiceVersions[fallback].CreatableSince > attr.CreatableSince {
				return compiledManifest{}, fmt.Errorf("invalid: type %q attribute %q default %q is creatable later than the attribute", typ.Name, name, fallback)
			}
		}
	}
	if len(compiled.nodes) == 0 {
		return compiledManifest{}, fmt.Errorf("invalid: must declare its nodes")
	}
	if len(compiled.marks) == 0 {
		return compiledManifest{}, fmt.Errorf("invalid: must declare its marks")
	}
	return compiled, nil
}

// normalizeVersions writes 1 for every since and creatable_since the manifest leaves out.
func normalizeVersions(value *BlockSchema) {
	one := func(versions *ElementVersions) {
		if versions.Since == 0 {
			versions.Since = 1
		}
		if versions.CreatableSince == 0 {
			versions.CreatableSince = 1
		}
	}
	for index := range value.Nodes {
		one(&value.Nodes[index].ElementVersions)
	}
	for index := range value.Marks {
		one(&value.Marks[index].ElementVersions)
	}
	for index := range value.Types {
		typ := &value.Types[index]
		one(&typ.ElementVersions)
		for name, attr := range typ.Attributes {
			one(&attr.ElementVersions)
			if len(attr.Choices) > 0 {
				choices := make(map[string]ElementVersions, len(attr.Choices))
				for _, choice := range attr.Choices {
					versions := attr.ChoiceVersions[choice]
					one(&versions)
					choices[choice] = versions
				}
				for choice, versions := range attr.ChoiceVersions {
					if _, listed := choices[choice]; !listed {
						choices[choice] = versions
					}
				}
				attr.ChoiceVersions = choices
			}
			typ.Attributes[name] = attr
		}
	}
}

// validateElementVersions refuses numbers no build can act on: an element read before version 1,
// read only from a version after the manifest's own, or created before it is read.
func validateElementVersions(element Element, versions ElementVersions, version int) error {
	switch {
	case versions.Since < 1 || versions.CreatableSince < 1:
		return fmt.Errorf("%s has since %d and creatable_since %d; both must be 1 or more", element, versions.Since, versions.CreatableSince)
	case versions.Since > version:
		return fmt.Errorf("%s has since %d, after the manifest's version %d", element, versions.Since, version)
	case versions.CreatableSince < versions.Since:
		return fmt.Errorf("%s has creatable_since %d, before its since %d", element, versions.CreatableSince, versions.Since)
	}
	return nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// SchemaVersion returns V, this build's schema version: the version a client presents during
// document admission. This build reads every element whose since is V or less, and creates every
// element whose creatable_since is V or less.
func SchemaVersion() int {
	return schema.Version
}

// SchemaCreatable returns R, the highest since among the elements this build may create: a build
// whose SchemaVersion is R or more reads everything this build writes.
func SchemaCreatable() int {
	return active.creatable
}

// creatable reports whether this build may create element: its manifest declares it, and its
// creatable_since is SchemaVersion or less.
func creatable(element Element) bool {
	return !active.uncreatable[element]
}

// eachElement calls yield with each manifest element nodes hold, in document order, until yield
// returns false: each node, mark and typed block, each attribute a typed block holds a value for,
// and each enum attribute's value. It reports whether yield never returned false.
func eachElement(nodes []*Node, yield func(Element) bool) bool {
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if typ, typed := typedBlock(node.Type); typed {
			if !yield(Element{Kind: TypedBlockElement, Name: node.Type}) {
				return false
			}
			for name, value := range node.Attrs {
				if name == BlockIDAttr || value == nil {
					continue
				}
				if !yield(Element{Kind: AttributeElement, Block: node.Type, Name: name}) {
					return false
				}
				if choice, ok := value.(string); ok && typ.Attributes[name].Kind == "enum" {
					if !yield(Element{Kind: ChoiceElement, Block: node.Type, Attribute: name, Name: choice}) {
						return false
					}
				}
			}
		} else if !yield(Element{Kind: NodeElement, Name: node.Type}) {
			return false
		}
		for _, mark := range node.Marks {
			if !yield(Element{Kind: MarkElement, Name: mark.Type}) {
				return false
			}
		}
		if !eachElement(node.Children, yield) {
			return false
		}
	}
	return true
}

// refuseUncreatable is the refusal of a write that makes nodes holding an element this build reads
// but may not create yet (creatable_since above SchemaVersion), naming the first one in document
// order, or nil. The validation of every tree a write produces runs it: markdown parsed for a
// write, Update, and the transforms that add content (Splice, InsertTableRows, RetypeBlock,
// SetBlockAttributes, SetBlockBody). A transform that only deletes or moves blocks adds no
// element, and reading a stored tree or stored markdown never refuses one. held is the document
// the write starts from, nil for a write that starts from nothing: an element held already holds
// is not created by the write, which keeps what a newer build wrote and edits beside it. held is
// called only when nodes hold such an element.
func refuseUncreatable(nodes []*Node, held func() *Node) error {
	if len(active.uncreatable) == 0 {
		return nil
	}
	var made []Element
	seen := make(map[Element]bool)
	eachElement(nodes, func(element Element) bool {
		if active.uncreatable[element] && !seen[element] {
			seen[element] = true
			made = append(made, element)
		}
		return true
	})
	if len(made) == 0 {
		return nil
	}
	present := make(map[Element]bool, len(made))
	if held != nil {
		if source := held(); source != nil {
			eachElement([]*Node{source}, func(element Element) bool {
				if seen[element] {
					present[element] = true
				}
				return true
			})
		}
	}
	for _, element := range made {
		if !present[element] {
			return fmt.Errorf("%w: this Dispatch build reads %s but cannot write it yet. A later release writes it", ErrSchema, element)
		}
	}
	return nil
}
