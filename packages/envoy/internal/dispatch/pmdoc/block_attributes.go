// The Go that the block schema's shape alone does not say: BlockAttributes, the ordered
// attribute list a typed block's `attributes` object decodes into, which
// schemas/block-schema.schema.json names through `x-go-type` and schema_generated.go holds the
// field of. encoding/json reads an object into a map, which holds no order, so this reads its
// keys through json.Decoder tokens and writes them back in the order it read them. It is written
// here, beside the generated file, rather than inside the generator: the generator emits what the
// schema shapes and nothing else.

package pmdoc

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// BlockAttribute is one attribute of a typed block: the name the schema declares it under, and
// what it holds.
type BlockAttribute struct {
	Name string
	BlockAttributeSchema
}

// BlockAttributes is a typed block's attributes in the order its schema declares them, which is
// the order the renderer writes them in a typed block's markdown.
type BlockAttributes []BlockAttribute

// Lookup is the attribute named name and whether the type declares it.
func (a BlockAttributes) Lookup(name string) (BlockAttributeSchema, bool) {
	for _, attribute := range a {
		if attribute.Name == name {
			return attribute.BlockAttributeSchema, true
		}
	}
	return BlockAttributeSchema{}, false
}

// Get is the attribute named name, or the zero schema when the type declares none, for a caller
// reading one field of an attribute it has already accepted.
func (a BlockAttributes) Get(name string) BlockAttributeSchema {
	attribute, _ := a.Lookup(name)
	return attribute
}

// Has reports whether the type declares an attribute named name.
func (a BlockAttributes) Has(name string) bool {
	_, ok := a.Lookup(name)
	return ok
}

// UnmarshalJSON reads the attributes object key by key, through json.Decoder tokens rather than
// into a map, so the attributes keep the order the JSON declares them in. A name declared twice
// is refused: the order of the two would be the reader's choice.
func (a *BlockAttributes) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if opening == nil {
		*a = nil
		return nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("block attributes must be a JSON object")
	}
	attributes := BlockAttributes{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("block attribute names must be strings")
		}
		if attributes.Has(name) {
			return fmt.Errorf("block attribute %q is declared twice", name)
		}
		var attribute BlockAttributeSchema
		if err := decoder.Decode(&attribute); err != nil {
			return err
		}
		attributes = append(attributes, BlockAttribute{Name: name, BlockAttributeSchema: attribute})
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	*a = attributes
	return nil
}

// MarshalJSON writes the attributes as the object they came from, its keys in declaration order,
// so a schema read and written again is the schema it was read from.
func (a BlockAttributes) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for index, attribute := range a {
		if index > 0 {
			out.WriteByte(',')
		}
		name, err := json.Marshal(attribute.Name)
		if err != nil {
			return nil, err
		}
		out.Write(name)
		out.WriteByte(':')
		value, err := json.Marshal(attribute.BlockAttributeSchema)
		if err != nil {
			return nil, err
		}
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
