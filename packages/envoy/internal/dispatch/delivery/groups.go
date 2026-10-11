package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// SessionGroup is one of the Delivery page's session groups (LEGION-682, migration 0093): a
// stable id, a name and the agent sessions it holds. A pull request whose first session is a
// member shows the group as its Parent agent, under ParentAgentValue, instead of the session.
// Mirrors packages/contracts/src/dispatch-api.ts's DeliverySessionGroup exactly.
type SessionGroup struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Sessions  []string    `json:"sessions"`
	CreatedBy model.Actor `json:"created_by"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedBy model.Actor `json:"updated_by"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// SessionGroupValuePrefix starts the Parent agent value of a group: `group:<id>`. The value names
// the group by its id, so a rename keeps every filter, colour choice and shared link that names
// it; the page shows the group's name.
const SessionGroupValuePrefix = "group:"

// ParentAgentValue is the group's Parent agent facet value, `group:<id>`.
func (g SessionGroup) ParentAgentValue() string {
	return SessionGroupValuePrefix + g.ID
}

// Querier is what a read in this package needs of its connection: the pool, or a transaction
// already open on it, which a group write's checks run inside (store.Pool refuses a second
// connection to a caller holding one).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrSessionGroupNotFound is a group id no group carries.
var ErrSessionGroupNotFound = errors.New("no such session group")

// ErrSessionGroupEmpty is a write that would leave a group holding no session: a group with none
// changes nothing, so it is deleted rather than kept.
var ErrSessionGroupEmpty = errors.New("a session group holds at least one session; delete the group instead")

// SessionInGroupError is a session another group already holds: a session belongs to at most one
// group, so its pull requests have one Parent agent.
type SessionInGroupError struct {
	Session   string
	GroupID   string
	GroupName string
}

func (e *SessionInGroupError) Error() string {
	return fmt.Sprintf("session %s is already in the group %q; remove it there first", e.Session, e.GroupName)
}

// SessionGroupNameTakenError is a name another group already has, compared without regard to
// case, since the page would show the two as one label.
type SessionGroupNameTakenError struct {
	Name string
}

func (e *SessionGroupNameTakenError) Error() string {
	return fmt.Sprintf("a session group named %q already exists", e.Name)
}

// SessionGroupNameIsTitleError is a group name that is the latest title of a session the group
// does not hold: the page would show that session and the group under one label.
type SessionGroupNameIsTitleError struct {
	Name    string
	Session string
}

func (e *SessionGroupNameIsTitleError) Error() string {
	return fmt.Sprintf("the group name %q is the title of session %s, which is not in the group; add that session to it or choose another name", e.Name, e.Session)
}

// sessionGroupColumns is the select list scanSessionGroup reads, over delivery_session_groups g
// left joined to its members m and grouped by g.id.
const sessionGroupColumns = `g.id::text, g.name,
	coalesce(array_agg(m.session_id order by m.session_id) filter (where m.session_id is not null), '{}'),
	g.created_by, g.created_at, g.updated_by, g.updated_at`

const sessionGroupFrom = `from delivery_session_groups g
	left join delivery_session_group_members m on m.group_id = g.id`

func scanSessionGroup(row pgx.Row) (SessionGroup, error) {
	var group SessionGroup
	var createdBy, updatedBy []byte
	if err := row.Scan(&group.ID, &group.Name, &group.Sessions, &createdBy, &group.CreatedAt, &updatedBy, &group.UpdatedAt); err != nil {
		return SessionGroup{}, err
	}
	if err := json.Unmarshal(createdBy, &group.CreatedBy); err != nil {
		return SessionGroup{}, fmt.Errorf("decode session group creator: %w", err)
	}
	if err := json.Unmarshal(updatedBy, &group.UpdatedBy); err != nil {
		return SessionGroup{}, fmt.Errorf("decode session group editor: %w", err)
	}
	return group, nil
}

