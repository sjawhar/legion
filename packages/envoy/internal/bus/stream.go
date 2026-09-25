package bus

import (
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/nats-io/nats.go"
)

// roleLaneSubjects are the role lanes and their exceptions twin. They travel over core NATS and
// the stream must never retain them.
var roleLaneSubjects = []string{
	"notifications.role.>",
	"notifications.envoy.exceptions.notifications.role.>",
}

// subjectCapturesRoleLanes reports whether some role-lane subject would land in a stream carrying
// subject.
func subjectCapturesRoleLanes(subject string) bool {
	return slices.ContainsFunc(roleLaneSubjects, func(roleLane string) bool { return subjectsOverlap(subject, roleLane) })
}

func streamCapturesRoleLanes(subjects []string) bool {
	return slices.ContainsFunc(subjects, subjectCapturesRoleLanes)
}

func purgeLegacyRoleMessages(js nats.JetStreamContext) error {
	for _, subject := range roleLaneSubjects {
		if err := js.PurgeStream(Stream, &nats.StreamPurgeRequest{Subject: subject}); err != nil {
			return err
		}
	}
	return nil
}

func migrateLegacyConsumerFilters(js nats.JetStreamContext, subjects []string) error {
	for name := range js.ConsumerNames(Stream) {
		info, err := js.ConsumerInfo(Stream, name)
		if err != nil {
			return err
		}
		if info.Config.FilterSubject != "notifications.>" {
			continue
		}
		config := info.Config
		config.FilterSubject = ""
		config.FilterSubjects = slices.Clone(subjects)
		if _, err := js.UpdateConsumer(Stream, &config); err != nil {
			return err
		}
	}
	return nil
}

func migrateRoleLanesOffStream(js nats.JetStreamContext, oldConfig, newConfig *nats.StreamConfig) error {
	if !streamCapturesRoleLanes(oldConfig.Subjects) || streamCapturesRoleLanes(newConfig.Subjects) {
		return nil
	}
	if err := migrateLegacyConsumerFilters(js, newConfig.Subjects); err != nil {
		return err
	}
	return purgeLegacyRoleMessages(js)
}

// subjectsOverlap reports whether some subject matches both patterns. JetStream refuses two such
// subjects in one stream.
func subjectsOverlap(a, b string) bool {
	aTokens, bTokens := strings.Split(a, "."), strings.Split(b, ".")
	for index := 0; index < len(aTokens) && index < len(bTokens); index++ {
		aToken, bToken := aTokens[index], bTokens[index]
		if aToken == ">" || bToken == ">" {
			return true
		}
		if aToken != "*" && bToken != "*" && aToken != bToken {
			return false
		}
	}
	return len(aTokens) == len(bTokens)
}

// streamReconciliation is the subject list the stream carries once this binary has started, and
// what that did to the deployed list: the list is the deployed one, then each of this binary's
// subjects the deployed list lacks. Every bus.Connect caller ensures this one stream (the
// listener, Dispatch, natstail and the MCP server, wherever they run), and they deploy
// separately, so a deployed subject this binary does not know may be one another live deployment
// still needs; start-up keeps it and names it in foreign. Two kinds of deployed subject go:
//   - one that captures the role lanes, which travel over core NATS and must never be retained
//     (migrateRoleLanesOffStream);
//   - one that overlaps a subject of this binary's (a widened, narrowed or split subject), because
//     JetStream refuses both in one stream and the start would fail. This binary's shape wins
//     (replaced), and the next start of a binary with the other shape puts that one back.
//
// Retiring any other subject is an operator step, taken once no deployment compiled with it can
// start again: `nats stream edit ENVOY_NOTIFICATIONS --subjects=... -f`.
type streamReconciliation struct {
	subjects []string
	replaced []subjectReplacement
	foreign  []string
}

// subjectReplacement is a deployed subject a start dropped and every one of its own subjects that
// overlapped it.
type subjectReplacement struct {
	dropped string
	kept    []string
}

func reconcileSubjects(deployed, own []string) streamReconciliation {
	result := streamReconciliation{subjects: make([]string, 0, len(deployed)+len(own))}
	for _, subject := range deployed {
		if subjectCapturesRoleLanes(subject) {
			continue
		}
		if !slices.Contains(own, subject) {
			var overlapping []string
			for _, ownSubject := range own {
				if subjectsOverlap(subject, ownSubject) {
					overlapping = append(overlapping, ownSubject)
				}
			}
			if len(overlapping) > 0 {
				result.replaced = append(result.replaced, subjectReplacement{dropped: subject, kept: overlapping})
				continue
			}
			result.foreign = append(result.foreign, subject)
		}
		result.subjects = append(result.subjects, subject)
	}
	for _, subject := range own {
		if !slices.Contains(result.subjects, subject) {
			result.subjects = append(result.subjects, subject)
		}
	}
	return result
}

// log reports the reconciliation once the stream carries it.
func (r streamReconciliation) log() {
	for _, replacement := range r.replaced {
		slog.Warn("envoy nats stream subject replaced by an overlapping one", slog.String("dropped", replacement.dropped), slog.Any("kept", replacement.kept))
	}
	if len(r.foreign) > 0 {
		slog.Info("envoy nats stream keeps subjects this binary does not compile", slog.Any("subjects", r.foreign))
	}
}

func ensureStreamWithConfig(js nats.JetStreamContext, cfg *nats.StreamConfig) error {
	info, err := js.StreamInfo(Stream)
	if err == nil {
		reconciliation := reconcileSubjects(info.Config.Subjects, cfg.Subjects)
		desired := *cfg
		desired.Subjects = reconciliation.subjects
		if info.Config.MaxAge == desired.MaxAge &&
			info.Config.Duplicates == desired.Duplicates &&
			slices.Equal(info.Config.Subjects, desired.Subjects) {
			reconciliation.log()
			return nil
		}
		migratingRoleLanes := streamCapturesRoleLanes(info.Config.Subjects) && !streamCapturesRoleLanes(desired.Subjects)
		if err := migrateRoleLanesOffStream(js, &info.Config, &desired); err != nil {
			return err
		}
		if _, err = js.UpdateStream(&desired); err != nil {
			return err
		}
		reconciliation.log()
		if migratingRoleLanes {
			return purgeLegacyRoleMessages(js)
		}
		return nil
	}
	if !errors.Is(err, nats.ErrStreamNotFound) {
		return err
	}
	_, err = js.AddStream(cfg)
	return err
}
