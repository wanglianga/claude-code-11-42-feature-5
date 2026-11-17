package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/service"
	"cargo.example/secstate/internal/store"
)

// Event spec helpers. The demo builds envelopes by hand so the exact business
// meaning of each import is explicit.

type evSpec struct {
	key        string // logical name used to look the event up later
	source     string
	message    string
	sourceCode string
	idem       string
	occurred   time.Time
	received   time.Time
	deviceID   string
	deviceNo   string
	packageID  string
	revision   string
	keys       map[string]string
	evidence   []string
	payload    map[string]any
	supersedes string
	// raw spelling override for the encoding-inconsistency rule
	rawKeys map[string]string
}

func (r *Runner) send(ctx context.Context, s evSpec) (service.IngestOutcome, error) {
	raw, err := json.Marshal(s.payload)
	if err != nil {
		return service.IngestOutcome{}, err
	}
	env := domain.EventEnvelope{
		MessageType: s.message, SourceCode: s.sourceCode, IdempotencyKey: s.idem,
		OccurredAt: s.occurred, DeviceID: s.deviceID, DeviceEventNo: s.deviceNo,
		PackageID: s.packageID, PackageRevision: s.revision,
		BusinessKeys: s.keys, EvidenceIndexes: s.evidence,
		SupersedesEventID: s.supersedes, Payload: raw,
	}
	if !s.received.IsZero() {
		rt := s.received
		env.ReceivedAt = &rt
	}
	out, err := r.svc.Ingest(ctx, service.IngestRequest{Source: s.source, Env: env})
	if out.EventID != "" {
		r.ids[s.key] = out.EventID
	}
	return out, err
}

// mustSend fails the run on an unexpected error.
func (r *Runner) mustSend(ctx context.Context, s evSpec) service.IngestOutcome {
	out, err := r.send(ctx, s)
	if err != nil {
		panic(fmt.Sprintf("send %s: %v", s.key, err))
	}
	return out
}

// ---------------------------------------------------------------------------
// SHIP-1: the clean path, with provisional then formal publication.

