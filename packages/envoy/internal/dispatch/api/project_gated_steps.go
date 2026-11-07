package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A project's gated steps are the design-gate steps the Legion deployment that serves it gates,
// in the order it gates them (model.Project.GatedSteps). Each step names one document of a root
// issue a person must approve before that tree's work runs, so the row tells a person reading
// Dispatch what the deployment holds work for, without reading its configuration.
//
// The write belongs to the deployment's daemon and to no person, which is why the route declares
// no personal scope: the daemon presents the credential the deployment configured, as it does on
// PATCH /api/v1/issues/{key}, and such a credential holds no scope at all. The list is the
// daemon's own configuration published back, not a choice a person makes here, so nothing is
// lost by letting a machine write it; a person who sets a deployment's gate sets it in that
// deployment's configuration.

// projectGatedSteps is the stored row the route answers: the project and the steps it gates.
type projectGatedSteps struct {
	Project string   `json:"project"`
	Steps   []string `json:"steps"`
}

// putProjectGatedSteps publishes the design-gate steps of a project's deployment. The daemon
// republishes at every boot, so the update names `gated_steps is distinct from $2`: a boot on an
// unchanged list touches no row and appends no event, which is what keeps a restarting daemon
// out of the project's event log. A body that names no list is refused (400 INVALID_PROJECT)
// rather than read as the empty list, so a malformed publish cannot clear a gate, and a list
// naming anything but the three steps is refused there too, before the database's own check
// would raise. A step named twice is refused as well, as the deployment's own loader refuses it:
// the deployment keeps one gate record per step, so a second naming is either a mistake or an
// order it cannot keep, and the database's check admits it (it holds the shape, not the route's
// input rules).

// projectGatedStepsInput is the body PUT /api/v1/projects/{key}/gated-steps takes. Steps is
// required: a body without it is refused (400 INVALID_PROJECT, putProjectGatedSteps below)
// rather than read as the empty list, so a malformed publish cannot clear a gate. It is a
// pointer so that an absent list and an explicit `[]` are told apart, and the enum is the three
// steps model.GatedStepsKnown names, which the handler refuses anything outside.
type projectGatedStepsInput struct {
	Steps *[]string    `json:"steps" required:"true" enum:"decisions,criteria,spec"`
	Actor *model.Actor `json:"actor"`
}

func (s *server) putProjectGatedSteps(w http.ResponseWriter, r *http.Request) {
	var input projectGatedStepsInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if input.Steps == nil {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, fmt.Sprintf(
			"steps is required: the design-gate steps this deployment gates (%s), in the order it gates them, or [] for a deployment that gates none",
			strings.Join(model.GatedStepsKnown, ", ")))
		return
	}
	// A nil list and an empty one are the same publish, and the column is not null: the empty
	// array, which reads as a deployment that gates nothing.
	steps := *input.Steps
	if steps == nil {
		steps = []string{}
	}
	for index, step := range steps {
		if !slices.Contains(model.GatedStepsKnown, step) {
			writeError(w, "INVALID_PROJECT", http.StatusBadRequest, fmt.Sprintf(
				"%q is not a design-gate step: steps names %s", step, strings.Join(model.GatedStepsKnown, ", ")))
			return
		}
		if slices.Contains(steps[:index], step) {
			writeError(w, "INVALID_PROJECT", http.StatusBadRequest, fmt.Sprintf(
				"%q is named twice: steps names each design-gate step once, in the order this deployment gates them", step))
			return
		}
	}

	project, err := s.loadProject(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err := tx.QueryRow(r.Context(), `
		update projects set gated_steps = $2
		where key = $1 and gated_steps is distinct from $2
		returning gated_steps
	`, project.Key, steps).Scan(&project.GatedSteps); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The deployment published the list the row already holds. Nothing changed, so
			// nothing is written and no event is appended; the answer is still the stored row.
			WriteJSON(w, http.StatusOK, projectGatedSteps{Project: project.Key, Steps: steps})
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(project.Key).event("project.updated", actor, project))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, projectGatedSteps{Project: project.Key, Steps: project.GatedSteps})
}
