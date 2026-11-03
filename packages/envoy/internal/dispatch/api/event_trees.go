package api

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// eventPayloadPrefix opens every key of an event's trees map: the path of the text the tree was
// read from, rooted at the envelope, so a reader finds a tree by the field it belongs to
// ("payload.body", "payload.options.0.label").
const eventPayloadPrefix = "payload"

// eventBodyTreePath is where the tree of a comment's or a message's body stands, which is the one
// tree almost every event of a feed carries.
const eventBodyTreePath = eventPayloadPrefix + ".body"

// attachEventTrees fills the trees of every listed event, for a caller that asked for them. The
// REST event lists do this only under the header; a /ws/live frame always does, since a browser
// can set no header on a socket (liveEventFrame).
func attachEventTrees(events []model.Event, trees bool) {
	if !trees {
		return
	}
	for index := range events {
		events[index].Trees = eventDisplayTrees(events[index])
	}
}

// eventDisplayTrees is the display tree of every stored text the event's payload carries, keyed
// by that text's path: the body a comment or a message event carries, and an ask event's
// question, options, answer text and resolution reason, the question and options an edit
// replaced among them. A payload is read as the JSON it is rather than as its Go type, because a
// read serves it as the database stored it (readEvents) and a publish as the type the write
// built, and both have to answer the same keys.
//
// The event's type says which fields can hold a text, and only those are read: the whole payload
// of a feed's event is several times the size of its body, and a decode of all of it -
// generically, as a map of raw members - was the warm read's largest cost. An event of a type
// that can hold no stored text is not read at all.
func eventDisplayTrees(event model.Event) map[string]json.RawMessage {
	ask := strings.HasPrefix(event.Type, "ask.")
	if !ask && !eventTypeHoldsBody(event.Type) {
		return nil
	}
	payload := eventPayloadJSON(event)
	if payload == nil {
		return nil
	}
	if ask {
		return askEventTrees(payload)
	}
	tree := displayTree(eventBodyText(payload))
	if tree == nil {
		return nil
	}
	return map[string]json.RawMessage{eventBodyTreePath: tree}
}

// eventTypeHoldsBody reports whether an event of this type can carry a stored body. The comment
// and suggestion events carry the comment itself (model.CommentEventPayload) and the message
// events the message (model.MessageEventPayload); those are the only payloads Dispatch appends
// that hold a markdown body, so an event of any other type - an issue update, a child's status,
// a document version - carries no stored text and no tree.
func eventTypeHoldsBody(eventType string) bool {
	return strings.HasPrefix(eventType, "comment.") ||
		strings.HasPrefix(eventType, "message.") ||
		strings.HasPrefix(eventType, "suggestion.")
}

// askTexts is the stored texts of one ask-shaped object: its question, the label and description
// of each option, its answer's text and its resolution's reason. The edit payload's `previous` is
// the same shape with the question and the options alone, and the fields it does not hold are
// simply not there.
type askTexts struct {
	Question   string             `json:"question"`
	Options    []askOptionTexts   `json:"options"`
	Answer     *askAnswerText     `json:"answer"`
	Resolution *askResolutionText `json:"resolution"`
}

// askOptionTexts is one answer choice's two stored texts.
type askOptionTexts struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// askAnswerText is the text a human wrote with their answer.
type askAnswerText struct {
	Text string `json:"text"`
}

// askResolutionText is why an ask no longer needs an answer.
type askResolutionText struct {
	Reason string `json:"reason"`
}

// askEventTexts is every stored text an ask event's payload can hold: the ask's own, the content
// an edit replaced (`previous`), the answer an edit or a second answer superseded
// (`previous_answer`), and a body, which no ask payload carries today and which is read here so
// that one that gains it answers the same key every other event's body answers.
type askEventTexts struct {
	askTexts
	Body           string         `json:"body"`
	Previous       *askTexts      `json:"previous"`
	PreviousAnswer *askAnswerText `json:"previous_answer"`
}