func (r *Runner) cleanScenario(ctx context.Context) error {
	sec := r.section("1. 干净货件 " + ShipClean + "：暂定→正式分阶段发布")
	t := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	r.now = t

	r.mustSend(ctx, evSpec{
		key: "clean.decl", source: "declaration", message: domain.TypeDeclaration,
		sourceCode: "DEC-01", idem: "dec-1", occurred: t, received: t,
		keys:    map[string]string{"awb": "123-45678901"},
		payload: map[string]any{"awb": "12345678901", "packages": []string{PkgClean}, "shipper": "ACME CARGO"},
	})
	*sec = r.addStep(*sec, Step{Name: "declaration", Detail: "1 package declared for AWB 12345678901", OK: true})

	r.mustSend(ctx, evSpec{
		key: "clean.scan", source: "scanner", message: domain.TypeScan, sourceCode: "SCN-01",
		idem: "scan-1", occurred: t.Add(4 * time.Minute), received: t.Add(4*time.Minute + 30*time.Second),
		deviceID: "SCN-01", deviceNo: "000001", packageID: PkgClean, revision: "1",
		keys:     map[string]string{"awb": "123-45678901", "leg": "CA1234/2026-11-17/PEK-SHA"},
		evidence: []string{"IMG-CLEAN-1"},
		payload:  map[string]any{"mode": "arrival", "device_id": "SCN-01", "location": "RECV-2"},
	})
	r.mustSend(ctx, evSpec{
		key: "clean.handover", source: "custody", message: domain.TypeHandover, sourceCode: "CUS-01",
		idem: "ho-1", occurred: t.Add(10 * time.Minute), received: t.Add(10 * time.Minute),
		keys:    map[string]string{"awb": "123-45678901"},
		payload: map[string]any{"mode": "screening", "from_party": "warehouse", "to_party": "screening"},
	})
	r.mustSend(ctx, evSpec{
		key: "clean.screen", source: "screening", message: domain.TypeScreening, sourceCode: "XRY-01",
		idem: "xry-1", occurred: t.Add(11 * time.Minute), received: t.Add(12 * time.Minute),
		deviceID: "XRY-01", deviceNo: "000101", packageID: PkgClean, revision: "1",
		evidence: []string{"IMG-CLEAN-1", "IMG-CLEAN-1-CT"},
		payload:  map[string]any{"result": "CLEAR", "device_id": "XRY-01", "required_evidence_count": 2},
	})
	r.mustSend(ctx, evSpec{
		key: "clean.archive", source: "imaging", message: domain.TypeEvidence, sourceCode: "IMG-01",
		idem: "img-1", occurred: t.Add(13 * time.Minute), received: t.Add(13 * time.Minute),
		packageID: PkgClean, evidence: []string{"IMG-CLEAN-1", "IMG-CLEAN-1-CT"},
		payload: map[string]any{"archive_state": "archived"},
	})

	// Provisional watermark at 02:30; scanner and screening already reported.
	wmProv := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "shipment", ScopeKey: ShipClean,
		CutoffReceivedAt: time.Date(2026, 11, 17, 2, 30, 0, 0, time.UTC),
		RequiredSources:  []string{"declaration", "scanner", "custody", "screening", "imaging"},
		RequireHandover:  true, Mode: domain.ModeEarlyProvisional, Label: "PROV-0230",
	})
	r.closeWatermark(ctx, wmProv, "declaration")
	r.closeWatermark(ctx, wmProv, "screening")

	r.now = time.Date(2026, 11, 17, 2, 30, 0, 0, time.UTC)
	pub1, err := r.svc.Publish(ctx, service.PublishRequest{
		ScopeType: "shipment", ScopeKey: ShipClean, WatermarkID: wmProv,
	})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{
		Name: "publish provisional @02:30", OK: pub1.Snapshot != nil && pub1.Snapshot.Kind == domain.SnapshotProvisional,
		Detail: fmt.Sprintf("kind=%s status=%s completeness=%.3f consumable=%v",
			snapKind(pub1), snapStatus(pub1), snapCompleteness(pub1), snapConsumable(pub1)),
	})
	r.check("provisional published before sources closed",
		pub1.Snapshot != nil && pub1.Snapshot.Kind == domain.SnapshotProvisional,
		"early_provisional mode must publish at the cutoff even if sources are open")

	// Later: stowage, manifest and departure handover arrive; a second
	// watermark closes all sources and produces the formal version.
	r.now = time.Date(2026, 11, 17, 2, 45, 0, 0, time.UTC)
	r.mustSend(ctx, evSpec{
		key: "clean.stow", source: "stowage", message: domain.TypeContainerStow, sourceCode: "STW-01",
		idem: "stow-1", occurred: r.now, received: r.now,
		packageID: PkgClean, payload: map[string]any{"container_id": ContA, "slot": "AKE12345MU-A1"},
	})
	r.mustSend(ctx, evSpec{
		key: "clean.dephandover", source: "custody", message: domain.TypeHandover, sourceCode: "CUS-01",
		idem: "ho-clean-dep", occurred: r.now.Add(5 * time.Minute), received: r.now.Add(5 * time.Minute),
		keys:    map[string]string{"awb": "123-45678901"},
		payload: map[string]any{"mode": "loading", "from_party": "screening", "to_party": "ramp"},
	})
	r.mustSend(ctx, evSpec{
		key: "clean.manifest", source: "manifest", message: domain.TypeFlightManifest, sourceCode: "MAN-01",
		idem: "man-1", occurred: r.now.Add(10 * time.Minute), received: r.now.Add(10 * time.Minute),
		keys: map[string]string{"awb": "123-45678901", "leg": Leg1},
		payload: map[string]any{"container_id": ContA, "leg": Leg1, "flight_no": "CA1234",
			"referenced_status": "verified", "required_evidence": []string{"IMG-CLEAN-1"}},
	})
	wmFormal := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "shipment", ScopeKey: ShipClean,
		CutoffReceivedAt: time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC),
		RequiredSources:  []string{"declaration", "scanner", "custody", "screening", "imaging", "stowage", "manifest"},
		RequireHandover:  true, Mode: domain.ModeEarlyProvisional, Label: "FORMAL-0300",
	})
	for _, s := range []string{"declaration", "scanner", "custody", "screening", "imaging", "stowage", "manifest"} {
		r.closeWatermark(ctx, wmFormal, s)
	}
	r.now = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	pub2, err := r.svc.Publish(ctx, service.PublishRequest{
		ScopeType: "shipment", ScopeKey: ShipClean, WatermarkID: wmFormal,
	})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{
		Name: "publish formal @03:00", OK: pub2.Snapshot != nil && pub2.Snapshot.Kind == domain.SnapshotFormal,
		Detail: fmt.Sprintf("kind=%s status=%s completeness=%.3f consumable=%v diff=%s",
			snapKind(pub2), snapStatus(pub2), snapCompleteness(pub2), snapConsumable(pub2),
			diffSummary(pub2)),
	})
	r.check("formal version follows provisional with explicit diff",
		pub2.Snapshot != nil && pub2.Snapshot.Kind == domain.SnapshotFormal &&
			pub2.Snapshot.Version == 2 && pub2.Snapshot.Diff.PreviousVersion == 1,
		"the second publication must be a new version superseding the provisional one")
	r.check("verified clean shipment is consumable-confirmed",
		pub2.Snapshot != nil && snapStatus(pub2) == domain.StatusVerified && snapConsumable(pub2),
		"a formal verified snapshot is the only shape downstream may treat as confirmed")

	// Point-in-time: reading as of 02:35 (after the provisional but before the
	// formal) must return the provisional version downstream actually had.
	asOf, err := r.svc.Store().LatestSnapshot(ctx, store.SnapshotQuery{
		ScopeType: "shipment", ScopeKey: ShipClean,
		PublishedBefore: ptr(time.Date(2026, 11, 17, 2, 35, 0, 0, time.UTC)),
	})
	if err == nil {
		*sec = r.addStep(*sec, Step{
			Name: "as-of publication 02:35 read", OK: asOf.Version == 1 && asOf.Kind == domain.SnapshotProvisional,
			Detail: fmt.Sprintf("v%d %s status=%s", asOf.Version, asOf.Kind, asOf.Status),
		})
		r.check("point-in-time query returns the then-readable provisional version",
			asOf.Version == 1 && asOf.Kind == domain.SnapshotProvisional,
			"as-of must reproduce what downstream could have read at that time")
	}

	// Container snapshot derived from the only shipment inside it.
	wmCont := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "container", ScopeKey: ContA,
		CutoffReceivedAt: time.Date(2026, 11, 17, 3, 30, 0, 0, time.UTC),
		RequiredSources:  []string{"declaration", "scanner", "custody", "screening", "imaging", "stowage", "manifest"},
		RequireHandover:  true, Mode: domain.ModeEarlyProvisional,
	})
	for _, s := range []string{"declaration", "scanner", "custody", "screening", "imaging", "stowage", "manifest"} {
		r.closeWatermark(ctx, wmCont, s)
	}
	r.now = time.Date(2026, 11, 17, 3, 30, 0, 0, time.UTC)
	pubC, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "container", ScopeKey: ContA, WatermarkID: wmCont})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{
		Name: "publish container " + ContA, OK: pubC.Snapshot != nil,
		Detail: fmt.Sprintf("status=%s derived_from=%d shipment(s)", snapStatus(pubC), derivedCount(pubC)),
	})

	// Leg snapshot: aggregates every shipment whose container flies the leg.
	wmLeg := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "leg", ScopeKey: Leg1,
		CutoffReceivedAt: time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC),
		RequiredSources:  []string{"declaration", "scanner", "custody", "screening", "imaging", "stowage", "manifest"},
		RequireHandover:  true, Mode: domain.ModeEarlyProvisional,
	})
	for _, s := range []string{"declaration", "scanner", "custody", "screening", "imaging", "stowage", "manifest"} {
		r.closeWatermark(ctx, wmLeg, s)
	}
	r.now = time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
	pubL, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "leg", ScopeKey: Leg1, WatermarkID: wmLeg})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{
		Name: "publish leg " + Leg1, OK: pubL.Snapshot != nil,
		Detail: fmt.Sprintf("status=%s derived_from=%d shipment(s)", snapStatus(pubL), derivedCount(pubL)),
	})
	r.check("leg snapshot aggregates shipments across the leg",
		pubL.Snapshot != nil && derivedCount(pubL) >= 1,
		"the leg-level publication folds every shipment whose container flies that leg")

	// Audit replay of each published version of the clean shipment.
	for _, id := range snapshotIDs(pub1, pub2) {
		rep, err := r.svc.Replay(ctx, id)
		if err != nil {
			return err
		}
		*sec = r.addStep(*sec, Step{
			Name: "replay " + shortID(id), OK: rep.Match,
			Detail: fmt.Sprintf("published=%s recomputed=%s match=%v",
				rep.PublishedStatus, rep.RecomputedStatus, rep.Match),
		})
		r.check("clean shipment version replays deterministically", rep.Match,
			"every published version must be reproducible from its pinned inputs")
	}
	return nil
}

