package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"cargo.example/secstate/internal/domain"
)

// CreateWatermark appends a watermark definition.
func (s *Store) CreateWatermark(ctx context.Context, w domain.Watermark) (domain.Watermark, error) {
	req, _ := json.Marshal(w.RequiredSources)
	err := s.pool.QueryRow(ctx, `
		INSERT INTO watermarks (id, scope_type, scope_key, cutoff_received_at,
		                        required_sources, require_handover, mode, label)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))
		RETURNING created_at`,
		w.ID, w.ScopeType, w.ScopeKey, w.CutoffReceivedAt, req, w.RequireHandover,
		w.Mode, w.Label).Scan(&w.CreatedAt)
	return w, err
}

// CloseWatermark records that a source has closed for a watermark.
func (s *Store) CloseWatermark(ctx context.Context, watermarkID, source, note string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO watermark_closures (watermark_id, source, completed_at, note)
		VALUES ($1,$2,$3,NULLIF($4,''))
		ON CONFLICT (watermark_id, source) DO NOTHING`,
		watermarkID, source, at, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDuplicate
	}
	return nil
}

// GetWatermark loads a watermark and its closures.
func (s *Store) GetWatermark(ctx context.Context, id string) (domain.Watermark, error) {
	var w domain.Watermark
	var req []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, scope_type, scope_key, cutoff_received_at, required_sources,
		       require_handover, mode, COALESCE(label,''), created_at
		FROM watermarks WHERE id=$1`, id).
		Scan(&w.ID, &w.ScopeType, &w.ScopeKey, &w.CutoffReceivedAt, &req,
			&w.RequireHandover, &w.Mode, &w.Label, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrNotFound
	}
	if err != nil {
		return w, err
	}
	_ = json.Unmarshal(req, &w.RequiredSources)
	w.ClosedSources, err = s.watermarkClosures(ctx, id)
	if err != nil {
		return w, err
	}
	closed := map[string]bool{}
	for _, c := range w.ClosedSources {
		closed[c] = true
	}
	for _, src := range w.RequiredSources {
		if !closed[src] {
			w.MissingSources = append(w.MissingSources, src)
		}
	}
	w.Complete = len(w.MissingSources) == 0
	return w, nil
}

func (s *Store) watermarkClosures(ctx context.Context, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source FROM watermark_closures WHERE watermark_id=$1 ORDER BY source`, id)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (string, error) {
		var x string
		err := r.Scan(&x)
		return x, err
	})
}

// ListWatermarks returns all watermarks newest first.
func (s *Store) ListWatermarks(ctx context.Context) ([]domain.Watermark, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, scope_type, scope_key, cutoff_received_at, required_sources,
		       require_handover, mode, COALESCE(label,''), created_at
		FROM watermarks ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	list, err := collectRows(rows, func(r pgx.Rows) (domain.Watermark, error) {
		var w domain.Watermark
		var req []byte
		err := r.Scan(&w.ID, &w.ScopeType, &w.ScopeKey, &w.CutoffReceivedAt, &req,
			&w.RequireHandover, &w.Mode, &w.Label, &w.CreatedAt)
		_ = json.Unmarshal(req, &w.RequiredSources)
		return w, err
	})
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].ClosedSources, _ = s.watermarkClosures(ctx, list[i].ID)
		closed := map[string]bool{}
		for _, c := range list[i].ClosedSources {
			closed[c] = true
		}
		for _, src := range list[i].RequiredSources {
			if !closed[src] {
				list[i].MissingSources = append(list[i].MissingSources, src)
			}
		}
		list[i].Complete = len(list[i].MissingSources) == 0
	}
	return list, nil
}

// AttemptsForShipment returns intake attempts relevant to a shipment's events.
func (s *Store) AttemptsForShipment(ctx context.Context, eventIDs []string, keys []string, upto time.Time) ([]Attempt, error) {
	if len(eventIDs) == 0 && len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT source, message_type, COALESCE(source_code,''), idempotency_key,
		       COALESCE(content_hash,''), occurred_at, received_at, outcome, detail,
		       COALESCE(raw_event_id::text,'')
		FROM intake_attempts
		WHERE received_at <= $1
		  AND (raw_event_id = ANY($2::uuid[]) OR idempotency_key = ANY($3::text[]))
		ORDER BY id`, upto, eventIDs, keys)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (Attempt, error) {
		var a Attempt
		var detail []byte
		err := r.Scan(&a.Source, &a.MessageType, &a.SourceCode, &a.IdempotencyKey,
			&a.ContentHash, &a.OccurredAt, &a.ReceivedAt, &a.Outcome, &detail, &a.RawEventID)
		if err != nil {
			return a, err
		}
		_ = json.Unmarshal(detail, &a.Detail)
		return a, nil
	})
}

