package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// deliveryLiveEncoded is one computed body, encoded once: the body as the GET route writes it, and
// each list item, map entry, count and whole field with its revision, so every subscriber of the
// view diffs against the one encoding. revs is every revision the body holds: a snapshot sends it,
// and a subscriber sent a delta against it holds it afterwards. It is shared by every subscriber
// of the view and never mutated.
type deliveryLiveEncoded struct {
	kind   string
	body   json.RawMessage
	items  map[string][]deliveryLiveItem
	maps   map[string]map[string]deliveryLiveValue
	counts map[string]map[string]map[string]int
	fields map[string]deliveryLiveValue
	revs   DeliveryLiveRevs
}

// deliveryLiveItem is one list item: its id (a string id as it is, a numeric one in decimal), its
// encoding and its revision.
type deliveryLiveItem struct {
	id  string
	raw json.RawMessage
	rev string
}

// deliveryLiveValue is one map entry's or whole field's encoding and revision.
type deliveryLiveValue struct {
	raw json.RawMessage
	rev string
}

// encodeDeliveryBody encodes body, a view of kind's GET body, by its kind's field classes. It
// refuses a list item without an id, and two items of one list with the same id, since either
// would leave a page holding something no delta can reach.
func encodeDeliveryBody(kind string, body any) (deliveryLiveEncoded, error) {
	classes, ok := deliveryLiveKinds[kind]
	if !ok {
		return deliveryLiveEncoded{}, fmt.Errorf("delivery live: unknown view kind %q", kind)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return deliveryLiveEncoded{}, fmt.Errorf("delivery live: encode %s body: %w", kind, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return deliveryLiveEncoded{}, fmt.Errorf("delivery live: read %s body: %w", kind, err)
	}
	field := func(name string) (json.RawMessage, error) {
		value, ok := top[name]
		if !ok {
			return nil, fmt.Errorf("delivery live: the %s body has no field %q", kind, name)
		}
		return value, nil
	}
	encoded := deliveryLiveEncoded{
		kind:   kind,
		body:   raw,
		items:  make(map[string][]deliveryLiveItem, len(classes.lists)),
		maps:   make(map[string]map[string]deliveryLiveValue, len(classes.maps)),
		counts: make(map[string]map[string]map[string]int, len(classes.counts)),
		fields: make(map[string]deliveryLiveValue, len(classes.whole)+len(classes.withChanges)),
		revs: DeliveryLiveRevs{
			Items:  make(map[string]map[string]string, len(classes.lists)),
			Maps:   make(map[string]map[string]string, len(classes.maps)),
			Counts: make(map[string]map[string]map[string]string, len(classes.counts)),
			Fields: make(map[string]string, len(classes.whole)+len(classes.withChanges)),
		},
	}

	for _, list := range classes.lists {
		value, err := field(list)
		if err != nil {
			return deliveryLiveEncoded{}, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err != nil {
			return deliveryLiveEncoded{}, fmt.Errorf("delivery live: read %s.%s: %w", kind, list, err)
		}
		encodedItems := make([]deliveryLiveItem, 0, len(items))
		revs := make(map[string]string, len(items))
		for _, item := range items {
			id, err := deliveryLiveItemID(item)
			if err != nil {
				return deliveryLiveEncoded{}, fmt.Errorf("delivery live: %s.%s: %w", kind, list, err)
			}
			if _, duplicate := revs[id]; duplicate {
				return deliveryLiveEncoded{}, fmt.Errorf("delivery live: %s.%s holds id %q twice", kind, list, id)
			}
			rev := deliveryLiveRevision(item)
			revs[id] = rev
			encodedItems = append(encodedItems, deliveryLiveItem{id: id, raw: item, rev: rev})
		}
		encoded.items[list] = encodedItems
		encoded.revs.Items[list] = revs
	}

	for _, name := range classes.maps {
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, err
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(value, &entries); err != nil {
			return deliveryLiveEncoded{}, fmt.Errorf("delivery live: read %s.%s: %w", kind, name, err)
		}
		values := make(map[string]deliveryLiveValue, len(entries))
		revs := make(map[string]string, len(entries))
		for key, entry := range entries {
			rev := deliveryLiveRevision(entry)
			values[key] = deliveryLiveValue{raw: entry, rev: rev}
			revs[key] = rev
		}
		encoded.maps[name] = values
		encoded.revs.Maps[name] = revs
	}

	for _, name := range classes.counts {
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, err
		}
		var facets map[string]map[string]int
		if err := json.Unmarshal(value, &facets); err != nil {
			return deliveryLiveEncoded{}, fmt.Errorf("delivery live: read %s.%s: %w", kind, name, err)
		}
		revs := make(map[string]map[string]string, len(facets))
		for facet, counts := range facets {
			facetRevs := make(map[string]string, len(counts))
			for key, count := range counts {
				facetRevs[key] = deliveryLiveCountRevision(count)
			}
			revs[facet] = facetRevs
		}
		encoded.counts[name] = facets
		encoded.revs.Counts[name] = revs
	}

	for name, excluded := range classes.whole {
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, err
		}
		rev, err := deliveryLiveWholeRevision(value, excluded)
		if err != nil {
			return deliveryLiveEncoded{}, fmt.Errorf("delivery live: %s.%s: %w", kind, name, err)
		}
		encoded.fields[name] = deliveryLiveValue{raw: value, rev: rev}
		encoded.revs.Fields[name] = rev
	}
	for _, name := range classes.withChanges {
		value, err := field(name)
		if err != nil {
			return deliveryLiveEncoded{}, err
		}
		rev := deliveryLiveRevision(value)
		encoded.fields[name] = deliveryLiveValue{raw: value, rev: rev}
		encoded.revs.Fields[name] = rev
	}
	return encoded, nil
}

