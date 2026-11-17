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

// LoadLinks returns all graph links ever written (with retractions joined)
// together with the retraction rows. Links created after `until` are excluded
// from the projection by domain.Graph's knowledge checks; passing until lets
// callers avoid loading very new data for old replay.
func (s *Store) LoadLinks(ctx context.Context, until *time.Time) ([]domain.Link, []domain.Retraction, error) {
	return s.loadLinks(ctx, until, 0)
}

// LoadLinksAt bounds the projection by BOTH knowledge time and the append
// sequence of the causing event. The sequence bound is what makes replay of a
// past publication exact: an event that was backdated (received_at in the
// past) but appended after the publication must not appear in that
// publication's inputs. maxSeq == 0 disables the sequence bound.
func (s *Store) LoadLinksAt(ctx context.Context, until time.Time, maxSeq int64) ([]domain.Link, []domain.Retraction, error) {
	return s.loadLinks(ctx, &until, maxSeq)
}

func (s *Store) loadLinks(ctx context.Context, until *time.Time, maxSeq int64) ([]domain.Link, []domain.Retraction, error) {
	var retCond string
	var args []any
	if until != nil {
		args = append(args, *until)
		retCond += fmt.Sprintf(" AND r.knowledge_at <= $%d", len(args))
	}
	if maxSeq > 0 {
		args = append(args, maxSeq)
		retCond += fmt.Sprintf(" AND re.seq <= $%d", len(args))
	}
	q := fmt.Sprintf(`
	SELECT l.id, l.link_key, l.link_version, l.kind, l.subject_type, l.subject_key,
	       l.object_type, l.object_key, l.valid_from, l.knowledge_at,
	       COALESCE(l.package_revision,''), l.source_event_id, l.attributes,
	       r.retracted_at, r.knowledge_at, COALESCE(r.reason,'')
	FROM graph_links l
	JOIN raw_events e ON e.event_id = l.source_event_id
	LEFT JOIN LATERAL (
	    SELECT r.retracted_at, r.knowledge_at, r.reason
	    FROM graph_retractions r
	    JOIN raw_events re ON re.event_id = r.source_event_id
	    WHERE r.link_id = l.id%s
	    ORDER BY r.id DESC LIMIT 1
	) r ON TRUE
	WHERE 1=1`, retCond)
	if until != nil {
		q += " AND l.knowledge_at <= $1"
	}
	if maxSeq > 0 {
		n := 1
		if until != nil {
			n = 2
		}
		q += fmt.Sprintf(" AND e.seq <= $%d", n)
	}
	q += ` ORDER BY l.valid_from, l.link_key, l.link_version`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	links, err := collectRows(rows, func(r pgx.Rows) (domain.Link, error) {
		var l domain.Link
		var attrs []byte
		var retAt, retK *time.Time
		var reason string
		err := r.Scan(&l.ID, &l.LinkKey, &l.LinkVersion, &l.Kind, &l.SubjectType, &l.SubjectKey,
			&l.ObjectType, &l.ObjectKey, &l.ValidFrom, &l.KnowledgeAt,
			&l.PackageRevision, &l.SourceEventID, &attrs,
			&retAt, &retK, &reason)
		if err != nil {
			return l, err
		}
		_ = json.Unmarshal(attrs, &l.Attributes)
		l.RetractedAt = retAt
		l.RetractionKnowledgeAt = retK
		l.RetractionReason = reason
		return l, nil
	})
	if err != nil {
		return nil, nil, err
	}

	rows2, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT l.link_key, l.link_version, r.retracted_at, r.knowledge_at, r.reason, r.source_event_id
		FROM graph_retractions r
		JOIN graph_links l ON l.id = r.link_id
		JOIN raw_events re ON re.event_id = r.source_event_id
		WHERE 1=1%s
		ORDER BY r.id`, retCond), args...)
	if err != nil {
		return nil, nil, err
	}
	rets, err := collectRows(rows2, func(r pgx.Rows) (domain.Retraction, error) {
		var rt domain.Retraction
		err := r.Scan(&rt.LinkKey, &rt.LinkVersion, &rt.EffectiveAt, &rt.KnowledgeAt,
			&rt.Reason, &rt.SourceEventID)
		return rt, err
	})
	if err != nil {
		return nil, nil, err
	}
	return links, rets, nil
}

// ApplyDerivation appends the graph effect of one accepted event inside tx.
// It is idempotent per event: it records which event produced which link
// version, and repeated application of the same event cannot create a new
// version because upserts compare against the currently active claim.
func (s *Store) ApplyDerivation(ctx context.Context, tx pgx.Tx, ev *domain.Event) ([]domain.Link, []domain.Retraction, error) {
	d := domain.Derive(ev)
	var written []domain.Link
	var rets []domain.Retraction
	if len(d.Upserts) == 0 && len(d.RetireKeys) == 0 {
		return nil, nil, nil
	}

	// Retire first so that a split retires the parent pair before children.
	for _, key := range d.RetireKeys {
		linkID, version, err := s.currentActiveLinkID(ctx, tx, key)
		if err != nil {
			return nil, nil, err
		}
		if linkID == 0 {
			continue // nothing to retire
		}
		// Never retire twice for the same event.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM graph_retractions WHERE link_id=$1 AND source_event_id=$2)`,
			linkID, ev.EventID).Scan(&exists); err != nil {
			return nil, nil, err
		}
		if exists {
			continue
		}
		reason := d.RetireReason
		if reason == "" {
			reason = "identity_change"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO graph_retractions (link_id, retracted_at, knowledge_at, reason, source_event_id)
			VALUES ($1,$2,$3,$4,$5)`, linkID, ev.OccurredAt, ev.ReceivedAt, reason, ev.EventID); err != nil {
			return nil, nil, err
		}
		rets = append(rets, domain.Retraction{
			LinkKey: key, LinkVersion: version, EffectiveAt: ev.OccurredAt,
			KnowledgeAt: ev.ReceivedAt, Reason: reason, SourceEventID: ev.EventID,
		})
	}

	for _, up := range d.Upserts {
		link, wrote, err := s.upsertLink(ctx, tx, ev, up)
		if err != nil {
			return nil, nil, err
		}
		if wrote {
			written = append(written, link)
		}
	}
	return written, rets, nil
}

// currentActiveLinkID finds the newest link row for a key that has not been
// retired (no retraction rows pointing at it).
func (s *Store) currentActiveLinkID(ctx context.Context, tx pgx.Tx, key string) (int64, int, error) {
	var id int64
	var ver int
	err := tx.QueryRow(ctx, `
		SELECT l.id, l.link_version FROM graph_links l
		WHERE l.link_key=$1 AND NOT EXISTS (SELECT 1 FROM graph_retractions r WHERE r.link_id=l.id)
		ORDER BY l.link_version DESC LIMIT 1`, key).Scan(&id, &ver)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return id, ver, err
}

// sameActiveClaim reports whether an un-retracted link already exists for the
// same key, object and effective instant. That is a restatement, not a change.
func (s *Store) sameActiveClaim(ctx context.Context, tx pgx.Tx, up domain.LinkRequest) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM graph_links l
		  WHERE l.link_key=$1 AND l.object_key=$2 AND l.valid_from=$3
		    AND NOT EXISTS (SELECT 1 FROM graph_retractions r WHERE r.link_id=l.id))`,
		up.LinkKey, up.ObjectKey, up.ValidFrom).Scan(&exists)
	return exists, err
}