// InsertViolations appends rule findings idempotently within tx and reports
// which ones were newly recorded.
func (s *Store) InsertViolations(ctx context.Context, tx pgx.Tx, vs []domain.RuleViolation) (int, error) {
	added := 0
	for _, v := range vs {
		detail, _ := json.Marshal(v.Detail)
		related, _ := json.Marshal(v.RelatedEvents)
		tag, err := tx.Exec(ctx, `
			INSERT INTO rule_violations
			  (code, severity, subject_type, subject_key, dedupe_key, knowledge_at,
			   detail, related_event_ids, first_detected_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (code, subject_type, subject_key, dedupe_key) DO NOTHING`,
			v.Code, v.Severity, v.SubjectType, v.SubjectKey, v.DedupeKey, v.KnowledgeAt,
			detail, related, v.FirstSeenAt)
		if err != nil {
			return added, err
		}
		added += int(tag.RowsAffected())
	}
	return added, nil
}

// ListViolations returns findings for a subject, optionally at/before a
// knowledge cutoff.
type ViolationFilter struct {
	SubjectType  string
	SubjectKey   string
	UpTo         *time.Time
	BlockingOnly bool
	Limit        int
}

func (s *Store) ListViolations(ctx context.Context, f ViolationFilter) ([]domain.RuleViolation, error) {
	q := `SELECT code, severity, subject_type, subject_key, dedupe_key, knowledge_at,
	             detail, related_event_ids, first_detected_at
	      FROM rule_violations WHERE 1=1`
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(cond, len(args))
	}
	if f.SubjectType != "" {
		add(" AND subject_type=$%d", f.SubjectType)
	}
	if f.SubjectKey != "" {
		add(" AND subject_key=$%d", f.SubjectKey)
	}
	if f.UpTo != nil {
		add(" AND knowledge_at <= $%d", *f.UpTo)
	}
	if f.BlockingOnly {
		q += " AND severity IN ('error','blocker')"
	}
	q += " ORDER BY knowledge_at, code, dedupe_key"
	if f.Limit > 0 {
		add(" LIMIT $%d", f.Limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (domain.RuleViolation, error) {
		var v domain.RuleViolation
		var detail, related []byte
		err := r.Scan(&v.Code, &v.Severity, &v.SubjectType, &v.SubjectKey, &v.DedupeKey,
			&v.KnowledgeAt, &detail, &related, &v.FirstSeenAt)
		_ = json.Unmarshal(detail, &v.Detail)
		_ = json.Unmarshal(related, &v.RelatedEvents)
		return v, err
	})
}

// Assessment is a stored fused result.
type Assessment struct {
	ID                   string
	ScopeType            string
	ScopeKey             string
	CutoffReceivedAt     time.Time
	MaxSeq               int64
	Status               string
	SecurityResult       string
	EvidenceCompleteness float64
	Result               domain.FusionResult
	Inputs               domain.Lineage
	ResultHash           string
	ComputedAt           time.Time
}

// InsertAssessment appends an assessment row.
func (s *Store) InsertAssessment(ctx context.Context, tx pgx.Tx, a Assessment) error {
	result, _ := json.Marshal(a.Result)
	inputs, _ := json.Marshal(a.Inputs)
	_, err := tx.Exec(ctx, `
		INSERT INTO status_assessments
		  (id, scope_type, scope_key, cutoff_received_at, max_seq, status,
		   security_result, evidence_completeness, result, inputs, result_hash)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11)`,
		a.ID, a.ScopeType, a.ScopeKey, a.CutoffReceivedAt, a.MaxSeq, a.Status,
		a.SecurityResult, a.EvidenceCompleteness, result, inputs, a.ResultHash)
	return err
}

