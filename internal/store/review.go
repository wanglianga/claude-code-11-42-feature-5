package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"cargo.example/secstate/internal/domain/review"
)

// Review persistence. Every write appends; the schema triggers reject UPDATE and
// DELETE on these tables just as they do on the event ledger.

// SaveReviewScope registers a review scope.
func (s *Store) SaveReviewScope(ctx context.Context, sc review.ScopeRegistry) (review.ScopeRegistry, error) {
	inc, _ := json.Marshal(sc.IncludedSystems)
	streams, _ := json.Marshal(sc.IncludedStreams)
	auth, _ := json.Marshal(sc.AuthorizedSubjects)
	exc, _ := json.Marshal(sc.ExcludedSystems)
	err := s.pool.QueryRow(ctx, `
		INSERT INTO review_scopes
		  (scope_id, shipment_key, label, included_systems, included_streams,
		   authorized_subjects, excluded_systems, cutoff_received_at, note)
		VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,NULLIF($9,''))
		RETURNING registered_at`,
		sc.ScopeID, sc.ShipmentKey, sc.Label, inc, streams, auth, exc, sc.Cutoff, sc.Note).
		Scan(&sc.RegisteredAt)
	if err != nil {
		if isUnique(err) {
			return review.ScopeRegistry{}, fmt.Errorf("%w: scope %s already registered", ErrDuplicate, sc.ScopeID)
		}
		return review.ScopeRegistry{}, err
	}
	return sc, nil
}

// GetReviewScope loads one scope.
func (s *Store) GetReviewScope(ctx context.Context, id string) (review.ScopeRegistry, error) {
	var sc review.ScopeRegistry
	var inc, streams, auth, exc []byte
	var label, note *string
	err := s.pool.QueryRow(ctx, `
		SELECT scope_id, shipment_key, label, included_systems, included_streams,
		       authorized_subjects, excluded_systems, cutoff_received_at, note, registered_at
		FROM review_scopes WHERE scope_id=$1`, id).
		Scan(&sc.ScopeID, &sc.ShipmentKey, &label, &inc, &streams, &auth, &exc,
			&sc.Cutoff, &note, &sc.RegisteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrNotFound
	}
	if err != nil {
		return sc, err
	}
	if label != nil {
		sc.Label = *label
	}
	if note != nil {
		sc.Note = *note
	}
	_ = json.Unmarshal(inc, &sc.IncludedSystems)
	_ = json.Unmarshal(streams, &sc.IncludedStreams)
	_ = json.Unmarshal(auth, &sc.AuthorizedSubjects)
	_ = json.Unmarshal(exc, &sc.ExcludedSystems)
	return sc, nil
}

