package api

import "github.com/sjawhar/legion/daemon/internal/workflow"

// testRepositories serves every repository the API tests record issues on, each at main.
var testRepositories = servedRepositories{"acme/widgets": "main", "sjawhar/legion": "main"}

type servedRepositories map[string]string

func (r servedRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if branch, ok := r[resolved]; ok {
		return workflow.Resolution{Repo: resolved, DefaultBranch: branch, GitHubRepo: resolved}, nil
	}
	return workflow.Resolution{Refusal: resolved + " is not mapped to the project in Dispatch"}, nil
}
