// Package service orchestrates the domain engine against the store. All public
// operations are transactional and idempotent where they touch the ledger.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/store"
)

// Service is the platform façade.
type Service struct {
	st    *store.Store
	cfg   Config
	nowFn func() time.Time
}

// Config carries runtime tunables.
type Config struct {
	ClockDriftThreshold time.Duration
	ConflictWindow      time.Duration
	CompletenessFloor   float64
}

// DefaultConfig mirrors the README defaults.
func DefaultConfig() Config {
	return Config{
		ClockDriftThreshold: 15 * time.Minute,
		ConflictWindow:      2 * time.Hour,
		CompletenessFloor:   0.85,
	}
}

// New builds a service.
func New(st *store.Store, cfg Config) *Service {
	if cfg.CompletenessFloor == 0 {
		cfg.CompletenessFloor = DefaultConfig().CompletenessFloor
	}
	if cfg.ClockDriftThreshold == 0 {
		cfg.ClockDriftThreshold = DefaultConfig().ClockDriftThreshold
	}
	if cfg.ConflictWindow == 0 {
		cfg.ConflictWindow = DefaultConfig().ConflictWindow
	}
	return &Service{st: st, cfg: cfg, nowFn: time.Now}
}

// SetClock overrides the wall clock (tests / demo freeze).
func (s *Service) SetClock(fn func() time.Time) { s.nowFn = fn }

// Store exposes the store (read-only helpers).
func (s *Service) Store() *store.Store { return s.st }

// Config returns the active config.
func (s *Service) Config() Config { return s.cfg }

// IngestRequest is the API-level ingest call.
type IngestRequest struct {
	Source string
	Env    domain.EventEnvelope
}

// IngestOutcome is the API-level result.
type IngestOutcome struct {
	Outcome     string                  `json:"outcome"`
	EventID     string                  `json:"event_id,omitempty"`
	Seq         int64                   `json:"seq,omitempty"`
	DuplicateOf string                  `json:"duplicate_of,omitempty"`
	Validation  domain.ValidationResult `json:"validation"`
	Contract    string                  `json:"contract,omitempty"`
	Warnings    []string                `json:"warnings,omitempty"`
	Detail      map[string]any          `json:"detail,omitempty"`
	Committed   time.Time               `json:"committed_at"`
}

