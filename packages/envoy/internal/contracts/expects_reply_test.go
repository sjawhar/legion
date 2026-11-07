package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

// expectsReplyHolder is a reader of the field as a message carries it: the tag the generated
// envelope gives expects_reply, over the new type.
type expectsReplyHolder struct {
	ExpectsReply ExpectsReply `json:"expects_reply,omitempty"`
}

func TestParseExpectsReplyReadsEveryAcceptedWireValue(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want ExpectsReply
	}{
		{name: "false", raw: `false`, want: ExpectsReplyNone},
		{name: "optional", raw: `"optional"`, want: ExpectsReplyOptional},
		{name: "required", raw: `"required"`, want: ExpectsReplyRequired},
		{name: "the old none", raw: `"none"`, want: ExpectsReplyNone},
		{name: "null", raw: `null`, want: ExpectsReplyUnset},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseExpectsReply(json.RawMessage(test.raw))
			if err != nil {
				t.Fatalf("ParseExpectsReply(%s) = %v", test.raw, err)
			}
			if got != test.want {
				t.Errorf("ParseExpectsReply(%s) = %q, want %q", test.raw, got, test.want)
			}

			var holder expectsReplyHolder
			if err := json.Unmarshal([]byte(`{"expects_reply":`+test.raw+`}`), &holder); err != nil {
				t.Fatalf("unmarshal {\"expects_reply\":%s}: %v", test.raw, err)
			}
			if holder.ExpectsReply != test.want {
				t.Errorf("unmarshalled expects_reply = %q, want %q", holder.ExpectsReply, test.want)
			}
		})
	}
}

func TestParseExpectsReplyRefusesAnythingElseByNamingTheVocabulary(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "true", raw: `true`},
		{name: "another word", raw: `"later"`},
		// The Go constant's value is this package's own name for wanting no reply, never a word
		// the wire carries, so a sender writing it is refused like any other unknown token.
		{name: "the Go name for no reply", raw: `"no-reply"`},
		{name: "the empty string", raw: `""`},
		{name: "a number", raw: `2`},
		{name: "an object", raw: `{"expects":"reply"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseExpectsReply(json.RawMessage(test.raw))
			if err == nil {
				t.Fatalf("ParseExpectsReply(%s) = %q, want a refusal", test.raw, got)
			}
			if !strings.Contains(err.Error(), "false, optional, required") {
				t.Errorf("refusal = %q, want it to name false, optional, required", err)
			}

			var holder expectsReplyHolder
			unmarshalErr := json.Unmarshal([]byte(`{"expects_reply":`+test.raw+`}`), &holder)
			if unmarshalErr == nil {
				t.Fatalf("unmarshal {\"expects_reply\":%s} = %q, want a refusal", test.raw, holder.ExpectsReply)
			}
			if !strings.Contains(unmarshalErr.Error(), "false, optional, required") {
				t.Errorf("unmarshal refusal = %q, want it to name false, optional, required", unmarshalErr)
			}
		})
	}
}

func TestExpectsReplyMarshalsNoReplyAsTheLiteralFalse(t *testing.T) {
	for _, test := range []struct {
		value ExpectsReply
		want  string
	}{
		{value: ExpectsReplyNone, want: `{"expects_reply":false}`},
		{value: ExpectsReplyOptional, want: `{"expects_reply":"optional"}`},
		{value: ExpectsReplyRequired, want: `{"expects_reply":"required"}`},
		// The absent value is dropped by the field's omitempty, which encoding/json decides
		// from the string's length before MarshalJSON runs, so its refusal is unreachable
		// through a tagged field.
		{value: ExpectsReplyUnset, want: `{}`},
	} {
		t.Run(string(test.value), func(t *testing.T) {
			encoded, err := json.Marshal(expectsReplyHolder{ExpectsReply: test.value})
			if err != nil {
				t.Fatalf("marshal %q: %v", test.value, err)
			}
			if string(encoded) != test.want {
				t.Errorf("marshal %q = %s, want %s", test.value, encoded, test.want)
			}
		})
	}
}

func TestExpectsReplyValidateRefusesAWordNoProducerMayWrite(t *testing.T) {
	for _, value := range []ExpectsReply{ExpectsReplyUnset, ExpectsReplyNone, ExpectsReplyOptional, ExpectsReplyRequired} {
		if err := value.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want it accepted", value, err)
		}
	}
	err := ExpectsReply("later").Validate()
	if err == nil || !strings.Contains(err.Error(), "false, optional, required") {
		t.Errorf("Validate(\"later\") = %v, want a refusal naming false, optional, required", err)
	}
}
