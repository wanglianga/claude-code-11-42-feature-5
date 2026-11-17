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

// AppendResult describes what an ingest attempt did.
type AppendResult struct {
	Outcome     string                  `json:"outcome"` // accepted|duplicate_identical|duplicate_conflicting|rejected
	Event       *domain.Event           `json:"event,omitempty"`
	DuplicateOf string                  `json:"duplicate_of,omitempty"`
	Validation  domain.ValidationResult `json:"validation"`
	Detail      map[string]any          `json:"detail,omitempty"`
	Contract    *Contract               `json:"contract,omitempty"`
}

// RecordAttempt appends an intake attempt row (audit of every post, including
// rejected and duplicate ones).
func (s *Store) RecordAttempt(ctx context.Context, tx pgx.Tx, a Attempt) error {
	detail, _ := json.Marshal(a.Detail)
	_, err := tx.Exec(ctx, `
		INSERT INTO intake_attempts
		  (source, message_type, source_code, idempotency_key, content_hash,
		   occurred_at, received_at, outcome, detail, raw_event_id)
		VALUES ($1,$2,NULLIF($3,''),$4,NULLIF($5,''),$6,$7,$8,$9,NULLIF($10,'')::uuid)`,
		a.Source, a.MessageType, a.SourceCode, a.IdempotencyKey, a.ContentHash,
		a.OccurredAt, a.ReceivedAt, a.Outcome, detail, a.RawEventID)
	return err
}

// Attempt is an intake audit row.
type Attempt struct {
	Source         string         `json:"source"`
	MessageType    string         `json:"message_type"`
	SourceCode     string         `json:"source_code"`
	IdempotencyKey string         `json:"idempotency_key"`
	ContentHash    string         `json:"content_hash"`
	OccurredAt     *time.Time     `json:"occurred_at"`
	ReceivedAt     time.Time      `json:"received_at"`
	Outcome        string         `json:"outcome"`
	Detail         map[string]any `json:"detail,omitempty"`
	RawEventID     string         `json:"raw_event_id"`
}

