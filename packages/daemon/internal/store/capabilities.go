package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// capabilityReportDocument is the claim_capabilities.report column: what a session measured beyond
// the columns the daemon reads by name. Its shape is the store's, not the wire's, so a change to
// what the plugin sends never rewrites what an earlier daemon kept.
type capabilityReportDocument struct {
	ElapsedMs int                     `json:"elapsedMs"`
	Rows      []capabilityRowDocument `json:"rows"`
}

// capabilityRowDocument is one row of capabilityReportDocument.
type capabilityRowDocument struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// PutCapabilityReport keeps report as its claim's latest, replacing the one the claim had: a claim
// has at most one, since a relaunched claim reports anew and the earlier report said nothing of the
// process now running. A report of a claim the store does not hold is refused by the foreign key.
func (s *Store) PutCapabilityReport(ctx context.Context, report capabilities.Report) error {
	return putCapabilityReport(ctx, s.pool, report)
}

// putCapabilityReport writes a report through whichever executor the caller has.
func putCapabilityReport(ctx context.Context, db execer, report capabilities.Report) error {
	if report.Generation > math.MaxInt64 {
		return fmt.Errorf("put capability report of %s: generation %d does not fit a bigint", report.Claim, report.Generation)
	}
	locator, err := json.Marshal(report.Locator)
	if err != nil {
		return fmt.Errorf("put capability report of %s: encode its locator: %w", report.Claim, err)
	}
	document := capabilityReportDocument{ElapsedMs: report.ElapsedMs, Rows: make([]capabilityRowDocument, len(report.Rows))}
	for i, row := range report.Rows {
		document.Rows[i] = capabilityRowDocument{Name: string(row.Name), OK: row.OK, Detail: row.Detail}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("put capability report of %s: encode its rows: %w", report.Claim, err)
	}
	_, err = db.Exec(ctx, `insert into claim_capabilities (claim_token, generation, incarnation, locator, measured_at, reported_at, report)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (claim_token) do update set generation = excluded.generation, incarnation = excluded.incarnation,
		locator = excluded.locator, measured_at = excluded.measured_at, reported_at = excluded.reported_at,
		report = excluded.report`,
		string(report.Claim), int64(report.Generation), report.Locator.Incarnation, locator,
		report.MeasuredAt, report.ReportedAt, encoded,
	)
	if err != nil {
		return fmt.Errorf("put capability report of %s: %w", report.Claim, err)
	}
	return nil
}

// CapabilityReports reads every claim's latest report, in claim token order. A stored locator that
// names a process and does not validate refuses the whole read, naming the claim, as Claims does:
// a daemon that loaded it would fence live sessions against a process it has no way to recognise.
// A locator naming the claim alone — what a ready records for a claim whose machine holds no
// process — names none, and is read as it is.
func (s *Store) CapabilityReports(ctx context.Context) ([]capabilities.Report, error) {
	rows, err := s.pool.Query(ctx, `select claim_token, generation, locator, measured_at, reported_at, report
		from claim_capabilities order by claim_token`)
	if err != nil {
		return nil, fmt.Errorf("read capability reports: %w", err)
	}
	defer rows.Close()
	var reports []capabilities.Report
	for rows.Next() {
		var (
			report     capabilities.Report
			token      string
			generation int64
			locator    []byte
			document   []byte
		)
		if err := rows.Scan(&token, &generation, &locator, &report.MeasuredAt, &report.ReportedAt, &document); err != nil {
			return nil, fmt.Errorf("read capability report: %w", err)
		}
		report.Claim, report.Generation = claim.Token(token), uint64(generation)
		if err := json.Unmarshal(locator, &report.Locator); err != nil {
			return nil, fmt.Errorf("read capability report of %s: decode its locator: %w", token, err)
		}
		if report.Locator != (runtime.Locator{Claim: report.Locator.Claim}) {
			if err := report.Locator.Validate(); err != nil {
				return nil, fmt.Errorf("read capability report of %s: %w", token, err)
			}
		}
		var decoded capabilityReportDocument
		if err := json.Unmarshal(document, &decoded); err != nil {
			return nil, fmt.Errorf("read capability report of %s: decode its rows: %w", token, err)
		}
		report.ElapsedMs = decoded.ElapsedMs
		report.Rows = make([]capabilities.Row, len(decoded.Rows))
		for i, row := range decoded.Rows {
			report.Rows[i] = capabilities.Row{Name: capabilities.Name(row.Name), OK: row.OK, Detail: row.Detail}
		}
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read capability reports: %w", err)
	}
	return reports, nil
}