// Ingest validates, deduplicates, appends and projects one event.
func (s *Service) Ingest(ctx context.Context, req IngestRequest) (IngestOutcome, error) {
	out := IngestOutcome{Committed: s.nowFn().UTC()}
	src := strings.TrimSpace(req.Source)
	env := req.Env
	if src == "" {
		return out, fmt.Errorf("%w: source is required", ErrInvalid)
	}
	if !domain.IsKnownMessageType(env.MessageType) {
		return out, fmt.Errorf("%w: unknown message_type %q", ErrInvalid, env.MessageType)
	}
	if env.SourceCode != "" && !domain.ValidBusinessKey(env.SourceCode) {
		return out, fmt.Errorf("%w: invalid source_code", ErrInvalid)
	}
	if env.OccurredAt.IsZero() {
		return out, fmt.Errorf("%w: occurred_at is required", ErrInvalid)
	}
	var payload map[string]any
	if len(env.Payload) == 0 {
		payload = map[string]any{}
	} else if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return out, fmt.Errorf("%w: payload is not a JSON object: %v", ErrInvalid, err)
	}
	received := s.nowFn().UTC()
	if env.ReceivedAt != nil && !env.ReceivedAt.IsZero() {
		received = env.ReceivedAt.UTC()
	}
	if received.Before(env.OccurredAt.Add(-24 * time.Hour)) {
		return out, fmt.Errorf("%w: received_at is implausibly earlier than occurred_at", ErrInvalid)
	}

	// Contract lookup + validation.
	contract, err := s.st.LatestContract(ctx, src, env.MessageType)
	if errors.Is(err, store.ErrNotFound) {
		return out, fmt.Errorf("%w: no contract registered for source=%s message_type=%s",
			ErrInvalid, src, env.MessageType)
	}
	if err != nil {
		return out, err
	}
	validation := domain.ValidatePayload(contract.Definition, payload)
	if len(validation.Errors) > 0 {
		out.Validation = validation
		out.Outcome = "rejected"
		if err := s.recordRejected(ctx, src, env, contract.Version, received, validation); err != nil {
			return out, err
		}
		out.Detail = map[string]any{"errors": validation.Errors}
		return out, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(validation.Errors, "; "))
	}
	out.Validation = validation
	out.Contract = fmt.Sprintf("%s/%s@v%d", src, env.MessageType, contract.Version)
	if len(validation.MissingFields) > 0 {
		out.Warnings = append(out.Warnings,
			"missing required fields (accepted, will reduce evidence completeness): "+
				strings.Join(validation.MissingFields, ","))
	}
	if len(validation.UnmappedFields) > 0 && contract.Compat == "backward" {
		out.Warnings = append(out.Warnings,
			"unmapped fields accepted under backward compatibility: "+
				strings.Join(validation.UnmappedFields, ","))
	}

	// Canonicalise identity.
	bkeys, rawkeys, packageKey, shipmentKey := canonicalKeys(src, env.MessageType, payload, env.BusinessKeys, env.PackageID)
	revision := domain.CanonicalPackageRevision(env.PackageRevision)
	if revision == "" {
		revision = domain.CanonicalPackageRevision(strPayload(payload, "package_revision"))
	}
	deviceID := env.DeviceID
	if deviceID == "" {
		deviceID = strPayload(payload, "device_id")
	}
	deviceEventNo := env.DeviceEventNo
	if deviceEventNo == "" {
		deviceEventNo = strPayload(payload, "device_event_no")
	}
	dedupeKey, dedupeBasis := domain.DedupeKey(src, env.IdempotencyKey, deviceID, deviceEventNo)
	if dedupeKey == "" {
		fallback := fmt.Sprintf("content:%s:%s:%s:%s", env.MessageType, packageKey,
			deviceID, env.OccurredAt.UTC().Format(time.RFC3339Nano))
		sum := sha256.Sum256([]byte(fallback))
		dedupeKey = "derived:" + hex.EncodeToString(sum[:16])
		dedupeBasis = "derived"
		out.Warnings = append(out.Warnings,
			"no idempotency_key or device_event_no supplied; derived a content key (basis=derived)")
	}
	evidence := domain.SortedUnique(append(append([]string{}, env.EvidenceIndexes...),
		strsPayload(payload, "evidence_indexes")...))
	supersedes := env.SupersedesEventID
	if supersedes == "" {
		supersedes = strPayload(payload, "supersedes_event_id")
	}
	if supersedes != "" {
		if _, err := uuid.Parse(supersedes); err != nil {
			return out, fmt.Errorf("%w: supersedes_event_id must be a uuid", ErrInvalid)
		}
	}

	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)

	// Idempotency: identical key -> identical content is a retransmission.
	if existing, err := s.st.FindByDedupeKey(ctx, tx, src, dedupeKey); err == nil {
		fp := domain.ContentFingerprint(&env, packageKey, revision)
		if existing.ContentHash == fp {
			out.Outcome = "duplicate_identical"
			out.EventID = existing.EventID
			out.Seq = existing.Seq
			out.DuplicateOf = existing.EventID
			if err := s.st.RecordAttempt(ctx, tx, store.Attempt{
				Source: src, MessageType: env.MessageType, SourceCode: env.SourceCode,
				IdempotencyKey: env.IdempotencyKey, ContentHash: fp,
				OccurredAt: &env.OccurredAt, ReceivedAt: received,
				Outcome: out.Outcome, RawEventID: existing.EventID,
				Detail: map[string]any{"duplicate_of": existing.EventID, "basis": dedupeBasis},
			}); err != nil {
				return out, err
			}
			if err := tx.Commit(ctx); err != nil {
				return out, err
			}
			return out, nil
		}
		out.Outcome = "duplicate_conflicting"
		out.DuplicateOf = existing.EventID
		out.Detail = map[string]any{
			"original_event_id": existing.EventID,
			"original_hash":     existing.ContentHash,
			"incoming_hash":     fp,
		}
		if err := s.st.RecordAttempt(ctx, tx, store.Attempt{
			Source: src, MessageType: env.MessageType, SourceCode: env.SourceCode,
			IdempotencyKey: env.IdempotencyKey, ContentHash: fp,
			OccurredAt: &env.OccurredAt, ReceivedAt: received,
			Outcome: out.Outcome, RawEventID: existing.EventID,
			Detail: map[string]any{"original_event_id": existing.EventID,
				"message": "content differs from the original; quarantined, original retained"},
		}); err != nil {
			return out, err
		}
		if err := tx.Commit(ctx); err != nil {
			return out, err
		}
		return out, fmt.Errorf("%w: idempotency key %s already used with different content (original %s)",
			ErrConflict, dedupeKey, existing.EventID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return out, err
	}

	eventID := uuid.NewString()
	ev := &domain.Event{
		EventID: eventID, Source: src, MessageType: env.MessageType,
		SourceCode: env.SourceCode, ContractVersion: contract.Version,
		IdempotencyKey: env.IdempotencyKey, DedupeKey: dedupeKey,
		BusinessKeys: bkeys, RawBusinessKeys: rawkeys,
		PackageID: env.PackageID, PackageKey: packageKey,
		PackageRevision: revision, ShipmentKey: shipmentKey,
		DeviceID: deviceID, DeviceEventNo: deviceEventNo,
		OccurredAt: env.OccurredAt.UTC(), ReceivedAt: received,
		Payload: payload, SupersedesEventID: supersedes,
		EvidenceIndexes: evidence,
		MissingFields:   validation.MissingFields,
		UnmappedFields:  validation.UnmappedFields,
	}
	ev.ContentHash = domain.ContentFingerprint(&env, packageKey, revision)
	// Fill the package key from the payload when the envelope omits it.
	if ev.PackageKey == "" {
		ev.PackageKey = canonicalPackageOrEmpty(strPayload(payload, "package_id"))
	}
	seq, err := s.st.InsertEvent(ctx, tx, ev)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			out.Outcome = "duplicate_conflicting"
			return out, fmt.Errorf("%w: concurrent duplicate for key %s", ErrConflict, dedupeKey)
		}
		return out, err
	}
	ev.Seq = seq
	if _, _, err := s.st.ApplyDerivation(ctx, tx, ev); err != nil {
		return out, err
	}
	if err := s.st.RecordAttempt(ctx, tx, store.Attempt{
		Source: src, MessageType: env.MessageType, SourceCode: env.SourceCode,
		IdempotencyKey: env.IdempotencyKey, ContentHash: ev.ContentHash,
		OccurredAt: &ev.OccurredAt, ReceivedAt: received,
		Outcome: "accepted", RawEventID: eventID,
		Detail: map[string]any{"seq": seq, "basis": dedupeBasis, "contract": out.Contract,
			"package_key": packageKey, "shipment_key": shipmentKey},
	}); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	out.Outcome = "accepted"
	out.EventID = eventID
	out.Seq = seq
	return out, nil
}