// deliveryLiveItemID is a list item's "id": a string as it is, a number in its JSON form.
func deliveryLiveItemID(item json.RawMessage) (string, error) {
	var keyed struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(item, &keyed); err != nil {
		return "", err
	}
	switch {
	case len(keyed.ID) == 0 || string(keyed.ID) == "null":
		return "", errors.New("an item has no id")
	case keyed.ID[0] == '"':
		var id string
		if err := json.Unmarshal(keyed.ID, &id); err != nil {
			return "", err
		}
		return id, nil
	default:
		return string(keyed.ID), nil
	}
}

// deliveryLiveCountRevision is a count's revision: its JSON encoding's.
func deliveryLiveCountRevision(count int) string {
	return deliveryLiveRevision(strconv.AppendInt(nil, int64(count), 10))
}

// deliveryLiveWholeRevision is a whole field's revision, over its encoding without the subfields
// excluded leaves out (the timeline's measures move their computed_at, window and freshness on
// every compute, and a page is sent the measures only when a number in them moved).
func deliveryLiveWholeRevision(value json.RawMessage, excluded []string) (string, error) {
	if len(excluded) == 0 {
		return deliveryLiveRevision(value), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return "", err
	}
	for _, name := range excluded {
		delete(object, name)
	}
	kept, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return deliveryLiveRevision(kept), nil
}

// snapshot is the snapshot frame of e for one subscription: the body, the change position it
// reflects, when that change was recorded, and every revision.
func (e deliveryLiveEncoded) snapshot(sub string, seq int64, changedAt *time.Time) DeliveryLiveSnapshot {
	return DeliveryLiveSnapshot{
		Type: "snapshot", Sub: sub, Seq: strconv.FormatInt(seq, 10), ChangedAt: changedAt,
		Body: e.body, Revs: e.revs,
	}
}

// deliveryLiveNull is a map entry's value in a delta when the key left.
var deliveryLiveNull = json.RawMessage("null")