// FindByDedupeKey returns an existing event with the same source+dedupe key.
func (s *Store) FindByDedupeKey(ctx context.Context, tx pgx.Tx, source, dedupeKey string) (*domain.Event, error) {
	q := tx
	if q == nil {
		return nil, errors.New("transaction required")
	}
	ev, err := scanEvent(q.QueryRow(ctx, selectEvent+` WHERE source=$1 AND dedupe_key=$2`, source, dedupeKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ev, err
}

const selectEvent = `
SELECT seq, event_id, source, message_type, COALESCE(source_code,''), contract_version,
       idempotency_key, dedupe_key, business_keys, raw_business_keys,
       COALESCE(package_id,''), COALESCE(package_key,''), COALESCE(package_revision,''),
       COALESCE(shipment_key,''), COALESCE(device_id,''), COALESCE(device_event_no,''),
       evidence_indexes, occurred_at, received_at, payload, content_hash,
       COALESCE(supersedes_event_id::text,''), missing_fields, unmapped_fields
FROM raw_events`

func scanEvent(row pgx.Row) (*domain.Event, error) {
	var ev domain.Event
	var bkeys, rbkeys, evid, missing, unmapped []byte
	err := row.Scan(&ev.Seq, &ev.EventID, &ev.Source, &ev.MessageType, &ev.SourceCode,
		&ev.ContractVersion, &ev.IdempotencyKey, &ev.DedupeKey, &bkeys, &rbkeys,
		&ev.PackageID, &ev.PackageKey, &ev.PackageRevision, &ev.ShipmentKey,
		&ev.DeviceID, &ev.DeviceEventNo, &evid, &ev.OccurredAt, &ev.ReceivedAt,
		&ev.Payload, &ev.ContentHash, &ev.SupersedesEventID, &missing, &unmapped)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(bkeys, &ev.BusinessKeys)
	_ = json.Unmarshal(rbkeys, &ev.RawBusinessKeys)
	_ = json.Unmarshal(evid, &ev.EvidenceIndexes)
	_ = json.Unmarshal(missing, &ev.MissingFields)
	_ = json.Unmarshal(unmapped, &ev.UnmappedFields)
	return &ev, nil
}

// InsertEvent appends an accepted event in the caller's transaction.
func (s *Store) InsertEvent(ctx context.Context, tx pgx.Tx, ev *domain.Event) (int64, error) {
	bkeys, _ := json.Marshal(ev.BusinessKeys)
	rbkeys, _ := json.Marshal(ev.RawBusinessKeys)
	evid, _ := json.Marshal(ev.EvidenceIndexes)
	missing, _ := json.Marshal(ev.MissingFields)
	unmapped, _ := json.Marshal(ev.UnmappedFields)
	var seq int64
	err := tx.QueryRow(ctx, `
		INSERT INTO raw_events
		  (event_id, source, message_type, source_code, contract_version, idempotency_key,
		   dedupe_key, business_keys, raw_business_keys, package_id, package_key,
		   package_revision, shipment_key, device_id, device_event_no, evidence_indexes,
		   occurred_at, received_at, payload, content_hash, supersedes_event_id,
		   missing_fields, unmapped_fields)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),
		        NULLIF($12,''),NULLIF($13,''),NULLIF($14,''),NULLIF($15,''),$16,
		        $17,$18,$19,$20,NULLIF($21,'')::uuid,$22,$23)
		RETURNING seq`,
		ev.EventID, ev.Source, ev.MessageType, ev.SourceCode, ev.ContractVersion,
		ev.IdempotencyKey, ev.DedupeKey, bkeys, rbkeys, ev.PackageID, ev.PackageKey,
		ev.PackageRevision, ev.ShipmentKey, ev.DeviceID, ev.DeviceEventNo, evid,
		ev.OccurredAt, ev.ReceivedAt, ev.Payload, ev.ContentHash, ev.SupersedesEventID,
		missing, unmapped).Scan(&seq)
	if err != nil {
		if isUnique(err) {
			return 0, ErrDuplicate
		}
		return 0, err
	}
	return seq, nil
}

// EventByID fetches one event.
func (s *Store) EventByID(ctx context.Context, id string) (*domain.Event, error) {
	ev, err := scanEvent(s.pool.QueryRow(ctx, selectEvent+` WHERE event_id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ev, err
}

// EventFilter narrows event listings.
type EventFilter struct {
	Source       string
	MessageType  string
	ShipmentKey  string
	PackageKey   string
	LegKey       string
	ContainerKey string
	UpToSeq      int64
	UpToReceived *time.Time
	Limit        int
}

func (f EventFilter) where() (string, []any) {
	q := " WHERE 1=1"
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(cond, len(args))
	}
	if f.Source != "" {
		add(" AND source=$%d", f.Source)
	}
	if f.MessageType != "" {
		add(" AND message_type=$%d", f.MessageType)
	}
	if f.ShipmentKey != "" {
		add(" AND shipment_key=$%d", f.ShipmentKey)
	}
	if f.PackageKey != "" {
		add(" AND package_key=$%d", f.PackageKey)
	}
	if f.UpToSeq > 0 {
		add(" AND seq <= $%d", f.UpToSeq)
	}
	if f.UpToReceived != nil {
		add(" AND received_at <= $%d", *f.UpToReceived)
	}
	q += " ORDER BY seq"
	if f.Limit > 0 {
		add(" LIMIT $%d", f.Limit)
	}
	return q, args
}

// ListEvents lists events matching a filter in append order.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]*domain.Event, error) {
	where, args := f.where()
	rows, err := s.pool.Query(ctx, selectEvent+where, args...)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (*domain.Event, error) {
		ev, err := scanEvent(r)
		return ev, err
	})
}

// EventCounts returns totals per message type.
func (s *Store) EventCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT message_type, count(*) FROM raw_events GROUP BY message_type`)
	if err != nil {
		return nil, err
	}
	pairs, err := collectRows(rows, func(r pgx.Rows) (kV, error) {
		var k string
		var v int64
		err := r.Scan(&k, &v)
		return kV{k, v}, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(pairs))
	for _, p := range pairs {
		out[p.K] = p.V
	}
	return out, nil
}

type kV struct {
	K string
	V int64
}

// ReceivedAtBoundaries returns per-source arrival span.
type SourceSpan struct {
	Source     string    `json:"source"`
	Count      int64     `json:"count"`
	FirstAt    time.Time `json:"first_at"`
	LastAt     time.Time `json:"last_at"`
	FirstOccur time.Time `json:"first_occurred_at"`
	LastOccur  time.Time `json:"last_occurred_at"`
}

func (s *Store) SourceSpans(ctx context.Context) ([]SourceSpan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, count(*), min(received_at), max(received_at),
		       min(occurred_at), max(occurred_at)
		FROM raw_events GROUP BY source ORDER BY source`)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (SourceSpan, error) {
		var sp SourceSpan
		err := r.Scan(&sp.Source, &sp.Count, &sp.FirstAt, &sp.LastAt, &sp.FirstOccur, &sp.LastOccur)
		return sp, err
	})
}

// Attempts lists intake attempts for audit.
func (s *Store) Attempts(ctx context.Context, source, idemKey string, limit int) ([]Attempt, error) {
	args := []any{}
	q := `SELECT source, message_type, COALESCE(source_code,''), idempotency_key,
	             COALESCE(content_hash,''), occurred_at, received_at, outcome, detail,
	             COALESCE(raw_event_id::text,'')
	      FROM intake_attempts WHERE 1=1`
	if source != "" {
		args = append(args, source)
		q += fmt.Sprintf(" AND source=$%d", len(args))
	}
	if idemKey != "" {
		args = append(args, idemKey)
		q += fmt.Sprintf(" AND idempotency_key=$%d", len(args))
	}
	if limit <= 0 {
		limit = 200
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
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