// ---------------------------------------------------------------------------
// SHIP-2: duplicate retransmission, split with late old-package evidence,
// misaligned evidence, correction publication.

func (r *Runner) duplicateSplitScenario(ctx context.Context) error {
	sec := r.section("2. 重复补传 + 拆包旧证据 " + ShipDupSplit)
	t := time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
	r.now = t

	r.mustSend(ctx, evSpec{
		key: "dup.decl", source: "declaration", message: domain.TypeDeclaration,
		sourceCode: "DEC-02", idem: "dec-2", occurred: t, received: t,
		keys:    map[string]string{"awb": "12345678902"},
		payload: map[string]any{"awb": "12345678902", "packages": []string{PkgDupA}, "shipper": "GLOBEX"},
	})
	t = t.Add(5 * time.Minute)
	r.mustSend(ctx, evSpec{
		key: "dup.scan", source: "scanner", message: domain.TypeScan, sourceCode: "SCN-02",
		idem: "scan-dup-1", occurred: t, received: t,
		deviceID: "SCN-02", deviceNo: "000002", packageID: PkgDupA, revision: "1",
		keys:     map[string]string{"awb": "12345678902"},
		evidence: []string{"IMG-DUP-1"},
		payload:  map[string]any{"mode": "arrival", "device_id": "SCN-02"},
	})
	r.mustSend(ctx, evSpec{
		key: "dup.handover", source: "custody", message: domain.TypeHandover, sourceCode: "CUS-02",
		idem: "ho-2", occurred: t.Add(time.Minute), received: t.Add(time.Minute),
		keys:    map[string]string{"awb": "12345678902"},
		payload: map[string]any{"mode": "screening", "from_party": "warehouse", "to_party": "screening"},
	})

	// Device screening for the parent package. Report conflict -> review fixes it.
	firstScreen := r.fileEvent(ctx, evSpec{
		key: "dup.screen1", source: "screening", message: domain.TypeScreening, sourceCode: "XRY-02",
		idem: "xry-dup-1", occurred: t.Add(7 * time.Minute), received: t.Add(8 * time.Minute),
		deviceID: "XRY-02", deviceNo: "000201", packageID: PkgDupA, revision: "1",
		evidence: []string{"IMG-DUP-2"},
		payload:  map[string]any{"result": "REJECT", "device_id": "XRY-02", "required_evidence_count": 1},
	})
	r.mustSend(ctx, evSpec{
		key: "dup.archive", source: "imaging", message: domain.TypeEvidence, sourceCode: "IMG-02",
		idem: "img-dup-1", occurred: t.Add(8 * time.Minute), received: t.Add(9 * time.Minute),
		packageID: PkgDupA, evidence: []string{"IMG-DUP-2"},
		payload: map[string]any{"archive_state": "archived"},
	})

	// 1) identical retransmission of the same device event -> suppressed.
	dupIdent := r.mustSendMust(ctx, evSpec{
		key: "dup.screen1.retx", source: "screening", message: domain.TypeScreening, sourceCode: "XRY-02",
		idem: "xry-dup-1-retrans", occurred: t.Add(7 * time.Minute),
		received: t.Add(10 * time.Minute), // later receipt, same content
		deviceID: "XRY-02", deviceNo: "000201", packageID: PkgDupA, revision: "1",
		evidence: []string{"IMG-DUP-2"},
		payload:  map[string]any{"result": "REJECT", "device_id": "XRY-02", "required_evidence_count": 1},
	})
	*sec = r.addStep(*sec, Step{Name: "identical retransmission", OK: dupIdent.Outcome == "duplicate_identical",
		Detail: "outcome=" + dupIdent.Outcome + " duplicate_of=" + dupIdent.DuplicateOf})
	r.check("identical retransmission suppressed as duplicate",
		dupIdent.Outcome == "duplicate_identical", "same idempotency content must not create a second event")

	// 2) same device event number but conflicts with content -> 409, original kept.
	conflict, err := r.send(ctx, evSpec{
		key: "dup.screen1.conflict", source: "screening", message: domain.TypeScreening, sourceCode: "XRY-02",
		idem: "xry-dup-1", occurred: t.Add(7 * time.Minute), received: t.Add(11 * time.Minute),
		deviceID: "XRY-02", deviceNo: "000201", packageID: PkgDupA, revision: "1",
		evidence: []string{"IMG-DUP-2"},
		payload:  map[string]any{"result": "CLEAR", "device_id": "XRY-02", "required_evidence_count": 1},
	})
	*sec = r.addStep(*sec, Step{Name: "conflicting retransmission",
		OK:     conflict.Outcome == "duplicate_conflicting" && err != nil,
		Detail: fmt.Sprintf("outcome=%s error=%v (original %s kept)", conflict.Outcome, err != nil, firstScreen)})
	r.check("conflicting retransmission quarantined, original retained",
		conflict.Outcome == "duplicate_conflicting", "a re-used idempotency key with new content must not overwrite")

	// 3) human review correcting the device result, using supersedes.
	review := r.mustSend(ctx, evSpec{
		key: "dup.review", source: "review", message: domain.TypeManualReview, sourceCode: "REV-02",
		idem: "rev-dup-1", occurred: t.Add(15 * time.Minute), received: t.Add(15 * time.Minute),
		packageID: PkgDupA, revision: "1",
		supersedes: firstScreen, evidence: []string{"IMG-DUP-2"},
		payload: map[string]any{"conclusion": "CLEAR", "reviewer": "op-7",
			"note": "图像复核确认结构件无违禁品，撤销设备误报"},
	})
	*sec = r.addStep(*sec, Step{Name: "review correction", OK: review.Outcome == "accepted",
		Detail: "review supersedes device verdict " + shortID(firstScreen)})
	r.check("correction references the event it replaces",
		review.EventID != "" && firstScreen != "", "supersedes relation is what makes the ruling auditable")

	// 4) split: PKG-DUP-1 -> 1A + 1B at 04:25. The graph creates a new version;
	// the old package's verdict must NOT be transferred to either child.
	t = t.Add(20 * time.Minute)
	split := r.mustSend(ctx, evSpec{
		key: "dup.split", source: "custody", message: domain.TypePackageChange, sourceCode: "CUS-02",
		idem: "split-dup-1", occurred: t, received: t,
		keys: map[string]string{"awb": "12345678902"},
		payload: map[string]any{"action": "split", "parent_packages": []string{PkgDupA},
			"child_packages": []string{PkgSplitA1, PkgSplitA2}, "reason": "security repackaging"},
	})
	*sec = r.addStep(*sec, Step{Name: "split at 04:25", OK: split.Outcome == "accepted",
		Detail: "PKG-DUP-1 -> PKG-SPLIT-1A + PKG-SPLIT-1B (new composition version, not a mutation)"})

	// 5) late scan that still cites the OLD package, arriving after the split.
	late := r.mustSend(ctx, evSpec{
		key: "dup.latestale", source: "scanner", message: domain.TypeScan, sourceCode: "SCN-02",
		idem: "scan-dup-stale", occurred: t.Add(-2 * time.Minute), received: t.Add(6 * time.Minute),
		deviceID: "SCN-02", deviceNo: "000090", packageID: PkgDupA, revision: "1",
		keys:     map[string]string{"awb": "12345678902"},
		evidence: []string{"IMG-DUP-STALE"},
		payload:  map[string]any{"mode": "transfer", "device_id": "SCN-02"},
	})
	_ = late
	*sec = r.addStep(*sec, Step{Name: "late scan citing retired package", OK: true,
		Detail: "arrived 6 minutes after the split; cites PKG-DUP-1 which no longer exists as a version"})

	// Children are screened independently so the shipment can reach verified.
	for i, child := range []string{PkgSplitA1, PkgSplitA2} {
		pkg := child
		r.mustSend(ctx, evSpec{
			key: "dup.child.screen." + pkg, source: "screening", message: domain.TypeScreening,
			sourceCode: "XRY-02", idem: "xry-child-" + pkg, occurred: t.Add(time.Duration(8+i) * time.Minute),
			received: t.Add(time.Duration(9+i) * time.Minute),
			deviceID: "XRY-03", deviceNo: fmt.Sprintf("00030%d", i), packageID: pkg, revision: "1",
			evidence: []string{"IMG-" + pkg},
			payload:  map[string]any{"result": "CLEAR", "device_id": "XRY-03", "required_evidence_count": 1},
		})
		r.mustSend(ctx, evSpec{
			key: "dup.child.archive." + pkg, source: "imaging", message: domain.TypeEvidence,
			sourceCode: "IMG-02", idem: "img-child-" + pkg, occurred: t.Add(time.Duration(10+i) * time.Minute),
			received:  t.Add(time.Duration(10+i) * time.Minute),
			packageID: pkg, evidence: []string{"IMG-" + pkg},
			payload: map[string]any{"archive_state": "archived"},
		})
	}
	*sec = r.addStep(*sec, Step{Name: "children re-screened", OK: true,
		Detail: "both split children carry their own screening verdict + evidence archive"})

	// Encoding inconsistency: the manifest source writes the AWB without dash.
	r.mustSend(ctx, evSpec{
		key: "dup.manifest", source: "manifest", message: domain.TypeFlightManifest, sourceCode: "MAN-02",
		idem: "man-dup-1", occurred: t.Add(25 * time.Minute), received: t.Add(25 * time.Minute),
		keys:    map[string]string{"awb": "1234567892", "leg": "CA1234/2026-11-17/PEK-SHA"},
		payload: map[string]any{"container_id": ContA, "leg": "CA1234/2026-11-17/PEK-SHA", "flight_no": "CA1234"},
	})

	// Publish with a wait_complete watermark, forcing the split/mismatch audit.
	wmWC := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "shipment", ScopeKey: ShipDupSplit,
		CutoffReceivedAt: t.Add(30 * time.Minute),
		RequiredSources:  []string{"declaration", "scanner", "custody", "screening", "imaging", "manifest"},
		RequireHandover:  true, Mode: domain.ModeEarlyProvisional,
	})
	for _, s := range []string{"declaration", "scanner", "custody", "screening", "imaging", "manifest"} {
		r.closeWatermark(ctx, wmWC, s)
	}
	r.now = t.Add(30 * time.Minute)
	pub, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "shipment", ScopeKey: ShipDupSplit, WatermarkID: wmWC})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{Name: "publish split shipment v1", OK: pub.Snapshot != nil,
		Detail: fmt.Sprintf("status=%s completeness=%.3f consumable=%v",
			snapStatus(pub), snapCompleteness(pub), snapConsumable(pub))})
	r.check("split shipment does not silently inherit the retired parent's verdict",
		pub.Snapshot != nil && snapStatus(pub) != domain.StatusVerified,
		"the children were never covered by the original declaration, so v1 cannot be verified")

	// The shipping party re-declares the two split children. This is the
	// expected way to make the successor versions declaratively covered; then
	// the next publication converges to verified.
	r.mustSend(ctx, evSpec{
		key: "dup.redecl", source: "declaration", message: domain.TypeDeclaration, sourceCode: "DEC-02",
		idem: "dec-2-redecl", occurred: t.Add(28 * time.Minute), received: t.Add(28 * time.Minute),
		keys: map[string]string{"awb": "12345678902"},
		payload: map[string]any{"awb": "12345678902",
			"packages": []string{PkgSplitA1, PkgSplitA2}, "shipper": "GLOBEX"},
	})
	r.now = t.Add(31 * time.Minute)
	wmRe := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "shipment", ScopeKey: ShipDupSplit,
		CutoffReceivedAt: t.Add(32 * time.Minute),
		RequiredSources:  []string{"declaration", "scanner", "custody", "screening", "imaging", "manifest"},
		RequireHandover:  true, Mode: domain.ModeEarlyProvisional,
	})
	for _, s := range []string{"declaration", "scanner", "custody", "screening", "imaging", "manifest"} {
		r.closeWatermark(ctx, wmRe, s)
	}
	pub2, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "shipment", ScopeKey: ShipDupSplit, WatermarkID: wmRe})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{Name: "publish split shipment v2 (after re-declaration)",
		OK: pub2.Snapshot != nil,
		Detail: fmt.Sprintf("status=%s completeness=%.3f diff=%s",
			snapStatus(pub2), snapCompleteness(pub2), diffSummary(pub2))})
	r.check("re-declaration converges the split shipment in a new version",
		pub2.Snapshot != nil && pub2.Snapshot.Version == 2 &&
			snapStatus(pub2) == domain.StatusVerified,
		"post-split re-declaration is what makes the successor packages contributable")

	// Verify: stale evidence is flagged misaligned, verifiable from the graph/disposition views.
	links, _, err := r.svc.Store().LoadLinks(ctx, nil)
	if err != nil {
		return err
	}
	g := domain.NewGraph(links)
	cut := t.Add(30 * time.Minute)
	q, err := r.svc.Quality(ctx, ShipDupSplit, &cut)
	if err != nil {
		return err
	}
	var crossPkg int
	for _, v := range q.Violations {
		if v.Code == domain.RuleCrossPackageReference {
			crossPkg++
		}
	}
	*sec = r.addStep(*sec, Step{Name: "quality after split", OK: crossPkg > 0,
		Detail: fmt.Sprintf("%d cross-package/misaligned findings", crossPkg)})

	// Late evidence is superseded and not reattributed.
	staleID := r.ids["dup.latestale"]
	if staleID != "" {
		if ev, err := r.svc.Store().EventByID(ctx, staleID); err == nil {
			d := domain.ClassifyEvent(g, ev, ShipDupSplit, cut)
			*sec = r.addStep(*sec, Step{Name: "stale scan disposition",
				OK:     d.Disposition == domain.DispositionSupersededPackage,
				Detail: "disposition=" + d.Disposition + "; " + d.Reason})
			r.check("late old-package scan is misaligned, not auto-transferred",
				d.Disposition == domain.DispositionSupersededPackage,
				"a package retired by a split must not have its evidence silently attached to a child")
			attributed := false
			if pub.Snapshot != nil && pub.Snapshot.Payload != nil {
				for _, ps := range pub.Snapshot.Payload.Packages {
					for _, e := range ps.ScreeningEvents {
						if e == staleID {
							attributed = true
						}
					}
				}
			}
			r.check("stale scan appears in no child's evidence set", !attributed,
				"the misaligned event must be retained for audit but absent from every successor's verdict inputs")
		}
	}

	// Composition versions prove the split produced a new version, not a mutation.
	comp := g.MembershipVersions(ShipDupSplit)
	*sec = r.addStep(*sec, Step{Name: "composition versions", OK: len(comp) >= 2,
		Detail: fmt.Sprintf("%d versions: %v", len(comp), versionSummaries(comp))})
	r.check("split produced a new composition version",
		len(comp) >= 2 && hasVersion(comp, []string{PkgSplitA1, PkgSplitA2}),
		"the parent membership must be retired and the two children added as a new version")

	// A correction-aware replay of every split-shipment publication.
	for _, p := range []service.PublishResult{pub, pub2} {
		if p.Snapshot == nil {
			continue
		}
		rep, err := r.svc.Replay(ctx, p.Snapshot.ID)
		if err != nil {
			return err
		}
		*sec = r.addStep(*sec, Step{Name: "replay split v" + fmt.Sprint(p.Snapshot.Version), OK: rep.Match,
			Detail: "published=" + rep.PublishedStatus + " recomputed=" + rep.RecomputedStatus})
		r.check("split shipment publication replays deterministically", rep.Match,
			"history including the split, the correction and the re-declaration must reproduce")
	}
	return nil
}

