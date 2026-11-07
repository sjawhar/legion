package contracts

import (
	"encoding/json"
	"fmt"
)

// ExpectsReply is the envelope's expects_reply field: what the sender wants back. On the wire it
// is the JSON literal false, "optional" or "required"; absent from a message that says nothing
// about a reply. This type is hand-written beside the generated mirror in generated.go, whose
// Envelope holds the field as this type and whose Validate() calls the Validate below, so one
// vocabulary is decided here and nowhere else. A constant's Go string is this package's own name
// for the value, not a word the wire carries: MarshalJSON writes the wire form and
// ParseExpectsReply reads every form accepted, the retired one named below among them.
type ExpectsReply string

const (
	// ExpectsReplyUnset is the absent field. It is never marshalled: every struct field holding
	// one carries omitempty, which encoding/json decides from the string's length before it
	// reaches MarshalJSON, so the empty value is dropped rather than written.
	ExpectsReplyUnset ExpectsReply = ""
	// ExpectsReplyNone wants no reply. It marshals to the JSON literal false.
	ExpectsReplyNone ExpectsReply = "no-reply"
	// ExpectsReplyOptional invites a reply without waiting on one.
	ExpectsReplyOptional ExpectsReply = "optional"
	// ExpectsReplyRequired waits on a reply.
	ExpectsReplyRequired ExpectsReply = "required"
)

// ExpectsReplyVocabulary is the wire vocabulary every refusal names, the literal false among the
// two words, so a sender reads what to write rather than the enum the field used to be. The
// listener's own 400 for this field composes its text from it (cmd/listener/api.go), so one edit
// moves both.
const ExpectsReplyVocabulary = "false, optional, required"

// expectsReplyLegacyNone is the retired wire word for wanting no reply, which the field carried
// before the JSON literal false replaced it and which ParseExpectsReply still reads as
// ExpectsReplyNone. Nothing writes it: retiring the leniency is deleting this constant and the
// one case of ParseExpectsReply that names it.
const expectsReplyLegacyNone = "none"

// MarshalJSON writes the no-reply value as the JSON literal false and the other two as their
// quoted words.
func (e ExpectsReply) MarshalJSON() ([]byte, error) {
	switch e {
	case ExpectsReplyNone:
		return []byte("false"), nil
	case ExpectsReplyOptional:
		return []byte(`"optional"`), nil
	case ExpectsReplyRequired:
		return []byte(`"required"`), nil
	case ExpectsReplyUnset:
		return nil, fmt.Errorf("expects_reply is absent and must be omitted, not written")
	default:
		return nil, expectsReplyRefusal()
	}
}

// UnmarshalJSON reads what ParseExpectsReply accepts.
func (e *ExpectsReply) UnmarshalJSON(raw []byte) error {
	parsed, err := ParseExpectsReply(raw)
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// ParseExpectsReply reads one expects_reply field as it arrived: the JSON literal false, the
// strings "optional" and "required", and - leniently - the old string "none", which means the
// same as false. A field that is absent, as a nil raw or as JSON null, is the unset value, since
// saying nothing about a reply is not an error. Anything else is refused by naming the vocabulary.
//
// The "none" case is the one piece of leniency here, and what removes it is a count, not a date:
// every producer and every skill line of this repository now writes false, so the case goes once
// no deployment pins a plugin build old enough to write "none".
func ParseExpectsReply(raw json.RawMessage) (ExpectsReply, error) {
	if len(raw) == 0 {
		return ExpectsReplyUnset, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ExpectsReplyUnset, expectsReplyRefusal()
	}
	switch read := value.(type) {
	case nil:
		return ExpectsReplyUnset, nil
	case bool:
		if !read {
			return ExpectsReplyNone, nil
		}
	case string:
		if read == expectsReplyLegacyNone {
			return ExpectsReplyNone, nil
		}
		switch ExpectsReply(read) {
		case ExpectsReplyOptional, ExpectsReplyRequired:
			return ExpectsReply(read), nil
		}
	}
	return ExpectsReplyUnset, expectsReplyRefusal()
}

// Validate refuses a value no producer may write. generated.go's Envelope.Validate calls it for
// expects_reply, the one property whose own type the generator defers to (valueChecks in
// packages/contracts/scripts/gen-go.ts), in place of a switch over words.
func (e ExpectsReply) Validate() error {
	switch e {
	case ExpectsReplyUnset, ExpectsReplyNone, ExpectsReplyOptional, ExpectsReplyRequired:
		return nil
	}
	return expectsReplyRefusal()
}

func expectsReplyRefusal() error {
	return fmt.Errorf("expects_reply must be one of: %s", ExpectsReplyVocabulary)
}
