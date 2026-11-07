package main

import (
	"math/rand/v2"
	"slices"
	"time"
)

// maxMentions is the most mention targets Dispatch takes on one comment
// (internal/dispatch/api/comment_create.go validateCommentMentions).
const maxMentions = 10

// mentionDelivery is the delivery mode of a mention that names none, the capability its target
// session must advertise (comment_create.go validateCommentMentions, envoy_resolve.go
// resolveDeliveryTarget). With -mention-frac, every session registers with it.
const mentionDelivery = "steer"

// commentWrite is whether a write kind posts a comment, the kinds -mention-frac mentions sessions
// in.
func commentWrite(kind string) bool {
	return kind == kindComment || kind == kindAnchored || kind == kindReply
}

// planMentions draws, from the write's own stream (seeded by its marker, so the event schedule is
// the same with and without mentions), whether a comment write mentions sessions, and which: k
// distinct sessions other than the writing one. Nil when it mentions none.
func planMentions(marker int64, author, sessions int, frac float64, k int) []int {
	if frac <= 0 {
		return nil
	}
	r := rand.New(rand.NewPCG(uint64(marker), 11))
	if r.Float64() >= frac {
		return nil
	}
	out := make([]int, 0, k)
	for len(out) < k {
		j := r.IntN(sessions)
		if j != author && !slices.Contains(out, j) {
			out = append(out, j)
		}
	}
	return out
}

// mentionTargets is the comment API's mentions field for the planned sessions.
func mentionTargets(sessions []int) []map[string]string {
	out := make([]map[string]string, len(sessions))
	for i, s := range sessions {
		out[i] = map[string]string{"target": "session:" + sessionID(s)}
	}
	return out
}

// mentionGroup is the comment writes of one kind of a split: offered, answered 2xx, and the write
// latency of the 2xx ones, from arrival to the end of the response.
type mentionGroup struct {
	Offered int  `json:"offered"`
	OK2xx   int  `json:"ok_2xx"`
	WriteMs stat `json:"write_ms"`
}

// mentionSplit is comment writes with mentions beside those without.
type mentionSplit struct {
	With    mentionGroup `json:"with_mentions"`
	Without mentionGroup `json:"without_mentions"`
}

// mentionSummary is the comment-create latency with and without mentions, over the window and in
// the minute after the restart.
type mentionSummary struct {
	Frac       float64      `json:"mention_frac"`
	PerComment int          `json:"mentions_per_comment"`
	Window     mentionSplit `json:"window"`
	// AfterRestart covers the comment writes sent from the restart's start to 60 s after it; nil
	// with no restart.
	AfterRestart *mentionSplit `json:"after_restart_60s"`
}

// summarizeMentions splits the comment writes by whether they mention sessions: those scheduled in
// the window, and those sent in the minute from restartAt (none when it is zero).
func summarizeMentions(writes []writeRec, frac float64, k int, restartAt time.Time) *mentionSummary {
	out := &mentionSummary{Frac: frac, PerComment: k}
	type split struct{ with, without []float64 }
	var window, after split
	add := func(s *mentionSplit, vals *split, w writeRec) {
		g, ms := &s.Without, &vals.without
		if w.Mentions > 0 {
			g, ms = &s.With, &vals.with
		}
		g.Offered++
		if w.TransportErr == "" && w.Status/100 == 2 {
			g.OK2xx++
			*ms = append(*ms, float64(w.Response-w.Arrival)/1e6)
		}
	}
	if !restartAt.IsZero() {
		out.AfterRestart = &mentionSplit{}
	}
	for _, w := range writes {
		if !commentWrite(w.Kind) {
			continue
		}
		if w.InWindow {
			add(&out.Window, &window, w)
		}
		if sent := time.Unix(0, w.Sent); out.AfterRestart != nil && !sent.Before(restartAt) && sent.Before(restartAt.Add(afterRestart)) {
			add(out.AfterRestart, &after, w)
		}
	}
	out.Window.With.WriteMs, out.Window.Without.WriteMs = mkStat(window.with), mkStat(window.without)
	if out.AfterRestart != nil {
		out.AfterRestart.With.WriteMs, out.AfterRestart.Without.WriteMs = mkStat(after.with), mkStat(after.without)
	}
	return out
}
