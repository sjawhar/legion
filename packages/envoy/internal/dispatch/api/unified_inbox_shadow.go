package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type inboxShadowUser struct {
	Person       string         `json:"person"`
	CheckedItems int            `json:"checked_items"`
	Mismatched   int            `json:"mismatched"`
	Errors       int            `json:"errors"`
	Missing      map[string]int `json:"missing"`
	Unexpected   map[string]int `json:"unexpected"`
	Expansions   map[string]int `json:"expansions"`
}

type inboxShadowReport struct {
	CheckedAt        *time.Time        `json:"checked_at"`
	Horizon          int64             `json:"horizon"`
	ActiveUsers      int               `json:"active_users"`
	CheckedUsers     int               `json:"checked_users"`
	UncoveredUsers   int               `json:"uncovered_users"`
	Mismatched       int               `json:"mismatched"`
	DailyMismatched  int               `json:"daily_mismatched"`
	Errors           int               `json:"errors"`
	Missing          map[string]int    `json:"missing"`
	Unexpected       map[string]int    `json:"unexpected"`
	Expansions       map[string]int    `json:"expansions"`
	Users            []inboxShadowUser `json:"users"`
	Ready            bool              `json:"ready"`
	CompleteZeroDays int               `json:"complete_zero_days"`
}

type inboxMembership map[[2]string]struct{}