// ---------------------------------------------------------------------------
// SHIP-3: contradictory device results with no handover -> conflict_pending.

func (r *Runner) conflictScenario(ctx context.Context) error {
	sec := r.section("3. 跨来源冲突 " + ShipConflict + "：冲突待裁定而非代选")
	t := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	r.now = t
	r.mustSend(ctx, evSpec{
		key: "conf.decl", source: "declaration", message: domain.TypeDeclaration, sourceCode: "DEC-03",
		idem: "dec-3", occurred: t, received: t,
		keys:    map[string]string{"awb": "65432100003"},
		payload: map[string]any{"awb": "65432100003", "packages": []string{PkgConfA}, "shipper": "INITECH"},
	})
	r.mustSend(ctx, evSpec{
		key: "conf.scan", source: "scanner", message: domain.TypeScan, sourceCode: "SCN-03",
		idem: "scan-3", occurred: t.Add(3 * time.Minute), received: t.Add(3 * time.Minute),
		deviceID: "SCN-03", deviceNo: "000003", packageID: PkgConfA, revision: "1",
		keys: map[string]string{"awb": "65432100003"}, evidence: []string{"IMG-CONF-1"},
		payload: map[string]any{"mode": "arrival", "device_id": "SCN-03"},
	})
	// Two machines disagree within ~4 minutes; no handover separates them.
	r.mustSend(ctx, evSpec{
		key: "conf.screen.clear", source: "screening", message: domain.TypeScreening, sourceCode: "XRY-04",
		idem: "xry-conf-clear", occurred: t.Add(6 * time.Minute), received: t.Add(6 * time.Minute),
		deviceID: "XRY-04", deviceNo: "000401", packageID: PkgConfA, revision: "1",
		evidence: []string{"IMG-CONF-A"},
		payload:  map[string]any{"result": "CLEAR", "device_id": "XRY-04", "required_evidence_count": 1},
	})
	r.mustSend(ctx, evSpec{
		key: "conf.screen.reject", source: "screening", message: domain.TypeScreening, sourceCode: "XRY-05",
		idem: "xry-conf-reject", occurred: t.Add(9 * time.Minute), received: t.Add(9 * time.Minute),
		deviceID: "XRY-05", deviceNo: "000501", packageID: PkgConfA, revision: "1",
		evidence: []string{"IMG-CONF-B"},
		payload:  map[string]any{"result": "REJECT", "device_id": "XRY-05", "required_evidence_count": 1},
	})
	// The manifest references status but the evidence archive never arrived.
	cut := t.Add(20 * time.Minute)
	wm := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "shipment", ScopeKey: ShipConflict, CutoffReceivedAt: cut,
		RequiredSources: []string{"declaration", "scanner", "screening", "manifest"},
		RequireHandover: false, Mode: domain.ModeEarlyProvisional,
	})
	for _, s := range []string{"declaration", "scanner", "screening", "manifest"} {
		r.closeWatermark(ctx, wm, s)
	}
	r.now = cut
	pub, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "shipment", ScopeKey: ShipConflict, WatermarkID: wm})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{Name: "publish conflict shipment", OK: pub.Snapshot != nil,
		Detail: fmt.Sprintf("status=%s conflicts=%d consumable=%v",
			snapStatus(pub), conflictCount(pub), snapConsumable(pub))})
	r.check("contradictory device results publish as conflict_pending",
		pub.Snapshot != nil && pub.Snapshot.Status == domain.StatusConflictPending,
		"without handover evidence the platform must not pick a verdict")
	r.check("conflict snapshot is not consumable-confirmed",
		pub.Snapshot != nil && !pub.Snapshot.ConsumableConfirmed,
		"foreign systems must not treat a conflict as a confirmed status")

	// Even a forced formal publish stays conflict_pending and non-consumable.
	wmForce := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "shipment", ScopeKey: ShipConflict, CutoffReceivedAt: cut,
		RequiredSources: []string{"declaration", "scanner", "screening", "manifest"},
		RequireHandover: false, Mode: domain.ModeWaitComplete,
	})
	// Not all sources closed -> wait mode must skip.
	pm, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "shipment", ScopeKey: ShipConflict, WatermarkID: wmForce})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{Name: "wait_complete with open sources", OK: pm.Skipped,
		Detail: "skipped=" + fmt.Sprint(pm.Skipped) + " reason=" + pm.Reason})
	r.check("wait_complete postpones publication until sources close", pm.Skipped,
		"the wait strategy deliberately holds the batch so no provisional value reaches downstream")

	for _, s := range []string{"declaration", "scanner", "screening", "manifest"} {
		r.closeWatermark(ctx, wmForce, s)
	}
	pf, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "shipment", ScopeKey: ShipConflict, WatermarkID: wmForce, Force: true})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{Name: "forced formal publish", OK: pf.Snapshot != nil,
		Detail: fmt.Sprintf("kind=%s status=%s consumable=%v",
			snapKind(pf), snapStatus(pf), snapConsumable(pf))})
	r.check("even a forced formal snapshot keeps conflict_pending and is not consumable",
		pf.Snapshot != nil && snapStatus(pf) == domain.StatusConflictPending && !snapConsumable(pf),
		"forcing the gate must never turn an unresolved conflict into a confirmed status")
	if pf.Snapshot != nil {
		rep, err := r.svc.Replay(ctx, pf.Snapshot.ID)
		if err != nil {
			return err
		}
		r.check("conflict snapshot replays deterministically", rep.Match,
			"the conflict set and decision must be reproducible")
	}
	return nil
}

