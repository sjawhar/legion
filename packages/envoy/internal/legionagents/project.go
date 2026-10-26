package legionagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// Project is a Legion project's answering record, the key project.<legion_project>: whether the
// project's daemon answers for its stopped agents, and when it last said so (RFC 3339 UTC). The
// daemon writes it every minute while its loop is configured (PUT /v1/legion/projects/<project>),
// and the listener hands the project's messages to the daemon only while it is fresh
// (cmd/listener, legion_handoff.go). No agent read reaches it: those read a session key or an issue
// or role prefix.
type Project struct {
	Answering bool   `json:"answering"`
	UpdatedAt string `json:"updated_at"`
}

func projectKey(project string) string { return "project." + project }

// ValidateProject refuses a name that is not a Legion project token (claim.ProjectToken): lower-case
// letters and digits, at most 64 bytes.
func ValidateProject(project string) error {
	if len(project) > maxProjectBytes {
		return invalid("legion_project is %d bytes, past %d", len(project), maxProjectBytes)
	}
	if !projectTokenPattern.MatchString(project) {
		return invalid("legion_project %q is not a Legion project token (lower-case letters and digits)", project)
	}
	return nil
}

// PutProject records project's answering record and answers it as stored. A project token or an
// updated_at that is not an RFC 3339 UTC time is refused with ErrInvalid and nothing written.
func (s *Store) PutProject(ctx context.Context, project string, record Project) (Project, error) {
	if err := ValidateProject(project); err != nil {
		return Project{}, err
	}
	if !isUTCTimestamp(record.UpdatedAt) {
		return Project{}, invalid("updated_at %q is not an RFC 3339 UTC time (2026-10-26T03:15:00Z)", record.UpdatedAt)
	}
	value, err := json.Marshal(record)
	if err != nil {
		return Project{}, fmt.Errorf("encode legion project %s: %w", project, err)
	}
	if err := ctx.Err(); err != nil {
		return Project{}, fmt.Errorf("write legion project %s: %w", project, err)
	}
	if _, err := s.bucket().Put(projectKey(project), value); err != nil {
		return Project{}, fmt.Errorf("write legion project key %s: %w", projectKey(project), err)
	}
	return record, nil
}

// Project reads project's answering record; found is false when the bucket holds none.
func (s *Store) Project(ctx context.Context, project string) (record Project, found bool, err error) {
	if err := ValidateProject(project); err != nil {
		return Project{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Project{}, false, err
	}
	stored, err := s.bucket().Get(projectKey(project))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, fmt.Errorf("read legion project %s: %w", project, err)
	}
	if err := json.Unmarshal(stored.Value(), &record); err != nil {
		return Project{}, false, fmt.Errorf("decode legion project %s: %w", project, err)
	}
	return record, true, nil
}