func readInboxMembership(ctx context.Context, q queryer, sql string, actorID string) (inboxMembership, error) {
	rows, err := q.Query(ctx, sql, unreadDirectRepliesArgs(actorID, nil)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := inboxMembership{}
	for rows.Next() {
		var cause, id string
		if err := rows.Scan(&cause, &id); err != nil {
			return nil, err
		}
		items[[2]string{cause, id}] = struct{}{}
	}
	return items, rows.Err()
}

func compareInboxMembership(person string, want, got inboxMembership) inboxShadowUser {
	user := inboxShadowUser{Person: person, CheckedItems: len(want), Missing: map[string]int{}, Unexpected: map[string]int{}, Expansions: map[string]int{"mention": 0}}
	for item := range want {
		if _, ok := got[item]; !ok {
			user.Missing[item[0]]++
			user.Mismatched++
		}
	}
	for item := range got {
		if _, ok := want[item]; !ok {
			user.Unexpected[item[0]]++
			user.Mismatched++
		}
	}
	return user
}

func addInboxCounts(into, from map[string]int) {
	for cause, n := range from {
		into[cause] += n
	}
}

// observeInboxShadow is the only writer of observation tables. A row lock, not
// another lease connection, serializes the scheduled pass and the visible read.
// Compare both implementations in one snapshot, including every signed-in person
// (a superset of active users) and every raw Clear identity that belongs to them.
func observeInboxShadow(ctx context.Context, pool *store.Pool, safe int64) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked bool
	err = tx.QueryRow(ctx, `select id from inbox_shadow_control for update skip locked`).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select p.email, array(select distinct raw from (
   select p.email as raw union all select login from user_agent_state where user_threads_login(login)=p.email
 ) identities order by raw) from people p order by p.email`)
	if err != nil {
		return err
	}
	type person struct {
		login   string
		aliases []string
	}
	people := []person{}
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.login, &p.aliases); err != nil {
			rows.Close()
			return err
		}
		people = append(people, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var now time.Time
	if err := tx.QueryRow(ctx, `select statement_timestamp()`).Scan(&now); err != nil {
		return err
	}
	for _, p := range people {
		if _, err := tx.Exec(ctx, `savepoint inbox_person`); err != nil {
			return err
		}
		user := compareInboxMembership(p.login, inboxMembership{}, inboxMembership{})
		for _, raw := range p.aliases {
			want, readErr := readInboxMembership(ctx, tx, legacyInboxMembershipQuery, raw)
			var got inboxMembership
			if readErr == nil {
				got, readErr = readInboxMembership(ctx, tx, unifiedInboxCandidates+` select cause,id::text from delivered`, raw)
			}
			if readErr != nil {
				if _, err := tx.Exec(ctx, `rollback to savepoint inbox_person`); err != nil {
					return err
				}
				user.Errors = 1
				break
			}
			compared := compareInboxMembership(p.login, want, got)
			user.CheckedItems += compared.CheckedItems
			user.Mismatched += compared.Mismatched
			addInboxCounts(user.Missing, compared.Missing)
			addInboxCounts(user.Unexpected, compared.Unexpected)
		}
		if user.Errors == 0 {
			// The optional expansion is counted independently and cannot make the legacy
			// diff red. Only public issue/document content is eligible for this arm.
			var count int
			err := tx.QueryRow(ctx, unifiedInboxCandidates+` select count(*) from (
    select distinct g.from_kind,g.from_id from `+refs.AllEdgesSQL+` g
    where g.kind='mentions' and g.to_kind='person' and g.to_id=$1
      and g.from_kind in ('comment','message','block')
      and (g.from_kind<>'message' or exists(select 1 from messages m where m.id::text=g.from_id and m.issue_key is not null))
      and not exists(select 1 from delivered d where d.kind=g.from_kind and d.id::text=g.from_id)
   ) expansions`, p.login, p.login, nil).Scan(&count)
			if err != nil {
				if _, rollbackErr := tx.Exec(ctx, `rollback to savepoint inbox_person`); rollbackErr != nil {
					return rollbackErr
				}
				user.Errors = 1
			} else {
				user.Expansions["mention"] = count
			}
		}
		if err := saveInboxShadow(ctx, tx, now, safe, user); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func saveInboxShadow(ctx context.Context, tx pgx.Tx, now time.Time, safe int64, user inboxShadowUser) error {
	missing, _ := json.Marshal(user.Missing)
	unexpected, _ := json.Marshal(user.Unexpected)
	expansions, _ := json.Marshal(user.Expansions)
	if _, err := tx.Exec(ctx, `insert into inbox_shadow_latest
 (person,checked_at,horizon,checked_items,mismatched,errors,missing,unexpected,expansions)
 values($1,$2,$3,$4,$5,$6,$7,$8,$9) on conflict(person) do update set
 checked_at=excluded.checked_at,horizon=excluded.horizon,checked_items=excluded.checked_items,
 mismatched=excluded.mismatched,errors=excluded.errors,missing=excluded.missing,unexpected=excluded.unexpected,expansions=excluded.expansions`,
		user.Person, now, safe, user.CheckedItems, user.Mismatched, user.Errors, missing, unexpected, expansions); err != nil {
		return err
	}
	// Accumulate cause counters without overwriting an earlier bad sample.
	_, err := tx.Exec(ctx, `insert into inbox_shadow_days
 (day,person,first_checked,last_checked,mismatched,errors,missing,unexpected,expansions)
 values(($2::timestamptz at time zone 'UTC')::date,$1,$2,$2,$3,$4,$5,$6,$7)
 on conflict(day,person) do update set
 max_gap_seconds=greatest(inbox_shadow_days.max_gap_seconds,extract(epoch from excluded.last_checked-inbox_shadow_days.last_checked)),
 last_checked=excluded.last_checked,samples=inbox_shadow_days.samples+1,
 mismatched=inbox_shadow_days.mismatched+excluded.mismatched,errors=inbox_shadow_days.errors+excluded.errors,
 missing=(select coalesce(jsonb_object_agg(k,n),'{}') from (select key k,sum(value::bigint) n from jsonb_each_text(inbox_shadow_days.missing || '{}'::jsonb) group by key) counts)
   || (select coalesce(jsonb_object_agg(key,coalesce((inbox_shadow_days.missing->>key)::bigint,0)+value::bigint),'{}') from jsonb_each_text(excluded.missing)),
 unexpected=inbox_shadow_days.unexpected || (select coalesce(jsonb_object_agg(key,coalesce((inbox_shadow_days.unexpected->>key)::bigint,0)+value::bigint),'{}') from jsonb_each_text(excluded.unexpected)),
 expansions=excluded.expansions`, user.Person, now, user.Mismatched, user.Errors, missing, unexpected, expansions)
	return err
}

func readInboxShadowReport(ctx context.Context, q queryer) (inboxShadowReport, error) {
	report := inboxShadowReport{Missing: map[string]int{}, Unexpected: map[string]int{}, Expansions: map[string]int{}, Users: []inboxShadowUser{}}
	rows, err := q.Query(ctx, `select p.email,l.checked_at,l.horizon,l.checked_items,l.mismatched,l.errors,l.missing,l.unexpected,l.expansions,
 coalesce(l.checked_at >= statement_timestamp()-interval '2 minutes',false)
 from people p left join inbox_shadow_latest l on l.person=p.email order by p.email`)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var user inboxShadowUser
		var at *time.Time
		var safe, items, mismatch, failures *int64
		var missing, unexpected, expansions []byte
		var fresh bool
		if err := rows.Scan(&user.Person, &at, &safe, &items, &mismatch, &failures, &missing, &unexpected, &expansions, &fresh); err != nil {
			rows.Close()
			return report, err
		}
		report.ActiveUsers++
		if !fresh {
			report.UncoveredUsers++
		}
		if at == nil {
			continue
		}
		report.CheckedUsers++
		if report.CheckedAt == nil || at.Before(*report.CheckedAt) {
			report.CheckedAt = at
		}
		if report.Horizon == 0 || *safe < report.Horizon {
			report.Horizon = *safe
		}
		user.CheckedItems = int(*items)
		user.Mismatched = int(*mismatch)
		user.Errors = int(*failures)
		if err := json.Unmarshal(missing, &user.Missing); err != nil {
			rows.Close()
			return report, err
		}
		if err := json.Unmarshal(unexpected, &user.Unexpected); err != nil {
			rows.Close()
			return report, err
		}
		if err := json.Unmarshal(expansions, &user.Expansions); err != nil {
			rows.Close()
			return report, err
		}
		report.Mismatched += user.Mismatched
		report.Errors += user.Errors
		addInboxCounts(report.Missing, user.Missing)
		addInboxCounts(report.Unexpected, user.Unexpected)
		addInboxCounts(report.Expansions, user.Expansions)
		report.Users = append(report.Users, user)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	if err := q.QueryRow(ctx, `select coalesce(sum(mismatched),0) from inbox_shadow_days where day=(statement_timestamp() at time zone 'UTC')::date`).Scan(&report.DailyMismatched); err != nil {
		return report, err
	}
	// A day counts only with full-day coverage, no two-minute gaps, no errors,
	// and no mismatch. Missing observations, new users and restarts fail closed.
	if err := q.QueryRow(ctx, `with days as (
 select ((statement_timestamp() at time zone 'UTC')::date-n)::date as day from generate_series(1,3) n
 ) select count(*) from days d where exists(select 1 from people) and not exists(
 select 1 from people p left join inbox_shadow_days s on s.person=p.email and s.day=d.day
 where s.person is null or s.mismatched<>0 or s.errors<>0 or s.max_gap_seconds>120 or s.samples<720
 or s.first_checked>(d.day::timestamp at time zone 'UTC')+interval '2 minutes'
 or s.last_checked<(d.day::timestamp at time zone 'UTC')+interval '1 day'-interval '2 minutes'
 )`).Scan(&report.CompleteZeroDays); err != nil {
		return report, err
	}
	report.Ready = report.CompleteZeroDays == 3 && report.UncoveredUsers == 0 && report.Mismatched == 0 && report.Errors == 0 && report.DailyMismatched == 0
	return report, nil
}

func (s *server) getInboxShadow(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOwnInbox(w, r); !ok {
		return
	}
	safe, err := s.horizon().Safe(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := observeInboxShadow(r.Context(), s.deps.Store.Pool, safe); err != nil {
		s.writeHandlerError(w, fmt.Errorf("observe inbox shadow: %w", err))
		return
	}
	report, err := readInboxShadowReport(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, 200, report)
}

// RunInboxShadowSweep checks all signed-in people once a minute, even when
// nobody opens the comparison page. Observation counts survive process restarts.
func RunInboxShadowSweep(ctx context.Context, pool *store.Pool, horizon *events.Horizon) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		pass, cancel := context.WithTimeout(ctx, 30*time.Second)
		safe, err := horizon.Safe(pass)
		if err == nil {
			err = observeInboxShadow(pass, pool, safe)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("dispatch: inbox shadow check failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