// ---------------------------------------------------------------------------
// SHIP-4: manifest references required evidence the ledger never received.

func (r *Runner) manifestGapScenario(ctx context.Context) error {
	sec := r.section("4. 舱单缺证据 " + ShipManifest + "：不得签发确认状态，只输出质量告警与待补证")
	t := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	r.now = t
	r.mustSend(ctx, evSpec{
		key: "man.decl", source: "declaration", message: domain.TypeDeclaration, sourceCode: "DEC-04",
		idem: "dec-4", occurred: t, received: t,
		keys:    map[string]string{"awb": "98765400004"},
		payload: map[string]any{"awb": "98765400004", "packages": []string{PkgManif}, "shipper": "HELIOS"},
	})
	r.mustSend(ctx, evSpec{
		key: "man.scan", source: "scanner", message: domain.TypeScan, sourceCode: "SCN-04",
		idem: "scan-4", occurred: t.Add(2 * time.Minute), received: t.Add(2 * time.Minute),
		deviceID: "SCN-04", deviceNo: "000004", packageID: PkgManif, revision: "1",
		keys: map[string]string{"awb": "98765400004"}, evidence: []string{"IMG-MAN-1"},
		payload: map[string]any{"mode": "arrival", "device_id": "SCN-04"},
	})
	r.mustSend(ctx, evSpec{
		key: "man.handover", source: "custody", message: domain.TypeHandover, sourceCode: "CUS-04",
		idem: "ho-4", occurred: t.Add(3 * time.Minute), received: t.Add(3 * time.Minute),
		keys:    map[string]string{"awb": "98765400004"},
		payload: map[string]any{"mode": "screening", "from_party": "warehouse", "to_party": "screening"},
	})
	// Verdict claims only one evidence index.
	r.mustSend(ctx, evSpec{
		key: "man.screen", source: "screening", message: domain.TypeScreening, sourceCode: "XRY-06",
		idem: "xry-man-1", occurred: t.Add(6 * time.Minute), received: t.Add(6 * time.Minute),
		deviceID: "XRY-06", deviceNo: "000601", packageID: PkgManif, revision: "1",
		evidence: []string{"IMG-MAN-1"},
		payload:  map[string]any{"result": "CLEAR", "device_id": "XRY-06", "required_evidence_count": 1},
	})
	r.mustSend(ctx, evSpec{
		key: "man.archive", source: "imaging", message: domain.TypeEvidence, sourceCode: "IMG-04",
		idem: "img-man-1", occurred: t.Add(7 * time.Minute), received: t.Add(7 * time.Minute),
		packageID: PkgManif, evidence: []string{"IMG-MAN-1"},
		payload: map[string]any{"archive_state": "archived"},
	})
	// The manifest cites a status AND a required evidence index never seen.
	r.mustSend(ctx, evSpec{
		key: "man.manifest", source: "manifest", message: domain.TypeFlightManifest, sourceCode: "MAN-04",
		idem: "man-4", occurred: t.Add(10 * time.Minute), received: t.Add(10 * time.Minute),
		keys: map[string]string{"awb": "98765400004", "leg": "CA1234/2026-11-17/PEK-SHA"},
		payload: map[string]any{"container_id": ContB, "leg": "CA1234/2026-11-17/PEK-SHA",
			"flight_no": "CA1234", "referenced_status": "verified", "required_evidence": []string{"IMG-MAN-1", "IMG-MAN-2"}},
	})
	cut := t.Add(15 * time.Minute)
	wm := r.createWatermark(ctx, service.CreateWatermarkRequest{
		ScopeType: "shipment", ScopeKey: ShipManifest, CutoffReceivedAt: cut,
		RequiredSources: []string{"declaration", "scanner", "custody", "screening", "imaging", "manifest"},
		RequireHandover: true, Mode: domain.ModeWaitComplete,
	})
	// Force close is NOT done: all sources are closed as far as the platform
	// knows, yet the manifest's required evidence is still missing.
	for _, s := range []string{"declaration", "scanner", "custody", "screening", "imaging", "manifest"} {
		r.closeWatermark(ctx, wm, s)
	}
	r.now = cut
	pub, err := r.svc.Publish(ctx, service.PublishRequest{ScopeType: "shipment", ScopeKey: ShipManifest, WatermarkID: wm})
	if err != nil {
		return err
	}
	*sec = r.addStep(*sec, Step{Name: "publish manifest-gap shipment", OK: pub.Snapshot != nil,
		Detail: fmt.Sprintf("status=%s consumable=%v pending=%v",
			snapStatus(pub), snapConsumable(pub), pendingCodes(pub))})
	r.check("manifest citing missing required evidence cannot yield a confirmed status",
		pub.Snapshot != nil && !snapConsumable(pub) &&
			(pub.Snapshot.Status == domain.StatusPendingEvidence || pub.Snapshot.Status == domain.StatusNotPublishable),
		"the snapshot must publish only quality warnings and pending-evidence items")
	r.check("manifest evidence gap is surfaced as a pending/blocking item",
		hasPendingCode(pub, "manifest_required_evidence_missing") ||
			hasPendingCode(pub, "evidence_not_archived") ||
			hasPendingCode(pub, "evidence_index_missing"),
		"a missing required evidence index must be visible to the operator")
	if pub.Snapshot != nil {
		rep, err := r.svc.Replay(ctx, pub.Snapshot.ID)
		if err != nil {
			return err
		}
		r.check("manifest-gap snapshot replays deterministically", rep.Match,
			"the refusal to confirm must be reproducible from history")
	}
	return nil
}