// ListSessionGroups reads every session group with its sessions, by name.
func ListSessionGroups(ctx context.Context, q Querier) ([]SessionGroup, error) {
	rows, err := q.Query(ctx, `select `+sessionGroupColumns+` `+sessionGroupFrom+`
		group by g.id order by lower(g.name), g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []SessionGroup{}
	for rows.Next() {
		group, err := scanSessionGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func getSessionGroup(ctx context.Context, q Querier, id string) (SessionGroup, error) {
	group, err := scanSessionGroup(q.QueryRow(ctx, `select `+sessionGroupColumns+` `+sessionGroupFrom+`
		where g.id = $1::uuid group by g.id`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionGroup{}, ErrSessionGroupNotFound
	}
	return group, err
}

// lockSessionGroups serialises every group write. The members' primary key and the name index
// already refuse membership and name races; the lock is for the title rule, which reads other
// groups' rows, so a rename and a removal checked at once cannot together strand a titled session.
func lockSessionGroups(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('delivery-session-groups'))`); err != nil {
		return fmt.Errorf("lock session groups: %w", err)
	}
	return nil
}

// refuseHeldSessions refuses the first of sessions another group than groupID holds (any group,
// when groupID is nil).
func refuseHeldSessions(ctx context.Context, tx pgx.Tx, groupID *string, sessions []string) error {
	if len(sessions) == 0 {
		return nil
	}
	var held SessionInGroupError
	err := tx.QueryRow(ctx, `
		select m.session_id, g.id::text, g.name
		from delivery_session_group_members m
		join delivery_session_groups g on g.id = m.group_id
		where m.session_id = any($1) and g.id is distinct from $2::uuid
		order by m.session_id
		limit 1
	`, sessions, groupID).Scan(&held.Session, &held.GroupID, &held.GroupName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return &held
}

// refuseTakenName refuses a name another group than groupID has, compared without regard to case.
func refuseTakenName(ctx context.Context, tx pgx.Tx, groupID *string, name string) error {
	var taken string
	err := tx.QueryRow(ctx, `
		select name from delivery_session_groups
		where lower(name) = lower($1) and id is distinct from $2::uuid
	`, name, groupID).Scan(&taken)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return &SessionGroupNameTakenError{Name: taken}
}

// sessionsTitled lists, by id, every session a population pull request names whose latest title
// (the one ResolveSessionTitles shows) is name without regard to case and that members does not
// hold. ResolveSessionTitles is the one latest-title query; this function only scopes its ids to
// the delivery population and applies the group rule.
func sessionsTitled(ctx context.Context, tx pgx.Tx, name string, members []string) ([]string, error) {
	sessionIDs, err := ListPopulationSessionIDs(ctx, tx)
	if err != nil {
		return nil, err
	}
	titles, err := ResolveSessionTitles(ctx, tx, sessionIDs)
	if err != nil {
		return nil, err
	}
	outside := []string{}
	for session, title := range titles {
		if strings.EqualFold(title, name) && !slices.Contains(members, session) {
			outside = append(outside, session)
		}
	}
	slices.Sort(outside)
	return outside, nil
}

// refuseTitleClash holds a group's name to the title rule: a name may be a session's title only
// while the group holds every session the population names under that title, so a Parent agent
// label means one thing. A write that sets the name (a create or a rename) is refused for any
// session outside the group with that title. A write that keeps the name is refused only for a
// session it removes: a session that took the group's name as its title after the group was
// named is already outside it, and the timeline tells the two apart by showing that session's id
// beside its title (api/delivery.go), so it does not block edits that change nothing about it.
func refuseTitleClash(ctx context.Context, tx pgx.Tx, name string, members, removed []string, setsName bool) error {
	outside, err := sessionsTitled(ctx, tx, name, members)
	if err != nil {
		return err
	}
	for _, session := range outside {
		if setsName || slices.Contains(removed, session) {
			return &SessionGroupNameIsTitleError{Name: name, Session: session}
		}
	}
	return nil
}

// CreateSessionGroup writes a new group named name holding sessions, in tx, after refusing a
// session another group holds, a name another group has, and a name that is the title of a
// session outside the group. name is already trimmed and sessions de-duplicated and non-empty.
func CreateSessionGroup(ctx context.Context, tx pgx.Tx, name string, sessions []string, actor model.Actor) (SessionGroup, error) {
	if len(sessions) == 0 {
		return SessionGroup{}, ErrSessionGroupEmpty
	}
	if err := lockSessionGroups(ctx, tx); err != nil {
		return SessionGroup{}, err
	}
	if err := refuseHeldSessions(ctx, tx, nil, sessions); err != nil {
		return SessionGroup{}, err
	}
	if err := refuseTakenName(ctx, tx, nil, name); err != nil {
		return SessionGroup{}, err
	}
	if err := refuseTitleClash(ctx, tx, name, sessions, nil, true); err != nil {
		return SessionGroup{}, err
	}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return SessionGroup{}, fmt.Errorf("encode session group author: %w", err)
	}
	var id string
	if err := tx.QueryRow(ctx, `
		insert into delivery_session_groups (name, created_by, updated_by) values ($1, $2, $2)
		returning id::text
	`, name, actorJSON).Scan(&id); err != nil {
		return SessionGroup{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into delivery_session_group_members (session_id, group_id)
		select unnest($1::text[]), $2::uuid
	`, sessions, id); err != nil {
		return SessionGroup{}, err
	}
	return getSessionGroup(ctx, tx, id)
}

// SessionGroupUpdate is one edit of a group: a new name, sessions to add and sessions to remove,
// each optional. Adding a session the group holds, or removing one it does not, changes nothing.
type SessionGroupUpdate struct {
	Name   *string
	Add    []string
	Remove []string
}

// UpdateSessionGroup applies update to the group id in tx, after the same refusals as
// CreateSessionGroup for what it adds and names, and ErrSessionGroupEmpty for an edit that
// removes every session.
func UpdateSessionGroup(ctx context.Context, tx pgx.Tx, id string, update SessionGroupUpdate, actor model.Actor) (SessionGroup, error) {
	if err := lockSessionGroups(ctx, tx); err != nil {
		return SessionGroup{}, err
	}
	current, err := getSessionGroup(ctx, tx, id)
	if err != nil {
		return SessionGroup{}, err
	}
	name := current.Name
	setsName := update.Name != nil && *update.Name != current.Name
	if setsName {
		name = *update.Name
	}
	members := []string{}
	removed := []string{}
	for _, session := range current.Sessions {
		if slices.Contains(update.Remove, session) {
			removed = append(removed, session)
			continue
		}
		members = append(members, session)
	}
	added := []string{}
	for _, session := range update.Add {
		if !slices.Contains(members, session) {
			added = append(added, session)
			members = append(members, session)
		}
	}
	if len(members) == 0 {
		return SessionGroup{}, ErrSessionGroupEmpty
	}
	if err := refuseHeldSessions(ctx, tx, &id, added); err != nil {
		return SessionGroup{}, err
	}
	if setsName {
		if err := refuseTakenName(ctx, tx, &id, name); err != nil {
			return SessionGroup{}, err
		}
	}
	if err := refuseTitleClash(ctx, tx, name, members, removed, setsName); err != nil {
		return SessionGroup{}, err
	}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return SessionGroup{}, fmt.Errorf("encode session group editor: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update delivery_session_groups set name = $2, updated_by = $3, updated_at = now() where id = $1::uuid
	`, id, name, actorJSON); err != nil {
		return SessionGroup{}, err
	}
	if len(removed) > 0 {
		if _, err := tx.Exec(ctx, `
			delete from delivery_session_group_members where group_id = $1::uuid and session_id = any($2)
		`, id, removed); err != nil {
			return SessionGroup{}, err
		}
	}
	if len(added) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into delivery_session_group_members (session_id, group_id)
			select unnest($2::text[]), $1::uuid
		`, id, added); err != nil {
			return SessionGroup{}, err
		}
	}
	return getSessionGroup(ctx, tx, id)
}

// DeleteSessionGroup deletes the group id and its memberships in tx, so its sessions show as
// themselves again; ErrSessionGroupNotFound when no group has that id.
func DeleteSessionGroup(ctx context.Context, tx pgx.Tx, id string) error {
	if err := lockSessionGroups(ctx, tx); err != nil {
		return err
	}
	var deleted string
	err := tx.QueryRow(ctx, `delete from delivery_session_groups where id = $1::uuid returning id::text`, id).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSessionGroupNotFound
	}
	return err
}