func (s *Store) upsertLink(ctx context.Context, tx pgx.Tx, ev *domain.Event, up domain.LinkRequest) (domain.Link, bool, error) {
	// A link that already exists for the SAME key, object and effective time is
	// a restatement (e.g. re-declaring an already-active package after a split)
	// and must not create a redundant version.
	exists, err := s.sameActiveClaim(ctx, tx, up)
	if err != nil {
		return domain.Link{}, false, err
	}
	if exists {
		return domain.Link{}, false, nil
	}

	// The single-valued stow slot: a new stow of the SAME package in a
	// DIFFERENT container retires the prior claim at the effective time.
	if up.Kind == domain.LinkPackageContainer {
		id, _, err := s.currentActiveLinkID(ctx, tx, up.LinkKey)
		if err != nil {
			return domain.Link{}, false, err
		}
		if id != 0 {
			var curObject string
			if err := tx.QueryRow(ctx, `SELECT object_key FROM graph_links WHERE id=$1`, id).Scan(&curObject); err != nil {
				return domain.Link{}, false, err
			}
			if curObject != up.ObjectKey {
				if _, err := tx.Exec(ctx, `
					INSERT INTO graph_retractions (link_id, retracted_at, knowledge_at, reason, source_event_id)
					VALUES ($1,$2,$3,'transload',$4)`, id, up.ValidFrom, up.KnowledgeAt, ev.EventID); err != nil {
					return domain.Link{}, false, err
				}
			}
		}
	}

	var nextVer int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(link_version),0)+1 FROM graph_links WHERE link_key=$1`,
		up.LinkKey).Scan(&nextVer); err != nil {
		return domain.Link{}, false, err
	}
	// Idempotency: if the newest version of this key was produced by this very
	// event with the same object, do nothing.
	var sameEvent bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM graph_links
		              WHERE link_key=$1 AND source_event_id=$2 AND object_key=$3 AND valid_from=$4)`,
		up.LinkKey, ev.EventID, up.ObjectKey, up.ValidFrom).Scan(&sameEvent); err != nil {
		return domain.Link{}, false, err
	}
	if sameEvent {
		return domain.Link{}, false, nil
	}
	attrs, _ := json.Marshal(up.Attributes)
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO graph_links
		  (link_key, link_version, kind, subject_type, subject_key, object_type, object_key,
		   valid_from, knowledge_at, package_revision, source_event_id, attributes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12)
		RETURNING id`,
		up.LinkKey, nextVer, up.Kind, up.SubjectType, up.SubjectKey, up.ObjectType, up.ObjectKey,
		up.ValidFrom, up.KnowledgeAt, up.PackageRevision, up.SourceEventID, attrs).Scan(&id)
	if err != nil {
		return domain.Link{}, false, fmt.Errorf("insert link %s: %w", up.LinkKey, err)
	}
	return domain.Link{
		ID: id, LinkKey: up.LinkKey, LinkVersion: nextVer, Kind: up.Kind,
		SubjectType: up.SubjectType, SubjectKey: up.SubjectKey,
		ObjectType: up.ObjectType, ObjectKey: up.ObjectKey,
		ValidFrom: up.ValidFrom, KnowledgeAt: up.KnowledgeAt,
		PackageRevision: up.PackageRevision, SourceEventID: up.SourceEventID,
		Attributes: up.Attributes,
	}, true, nil
}

// GraphSubjectsEnvelope lists the shipments/containers/legs the platform knows.
type GraphSubjects struct {
	Shipments  []string `json:"shipments"`
	Containers []string `json:"containers"`
	Legs       []string `json:"legs"`
	Packages   []string `json:"packages"`
}

// Subjects enumerates every subject key appearing in the graph.
func (s *Store) Subjects(ctx context.Context) (GraphSubjects, error) {
	out := GraphSubjects{
		Shipments: []string{}, Containers: []string{}, Legs: []string{}, Packages: []string{},
	}
	var err error
	if out.Shipments, err = s.distinctKeys(ctx, "shipment_package", "subject_key"); err != nil {
		return out, err
	}
	if out.Packages, err = s.distinctKeys(ctx, "shipment_package", "object_key"); err != nil {
		return out, err
	}
	if out.Containers, err = s.distinctKeys(ctx, "package_container", "object_key"); err != nil {
		return out, err
	}
	if out.Legs, err = s.distinctKeys(ctx, "container_leg", "object_key"); err != nil {
		return out, err
	}
	return out, nil
}

// distinctKeys returns the distinct values of a column for a link kind. It
// always returns a non-nil slice so JSON renders [] rather than null.
func (s *Store) distinctKeys(ctx context.Context, kind, col string) ([]string, error) {
	q := fmt.Sprintf(`SELECT DISTINCT %s FROM graph_links WHERE kind=$1 ORDER BY 1`, col)
	rows, err := s.pool.Query(ctx, q, kind)
	if err != nil {
		return nil, err
	}
	out, err := collectRows(rows, func(r pgx.Rows) (string, error) {
		var k string
		err := r.Scan(&k)
		return k, err
	})
	if out == nil {
		out = []string{}
	}
	return out, err
}

// guard: distinctKeys never returns a nil slice so JSON renders [] not null.

// SaveDisposition appends an evidence disposition idempotently.
func (s *Store) SaveDisposition(ctx context.Context, tx pgx.Tx, d domain.Disposition) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO evidence_dispositions
		  (event_id, package_key, shipment_key, disposition, reason, knowledge_at)
		VALUES ($1,$2,NULLIF($3,''),$4,$5,$6)
		ON CONFLICT (event_id, disposition, package_key) DO NOTHING`,
		d.EventID, d.PackageKey, d.ShipmentKey, d.Disposition, d.Reason, d.KnowledgeAt)
	return err
}

