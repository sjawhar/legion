package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// machineID is the machine every seeded record names; the script runs the listener under the same
// ENVOY_MACHINE_ID.
const machineID = "e2e-refill"

// recordSet is one named set of records: count records, written at variant.
type recordSet struct {
	name    string
	count   int
	variant int
}

func parseSets(spec string) ([]recordSet, error) {
	var sets []recordSet
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rest, ok := strings.Cut(part, "=")
		if !ok || name == "" || strings.ContainsAny(name, "_.") {
			return nil, fmt.Errorf("set %q: want name=count[:variant], a name without _ or .", part)
		}
		countText, variantText, hasVariant := strings.Cut(rest, ":")
		count, err := strconv.Atoi(countText)
		if err != nil || count < 0 {
			return nil, fmt.Errorf("set %q: count %q is not a whole number", part, countText)
		}
		variant := 1
		if hasVariant {
			if variant, err = strconv.Atoi(variantText); err != nil || variant < 1 || variant > 9 {
				return nil, fmt.Errorf("set %q: variant %q is not 1-9", part, variantText)
			}
		}
		sets = append(sets, recordSet{name: name, count: count, variant: variant})
	}
	return sets, nil
}

func sessionID(set string, index int) string { return fmt.Sprintf("ses_refill_%s_%d", set, index) }

// parseSessionID names the set and index of a seeded session id.
func parseSessionID(id string) (string, int, bool) {
	rest, ok := strings.CutPrefix(id, "ses_refill_")
	if !ok {
		return "", 0, false
	}
	cut := strings.LastIndexByte(rest, '_')
	if cut <= 0 {
		return "", 0, false
	}
	index, err := strconv.Atoi(rest[cut+1:])
	if err != nil || index < 0 {
		return "", 0, false
	}
	return rest[:cut], index, true
}

func sessionTitle(set string, index, variant int) string {
	return fmt.Sprintf("refill %s %d v%d", set, index, variant)
}

func variantTopic(set string, variant int) string {
	return fmt.Sprintf("notifications.refill.%s.v%d", set, variant)
}

// topics are the topics a seeded session subscribes to: its set's variant topic, and the role topic
// for the first session of a set seeded with a role, which makes that session the role's holder in
// every list row (cmd/listener roleNames).
func topics(set recordSet, index int, role string) []string {
	out := []string{variantTopic(set.name, set.variant)}
	if role != "" && index == 0 {
		out = append(out, contracts.RoleTopicPrefix+role)
	}
	return out
}

func sessionRecord(set recordSet, index int, now int64) session.SessionEntry {
	return session.SessionEntry{
		MachineID:      machineID,
		Dir:            "/refill/" + set.name,
		Title:          sessionTitle(set.name, index, set.variant),
		UpdatedAt:      now,
		SelfSubscribed: true,
	}
}

func interestRecord(set recordSet, index int, now int64, role string) store.Interest {
	id := sessionID(set.name, index)
	return store.Interest{
		SessionID: id,
		MachineID: machineID,
		Dir:       "/refill/" + set.name,
		Topics:    append(topics(set, index, role), contracts.AgentSubject(id)),
		UpdatedAt: now,
	}
}
