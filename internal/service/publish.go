package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/store"
)

// CreateWatermarkRequest creates the agreed closing line for a scope.
type CreateWatermarkRequest struct {
	ScopeType        string    `json:"scope_type"`
	ScopeKey         string    `json:"scope_key"`
	CutoffReceivedAt time.Time `json:"cutoff_received_at"`
	RequiredSources  []string  `json:"required_sources"`
	RequireHandover  bool      `json:"require_handover"`
	Mode             string    `json:"mode"`
	Label            string    `json:"label,omitempty"`
}

// CreateWatermark validates and stores a watermark.
func (s *Service) CreateWatermark(ctx context.Context, req CreateWatermarkRequest) (domain.Watermark, error) {
	switch req.ScopeType {
	case "shipment", "container", "leg", "all":
	default:
		return domain.Watermark{}, fmt.Errorf("%w: scope_type must be shipment|container|leg|all", ErrInvalid)
	}
	if req.Mode != domain.ModeWaitComplete && req.Mode != domain.ModeEarlyProvisional {
		return domain.Watermark{}, fmt.Errorf("%w: mode must be wait_complete|early_provisional", ErrInvalid)
	}
	if len(req.RequiredSources) == 0 {
		return domain.Watermark{}, fmt.Errorf("%w: required_sources must not be empty", ErrInvalid)
	}
	if req.CutoffReceivedAt.IsZero() {
		req.CutoffReceivedAt = s.nowFn().UTC()
	}
	w := domain.Watermark{
		ID: uuid.NewString(), ScopeType: req.ScopeType, ScopeKey: req.ScopeKey,
		CutoffReceivedAt: req.CutoffReceivedAt.UTC(),
		RequiredSources:  domain.SortedUnique(req.RequiredSources),
		RequireHandover:  req.RequireHandover, Mode: req.Mode, Label: req.Label,
	}
	created, err := s.st.CreateWatermark(ctx, w)
	if err != nil {
		return domain.Watermark{}, err
	}
	created.ClosedSources = []string{}
	created.MissingSources = append([]string{}, created.RequiredSources...)
	created.Complete = false
	return created, nil
}

// CloseWatermarkSource records a source closure for a watermark.
func (s *Service) CloseWatermarkSource(ctx context.Context, watermarkID, source, note string) (domain.Watermark, error) {
	wm, err := s.st.GetWatermark(ctx, watermarkID)
	if err != nil {
		return domain.Watermark{}, err
	}
	required := false
	for _, r := range wm.RequiredSources {
		if r == source {
			required = true
		}
	}
	if !required {
		return domain.Watermark{}, fmt.Errorf("%w: %q is not a required source of this watermark", ErrInvalid, source)
	}
	if err := s.st.CloseWatermark(ctx, watermarkID, source, note, s.nowFn().UTC()); err != nil &&
		!errors.Is(err, store.ErrDuplicate) {
		return domain.Watermark{}, err
	}
	return s.st.GetWatermark(ctx, watermarkID)
}

// PublishRequest asks for a snapshot for one scope.
type PublishRequest struct {
	ScopeType   string `json:"scope_type"`
	ScopeKey    string `json:"scope_key"`
	WatermarkID string `json:"watermark_id"`
	Force       bool   `json:"force,omitempty"` // publish despite incomplete sources
}

// PublishResult is the API response of a publication attempt.
type PublishResult struct {
	Snapshot     *domain.Snapshot     `json:"snapshot,omitempty"`
	Watermark    domain.Watermark     `json:"watermark"`
	Skipped      bool                 `json:"skipped"`
	Reason       string               `json:"reason,omitempty"`
	Diff         *domain.SnapshotDiff `json:"diff,omitempty"`
	AssessmentID string               `json:"assessment_id,omitempty"`
}

