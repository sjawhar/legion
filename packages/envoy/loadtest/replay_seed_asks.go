package main

// loadgen replay-seed, step 4 and step 5: an issue's asks, then its fields and claim. The seeder
// (replay_seed.go) drives these; they are here only to keep that file readable.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func (s *replaySeeder) ask(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk, thread []commentStep, orphans *[]localComment) error {
	local, err := s.ensureAsk(ctx, key, docs, a)
	if err != nil {
		return err
	}
	before, after := splitAtAnswer(thread, a.Answer)
	for _, st := range before {
		if err := s.comment(ctx, key, docs, st, orphans); err != nil {
			return err
		}
	}
	if a.State == "answered" {
		if err := s.answer(ctx, key, docs, a, local); err != nil {
			return err
		}
	}
	for _, st := range after {
		if err := s.comment(ctx, key, docs, st, orphans); err != nil {
			return err
		}
	}
	return nil
}

func (s *replaySeeder) ensureAsk(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk) (string, error) {
	if local, ok := s.ids.get(idAsk, a.ID); ok {
		return local, nil
	}
	adopt := func(query string, args ...any) (string, error) { return s.adoptUnmapped(ctx, idAsk, query, args...) }
	switch {
	case a.BlockID != nil:
		// A typed ask block lives in its document's text, which the history wrote; settlement
		// indexed it.
		local, err := adopt(`select id::text from asks where issue_key = $1 and block_id = $2 order by created_at, id`, key, *a.BlockID)
		if err != nil {
			return "", err
		}
		if local == "" {
			return "", fmt.Errorf("ask %s: no ask was indexed from its typed block %s", a.ID, *a.BlockID)
		}
		return local, s.ids.put(ctx, idAsk, a.ID, local)
	case a.Approval != nil:
		d := docs[a.Approval.ArtifactID]
		if d == nil {
			return "", fmt.Errorf("ask %s asks to approve %s, which is not one of the issue's seeded documents", a.ID, a.Approval.ArtifactID)
		}
		local, err := adopt(`select id::text from asks where issue_key = $1 and kind = 'approval' and approval->>'artifact_id' = $2 order by created_at, id`, key, d.local)
		if err != nil {
			return "", err
		}
		if local == "" {
			w, body, err := approvalBody(a)
			if err != nil {
				return "", err
			}
			res, err := s.write(ctx, seedWrite{kind: "ask", issue: key, docBytes: len(d.text), docID: d.local, step: "approval-request", logged: a.ID,
				method: "POST", path: "/api/v1/artifacts/" + d.local + "/approval-requests", w: w, body: jsonBody(body)})
			if err != nil {
				return "", err
			}
			if local = idOf(res); local == "" {
				return "", fmt.Errorf("approval-request %s: no ask in the response", a.ID)
			}
		}
		return local, s.ids.put(ctx, idAsk, a.ID, local)
	}
	local, err := adopt(`select id::text from asks where issue_key = $1 and kind = 'question' and block_id is null and question = $2 order by created_at, id`, key, a.Question)
	if err != nil {
		return "", err
	}
	if local != "" {
		return local, s.ids.put(ctx, idAsk, a.ID, local)
	}
	w, body, err := questionBody(a)
	if err != nil {
		return "", err
	}
	path := "/api/v1/issues/" + key + "/asks"
	if a.Anchor == nil {
		res, err := s.write(ctx, seedWrite{kind: "ask", issue: key, step: "ask-create", logged: a.ID,
			method: "POST", path: path, w: w, body: jsonBody(body)})
		if err != nil {
			return "", err
		}
		return idOf(res), s.mapCreated(ctx, idAsk, a.ID, res)
	}
	d := docs[a.Anchor.ArtifactID]
	if d == nil {
		return "", fmt.Errorf("ask %s is anchored on %s, which is not one of the issue's seeded documents", a.ID, a.Anchor.ArtifactID)
	}
	return s.anchored(ctx, "ask", key, d, a.ID, a.Anchor, w, path, body)
}

// approvalBody is the approval request body of approval ask a, as whom.
func approvalBody(a *replayAsk) (who, map[string]any, error) {
	w, actor, err := actorOf(&a.Author)
	if err != nil {
		return w, nil, fmt.Errorf("ask %s: %w", a.ID, err)
	}
	body := map[string]any{}
	if summary := approvalSummary(a.Question); summary != "" {
		body["summary"] = summary
	}
	return w, withActor(body, actor), nil
}

