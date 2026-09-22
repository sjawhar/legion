package record

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Project joins durable issue facts to current supervision claims for GET /legion/v1/state. The
// caller owns the repeatable-read transaction, so its record reads share one snapshot; claims are
// intentionally live process facts and arrive separately from the supervisor.
func Project(ctx context.Context, tx pgx.Tx, s Store, claims []supervise.Claim) (api.State, error) {
	issues, err := s.Issues(ctx, tx)
	if err != nil {
		return api.State{}, err
	}
	knownIssues := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		knownIssues[issue.Key] = struct{}{}
	}
	slots, err := s.Slots(ctx, tx)
	if err != nil {
		return api.State{}, err
	}
	pending, err := s.PendingStatusWrites(ctx, tx)
	if err != nil {
		return api.State{}, err
	}

	claimViews := make(map[claim.Token]api.ClaimView, len(claims))
	for _, current := range claims {
		if current.Locator != nil {
			if err := current.Locator.Validate(); err != nil {
				return api.State{}, fmt.Errorf("project claim %s: %w", current.Token, err)
			}
		}
		claimViews[current.Token] = api.ClaimView{
			Session: current.Session,
			State:   string(current.State),
			Locator: current.Locator,
		}
	}

	slotViews := make(map[string]api.SlotView, len(slots))
	active := make([]string, 0, len(slots))
	for _, slot := range slots {
		if _, known := knownIssues[slot.Issue]; !known {
			continue
		}
		slotViews[slot.Issue] = api.SlotView{Index: slot.Index, AdmittedAt: slot.AdmittedAt}
		active = append(active, slot.Issue)
	}

	projected := api.State{Issues: make(map[string]api.Issue, len(issues))}
	waiting := make([]Issue, 0)
	for _, issue := range issues {
		view := api.Issue{
			Key:        issue.Key,
			Generation: issue.Generation,
			Phase:      issue.Phase,
			Status:     issue.Status,
			Workers:    map[claim.Role]api.PhaseView{},
		}
		if slot, ok := slotViews[issue.Key]; ok {
			view.Slot = &slot
		} else if issue.Status == "todo" {
			waiting = append(waiting, issue)
		}

		phases, err := s.Phases(ctx, tx, issue.Key)
		if err != nil {
			return api.State{}, err
		}
		for _, phase := range phases {
			claimView, live := claimViews[phase.Claim]
			if !live {
				// The workflow phase remains visible on the issue even after its claim is gone. A
				// PhaseView has a required claim view, so there is no valid worker member to emit.
				continue
			}
			if phase.Role == claim.RoleArchitect {
				view.Architect = &claimView
				continue
			}
			view.Workers[phase.Role] = api.PhaseView{
				Claim:         claimView,
				HandoffCommit: phase.HandoffCommit,
				Rounds:        phase.Rounds,
			}
		}

		pr, err := s.PullRequest(ctx, tx, issue.Key)
		if err != nil {
			return api.State{}, err
		}
		if pr != nil {
			view.PullRequest = &api.PullRequestView{
				Number:         pr.Number,
				Head:           pr.HeadSHA,
				ChecksVerdict:  pr.Verdict,
				ReviewDecision: pr.ReviewDecision,
				FixAttempts:    pr.FixAttempts,
			}
		}

		gate, err := s.Gate(ctx, tx, issue.Key)
		if err != nil {
			return api.State{}, err
		}
		if gate != nil {
			view.DesignGate = &api.GateView{
				ArtifactID:      gate.ArtifactID,
				CurrentVersion:  gate.LatestVersion,
				ApprovedVersion: gate.ApprovedVersion,
			}
		}
		projected.Issues[issue.Key] = view
	}

	sort.Slice(waiting, func(i, j int) bool {
		if waiting[i].LastDispatchSeq == waiting[j].LastDispatchSeq {
			return waiting[i].Key < waiting[j].Key
		}
		return waiting[i].LastDispatchSeq < waiting[j].LastDispatchSeq
	})
	projected.Admission.Active = active
	projected.Admission.Waiting = make([]string, len(waiting))
	for i, issue := range waiting {
		projected.Admission.Waiting[i] = issue.Key
	}
	projected.PendingStatusWrites = make([]api.PendingStatusWrite, 0, len(pending))
	for _, row := range pending {
		if _, known := knownIssues[row.Issue]; !known {
			continue
		}
		projected.PendingStatusWrites = append(projected.PendingStatusWrites, api.PendingStatusWrite{
			Issue:     row.Issue,
			Payload:   row.Payload,
			Attempts:  row.Attempts,
			NextAt:    row.NextAt,
			LastError: row.LastError,
		})
	}
	return projected, nil
}