// Publish computes and, when the gate allows it, appends a snapshot.
//
// Publication strategy is encoded in the watermark mode:
//   - wait_complete: nothing is published until every required source closed.
//     A late device message postpones the whole batch's availability.
//   - early_provisional: a provisional snapshot is published at the cutoff; once
//     sources close, a formal snapshot is published as a new version with an
//     explicit diff.
//
// In both modes, a formal snapshot still carries the fused status, so a
// shipment whose evidence is incomplete publishes formally as
// pending_evidence/conflict_pending/not_publishable and is flagged
// consumable_confirmed=false — a "confirmed" downstream status is never
// fabricated from partial data.
func (s *Service) Publish(ctx context.Context, req PublishRequest) (PublishResult, error) {
	wm, err := s.st.GetWatermark(ctx, req.WatermarkID)
	if err != nil {
		return PublishResult{}, err
	}
	if wm.ScopeType != "all" {
		if wm.ScopeType != req.ScopeType || wm.ScopeKey != req.ScopeKey {
			return PublishResult{}, fmt.Errorf("%w: watermark %s governs %s/%s, not %s/%s",
				ErrInvalid, wm.ID, wm.ScopeType, wm.ScopeKey, req.ScopeType, req.ScopeKey)
		}
	}
	cutoff := wm.CutoffReceivedAt
	if cutoff.IsZero() {
		cutoff = s.nowFn().UTC()
	}

	mode := wm.Mode
	kind := domain.SnapshotFormal
	gateReason := ""
	switch mode {
	case domain.ModeWaitComplete:
		if !wm.Complete && !req.Force {
			return PublishResult{
				Watermark: wm, Skipped: true,
				Reason: fmt.Sprintf("wait_complete: watermark not closed (missing sources %v); no snapshot published",
					wm.MissingSources),
			}, nil
		}
	case domain.ModeEarlyProvisional:
		if !wm.Complete {
			kind = domain.SnapshotProvisional
			gateReason = fmt.Sprintf("sources still open: %v; this version is provisional", wm.MissingSources)
		}
	}

	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return PublishResult{}, err
	}
	defer tx.Rollback(ctx)

	res, err := s.buildScope(ctx, tx, req.ScopeType, req.ScopeKey, cutoff, kind)
	if err != nil {
		return PublishResult{}, err
	}
	if wm.RequireHandover && !res.hasHandover && !req.Force {
		if mode == domain.ModeWaitComplete {
			return PublishResult{
				Watermark: wm, Skipped: true,
				Reason: "watermark requires a screening handover record; none received for this scope before the cutoff",
			}, nil
		}
		kind = domain.SnapshotProvisional
		gateReason = "required handover evidence missing; provisional only"
	}
	if kind == domain.SnapshotProvisional && gateReason == "" {
		gateReason = "provisional snapshot: source completeness or handover condition not met"
	}

	version, prev, err := s.st.NextSnapshotVersion(ctx, tx, req.ScopeType, req.ScopeKey)
	if err != nil {
		return PublishResult{}, err
	}
	payload := res.payload
	payload.SnapshotKind = kind
	payload.Watermark = domain.WatermarkRef{
		ID: wm.ID, Mode: wm.Mode, CutoffReceivedAt: cutoff,
		Complete: wm.Complete, Label: wm.Label,
	}
	payload.Note = gateReason
	consumable := kind == domain.SnapshotFormal && res.fusion.Status == domain.StatusVerified
	payload.ConsumableConfirmed = consumable
	var prevPayload *domain.SnapshotPayload
	prevVersion := 0
	if prev != nil {
		prevPayload = prev.Payload
		prevVersion = prev.Version
		payload.Lineage.PreviousSnapshotID = prev.ID
	}
	diff := domain.DiffSnapshots(prevPayload, prevVersion, payload)

	snap := domain.Snapshot{
		ID: uuid.NewString(), ScopeType: req.ScopeType, ScopeKey: req.ScopeKey,
		Version: version, Kind: kind, WatermarkID: wm.ID,
		AssessmentID: res.assessmentID, CutoffReceivedAt: cutoff,
		Status: res.fusion.Status, SecurityResult: res.fusion.SecurityResult,
		EvidenceCompleteness: res.fusion.EvidenceCompleteness,
		ConsumableConfirmed:  consumable,
		QualitySummary:       payload.QualitySummary,
		Diff:                 diff,
		PublishedAt:          s.nowFn().UTC(),
		Payload:              payload,
	}
	if prev != nil {
		snap.Supersedes = prev.ID
	}
	if err := s.st.InsertAssessment(ctx, tx, store.Assessment{
		ID: res.assessmentID, ScopeType: req.ScopeType, ScopeKey: req.ScopeKey,
		CutoffReceivedAt: cutoff, MaxSeq: payload.Lineage.MaxSeq,
		Status: res.fusion.Status, SecurityResult: res.fusion.SecurityResult,
		EvidenceCompleteness: res.fusion.EvidenceCompleteness,
		Result:               res.fusion, Inputs: payload.Lineage,
		ResultHash: payload.Lineage.ResultHash,
	}); err != nil {
		return PublishResult{}, err
	}
	if err := s.st.InsertSnapshot(ctx, tx, snap); err != nil {
		return PublishResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishResult{}, err
	}
	return PublishResult{
		Snapshot: &snap, Watermark: wm, Diff: &diff,
		AssessmentID: res.assessmentID, Reason: gateReason,
	}, nil
}

