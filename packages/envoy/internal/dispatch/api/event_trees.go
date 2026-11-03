package api

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// eventPayloadPrefix opens every key of an event's trees map: the path of the text the tree was
// read from, rooted at the envelope, so a reader finds a tree by the field it belongs to
// ("payload.body", "payload.options.0.label").
const eventPayloadPrefix = "payload"

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
func eventDisplayTrees(event model.Event) map[string]json.RawMessage {
	payload := eventPayloadFields(event)
	if payload == nil {
		return nil
	}
	trees := map[string]json.RawMessage{}
	addDisplayTree(trees, payload, eventPayloadPrefix, "body")
	if strings.HasPrefix(event.Type, "ask.") {
		addAskPayloadTrees(trees, payload, eventPayloadPrefix)
		addAskPayloadTrees(trees, nestedFields(payload, "previous"), eventPayloadPrefix+".previous")
		addDisplayTree(trees, nestedFields(payload, "previous_answer"), eventPayloadPrefix+".previous_answer", "text")
	}
	if len(trees) == 0 {
		return nil
	}
	return trees
}

// addAskPayloadTrees fills the trees of one ask-shaped object standing at path: its question, the
// label and description of each option, its answer's text and its resolution's reason. The edit
// payload's `previous` is the same shape with the question and the options alone, and the fields
// it does not hold are simply not there.
func addAskPayloadTrees(trees map[string]json.RawMessage, fields map[string]json.RawMessage, path string) {
	if fields == nil {
		return
	}
	addDisplayTree(trees, fields, path, "question")
	addDisplayTree(trees, nestedFields(fields, "answer"), path+".answer", "text")
	addDisplayTree(trees, nestedFields(fields, "resolution"), path+".resolution", "reason")
	for index, option := range nestedList(fields, "options") {
		optionPath := path + ".options." + strconv.Itoa(index)
		addDisplayTree(trees, option, optionPath, "label")
		addDisplayTree(trees, option, optionPath, "description")
	}
}

// addDisplayTree reads the field named name of fields for display and writes its tree under
// "<path>.<name>", where that field is a string with something in it. A field the payload does
// not hold, or holds as null or as anything but a string, has no text to read and no tree.
func addDisplayTree(trees map[string]json.RawMessage, fields map[string]json.RawMessage, path, name string) {
	raw, held := fields[name]
	if !held {
		return
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || text == "" {
		return
	}
	if tree := displayTree(text); tree != nil {
		trees[path+"."+name] = tree
	}
}

// eventPayloadFields is the payload's own fields, whatever the payload is held as: the JSON the
// database returned, or the typed value a write published. A payload that is not a JSON object -
// no payload Dispatch appends is anything else - has no fields and no trees.
func eventPayloadFields(event model.Event) map[string]json.RawMessage {
	raw, held := event.Payload.(json.RawMessage)
	if !held {
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			slog.Warn("dispatch: event payload encoding failed", "type", event.Type, "error", err)
			return nil
		}
		raw = encoded
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return fields
}

// nestedFields is the object standing at name, or nil where the payload holds none.
func nestedFields(fields map[string]json.RawMessage, name string) map[string]json.RawMessage {
	raw, held := fields[name]
	if !held {
		return nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil
	}
	return nested
}

// nestedList is the list of objects standing at name, or nil where the payload holds none.
func nestedList(fields map[string]json.RawMessage, name string) []map[string]json.RawMessage {
	raw, held := fields[name]
	if !held {
		return nil
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}
