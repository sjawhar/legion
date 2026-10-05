package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// getDeliverySettings answers GET /api/v1/settings/delivery with the one delivery_settings row,
// or null when none has been written yet.
func (s *server) getDeliverySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	settings, err := delivery.GetSettings(r.Context(), s.deps.Store.Pool)
	if err != nil {
		if errors.Is(err, delivery.ErrNoSettings) {
			WriteJSON(w, http.StatusOK, nil)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}

// putDeliverySettings answers PUT /api/v1/settings/delivery: set (or replace) the one
// delivery_settings row after proving the GitHub App can read the deploy repository, the same
// access-check-before-write shape putArchitectureSource uses.
func (s *server) putDeliverySettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		DeployRepo           string   `json:"deploy_repo"`
		DeployWorkflowPath   string   `json:"deploy_workflow_path"`
		ProductionJobName    string   `json:"production_job_name"`
		PRChecksWorkflowPath string   `json:"pr_checks_workflow_path"`
		PopulationAuthors    []string `json:"population_authors"`
		ExcludedRepos        []string `json:"excluded_repos"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	owner, repoName, err := parseSourceRepo(input.DeployRepo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for name, value := range map[string]string{
		"deploy_workflow_path":    input.DeployWorkflowPath,
		"production_job_name":     input.ProductionJobName,
		"pr_checks_workflow_path": input.PRChecksWorkflowPath,
	} {
		if strings.TrimSpace(value) == "" {
			writeError(w, "DELIVERY_SETTINGS_INPUT", http.StatusBadRequest, name+" is required")
			return
		}
	}
	if len(input.PopulationAuthors) == 0 {
		writeError(w, "DELIVERY_SETTINGS_INPUT", http.StatusBadRequest, "population_authors must name at least one author")
		return
	}

	// The access check runs before any write: a failed re-PUT leaves the stored settings
	// untouched, matching putArchitectureSource's own ordering.
	if _, err := s.deps.GitHub.CheckSource(r.Context(), owner, repoName); err != nil {
		if errors.Is(err, githubapp.ErrNoAppKey) || errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrNoContentsRead) {
			writeError(w, "DELIVERY_SETTINGS_ACCESS", http.StatusConflict, err.Error())
			return
		}
		s.writeHandlerError(w, err)
		return
	}

	settings, err := delivery.PutSettings(r.Context(), s.deps.Store.Pool, model.DeliverySettings{
		DeployRepo: owner + "/" + repoName, DeployWorkflowPath: input.DeployWorkflowPath,
		ProductionJobName: input.ProductionJobName, PRChecksWorkflowPath: input.PRChecksWorkflowPath,
		PopulationAuthors: input.PopulationAuthors, ExcludedRepos: input.ExcludedRepos,
	}, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}