// ListReviewScopes lists registered scopes newest first.
func (s *Store) ListReviewScopes(ctx context.Context) ([]review.ScopeRegistry, error) {
	rows, err := s.pool.Query(ctx, `SELECT scope_id FROM review_scopes ORDER BY registered_at DESC, scope_id`)
	if err != nil {
		return nil, err
	}
	ids, err := collectRows(rows, func(r pgx.Rows) (string, error) {
		var id string
		err := r.Scan(&id)
		return id, err
	})
	if err != nil {
		return nil, err
	}
	var out []review.ScopeRegistry
	for _, id := range ids {
		sc, err := s.GetReviewScope(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// recordHash hashes the canonical record body so a re-import of the same record
// is a no-op and a changed record under the same id is visible.
func recordHash(r *review.Record) string {
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SaveReviewRecords appends imported records and the import audit row. It is
// idempotent per (scope, record_id) content hash: identical re-imports are
// skipped, conflicting ones are rejected so a dossier cannot be rewritten.
func (s *Store) SaveReviewRecords(ctx context.Context, scopeID, batchKey string,
	recs []*review.Record, res *review.ImportResult) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	saved := 0
	for _, r := range recs {
		body, _ := json.Marshal(r)
		h := recordHash(r)
		tag, err := tx.Exec(ctx, `
			INSERT INTO review_records (scope_id, record_id, system, stream, kind, body, content_hash, received_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (scope_id, record_id) DO NOTHING`,
			scopeID, r.RecordID, r.System, r.Stream, r.Kind, body, h, receivedAtOf(r))
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() > 0 {
			saved++
			continue
		}
		// Existing row: identical content is a retransmission; different content
		// is a conflict that must not overwrite the original dossier record.
		var existing string
		if err := tx.QueryRow(ctx, `SELECT content_hash FROM review_records WHERE scope_id=$1 AND record_id=$2`,
			scopeID, r.RecordID).Scan(&existing); err != nil {
			return 0, err
		}
		if existing != h {
			return 0, fmt.Errorf("%w: review record %s already imported with different content", ErrDuplicate, r.RecordID)
		}
	}
	excluded, _ := json.Marshal(res.Excluded)
	late, _ := json.Marshal(res.LateArrivals)
	_, err = tx.Exec(ctx, `
		INSERT INTO review_imports (scope_id, batch_key, imported_count, excluded, late_arrivals)
		VALUES ($1,$2,$3,$4,$5)`,
		scopeID, batchKey, saved, excluded, late)
	if err != nil {
		if isUnique(err) {
			return 0, fmt.Errorf("%w: import batch %s already recorded", ErrDuplicate, batchKey)
		}
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return saved, nil
}

func receivedAtOf(r *review.Record) time.Time {
	if r.Received != nil {
		return r.Received.Earliest.UTC()
	}
	if !r.PublishedAt.IsZero() {
		return r.PublishedAt.UTC()
	}
	return r.Occurred.Latest.UTC()
}

// LoadReviewRecords returns the records imported into a scope.
func (s *Store) LoadReviewRecords(ctx context.Context, scopeID string) ([]*review.Record, []review.ScopeDecision, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT body FROM review_records WHERE scope_id=$1 ORDER BY record_id`, scopeID)
	if err != nil {
		return nil, nil, err
	}
	recs, err := collectRows(rows, func(r pgx.Rows) (*review.Record, error) {
		var body []byte
		if err := r.Scan(&body); err != nil {
			return nil, err
		}
		var rec review.Record
		if err := json.Unmarshal(body, &rec); err != nil {
			return nil, err
		}
		return &rec, nil
	})
	if err != nil {
		return nil, nil, err
	}
	// Exclusion and late-arrival audit rows are informative only; they do not
	// affect the reviewed material.
	decRows, err := s.pool.Query(ctx, `
		SELECT COALESCE(excluded,'[]'::jsonb), COALESCE(late_arrivals,'[]'::jsonb)
		FROM review_imports WHERE scope_id=$1`, scopeID)
	if err != nil {
		return nil, nil, err
	}
	var excluded []review.ScopeDecision
	_, err = collectRows(decRows, func(r pgx.Rows) (struct{}, error) {
		var ex, late []byte
		if err := r.Scan(&ex, &late); err != nil {
			return struct{}{}, err
		}
		var xs, ls []review.ScopeDecision
		_ = json.Unmarshal(ex, &xs)
		_ = json.Unmarshal(late, &ls)
		excluded = append(excluded, xs...)
		return struct{}{}, nil
	})
	return recs, excluded, err
}

// CaseRecord is a review case.
type CaseRecord struct {
	CaseID    string    `json:"case_id"`
	ScopeID   string    `json:"scope_id"`
	Title     string    `json:"title,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveReviewCase registers a case.
func (s *Store) SaveReviewCase(ctx context.Context, c CaseRecord) (CaseRecord, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO review_cases (case_id, scope_id, title)
		VALUES ($1,$2,NULLIF($3,'')) RETURNING created_at`,
		c.CaseID, c.ScopeID, c.Title).Scan(&c.CreatedAt)
	if err != nil {
		if isUnique(err) {
			return CaseRecord{}, fmt.Errorf("%w: case %s already exists", ErrDuplicate, c.CaseID)
		}
		return CaseRecord{}, err
	}
	return c, nil
}

// GetReviewCase loads a case.
func (s *Store) GetReviewCase(ctx context.Context, id string) (CaseRecord, error) {
	var c CaseRecord
	var title *string
	err := s.pool.QueryRow(ctx, `
		SELECT case_id, scope_id, title, created_at FROM review_cases WHERE case_id=$1`, id).
		Scan(&c.CaseID, &c.ScopeID, &title, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if title != nil {
		c.Title = *title
	}
	return c, err
}

// RunRecord is a stored review run.
type RunRecord struct {
	RunID           string              `json:"run_id"`
	CaseID          string              `json:"case_id"`
	ScopeID         string              `json:"scope_id"`
	Cutoff          time.Time           `json:"cutoff_received_at"`
	Target          review.Target       `json:"target"`
	SummaryVerdict  string              `json:"summary_verdict"`
	BudgetExhausted bool                `json:"budget_exhausted"`
	MaterialIDs     []string            `json:"material_record_ids"`
	MaterialHash    string              `json:"material_hash"`
	Analysis        *review.Analysis    `json:"analysis"`
	CrossCheck      []review.CrossCheck `json:"cross_check,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
}

// SaveReviewRun appends a run and its examples.
func (s *Store) SaveReviewRun(ctx context.Context, run RunRecord) (RunRecord, error) {
	target, _ := json.Marshal(run.Target)
	ids, _ := json.Marshal(run.MaterialIDs)
	an, _ := json.Marshal(run.Analysis)
	cc, _ := json.Marshal(run.CrossCheck)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RunRecord{}, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `
		INSERT INTO review_runs
		  (run_id, case_id, scope_id, cutoff_received_at, target, summary_verdict,
		   budget_exhausted, material_ids, material_hash, analysis, cross_check)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at`,
		run.RunID, run.CaseID, run.ScopeID, run.Cutoff, target, run.SummaryVerdict,
		run.BudgetExhausted, ids, run.MaterialHash, an, cc).Scan(&run.CreatedAt)
	if err != nil {
		return RunRecord{}, err
	}
	for i, ex := range run.Analysis.Examples {
		body, _ := json.Marshal(ex)
		id := fmt.Sprintf("%03d-%s|%s", i, ex.Kind, ex.Note)
		if _, err := tx.Exec(ctx, `
			INSERT INTO review_examples (example_id, run_id, case_id, kind, body)
			VALUES ($1,$2,$3,$4,$5)`, id, run.RunID, run.CaseID, ex.Kind, body); err != nil {
			return RunRecord{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RunRecord{}, err
	}
	return run, nil
}

// GetReviewRun loads a run.
func (s *Store) GetReviewRun(ctx context.Context, id string) (RunRecord, error) {
	var run RunRecord
	var target, ids, an, cc []byte
	err := s.pool.QueryRow(ctx, `
		SELECT run_id, case_id, scope_id, cutoff_received_at, target, summary_verdict,
		       budget_exhausted, material_ids, material_hash, analysis, cross_check, created_at
		FROM review_runs WHERE run_id=$1`, id).
		Scan(&run.RunID, &run.CaseID, &run.ScopeID, &run.Cutoff, &target, &run.SummaryVerdict,
			&run.BudgetExhausted, &ids, &run.MaterialHash, &an, &cc, &run.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, ErrNotFound
	}
	if err != nil {
		return run, err
	}
	_ = json.Unmarshal(target, &run.Target)
	_ = json.Unmarshal(ids, &run.MaterialIDs)
	_ = json.Unmarshal(cc, &run.CrossCheck)
	var analysis review.Analysis
	if err := json.Unmarshal(an, &analysis); err != nil {
		return run, err
	}
	run.Analysis = &analysis
	return run, nil
}

// ListReviewRuns lists a case's runs newest first.
func (s *Store) ListReviewRuns(ctx context.Context, caseID string) ([]RunRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id FROM review_runs WHERE case_id=$1 ORDER BY created_at DESC, run_id`, caseID)
	if err != nil {
		return nil, err
	}
	ids, err := collectRows(rows, func(r pgx.Rows) (string, error) {
		var id string
		err := r.Scan(&id)
		return id, err
	})
	if err != nil {
		return nil, err
	}
	var out []RunRecord
	for _, id := range ids {
		run, err := s.GetReviewRun(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// ListReviewExamples lists the examples stored for a run.
func (s *Store) ListReviewExamples(ctx context.Context, runID string) ([]review.OrderExample, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT body FROM review_examples WHERE run_id=$1 ORDER BY kind, example_id`, runID)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (review.OrderExample, error) {
		var body []byte
		var ex review.OrderExample
		if err := r.Scan(&body); err != nil {
			return ex, err
		}
		err := json.Unmarshal(body, &ex)
		return ex, err
	})
}