// DispositionRow is a stored disposition.
type DispositionRow struct {
	EventID          string    `json:"event_id"`
	ShipmentKey      string    `json:"shipment_key"`
	PackageKey       string    `json:"package_key"`
	Disposition      string    `json:"disposition"`
	Reason           string    `json:"reason"`
	KnowledgeAt      time.Time `json:"knowledge_at"`
	EventMessageType string    `json:"event_message_type"`
	EventSource      string    `json:"event_source"`
}

// EvidenceEligible reports whether the disposition counts as usable evidence.
func (d DispositionRow) EvidenceEligible() bool { return d.Disposition == domain.DispositionOK }

// Disposition converts the row to the domain value.
func (d DispositionRow) Domain() domain.Disposition {
	return domain.Disposition{
		EventID: d.EventID, ShipmentKey: d.ShipmentKey, PackageKey: d.PackageKey,
		Disposition: d.Disposition, Reason: d.Reason, KnowledgeAt: d.KnowledgeAt,
	}
}

// DispositionsForShipment lists disposition rows for a shipment.
func (s *Store) DispositionsForShipment(ctx context.Context, shipmentKey string) ([]DispositionRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.event_id, COALESCE(d.shipment_key,''), COALESCE(d.package_key,''),
		       d.disposition, d.reason, d.knowledge_at,
		       COALESCE(e.message_type,''), COALESCE(e.source,'')
		FROM evidence_dispositions d
		LEFT JOIN raw_events e ON e.event_id = d.event_id
		WHERE d.shipment_key = $1
		ORDER BY d.knowledge_at, d.event_id`, shipmentKey)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (DispositionRow, error) {
		var d DispositionRow
		err := r.Scan(&d.EventID, &d.ShipmentKey, &d.PackageKey, &d.Disposition, &d.Reason,
			&d.KnowledgeAt, &d.EventMessageType, &d.EventSource)
		return d, err
	})
}
