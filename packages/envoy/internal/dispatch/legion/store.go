package legion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Settings is one project's Legion setting: the daemon Dispatch reads that project's state from.
type Settings struct {
	Project   string      `json:"project"`
	StateURL  string      `json:"state_url"`
	CreatedBy model.Actor `json:"created_by"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// settingsColumns is the legion_settings select list scanSettings reads, in scan order; every
// query that returns a setting selects exactly this.
const settingsColumns = `project, state_url, created_by, updated_at`

// ErrNoSettings is "this project names no Legion daemon": the sampler reads nothing for it and
// the operator relay has nowhere to send a change.
var ErrNoSettings = errors.New("this project has no Legion daemon configured")

// scanSettings decodes one settingsColumns row.
func scanSettings(row pgx.Row) (Settings, error) {
	var settings Settings
	var createdBy []byte
	if err := row.Scan(&settings.Project, &settings.StateURL, &createdBy, &settings.UpdatedAt); err != nil {
		return Settings{}, err
	}
	if err := json.Unmarshal(createdBy, &settings.CreatedBy); err != nil {
		return Settings{}, fmt.Errorf("decode the Legion setting's author: %w", err)
	}
	return settings, nil
}

// ListSettings is every project's Legion setting, by project key; a deployment with none reads an
// empty list.
func ListSettings(ctx context.Context, pool *store.Pool) ([]Settings, error) {
	rows, err := pool.Query(ctx, `select `+settingsColumns+` from legion_settings order by project`)
	if err != nil {
		return nil, fmt.Errorf("read the Legion settings: %w", err)
	}
	defer rows.Close()
	listed := []Settings{}
	for rows.Next() {
		settings, err := scanSettings(rows)
		if err != nil {
			return nil, err
		}
		listed = append(listed, settings)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion settings: %w", err)
	}
	return listed, nil
}

// GetSettings is one project's setting, or ErrNoSettings when it has none.
func GetSettings(ctx context.Context, pool *store.Pool, project string) (Settings, error) {
	settings, err := scanSettings(pool.QueryRow(ctx, `select `+settingsColumns+` from legion_settings where project = $1`, project))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, ErrNoSettings
		}
		return Settings{}, err
	}
	return settings, nil
}

// PutSettings sets the daemon a project is read from, and answers the row as it stands. created_by
// is the actor that first set the project's setting, as repo_projects.created_by is the actor that
// first mapped a repository: a later write changes the URL and the time and keeps it.
func PutSettings(ctx context.Context, pool *store.Pool, project, stateURL string, actor model.Actor) (Settings, error) {
	createdBy, err := json.Marshal(actor)
	if err != nil {
		return Settings{}, fmt.Errorf("encode the Legion setting's author: %w", err)
	}
	settings, err := scanSettings(pool.QueryRow(ctx, `
		insert into legion_settings (project, state_url, created_by, updated_at)
		values ($1, $2, $3, now())
		on conflict (project) do update set state_url = excluded.state_url, updated_at = now()
		returning `+settingsColumns, project, stateURL, createdBy))
	if err != nil {
		return Settings{}, fmt.Errorf("write the Legion setting of %s: %w", project, err)
	}
	return settings, nil
}

// DeleteSettings removes a project's setting and reports whether it had one. The project's reads
// and observations stay: they are what was observed, and nothing re-reads them.
func DeleteSettings(ctx context.Context, pool *store.Pool, project string) (bool, error) {
	tag, err := pool.Exec(ctx, `delete from legion_settings where project = $1`, project)
	if err != nil {
		return false, fmt.Errorf("delete the Legion setting of %s: %w", project, err)
	}
	return tag.RowsAffected() > 0, nil
}

// RecordRead appends one legion_reads row for a read of project's daemon: how many bytes it sent,
// and why it failed when it did. readErr nil is a read that succeeded.
func RecordRead(ctx context.Context, pool *store.Pool, project string, at time.Time, bytes int, readErr error) error {
	var failure *string
	if readErr != nil {
		message := readErr.Error()
		failure = &message
	}
	if _, err := pool.Exec(ctx, `
		insert into legion_reads (project, read_at, ok, error, bytes)
		values ($1, $2, $3, $4, $5)
	`, project, at, readErr == nil, failure, bytes); err != nil {
		return fmt.Errorf("record the Legion state read of %s: %w", project, err)
	}
	return nil
}

// RecordObservations appends a row for each observation whose value differs from the newest row
// that project, issue and field already hold, and answers how many it wrote. An unchanged value
// writes nothing, and a first observation whose value is null writes nothing either: nothing moved
// from nothing.
//
// One statement, so a pass of some hundreds of fields is one round trip, and Postgres's own jsonb
// comparison decides what changed: a Go encoding that ordered an object's keys differently from
// jsonb would otherwise write a row every minute for a value that never moved. A row the lease
// already let another replica write is therefore not written twice even if the leases overlapped.
func RecordObservations(ctx context.Context, pool *store.Pool, project string, at time.Time, observations []Observation) (int64, error) {
	if len(observations) == 0 {
		return 0, nil
	}
	issueKeys := make([]string, len(observations))
	fields := make([]string, len(observations))
	values := make([]string, len(observations))
	for index, observation := range observations {
		issueKeys[index], fields[index], values[index] = observation.IssueKey, observation.Field, string(observation.Value)
	}
	tag, err := pool.Exec(ctx, `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, found.issue_key, found.field, found.value::jsonb, $5::timestamptz
		from unnest($2::text[], $3::text[], $4::text[]) as found(issue_key, field, value)
		where coalesce((
			select newest.value
			from legion_observations newest
			where newest.project = $1 and newest.issue_key = found.issue_key and newest.field = found.field
			order by newest.observed_at desc
			limit 1
		), 'null'::jsonb) is distinct from found.value::jsonb
	`, project, issueKeys, fields, values, at)
	if err != nil {
		return 0, fmt.Errorf("record the Legion observations of %s: %w", project, err)
	}
	return tag.RowsAffected(), nil
}