// diffDeliveryBody is what a subscriber holding held is sent so that it holds next: every list item
// whose revision differs (whole, in the body's order) or that left (by id), every map entry and
// count that differs (null for one that left), and every whole field that differs. The bool is
// false when none of those differs, and the delta is then not sent; a field sent only with changes
// (the window) never makes a delta by itself, and is included whenever it moved. Sub, Seq and
// ChangedAt are the caller's.
// After a delta is sent, the subscriber holds next.revs.
func diffDeliveryBody(next deliveryLiveEncoded, held DeliveryLiveRevs) (DeliveryLiveDelta, bool) {
	classes := deliveryLiveKinds[next.kind]
	delta := DeliveryLiveDelta{
		Type:   "delta",
		Set:    map[string]json.RawMessage{},
		Items:  map[string]DeliveryLiveItemChanges{},
		Maps:   map[string]map[string]json.RawMessage{},
		Counts: map[string]map[string]map[string]*int{},
	}
	changed := false

	for _, list := range classes.lists {
		heldRevs := held.Items[list]
		nextRevs := next.revs.Items[list]
		changes := DeliveryLiveItemChanges{Upsert: []json.RawMessage{}, Remove: []string{}}
		revs := map[string]*string{}
		for _, item := range next.items[list] {
			if heldRevs[item.id] == item.rev {
				continue
			}
			rev := item.rev
			changes.Upsert = append(changes.Upsert, item.raw)
			revs[item.id] = &rev
		}
		for id := range heldRevs {
			if _, kept := nextRevs[id]; !kept {
				changes.Remove = append(changes.Remove, id)
				revs[id] = nil
			}
		}
		if len(revs) == 0 {
			continue
		}
		slices.Sort(changes.Remove)
		delta.Items[list] = changes
		if delta.Revs.Items == nil {
			delta.Revs.Items = map[string]map[string]*string{}
		}
		delta.Revs.Items[list] = revs
		changed = true
	}

	for _, name := range classes.maps {
		heldRevs := held.Maps[name]
		nextValues := next.maps[name]
		values := map[string]json.RawMessage{}
		revs := map[string]*string{}
		for key, value := range nextValues {
			if heldRevs[key] == value.rev {
				continue
			}
			rev := value.rev
			values[key] = value.raw
			revs[key] = &rev
		}
		for key := range heldRevs {
			if _, kept := nextValues[key]; !kept {
				values[key] = deliveryLiveNull
				revs[key] = nil
			}
		}
		if len(revs) == 0 {
			continue
		}
		delta.Maps[name] = values
		if delta.Revs.Maps == nil {
			delta.Revs.Maps = map[string]map[string]*string{}
		}
		delta.Revs.Maps[name] = revs
		changed = true
	}

	for _, name := range classes.counts {
		heldFacets := held.Counts[name]
		nextFacets := next.counts[name]
		nextRevs := next.revs.Counts[name]
		counts := map[string]map[string]*int{}
		revs := map[string]map[string]*string{}
		note := func(facet, key string, count *int, rev *string) {
			if counts[facet] == nil {
				counts[facet] = map[string]*int{}
				revs[facet] = map[string]*string{}
			}
			counts[facet][key] = count
			revs[facet][key] = rev
		}
		for facet, values := range nextFacets {
			heldRevs := heldFacets[facet]
			for key, count := range values {
				rev := nextRevs[facet][key]
				if heldRevs[key] == rev {
					continue
				}
				count := count
				note(facet, key, &count, &rev)
			}
		}
		for facet, heldRevs := range heldFacets {
			for key := range heldRevs {
				if _, kept := nextFacets[facet][key]; !kept {
					note(facet, key, nil, nil)
				}
			}
		}
		if len(counts) == 0 {
			continue
		}
		delta.Counts[name] = counts
		if delta.Revs.Counts == nil {
			delta.Revs.Counts = map[string]map[string]map[string]*string{}
		}
		delta.Revs.Counts[name] = revs
		changed = true
	}

	for name := range classes.whole {
		value := next.fields[name]
		if held.Fields[name] == value.rev {
			continue
		}
		delta.Set[name] = value.raw
		if delta.Revs.Fields == nil {
			delta.Revs.Fields = map[string]string{}
		}
		delta.Revs.Fields[name] = value.rev
		changed = true
	}

	if !changed {
		return DeliveryLiveDelta{}, false
	}
	for _, name := range classes.withChanges {
		value := next.fields[name]
		if held.Fields[name] == value.rev {
			continue
		}
		delta.Set[name] = value.raw
		if delta.Revs.Fields == nil {
			delta.Revs.Fields = map[string]string{}
		}
		delta.Revs.Fields[name] = value.rev
	}
	return delta, true
}