// scopeResult is the computed publication unit.
type scopeResult struct {
	fusion       domain.FusionResult
	payload      *domain.SnapshotPayload
	assessmentID string
	hasHandover  bool
}

// buildScope loads every input at the cutoff, folds the graph, evaluates rules
// and fuses the status for a shipment, container or leg scope.
func (s *Service) buildScope(ctx context.Context, tx pgx.Tx, scopeType, scopeKey string,
	cutoff time.Time, kind string) (*scopeResult, error) {
	links, _, err := s.st.LoadLinks(ctx, &cutoff)
	if err != nil {
		return nil, err
	}
	graph := domain.NewGraph(links)
	allEvents, err := s.st.ListEvents(ctx, store.EventFilter{UpToReceived: &cutoff})
	if err != nil {
		return nil, err
	}
	var maxSeq int64
	for _, ev := range allEvents {
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}
	shipmentKeys, err := s.scopeShipments(ctx, graph, scopeType, scopeKey, cutoff)
	if err != nil {
		return nil, err
	}
	if len(shipmentKeys) == 0 {
		return nil, fmt.Errorf("%w: no shipments resolve to %s %s at the cutoff", ErrInvalid, scopeType, scopeKey)
	}

	var bundles []shipmentBundle
	var unionEvents []*domain.Event
	unionIDs := map[string]bool{}
	var allViolations []domain.RuleViolation
	hasHandover := false
	for _, shipKey := range shipmentKeys {
		b, err := s.buildShipment(ctx, tx, graph, allEvents, shipKey, cutoff)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, b)
		for _, ev := range b.events {
			if !unionIDs[ev.EventID] {
				unionIDs[ev.EventID] = true
				unionEvents = append(unionEvents, ev)
			}
			if ev.MessageType == domain.TypeHandover &&
				(ev.Mode() == domain.ModeScreening || ev.Mode() == domain.ModeLoading) {
				hasHandover = true
			}
		}
		allViolations = append(allViolations, b.violations...)
	}

	fused := aggregateFusion(bundles)
	fused.InputRuleKeys = ruleKeysOf(allViolations)
	fused.InputEventIDs = idsOfEvents(unionEvents)
	scopedPins := scopePins(graph, shipmentKeys, cutoff)
	fused.InputGraphLinks = pinStrings(scopedPins)

	lineage := domain.Lineage{
		CutoffReceivedAt:  cutoff,
		MaxSeq:            maxSeq,
		InputEvents:       lineageEvents(unionEvents),
		InputGraphLinks:   scopedPins,
		InputRuleKeys:     fused.InputRuleKeys,
		InputCorrections:  fused.Corrections,
		Weights:           domain.DefaultEvidenceWeights(),
		CompletenessFloor: s.cfg.CompletenessFloor,
	}
	lineage.ResultHash = domain.HashResult(fused, lineage)

	timeline := domain.BuildTimeline(unionEvents, unionDispositions(graph, unionEvents, shipmentKeys[0], cutoff), cutoff)
	payload := &domain.SnapshotPayload{
		Scope:                  domain.ScopeRef{Type: scopeType, Key: scopeKey},
		Status:                 fused.Status,
		SecurityResult:         fused.SecurityResult,
		SnapshotKind:           kind,
		EvidenceCompleteness:   fused.EvidenceCompleteness,
		CompletenessComponents: fused.Components,
		Packages:               fused.Packages,
		PendingItems:           fused.PendingItems,
		Conflicts:              fused.Conflicts,
		Corrections:            fused.Corrections,
		Timeline:               timeline,
		ManifestRefs:           buildManifestRefs(unionEvents),
		Decision:               fused.Decision,
		BlockingReasons:        fused.BlockingReasons,
		QualitySummary:         buildQualitySummary(unionEvents, allViolations),
		Lineage:                lineage,
	}
	if scopeType == "shipment" {
		payload.CompositionVersions = graph.MembershipVersions(scopeKey)
		payload.DerivedFrom = []domain.SnapshotRef{{ScopeType: "shipment", ScopeKey: scopeKey}}
	} else {
		for _, b := range bundles {
			payload.DerivedFrom = append(payload.DerivedFrom, domain.SnapshotRef{
				ScopeType: "shipment", ScopeKey: b.key, Status: b.fusion.Status,
			})
		}
	}
	return &scopeResult{
		fusion: fused, payload: payload, assessmentID: uuid.NewString(),
		hasHandover: hasHandover,
	}, nil
}