// questionBody is the body that opens question ask a, as whom (anchored adds an anchor).
func questionBody(a *replayAsk) (who, map[string]any, error) {
	w, actor, err := actorOf(&a.Author)
	if err != nil {
		return w, nil, fmt.Errorf("ask %s: %w", a.ID, err)
	}
	options := a.Options
	if options == nil {
		options = []replayAskOption{}
	}
	body := withActor(map[string]any{"question": a.Question, "kind": "question", "options": options, "multiple": a.Multiple}, actor)
	if a.Urgency != "" {
		body["urgency"] = a.Urgency
	}
	return w, body, nil
}

// answerBody is the body that answers question ask a, whose edited_at is editedAt.
func answerBody(a *replayAsk, editedAt json.RawMessage) map[string]any {
	body := map[string]any{"selected": nonNil(a.Answer.Selected), "expected_edited_at": editedAt}
	if a.Answer.Text != nil {
		body["text"] = *a.Answer.Text
	}
	return body
}

// askResolveBody is the body that resolves ask a as its logged resolution says, as whom.
func askResolveBody(a *replayAsk) (who, map[string]any, error) {
	w, actor, err := actorOf(&a.Resolution.By)
	if err != nil {
		return w, nil, fmt.Errorf("ask-resolve %s: %w", a.ID, err)
	}
	return w, withActor(map[string]any{"kind": a.Resolution.Kind, "reason": a.Resolution.Reason}, actor), nil
}

// answer re-sends an answered ask's answer as the answering user: an approval through a review, a
// question (a typed ask block's too) through POST /asks/{id}/answer. A typed ask block's answer
// rewrites its block, which Dispatch versions once settled; history waits for that version.
func (s *replaySeeder) answer(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk, local string) error {
	if _, ok := s.ids.get(idAnswer, a.ID); ok {
		return nil
	}
	if a.Answer == nil {
		return fmt.Errorf("ask %s is answered with no answer in the bundle", a.ID)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `select state from asks where id = $1`, local).Scan(&state); err != nil {
		return err
	}
	if state == "open" {
		w := who{person: a.Answer.User}
		if a.Approval != nil {
			body, err := reviewFor(a.Answer)
			if err != nil {
				return fmt.Errorf("ask %s: %w", a.ID, err)
			}
			d := docs[a.Approval.ArtifactID]
			if _, err := s.write(ctx, seedWrite{kind: "answer", issue: key, docBytes: len(d.text), docID: d.local, step: "review", logged: a.ID,
				method: "POST", path: "/api/v1/artifacts/" + d.local + "/reviews", w: w, body: jsonBody(body)}); err != nil {
				return err
			}
			return s.ids.put(ctx, idAnswer, a.ID, local)
		}
		var block *issueDoc
		if a.BlockID != nil {
			if a.BlockArtifactID == nil || docs[*a.BlockArtifactID] == nil {
				return fmt.Errorf("ask %s: an answered typed ask block names no seeded document", a.ID)
			}
			block = docs[*a.BlockArtifactID]
		}
		editedAt, err := s.askEditedAt(ctx, a.ID, local)
		if err != nil {
			return err
		}
		body := answerBody(a, editedAt)
		docBytes, docID := 0, ""
		if block != nil {
			docBytes, docID = len(block.text), block.local
		}
		if _, err := s.write(ctx, seedWrite{kind: "answer", issue: key, docBytes: docBytes, docID: docID, step: "answer", logged: a.ID,
			method: "POST", path: "/api/v1/asks/" + local + "/answer", w: w, body: jsonBody(body)}); err != nil {
			return err
		}
	}
	return s.ids.put(ctx, idAnswer, a.ID, local)
}

