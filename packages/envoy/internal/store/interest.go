package store

import (
	"slices"

	"github.com/sjawhar/envoy/internal/bus"
)

type Interest struct {
	SessionID string   `json:"session_id"`
	MachineID string   `json:"machine_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
	UpdatedAt int64    `json:"updated_at"`
}

func Merge(item Interest, topics []string) Interest {
	seen := map[string]bool{}
	out := make([]string, 0, len(item.Topics)+len(topics))
	for _, topic := range item.Topics {
		if seen[topic] {
			continue
		}
		seen[topic] = true
		out = append(out, topic)
	}
	for _, topic := range topics {
		if seen[topic] {
			continue
		}
		seen[topic] = true
		out = append(out, topic)
	}
	item.Topics = out
	return item
}

func Remove(item Interest, topics []string) Interest {
	if len(topics) == 0 {
		item.Topics = nil
		return item
	}
	cut := map[string]bool{}
	for _, topic := range topics {
		cut[topic] = true
	}
	out := make([]string, 0, len(item.Topics))
	for _, topic := range item.Topics {
		if cut[topic] {
			continue
		}
		out = append(out, topic)
	}
	item.Topics = out
	return item
}

<<<<<<< conflict 1 of 1
+++++++ zwxwlryn 92c90c86 "merge: main into INFRA-771, past f5bdbada so worker-image/build has repo-paths.sh"
// dropPrivateStreamTopics removes from topics every one reaching a private stream subject that is
// not sessionID's own (bus.ReachesPrivateStreamSubject): another session's inbox, a
// failed-delivery lane, a controller notice lane of a project whose controller role roles does not
// name, or a wildcard overlapping one. Registry.Match fans a stored topic out to its serve-port
// session, so such a topic would hand that session messages meant for another session or for
// another project's controller. The listener refuses one at /v1/interests/subscribe; this drops
// one a row stored before that guard held, and one whose role the session has since lost, so every
// write of a row (Upsert, Remove) cleans it. roles is what the session holds now
// (Registry.RolesHeldBy, read by controllerLaneRoles). sessionID is the row's key, not the
// session_id its stored value names. It filters topics in place, so callers pass a slice they own
// (Merge's and Remove's results are).
func dropPrivateStreamTopics(sessionID string, roles []string, topics []string) []string {
%%%%%%% diff from: rmmmynwt 8bfdf258 "fix(dispatch): a version that restores the approved text keeps its approval (#2703)"
\\\\\\\        to: lmonpkut e2b93abb "ci: the envoy-go lane gets 60 minutes, and its test steps time out inside it (#2739)"
 // dropPrivateStreamTopics removes from topics every one reaching a private stream subject other
 // than sessionID's own agent subject (bus.ReachesPrivateStreamSubject): another session's inbox, a
 // failed-delivery lane, the Legion controller lane, or a wildcard overlapping one. Registry.Match
-// fans a stored topic out to its serve-port session, so such a topic would hand that session
-// messages meant for another session or for the controller's role holder. The listener refuses
-// one at /v1/interests/subscribe; this drops one a row stored before that guard holds, so every
-// write of a row (Upsert, Remove) cleans it. sessionID is the row's key, not the session_id its
-// stored value names. It filters topics in place, so callers pass a slice they own (Merge's and
-// Remove's results are).
+// fans a stored topic out to the listener the row names, which delivers it to that row's session,
+// so such a topic would hand that session messages meant for another session or for the
+// controller's role holder. The listener refuses one at /v1/interests/subscribe; this drops one a
+// row stored before that guard holds, so every write of a row (Upsert, Remove) cleans it.
+// sessionID is the row's key, not the session_id its stored value names. It filters topics in
+// place, so callers pass a slice they own (Merge's and Remove's results are).
 func dropPrivateStreamTopics(sessionID string, topics []string) []string {
>>>>>>> conflict 1 of 1 ends
	return slices.DeleteFunc(topics, func(topic string) bool {
		return bus.ReachesPrivateStreamSubject(sessionID, topic, roles)
	})
}

// MergedTopics is the topic set an upsert of topics into a row of sessionID holding cur stores:
// their union (Merge), without a topic reaching a private stream subject that is not the session's
// own (dropPrivateStreamTopics), sorted. mergeForUpsert stores exactly this, and the listener's
// registration route filters the session's consumer on it before the write (cmd/listener's
// subscribeHandler, through Registry.PlanInterest), so the filter and the row it stores name one
// set: a filter naming a topic the row does not would carry that topic into a session no interest
// of its own follows, and a row holding one the filter does not would say a session follows a
// topic its consumer never delivers.
func MergedTopics(sessionID string, roles []string, cur []string, topics []string) []string {
	merged := dropPrivateStreamTopics(sessionID, roles, Merge(Interest{Topics: cur}, topics).Topics)
	slices.Sort(merged)
	return merged
}

// InterestPlan is one registration's whole decision about a session's topics: the topics its
// interest row will hold, which are its session consumer's filter too, and the roles read to
// decide them, which name the one private stream subject a session registers beside its own agent
// subject (dropPrivateStreamTopics). Registry.PlanInterest reads the roles once and merges once;
// the listener's registration route builds the filter from the plan and hands the same plan back
// to Registry.UpsertPlanned, so no second read can decide the row differently from the filter.
//
// Revision is the bucket revision of the row the plan merged its topics into, 0 for a session
// with no row: Registry.UpsertPlanned writes at that revision and refuses the write when another
// writer has stored the row since (ErrInterestPlanConflict), and the route then plans again from
// the topics it asked for (Registry.PlanInterestAfterConflict), filters the consumer on the new
// plan and writes that, so an unsubscribe that landed in the seconds between a plan and its
// write is not undone and the filter is never left standing for a plan the row does not hold.
type InterestPlan struct {
	Roles    []string
	Topics   []string
	Revision uint64
}