type shipmentBundle struct {
	key        string
	events     []*domain.Event
	fusion     domain.FusionResult
	violations []domain.RuleViolation
}

func (s *Service) buildShipment(ctx context.Context, tx pgx.Tx, graph *domain.Graph,
	allEvents []*domain.Event, shipKey string, cutoff time.Time) (shipmentBundle, error) {
	evs := filterEventsForShipment(graph, allEvents, shipKey, cutoff)
	disp := dispositionMap(graph, evs, shipKey, cutoff)
	timeline := domain.BuildTimeline(evs, disp, cutoff)
	attempts, err := s.attemptsFor(ctx, evs, cutoff)
	if err != nil {
		return shipmentBundle{}, err
	}
	violations := domain.EvaluateRules(domain.RuleInput{
		ShipmentKey: shipKey, Events: evs, Timeline: timeline, Dispositions: disp,
		Attempts: attempts, Now: cutoff,
		Config: domain.RuleConfig{
			ClockDriftThreshold: s.cfg.ClockDriftThreshold,
			ConflictWindow:      s.cfg.ConflictWindow,
		},
	})
	for _, d := range disp {
		if err := s.st.SaveDisposition(ctx, tx, d); err != nil {
			return shipmentBundle{}, err
		}
	}
	if _, err := s.st.InsertViolations(ctx, tx, violations); err != nil {
		return shipmentBundle{}, err
	}
	return shipmentBundle{
		key: shipKey, events: evs, violations: violations,
		fusion: s.fuseShipment(graph, evs, shipKey, cutoff, violations),
	}, nil
}

// fuseShipment is the single fusion entry point used by publication, replay and
// preview, so all three produce identical results for identical inputs.
func (s *Service) fuseShipment(graph *domain.Graph, evs []*domain.Event, shipKey string,
	cutoff time.Time, violations []domain.RuleViolation) domain.FusionResult {
	disp := dispositionMap(graph, evs, shipKey, cutoff)
	return domain.Fuse(domain.FusionInput{
		ShipmentKey:       shipKey,
		KnowledgeAt:       cutoff,
		ActivePackages:    graph.ActivePackages(shipKey, cutoff, cutoff),
		Events:            evs,
		Dispositions:      disp,
		Violations:        violations,
		ManifestEvidence:  manifestEvidenceOf(evs),
		Weights:           domain.DefaultEvidenceWeights(),
		CompletenessFloor: s.cfg.CompletenessFloor,
		ConflictWindow:    s.cfg.ConflictWindow,
	})
}

// filterEventsForShipment returns events attributable to the shipment, either
// directly or via a package that ever belonged to it (so misaligned late
// evidence remains auditable rather than disappearing).
func filterEventsForShipment(graph *domain.Graph, all []*domain.Event, shipKey string, cutoff time.Time) []*domain.Event {
	pkgSet := map[string]bool{}
	for _, p := range graph.ActivePackages(shipKey, cutoff, cutoff) {
		pkgSet[p] = true
	}
	for _, p := range graph.ShipmentPackagesEver(shipKey) {
		pkgSet[p] = true
	}
	var out []*domain.Event
	for _, ev := range all {
		if ev.ShipmentKey == shipKey {
			out = append(out, ev)
			continue
		}
		if ev.PackageKey != "" && pkgSet[ev.PackageKey] {
			out = append(out, ev)
		}
	}
	return out
}