// verifyAtEnd runs a final "no mutation overwrote history" check across the
// ledger and asserts every publication replays.
func (r *Runner) verifyAtEnd(ctx context.Context) {
	sec := r.section("6. 收尾校验：账本不可变 + 所有版本可复演 + 顶层约束")
	n, err := r.svc.Store().Pool().Exec(ctx, "")
	_ = n
	_ = err

	// Append-only proof: an UPDATE on raw_events must be rejected by the DB.
	_, upErr := r.svc.Store().Pool().Exec(ctx, `UPDATE raw_events SET payload='{}' WHERE seq=1`)
	if upErr != nil {
		*sec = r.addStep(*sec, Step{Name: "append-only guard", OK: true,
			Detail: "UPDATE raw_events rejected by trigger: " + upErr.Error()})
		r.check("ledger rejects overwrite", true, "UPDATE on raw_events is blocked by the DB")
	} else {
		*sec = r.addStep(*sec, Step{Name: "append-only guard", OK: false, Detail: "UPDATE unexpectedly succeeded"})
		r.check("ledger rejects overwrite", false, "an UPDATE on raw_events succeeded; history is mutable")
	}

	// Every published snapshot replays.
	snaps, err := r.svc.Store().ListAllSnapshots(ctx, 200)
	if err == nil {
		bad := 0
		for _, sn := range snaps {
			rep, err := r.svc.Replay(ctx, sn.ID)
			if err != nil || !rep.Match {
				bad++
			}
		}
		*sec = r.addStep(*sec, Step{Name: "replay all snapshots", OK: bad == 0,
			Detail: fmt.Sprintf("%d/%d snapshots replay deterministically", len(snaps)-bad, len(snaps))})
		r.check("all published versions replay deterministically", bad == 0,
			"each downstream state must be reproducible by historical version")
	}

	// Summary of produced statuses.
	statuses := map[string]int{}
	for _, sn := range snaps {
		statuses[sn.Status]++
	}
	*sec = r.addStep(*sec, Step{Name: "status coverage", OK: len(statuses) >= 3,
		Detail: fmt.Sprintf("%v", statuses)})
}

