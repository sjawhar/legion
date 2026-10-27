package store

import (
	"fmt"
	"testing"
)

// benchmarkRegistry is a registry whose cache holds sessions sessions of machine "bench", each
// following what a working session follows: its own agent subject, one Dispatch issue (twenty
// sessions share each), one pull request of one of twenty repositories, and one Slack thread.
func benchmarkRegistry(sessions int) *Registry {
	r := &Registry{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range sessions {
		sessionID := fmt.Sprintf("ses_bench_%05d", i)
		r.cacheInterestLocked(sessionID, Interest{
			SessionID: sessionID,
			MachineID: "bench",
			Topics: []string{
				"notifications.agent." + sessionID,
				fmt.Sprintf("notifications.dispatch.issue.PROJ-%d.>", i/20),
				fmt.Sprintf("notifications.github.sjawhar.repo%d.pr.%d.>", i%20, i),
				fmt.Sprintf("notifications.slack.T1.C%d.thread.%d.>", i%10, i),
			},
		}, 1)
	}
	return r
}

// benchmarkSubjects are the subjects a durable sees: an issue comment twenty sessions follow, a pull
// request review one session follows, a session's inbox, and three nobody follows (another issue,
// another repository's pull request, an exception lane).
func benchmarkSubjects(sessions int) []string {
	return []string{
		fmt.Sprintf("notifications.dispatch.issue.PROJ-%d.comment", sessions/40),
		fmt.Sprintf("notifications.github.sjawhar.repo%d.pr.%d.review", (sessions/2)%20, sessions/2),
		fmt.Sprintf("notifications.agent.ses_bench_%05d", sessions/3),
		"notifications.dispatch.issue.OTHER-1.comment",
		"notifications.github.someone.elsewhere.pr.7.review",
		"notifications.envoy.exceptions.notifications.agent.ses_gone",
	}
}

func BenchmarkMatch(b *testing.B) {
	for _, sessions := range []int{400, 1000, 3000} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			r := benchmarkRegistry(sessions)
			subjects := benchmarkSubjects(sessions)
			matched := 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				matched += len(r.Match("bench", subjects[i%len(subjects)]))
			}
			if matched == 0 {
				b.Fatal("no subject matched any session")
			}
		})
	}
}