func dispositionMap(graph *domain.Graph, evs []*domain.Event, shipKey string, cutoff time.Time) map[string]domain.Disposition {
	out := map[string]domain.Disposition{}
	for _, ev := range evs {
		ship := ev.ShipmentKey
		if ship == "" && ev.PackageKey != "" {
			if s, ok := graph.ShipmentOfPackage(ev.PackageKey, ev.OccurredAt, cutoff); ok {
				ship = s
			}
		}
		if ship == "" {
			ship = shipKey
		}
		out[ev.EventID] = domain.ClassifyEvent(graph, ev, ship, cutoff)
	}
	return out
}

// unionDispositions classifies a mixed event set for timeline explanation.
func unionDispositions(graph *domain.Graph, evs []*domain.Event, fallbackShipment string, cutoff time.Time) map[string]domain.Disposition {
	return dispositionMap(graph, evs, fallbackShipment, cutoff)
}

// scopeShipments resolves the shipment keys a scope covers at the cutoff.
func (s *Service) scopeShipments(ctx context.Context, graph *domain.Graph, scopeType, scopeKey string, cutoff time.Time) ([]string, error) {
	switch scopeType {
	case "shipment":
		return []string{scopeKey}, nil
	case "container":
		var out []string
		for _, l := range graph.Links() {
			if l.Kind != domain.LinkPackageContainer || l.ObjectKey != scopeKey {
				continue
			}
			if l.KnowledgeAt.After(cutoff) {
				continue
			}
			if ship, ok := graph.ShipmentOfPackage(l.SubjectKey, cutoff, cutoff); ok {
				out = append(out, ship)
			}
			if ship, ok := graph.PackageWasMember(l.SubjectKey); ok {
				out = append(out, ship)
			}
		}
		out = domain.SortedUnique(out)
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: container %s has no linked shipments at the cutoff", ErrInvalid, scopeKey)
		}
		return out, nil
	case "leg":
		out := graph.ShipmentsOnLeg(scopeKey, cutoff, cutoff)
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: leg %s has no linked shipments at the cutoff", ErrInvalid, scopeKey)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: unknown scope_type %q", ErrInvalid, scopeType)
	}
}

// aggregateFusion folds per-shipment fusions into a single scope result. The
// published status is never better than the weakest shipment: one conflict
// inside a ULD makes the whole ULD a ruling case, which stops a loader from
// reading a partially-confirmed container as confirmed.
func aggregateFusion(bundles []shipmentBundle) domain.FusionResult {
	if len(bundles) == 1 {
		return bundles[0].fusion
	}
	var out domain.FusionResult
	statuses := make([]string, 0, len(bundles))
	completeness := 0.0
	pendingSeen := map[string]bool{}
	conflictSeen := map[string]bool{}
	blockSeen := map[string]bool{}
	reasonSeen := map[string]bool{}
	results := map[string]bool{}
	var decisions []string
	out.Components = componentAverages(bundles)
	for _, b := range bundles {
		statuses = append(statuses, b.fusion.Status)
		completeness += b.fusion.EvidenceCompleteness
		out.Packages = append(out.Packages, b.fusion.Packages...)
		out.Corrections = append(out.Corrections, b.fusion.Corrections...)
		for _, p := range b.fusion.PendingItems {
			k := p.Code + "|" + p.PackageKey
			if !pendingSeen[k] {
				pendingSeen[k] = true
				out.PendingItems = append(out.PendingItems, p)
			}
		}
		for _, c := range b.fusion.Conflicts {
			k := c.PackageKey + "@" + c.WindowStart.UTC().Format(time.RFC3339Nano)
			if !conflictSeen[k] {
				conflictSeen[k] = true
				out.Conflicts = append(out.Conflicts, c)
			}
		}
		for _, c := range b.fusion.BlockingRuleCodes {
			blockSeen[c] = true
		}
		for _, r := range b.fusion.BlockingReasons {
			if !reasonSeen[r] {
				reasonSeen[r] = true
				out.BlockingReasons = append(out.BlockingReasons, r)
			}
		}
		if b.fusion.SecurityResult != "" {
			results[b.fusion.SecurityResult] = true
		}
		decisions = append(decisions, fmt.Sprintf("%s: %s", b.key, b.fusion.Decision))
	}
	completeness /= float64(len(bundles))
	out.EvidenceCompleteness = round3(completeness)
	for c := range blockSeen {
		out.BlockingRuleCodes = append(out.BlockingRuleCodes, c)
	}
	sort.Strings(out.BlockingRuleCodes)
	out.Status = domain.AggregateContainerStatus(statuses)
	if out.Status == domain.StatusVerified && len(results) > 1 {
		out.Status = domain.StatusConflictPending
	}
	if len(results) == 1 {
		for r := range results {
			out.SecurityResult = r
		}
	}
	if out.Status != domain.StatusVerified {
		out.SecurityResult = ""
	} else if len(results) > 1 {
		out.Status = domain.StatusConflictPending
	}
	out.Decision = fmt.Sprintf("%d shipments folded (weakest status wins): %s",
		len(bundles), joinStrings(decisions, " | "))
	return out
}