// askEventTrees is the trees of an ask event: the ask's own texts, the ones an edit replaced, and
// the superseded answer's. An ask event is a handful of a feed's events where a comment is
// hundreds of them, and its texts stand in five places, so its payload is decoded in one pass
// into the fields that can hold one rather than scanned for each.
//
// A payload no text can be read from - one that is not a JSON object, which no payload Dispatch
// appends is - decodes to no texts, and so does a field of an unexpected type: each such text
// stays empty, which has no tree, so a malformed field costs its own tree and no other's, as
// reading each field on its own did. That is what the decode's error says, and why it is not
// read.
func askEventTrees(raw json.RawMessage) map[string]json.RawMessage {
	var payload askEventTexts
	_ = json.Unmarshal(raw, &payload)
	trees := map[string]json.RawMessage{}
	addDisplayTree(trees, eventPayloadPrefix, "body", payload.Body)
	addAskTrees(trees, eventPayloadPrefix, &payload.askTexts)
	if payload.Previous != nil {
		addAskTrees(trees, eventPayloadPrefix+".previous", payload.Previous)
	}
	if payload.PreviousAnswer != nil {
		addDisplayTree(trees, eventPayloadPrefix+".previous_answer", "text", payload.PreviousAnswer.Text)
	}
	if len(trees) == 0 {
		return nil
	}
	return trees
}

// addAskTrees fills the trees of one ask-shaped object standing at path.
func addAskTrees(trees map[string]json.RawMessage, path string, texts *askTexts) {
	addDisplayTree(trees, path, "question", texts.Question)
	if texts.Answer != nil {
		addDisplayTree(trees, path+".answer", "text", texts.Answer.Text)
	}
	if texts.Resolution != nil {
		addDisplayTree(trees, path+".resolution", "reason", texts.Resolution.Reason)
	}
	for index, option := range texts.Options {
		optionPath := path + ".options." + strconv.Itoa(index)
		addDisplayTree(trees, optionPath, "label", option.Label)
		addDisplayTree(trees, optionPath, "description", option.Description)
	}
}

// addDisplayTree reads text for display and writes its tree under "<path>.<name>", where there is
// something in it. A field the payload does not hold, or holds as null or as anything but a
// string, decoded to the empty string and has no text to read and no tree.
func addDisplayTree(trees map[string]json.RawMessage, path, name, text string) {
	if tree := displayTree(text); tree != nil {
		trees[path+"."+name] = tree
	}
}

// eventPayloadJSON is the event's payload as JSON, whatever it is held as: the JSON the database
// returned, or the typed value a write published, which is encoded here. It answers nil where
// that encoding failed, which leaves the event without trees.
func eventPayloadJSON(event model.Event) json.RawMessage {
	if raw, held := event.Payload.(json.RawMessage); held {
		return raw
	}
	encoded, err := json.Marshal(event.Payload)
	if err != nil {
		slog.Warn("dispatch: event payload encoding failed", "type", event.Type, "error", err)
		return nil
	}
	return encoded
}

// quotedBodyName is the member a comment's or a message's text stands in, as the payload writes
// it: the scan below compares a member's name against these bytes, which costs it no string of
// its own. Neither Postgres's jsonb nor Go's encoder writes an escape in a name of ASCII
// letters, so the two are the same bytes wherever the member is there at all.
const quotedBodyName = `"body"`

// eventBodyText is the text the payload's body member holds, and the empty string where it holds
// none. It reads the payload only as far as that member and no further: an event's payload is
// several times its body in bytes, and Postgres orders a jsonb object's members by the length of
// their names and then by their bytes, so `body` stands among the first of a comment's or a
// message's and the rest of them are never parsed.
func eventBodyText(raw json.RawMessage) string {
	scanner, held := eventPayloadScanners.Get().(*eventPayloadScanner)
	if !held {
		scanner = &eventPayloadScanner{}
		scanner.decoder = jsontext.NewDecoder(&scanner.reader)
	}
	defer eventPayloadScanners.Put(scanner)
	scanner.reader.Reset(raw)
	scanner.decoder.Reset(&scanner.reader)
	// A payload that is not a JSON object - no payload Dispatch appends is anything else - holds
	// no member at all, and the opening brace is read past so the members follow.
	if scanner.decoder.PeekKind() != '{' {
		return ""
	}
	if _, err := scanner.decoder.ReadToken(); err != nil {
		return ""
	}
	for scanner.decoder.PeekKind() == '"' {
		name, err := scanner.decoder.ReadValue()
		if err != nil {
			return ""
		}
		if string(name) != quotedBodyName {
			if err := scanner.decoder.SkipValue(); err != nil {
				return ""
			}
			continue
		}
		value, err := scanner.decoder.ReadValue()
		if err != nil {
			return ""
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return ""
		}
		return text
	}
	return ""
}

// eventPayloadScanner reads one payload's members in the order they stand, far enough to answer
// for the one wanted. The decoder and the reader under it are kept between events because a feed
// reads one for every event it answers, and each would otherwise allocate a buffer the size of
// the payload it read.
type eventPayloadScanner struct {
	reader  bytes.Reader
	decoder *jsontext.Decoder
}

// eventPayloadScanners holds those scanners between reads.
var eventPayloadScanners sync.Pool