// ErrInvalid signals a caller error (422).
var ErrInvalid = errors.New("invalid request")

// ErrConflict signals a conflicting request (409).
var ErrConflict = errors.New("conflict")

func (s *Service) recordRejected(ctx context.Context, src string, env domain.EventEnvelope,
	version int, received time.Time, v domain.ValidationResult) error {
	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.st.RecordAttempt(ctx, tx, store.Attempt{
		Source: src, MessageType: env.MessageType, SourceCode: env.SourceCode,
		IdempotencyKey: env.IdempotencyKey, ReceivedAt: received,
		Outcome: "rejected",
		Detail:  map[string]any{"errors": v.Errors, "missing_fields": v.MissingFields},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// canonicalKeys folds every business key the event carries. Raw spellings are
// preserved for the encoding-consistency rule. The package identifier may come
// from the envelope or the payload; the same applies to business keys.
func canonicalKeys(src, mt string, payload map[string]any, envelopeKeys map[string]string, envelopePackageID string) (
	bkeys, raw map[string]string, packageKey, shipmentKey string) {
	bkeys = map[string]string{}
	raw = map[string]string{}
	add := func(kind, rawVal string) string {
		if rawVal == "" {
			return ""
		}
		canon, _, ok := domain.NormalizeKey(kind, rawVal)
		if !ok || canon == "" {
			return ""
		}
		bkeys[kind] = canon
		if raw[kind] == "" {
			raw[kind] = rawVal
		}
		return canon
	}
	for k, v := range envelopeKeys {
		add(k, v)
	}
	for _, k := range []string{"awb", "shipment", "container_id", "leg", "package_id"} {
		if v := strPayload(payload, k); v != "" {
			kind := k
			if k == "container_id" {
				kind = domain.KeyContainer
			}
			if k == "package_id" {
				kind = domain.KeyPackage
			}
			add(kind, v)
		}
	}
	if awb := bkeys[domain.KeyShipment]; awb != "" {
		if _, exists := bkeys[domain.KeyAwb]; !exists {
			bkeys[domain.KeyAwb] = awb
		}
	}
	pkgRaw := envelopePackageID
	if pkgRaw == "" {
		pkgRaw = strPayload(payload, "package_id")
	}
	packageKey = canonicalPackageOrEmpty(pkgRaw)
	if packageKey != "" {
		bkeys[domain.KeyPackage] = packageKey
		if raw[domain.KeyPackage] == "" {
			raw[domain.KeyPackage] = pkgRaw
		}
	}
	shipmentKey = bkeys[domain.KeyShipment]
	if shipmentKey == "" {
		shipmentKey = bkeys[domain.KeyAwb]
	}
	_ = src
	_ = mt
	return bkeys, raw, packageKey, shipmentKey
}

func canonicalPackageOrEmpty(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	c, _, ok := domain.NormalizeKey(domain.KeyPackage, raw)
	if !ok {
		return ""
	}
	return c
}

func strPayload(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

func strsPayload(p map[string]any, k string) []string {
	arr, ok := p[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// round3 is retained for deterministic float formatting in payloads.
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