func componentAverages(bundles []shipmentBundle) []domain.Component {
	base := bundles[0].fusion.Components
	out := make([]domain.Component, len(base))
	for i, c := range base {
		out[i] = c
		out[i].SourceEvents = append([]string{}, c.SourceEvents...)
		out[i].Missing = append([]string{}, c.Missing...)
	}
	n := float64(len(bundles))
	if n <= 1 {
		return out
	}
	for j := 1; j < len(bundles); j++ {
		for i, c := range bundles[j].fusion.Components {
			out[i].Score = round3(out[i].Score + c.Score)
			out[i].Contribution = round3(out[i].Contribution + c.Contribution)
			out[i].SourceEvents = domain.SortedUnique(append(out[i].SourceEvents, c.SourceEvents...))
			out[i].Missing = domain.SortedUnique(append(out[i].Missing, c.Missing...))
		}
	}
	for i := range out {
		out[i].Score = round3(out[i].Score / n)
		out[i].Contribution = round3(out[i].Contribution / n)
	}
	return out
}

// attemptsFor loads the intake audit facts for a set of events.
func (s *Service) attemptsFor(ctx context.Context, evs []*domain.Event, cutoff time.Time) ([]domain.AttemptInfo, error) {
	var ids, keys []string
	for _, ev := range evs {
		ids = append(ids, ev.EventID)
		if ev.IdempotencyKey != "" {
			keys = append(keys, ev.IdempotencyKey)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.st.AttemptsForShipment(ctx, ids, keys, cutoff)
	if err != nil {
		return nil, err
	}
	out := make([]domain.AttemptInfo, 0, len(rows))
	for _, r := range rows {
		ai := domain.AttemptInfo{
			Source: r.Source, MessageType: r.MessageType, IdempotencyKey: r.IdempotencyKey,
			Outcome: r.Outcome, ContentHash: r.ContentHash, OccurredAt: r.OccurredAt,
			ReceivedAt: r.ReceivedAt, OriginalEvent: r.RawEventID,
		}
		if m, ok := r.Detail["message"].(string); ok {
			ai.Message = m
		}
		out = append(out, ai)
	}
	return out, nil
}

func manifestEvidenceOf(evs []*domain.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.MessageType != domain.TypeFlightManifest {
			continue
		}
		out = append(out, strsPayload(ev.Payload, "required_evidence")...)
	}
	return domain.SortedUnique(out)
}

func buildQualitySummary(evs []*domain.Event, violations []domain.RuleViolation) domain.QualitySummary {
	qs := domain.QualitySummary{ViolationsByCode: map[string]int{}, Sources: []string{}, BlockingCodes: []string{}}
	unaligned := 0
	for _, ev := range evs {
		qs.EventCount++
		qs.MissingFields += len(ev.MissingFields)
		qs.Sources = append(qs.Sources, ev.Source)
	}
	qs.Sources = domain.SortedUnique(qs.Sources)
	for _, v := range violations {
		qs.ViolationsByCode[v.Code]++
		switch v.Severity {
		case domain.SevInfo:
			qs.Info++
		case domain.SevWarning:
			qs.Warnings++
		}
		if v.Blocking() {
			qs.BlockingCodes = append(qs.BlockingCodes, v.Code)
		}
		if v.Code == domain.RuleCrossPackageReference {
			unaligned++
		}
	}
	qs.BlockingCodes = domain.SortedUnique(qs.BlockingCodes)
	qs.UnalignedEvidence = unaligned
	return qs
}

func buildManifestRefs(evs []*domain.Event) []domain.ManifestRef {
	var out []domain.ManifestRef
	for _, ev := range evs {
		if ev.MessageType != domain.TypeFlightManifest {
			continue
		}
		ref := domain.ManifestRef{
			EventID: ev.EventID, Source: ev.Source,
			ReferencedStatus: strPayload(ev.Payload, "referenced_status"),
			RequiredEvidence: strsPayload(ev.Payload, "required_evidence"),
		}
		if ref.RequiredEvidence == nil {
			ref.RequiredEvidence = []string{}
		}
		for _, want := range ref.RequiredEvidence {
			found := false
			for _, other := range evs {
				for _, idx := range other.EvidenceIndexes {
					if idx == want {
						found = true
						break
					}
				}
			}
			if !found {
				ref.MissingEvidence = append(ref.MissingEvidence, want)
			}
		}
		out = append(out, ref)
	}
	return out
}

func lineageEvents(evs []*domain.Event) []domain.LineageEvent {
	out := make([]domain.LineageEvent, 0, len(evs))
	for _, ev := range evs {
		out = append(out, domain.LineageEvent{
			Seq: ev.Seq, EventID: ev.EventID, Source: ev.Source,
			MessageType: ev.MessageType, ReceivedAt: ev.ReceivedAt,
			ContentHash: ev.ContentHash,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// scopePins pins only the graph relationships that contribute to the scope:
// the shipments' membership links, their packages' container stows, and the
// legs of those containers. A shipment snapshot never pins an unrelated ULD.
func scopePins(graph *domain.Graph, shipmentKeys []string, cutoff time.Time) []domain.GraphPin {
	shipSet := map[string]bool{}
	for _, s := range shipmentKeys {
		shipSet[s] = true
	}
	packages := map[string]bool{}
	containers := map[string]bool{}
	legs := map[string]bool{}
	for _, l := range graph.Links() {
		if l.KnowledgeAt.After(cutoff) {
			continue
		}
		switch l.Kind {
		case domain.LinkShipmentPackage:
			if shipSet[l.SubjectKey] {
				packages[l.ObjectKey] = true
			}
		case domain.LinkPackageContainer:
			if packages[l.SubjectKey] {
				containers[l.ObjectKey] = true
			}
		case domain.LinkContainerLeg:
			if containers[l.SubjectKey] {
				legs[l.ObjectKey] = true
			}
		}
	}
	var out []domain.GraphPin
	for _, l := range graph.Links() {
		if l.KnowledgeAt.After(cutoff) {
			continue
		}
		keep := false
		switch l.Kind {
		case domain.LinkShipmentPackage:
			keep = shipSet[l.SubjectKey] || packages[l.ObjectKey]
		case domain.LinkPackageContainer:
			keep = packages[l.SubjectKey] || packages[l.ObjectKey]
		case domain.LinkContainerLeg:
			keep = containers[l.SubjectKey] || legs[l.ObjectKey]
		}
		if !keep {
			continue
		}
		out = append(out, domain.GraphPin{
			LinkKey: l.LinkKey, LinkVersion: l.LinkVersion, Kind: l.Kind,
			SubjectKey: l.SubjectKey, ObjectKey: l.ObjectKey,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LinkKey != out[j].LinkKey {
			return out[i].LinkKey < out[j].LinkKey
		}
		return out[i].LinkVersion < out[j].LinkVersion
	})
	return out
}

func pinStrings(pins []domain.GraphPin) []string {
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, fmt.Sprintf("%s@v%d", p.LinkKey, p.LinkVersion))
	}
	return out
}

func ruleKeysOf(vs []domain.RuleViolation) []string {
	out := make([]string, 0, len(vs))
	seen := map[string]bool{}
	for _, v := range vs {
		k := v.Code + "|" + v.DedupeKey
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func idsOfEvents(evs []*domain.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.EventID)
	}
	sort.Strings(out)
	return out
}

func joinStrings(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}