// GetAssessment loads an assessment by id.
func (s *Store) GetAssessment(ctx context.Context, id string) (Assessment, error) {
	var a Assessment
	var result, inputs []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, scope_type, scope_key, cutoff_received_at, max_seq, status,
		       COALESCE(security_result,''), evidence_completeness, result, inputs,
		       result_hash, computed_at
		FROM status_assessments WHERE id=$1`, id).
		Scan(&a.ID, &a.ScopeType, &a.ScopeKey, &a.CutoffReceivedAt, &a.MaxSeq, &a.Status,
			&a.SecurityResult, &a.EvidenceCompleteness, &result, &inputs,
			&a.ResultHash, &a.ComputedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	_ = json.Unmarshal(result, &a.Result)
	_ = json.Unmarshal(inputs, &a.Inputs)
	return a, nil
}

// SnapshotRow is a stored snapshot.
type SnapshotRow struct {
	domain.Snapshot
	PayloadJSON json.RawMessage `json:"-"`
}

// NextSnapshotVersion returns the next version number for a scope.
func (s *Store) NextSnapshotVersion(ctx context.Context, tx pgx.Tx, scopeType, scopeKey string) (int, *domain.Snapshot, error) {
	var prev domain.Snapshot
	var next int
	err := tx.QueryRow(ctx, `
		SELECT version FROM snapshots WHERE scope_type=$1 AND scope_key=$2
		ORDER BY version DESC LIMIT 1`, scopeType, scopeKey).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 1, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	prev, err = scanSnapshot(tx.QueryRow(ctx, snapshotSelect+`
		WHERE scope_type=$1 AND scope_key=$2 ORDER BY version DESC LIMIT 1`,
		scopeType, scopeKey))
	if err != nil {
		return 0, nil, err
	}
	return next + 1, &prev, nil
}

const snapshotSelect = `
SELECT id, scope_type, scope_key, version, snapshot_kind, watermark_id, assessment_id,
       cutoff_received_at, status, COALESCE(security_result,''), evidence_completeness,
       consumable_confirmed, payload, quality_summary, diff,
       COALESCE(supersedes_snapshot_id::text,''), published_at
FROM snapshots`

func scanSnapshot(row pgx.Row) (domain.Snapshot, error) {
	var sn domain.Snapshot
	var payload, quality, diff []byte
	err := row.Scan(&sn.ID, &sn.ScopeType, &sn.ScopeKey, &sn.Version, &sn.Kind,
		&sn.WatermarkID, &sn.AssessmentID, &sn.CutoffReceivedAt, &sn.Status,
		&sn.SecurityResult, &sn.EvidenceCompleteness, &sn.ConsumableConfirmed,
		&payload, &quality, &diff, &sn.Supersedes, &sn.PublishedAt)
	if err != nil {
		return sn, err
	}
	if len(payload) > 0 {
		var p domain.SnapshotPayload
		if err := json.Unmarshal(payload, &p); err == nil {
			sn.Payload = &p
		}
	}
	_ = json.Unmarshal(quality, &sn.QualitySummary)
	_ = json.Unmarshal(diff, &sn.Diff)
	return sn, err
}

// InsertSnapshot appends a published snapshot inside tx.
func (s *Store) InsertSnapshot(ctx context.Context, tx pgx.Tx, sn domain.Snapshot) error {
	var payload []byte
	if sn.Payload != nil {
		payload, _ = json.Marshal(sn.Payload)
	}
	quality, _ := json.Marshal(sn.QualitySummary)
	diff, _ := json.Marshal(sn.Diff)
	_, err := tx.Exec(ctx, `
		INSERT INTO snapshots
		  (id, scope_type, scope_key, version, snapshot_kind, watermark_id, assessment_id,
		   cutoff_received_at, status, security_result, evidence_completeness,
		   consumable_confirmed, payload, quality_summary, diff, supersedes_snapshot_id,
		   published_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13,$14,$15,
		        NULLIF($16,'')::uuid,$17)`,
		sn.ID, sn.ScopeType, sn.ScopeKey, sn.Version, sn.Kind, sn.WatermarkID,
		sn.AssessmentID, sn.CutoffReceivedAt, sn.Status, sn.SecurityResult,
		sn.EvidenceCompleteness, sn.ConsumableConfirmed, payload, quality, diff, sn.Supersedes,
		sn.PublishedAt)
	return err
}

// GetSnapshot loads one snapshot.
func (s *Store) GetSnapshot(ctx context.Context, id string) (domain.Snapshot, error) {
	sn, err := scanSnapshot(s.pool.QueryRow(ctx, snapshotSelect+` WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return sn, ErrNotFound
	}
	return sn, err
}

// LatestSnapshot returns the latest snapshot for a scope, optionally filtered
// by kind and/or by the latest version whose cutoff is <= asOf.
type SnapshotQuery struct {
	ScopeType       string
	ScopeKey        string
	Kind            string
	AsOf            *time.Time // cutoff_received_at <= asOf
	PublishedBefore *time.Time
}

func (s *Store) LatestSnapshot(ctx context.Context, q SnapshotQuery) (domain.Snapshot, error) {
	sql := snapshotSelect + ` WHERE scope_type=$1 AND scope_key=$2`
	args := []any{q.ScopeType, q.ScopeKey}
	if q.Kind != "" {
		args = append(args, q.Kind)
		sql += fmt.Sprintf(" AND snapshot_kind=$%d", len(args))
	}
	if q.AsOf != nil {
		args = append(args, *q.AsOf)
		sql += fmt.Sprintf(" AND cutoff_received_at <= $%d", len(args))
	}
	if q.PublishedBefore != nil {
		args = append(args, *q.PublishedBefore)
		sql += fmt.Sprintf(" AND published_at <= $%d", len(args))
	}
	sql += " ORDER BY version DESC LIMIT 1"
	sn, err := scanSnapshot(s.pool.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return sn, ErrNotFound
	}
	return sn, err
}

// ListSnapshots returns snapshots for a scope oldest first.
func (s *Store) ListSnapshots(ctx context.Context, scopeType, scopeKey string) ([]domain.Snapshot, error) {
	q := snapshotSelect + ` WHERE 1=1`
	var args []any
	if scopeType != "" {
		args = append(args, scopeType)
		q += fmt.Sprintf(" AND scope_type=$%d", len(args))
	}
	if scopeKey != "" {
		args = append(args, scopeKey)
		q += fmt.Sprintf(" AND scope_key=$%d", len(args))
	}
	q += " ORDER BY published_at, version"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (domain.Snapshot, error) {
		return scanSnapshot(r)
	})
}

// ListAllSnapshots lists every snapshot newest first (dashboard).
func (s *Store) ListAllSnapshots(ctx context.Context, limit int) ([]domain.Snapshot, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, snapshotSelect+` ORDER BY published_at DESC, version DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (domain.Snapshot, error) {
		return scanSnapshot(r)
	})
}