// ---------------------------------------------------------------------------
// helpers

func (r *Runner) fileEvent(ctx context.Context, s evSpec) string {
	out := r.mustSend(ctx, s)
	r.ids[s.key] = out.EventID
	return out.EventID
}

func (r *Runner) mustSendMust(ctx context.Context, s evSpec) service.IngestOutcome {
	out, err := r.send(ctx, s)
	if err != nil {
		panic(fmt.Sprintf("send %s: %v", s.key, err))
	}
	return out
}

func (r *Runner) createWatermark(ctx context.Context, req service.CreateWatermarkRequest) string {
	wm, err := r.svc.CreateWatermark(ctx, req)
	if err != nil {
		panic(fmt.Sprintf("create watermark %s: %v", req.Label, err))
	}
	return wm.ID
}

func (r *Runner) closeWatermark(ctx context.Context, id, source string) {
	if _, err := r.svc.CloseWatermarkSource(ctx, id, source, "source reported complete"); err != nil {
		panic(fmt.Sprintf("close watermark %s/%s: %v", id, source, err))
	}
}

func ptr(t time.Time) *time.Time { return &t }

func snapKind(p service.PublishResult) string {
	if p.Snapshot == nil {
		return "-"
	}
	return p.Snapshot.Kind
}
func snapStatus(p service.PublishResult) string {
	if p.Snapshot == nil {
		return "-"
	}
	return p.Snapshot.Status
}
func snapCompleteness(p service.PublishResult) float64 {
	if p.Snapshot == nil {
		return 0
	}
	return p.Snapshot.EvidenceCompleteness
}
func snapConsumable(p service.PublishResult) bool {
	return p.Snapshot != nil && p.Snapshot.ConsumableConfirmed
}
func diffSummary(p service.PublishResult) string {
	if p.Diff == nil {
		return "-"
	}
	return p.Diff.Summary
}
func conflictCount(p service.PublishResult) int {
	if p.Snapshot == nil || p.Snapshot.Payload == nil {
		return 0
	}
	return len(p.Snapshot.Payload.Conflicts)
}
func derivedCount(p service.PublishResult) int {
	if p.Snapshot == nil || p.Snapshot.Payload == nil {
		return 0
	}
	return len(p.Snapshot.Payload.DerivedFrom)
}
func pendingCodes(p service.PublishResult) []string {
	if p.Snapshot == nil || p.Snapshot.Payload == nil {
		return nil
	}
	out := []string{}
	for _, it := range p.Snapshot.Payload.PendingItems {
		out = append(out, it.Code)
	}
	sort.Strings(out)
	return domain.SortedUnique(out)
}
func hasPendingCode(p service.PublishResult, code string) bool {
	if p.Snapshot == nil || p.Snapshot.Payload == nil {
		return false
	}
	for _, it := range p.Snapshot.Payload.PendingItems {
		if it.Code == code {
			return true
		}
	}
	return false
}
func snapshotIDs(ps ...service.PublishResult) []string {
	var out []string
	for _, p := range ps {
		if p.Snapshot != nil {
			out = append(out, p.Snapshot.ID)
		}
	}
	return out
}
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
func versionSummaries(vs []domain.CompositionVersion) []string {
	var out []string
	for _, v := range vs {
		out = append(out, fmt.Sprintf("v%d(%s)", v.Version, joinShort(v.PackageKeys)))
	}
	return out
}
func joinShort(xs []string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += "+"
		}
		if len(x) > 10 {
			x = x[len(x)-10:]
		}
		s += x
	}
	return s
}
func hasVersion(vs []domain.CompositionVersion, pkgs []string) bool {
	for _, v := range vs {
		if sameSet(v.PackageKeys, pkgs) {
			return true
		}
	}
	return false
}
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	am := map[string]bool{}
	for _, x := range a {
		am[x] = true
	}
	for _, x := range b {
		if !am[x] {
			return false
		}
	}
	return true
}