// resolveAsk resolves a resolved typed ask block as its resolving actor through POST
// /asks/{id}/resolve with the logged resolution's kind and reason. Dispatch writes state="resolved"
// into the block, which it versions once settled; history waits for that version.
func (s *replaySeeder) resolveAsk(ctx context.Context, key string, docs map[string]*issueDoc, a *replayAsk, local string) error {
	if _, ok := s.ids.get(idAskResolve, a.ID); ok {
		return nil
	}
	if a.Resolution == nil {
		return fmt.Errorf("ask %s is resolved with no resolution in the bundle", a.ID)
	}
	if a.BlockArtifactID == nil || docs[*a.BlockArtifactID] == nil {
		return fmt.Errorf("ask %s: a resolved typed ask block names no seeded document", a.ID)
	}
	block := docs[*a.BlockArtifactID]
	var state string
	if err := s.pool.QueryRow(ctx, `select state from asks where id = $1`, local).Scan(&state); err != nil {
		return err
	}
	if state != "resolved" {
		w, body, err := askResolveBody(a)
		if err != nil {
			return err
		}
		if _, err := s.write(ctx, seedWrite{kind: "resolve", issue: key, docBytes: len(block.text), docID: block.local, step: "ask-resolve", logged: a.ID,
			method: "POST", path: "/api/v1/asks/" + local + "/resolve", w: w, body: jsonBody(body)}); err != nil {
			return err
		}
	}
	return s.ids.put(ctx, idAskResolve, a.ID, local)
}

// waitVersion waits (up to two minutes) until the document owes no settlement and stands at
// version want; standing past it is an error at once.
func (s *replaySeeder) waitVersion(ctx context.Context, d *issueDoc, want int) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var pending, got int
		if err := s.pool.QueryRow(ctx, `select (select count(*) from doc_settlements_pending where artifact_id = $1),
			(select coalesce(max(number), 0) from artifact_versions where artifact_id = $1)`, d.local).Scan(&pending, &got); err != nil {
			return err
		}
		if got > want {
			return fmt.Errorf("document %s stands at version %d, want %d", d.a.ID, got, want)
		}
		if got == want && pending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("document %s stands at version %d with %d settlements owed after 2 minutes, want version %d", d.a.ID, got, pending, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// askEditedAt is the ask's edited_at as Dispatch serves it (GET /api/v1/asks/{id}), which an
// answer must send back as expected_edited_at: a string, or null for an ask never edited.
func (s *replaySeeder) askEditedAt(ctx context.Context, logged, local string) (json.RawMessage, error) {
	res := s.api.call(ctx, "GET", "/api/v1/asks/"+local, who{}, nil)
	if !res.ok() {
		return nil, &seedError{step: "ask-read", logged: logged, status: res.status, code: errorCode(res.body)}
	}
	var r struct {
		EditedAt json.RawMessage `json:"edited_at"`
		Ask      *struct {
			EditedAt json.RawMessage `json:"edited_at"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(res.body, &r); err != nil {
		return nil, fmt.Errorf("ask-read %s: undecodable response", logged)
	}
	if r.Ask != nil && len(r.Ask.EditedAt) > 0 {
		return r.Ask.EditedAt, nil
	}
	if len(r.EditedAt) > 0 {
		return r.EditedAt, nil
	}
	return json.RawMessage("null"), nil
}

// --- step 5: fields and claim --------------------------------------------------------------------

func (s *replaySeeder) finishIssue(ctx context.Context, p seedIssuePlan) error {
	res := s.api.call(ctx, "GET", "/api/v1/issues/"+p.Key, who{}, nil)
	if !res.ok() {
		return &seedError{step: "issue-read", logged: p.Key, status: res.status, code: errorCode(res.body)}
	}
	var cur liveIssue
	if err := json.Unmarshal(res.body, &cur); err != nil {
		return fmt.Errorf("issue-read %s: undecodable response", p.Key)
	}
	if patch := issuePatch(cur, p.Issue); patch != nil {
		patch["actor"] = sessionActor(replaySeedActorID)
		if _, err := s.write(ctx, seedWrite{kind: "patch", issue: p.Key, step: "patch", logged: p.Key,
			method: "PATCH", path: "/api/v1/issues/" + p.Key, body: jsonBody(patch)}); err != nil {
			return err
		}
	}
	if claimWanted(cur, p.Issue) {
		w, actor, err := actorOf(&p.Issue.Claim.Actor)
		if err != nil {
			return fmt.Errorf("claim %s: %w", p.Key, err)
		}
		if _, err := s.write(ctx, seedWrite{kind: "claim", issue: p.Key, step: "claim", logged: p.Key,
			method: "POST", path: "/api/v1/issues/" + p.Key + "/claim", w: w, body: jsonBody(withActor(map[string]any{}, actor))}); err != nil {
			return err
		}
	}
	return nil
}
