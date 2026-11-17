package domain

import (
	"testing"
	"time"
)

func T(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }

func TestNormalizeKey(t *testing.T) {
	cases := []struct {
		kind, raw, want string
		canonical       bool
		ok              bool
	}{
		{KeyAwb, "123-45678901", "AWB12345678901", false, true},
		{KeyAwb, "12345678901", "AWB12345678901", false, true},
		{KeyAwb, "AWB12345678901", "AWB12345678901", true, true},
		{KeyAwb, "1234567892", "", false, false}, // 10 digits after prefix: invalid
		{KeyLeg, "ca1234/2026-11-17/pek-sha", "CA1234/2026-11-17/PEK-SHA", false, true},
		{KeyLeg, "CA1234/2026-11-17/PEK-SHA", "CA1234/2026-11-17/PEK-SHA", true, true},
		{KeyContainer, "ake-12345-mu", "AKE12345MU", false, true},
		{KeyContainer, "AKE12345MU", "AKE12345MU", true, true},
		{KeyPackage, "pkg-a-01", "PKG-A-01", false, true},
	}
	for _, c := range cases {
		got, canonical, ok := NormalizeKey(c.kind, c.raw)
		if ok != c.ok {
			t.Fatalf("%s %q: ok=%v want %v", c.kind, c.raw, ok, c.ok)
		}
		if !ok {
			continue
		}
		if got != c.want {
			t.Errorf("%s %q: got %q want %q", c.kind, c.raw, got, c.want)
		}
		if canonical != c.canonical {
			t.Errorf("%s %q: canonical=%v want %v", c.kind, c.raw, canonical, c.canonical)
		}
	}
}

func TestCanonicalPackageRevision(t *testing.T) {
	for _, c := range [][2]string{{"Rev 2", "2"}, {"v3", "3"}, {"4", "4"}, {" rev1 ", "1"}} {
		if got := CanonicalPackageRevision(c[0]); got != c[1] {
			t.Errorf("%q -> %q want %q", c[0], got, c[1])
		}
	}
}

func TestValidatePayload(t *testing.T) {
	def := ContractDefinition{Policy: "backward", Fields: []FieldSpecBinding{
		{Name: "result", Spec: FieldSpec{Kind: KindEnum, Required: true, Enum: []string{"CLEAR", "REJECT"}}},
		{Name: "count", Spec: FieldSpec{Kind: KindNumber, Required: false}},
		{Name: "at", Spec: FieldSpec{Kind: KindTime, Required: false}},
	}}
	ok := ValidatePayload(def, map[string]any{"result": "CLEAR", "count": 2.0})
	if !ok.OK || len(ok.MissingFields) != 0 {
		t.Fatalf("expected valid, got %+v", ok)
	}
	missing := ValidatePayload(def, map[string]any{"count": 2.0})
	if missing.OK {
		t.Fatal("missing required field should not be OK")
	}
	badEnum := ValidatePayload(def, map[string]any{"result": "MAYBE"})
	if badEnum.OK || len(badEnum.Errors) == 0 {
		t.Fatalf("enum violation must be rejected: %+v", badEnum)
	}
	badTime := ValidatePayload(def, map[string]any{"result": "CLEAR", "at": "not-a-time"})
	if badTime.OK {
		t.Fatal("invalid timestamp must be rejected")
	}
	// strict policy rejects unmapped fields
	def.Policy = "strict"
	strict := ValidatePayload(def, map[string]any{"result": "CLEAR", "extra": 1})
	if strict.OK {
		t.Fatal("strict policy must reject unmapped fields")
	}
}

func envelope(mt string, at time.Time) *EventEnvelope {
	return &EventEnvelope{MessageType: mt, SourceCode: "S1", OccurredAt: at, Payload: []byte(`{}`)}
}

func evMt(mt string, at, recv time.Time, pkg, ship string) *Event {
	return &Event{
		EventID: mt + "-" + at.Format("150405") + pkg, MessageType: mt,
		OccurredAt: at, ReceivedAt: recv, PackageKey: pkg, ShipmentKey: ship,
		Source: "test", Payload: map[string]any{},
	}
}

func TestContentFingerprintStability(t *testing.T) {
	a := envelope(TypeScan, T(17, 2))
	a.DeviceID, a.DeviceEventNo = "SCN-1", "0001"
	b := envelope(TypeScan, T(17, 2))
	b.DeviceID, b.DeviceEventNo = "SCN-1", "0001"
	if ContentFingerprint(a, "PKG-1", "1") != ContentFingerprint(b, "PKG-1", "1") {
		t.Fatal("identical content must hash equally")
	}
	b.Payload = []byte(`{"x":1}`)
	if ContentFingerprint(a, "PKG-1", "1") == ContentFingerprint(b, "PKG-1", "1") {
		t.Fatal("different content must hash differently")
	}
}

func TestDedupeKey(t *testing.T) {
	k, basis := DedupeKey("scanner", "idem-1", "", "")
	if basis != "idempotency_key" || k != "idem:idem-1" {
		t.Fatalf("unexpected %q %q", k, basis)
	}
	k, basis = DedupeKey("scanner", "idem-1", "SCN-1", "0042")
	if basis != "device" || k != "device:SCN-1#0042" {
		t.Fatalf("device basis should win: %q %q", k, basis)
	}
	if k, _ := DedupeKey("scanner", "", "", ""); k != "" {
		t.Fatalf("no keys should yield empty, got %q", k)
	}
}

func TestEventStageOrdering(t *testing.T) {
	if EventStage(TypeDeclaration, "") >= EventStage(TypeScreening, "") {
		t.Fatal("declaration must precede screening")
	}
	if EventStage(TypeScreening, "") >= EventStage(TypeFlightManifest, "") {
		t.Fatal("screening must precede manifest")
	}
	if EventStage(TypeHandover, ModeScreening) >= EventStage(TypeScreening, "") {
		t.Fatal("screening handover must precede the screening result")
	}
}

func TestBuildTimelineStageFirst(t *testing.T) {
	// A screening event whose device clock is behind reports an earlier
	// occurred_at than the declaration, but stage ordering must keep the
	// declaration first.
	decl := evMt(TypeDeclaration, T(17, 3), T(17, 3), "", "SHIP")
	decl.Payload = map[string]any{"packages": []string{"PKG-1"}}
	scan := evMt(TypeScan, T(17, 2), T(17, 4), "PKG-1", "SHIP")
	scan.Payload = map[string]any{"mode": "arrival"}
	scr := evMt(TypeScreening, T(17, 1), T(17, 5), "PKG-1", "SHIP")
	scr.Payload = map[string]any{"result": "CLEAR"}
	tl := BuildTimeline([]*Event{scr, scan, decl}, nil, T(17, 6))
	if tl[0].MessageType != TypeDeclaration {
		t.Fatalf("declaration must sort first, got %s", tl[0].MessageType)
	}
	if tl[len(tl)-1].MessageType != TypeScreening {
		t.Fatalf("screening must sort last, got %s", tl[len(tl)-1].MessageType)
	}
}

func TestGraphSplitCreatesNewVersion(t *testing.T) {
	decl := evMt(TypeDeclaration, T(17, 2), T(17, 2), "", "SHIP")
	decl.Payload = map[string]any{"packages": []string{"PKG-A"}}
	decl.EventID = "decl"

	split := evMt(TypePackageChange, T(17, 3), T(17, 3), "", "SHIP")
	split.Payload = map[string]any{"action": "split", "parent_packages": []string{"PKG-A"},
		"child_packages": []string{"PKG-A1", "PKG-A2"}}
	split.EventID = "split"

	links, rets := foldEvents(t, []*Event{decl, split})
	g := NewGraph(applyRetractions(links, rets))

	// Before the split the parent is active.
	before := g.ActivePackages("SHIP", T(17, 2), T(17, 2))
	if len(before) != 1 || before[0] != "PKG-A" {
		t.Fatalf("before split want [PKG-A], got %v", before)
	}
	// After the split the children are active and the parent is retired.
	after := g.ActivePackages("SHIP", T(17, 5), T(17, 5))
	if len(after) != 2 {
		t.Fatalf("after split want 2 children, got %v", after)
	}
	if g.MembershipActiveAtKnowledge("PKG-A", "SHIP", T(17, 5)) {
		t.Fatal("parent must not be active after the split")
	}
	vers := g.MembershipVersions("SHIP")
	if len(vers) != 2 {
		t.Fatalf("want 2 composition versions, got %d: %+v", len(vers), vers)
	}
}

func TestClassifyLateEvidenceAfterSplit(t *testing.T) {
	decl := evMt(TypeDeclaration, T(17, 2), T(17, 2), "", "SHIP")
	decl.Payload = map[string]any{"packages": []string{"PKG-A"}}
	split := evMt(TypePackageChange, T(17, 3), T(17, 3), "", "SHIP")
	split.Payload = map[string]any{"action": "split", "parent_packages": []string{"PKG-A"},
		"child_packages": []string{"PKG-A1", "PKG-A2"}}
	links, rets := foldEvents(t, []*Event{decl, split})
	g := NewGraph(applyRetractions(links, rets))

	late := evMt(TypeScan, T(17, 2), T(17, 4), "PKG-A", "SHIP")
	d := ClassifyEvent(g, late, "SHIP", T(17, 5))
	if d.Disposition != DispositionSupersededPackage {
		t.Fatalf("late scan on retired package must be superseded, got %s (%s)", d.Disposition, d.Reason)
	}
	early := evMt(TypeScan, T(17, 2), T(17, 2), "PKG-A", "SHIP")
	d2 := ClassifyEvent(g, early, "SHIP", T(17, 2))
	if d2.Disposition != DispositionOK {
		t.Fatalf("evidence aligned when it arrived must be OK, got %s (%s)", d2.Disposition, d2.Reason)
	}
}

func TestFuseConflictNoHandover(t *testing.T) {
	c := evMt(TypeScreening, T(17, 5), T(17, 5), "PKG-A", "SHIP")
	c.Payload = map[string]any{"result": "CLEAR"}
	c.Source = "xry-1"
	r := evMt(TypeScreening, T(17, 6), T(17, 6), "PKG-A", "SHIP")
	r.Payload = map[string]any{"result": "REJECT"}
	r.Source = "xry-2"
	res := Fuse(FusionInput{
		ShipmentKey: "SHIP", KnowledgeAt: T(17, 7), ActivePackages: []string{"PKG-A"},
		Events: []*Event{c, r}, Dispositions: map[string]Disposition{},
		Weights: DefaultEvidenceWeights(), CompletenessFloor: 0.85, ConflictWindow: 2 * time.Hour,
	})
	if res.Status != StatusConflictPending {
		t.Fatalf("want conflict_pending, got %s (%s)", res.Status, res.Decision)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(res.Conflicts))
	}
}

func TestFuseConflictOutsideWindowIsRescreenDrift(t *testing.T) {
	// Two differing verdicts hours apart, no handover: outside the 2h window
	// this is ordinary re-screening drift, so the newest verdict wins and the
	// status is not a hard conflict.
	c := evMt(TypeScreening, T(17, 5), T(17, 5), "PKG-A", "SHIP")
	c.Payload = map[string]any{"result": "REJECT"}
	r := evMt(TypeScreening, T(17, 9), T(17, 9), "PKG-A", "SHIP")
	r.Payload = map[string]any{"result": "CLEAR"}
	res := Fuse(FusionInput{
		ShipmentKey: "SHIP", KnowledgeAt: T(17, 10), ActivePackages: []string{"PKG-A"},
		Events: []*Event{c, r}, Dispositions: map[string]Disposition{},
		Weights: DefaultEvidenceWeights(), CompletenessFloor: 0.5, ConflictWindow: 2 * time.Hour,
	})
	if res.Status == StatusConflictPending {
		t.Fatalf("drift outside the window must not be a hard conflict: %s", res.Decision)
	}
	if res.Packages[0].Result != "CLEAR" {
		t.Fatalf("newest verdict must win, got %s", res.Packages[0].Result)
	}
}

func TestFuseConflictResolvedByHandover(t *testing.T) {
	c := evMt(TypeScreening, T(17, 5), T(17, 5), "PKG-A", "SHIP")
	c.Payload = map[string]any{"result": "REJECT"}
	r := evMt(TypeScreening, T(17, 6), T(17, 7), "PKG-A", "SHIP")
	r.Payload = map[string]any{"result": "CLEAR"}
	r.Source = "xry-2"
	h := evMt(TypeHandover, T(17, 6), T(17, 6), "", "SHIP")
	h.Payload = map[string]any{"mode": "screening"}
	res := Fuse(FusionInput{
		ShipmentKey: "SHIP", KnowledgeAt: T(17, 8), ActivePackages: []string{"PKG-A"},
		Events: []*Event{c, r, h}, Dispositions: map[string]Disposition{},
		Weights: DefaultEvidenceWeights(), CompletenessFloor: 0.5, ConflictWindow: 2 * time.Hour,
	})
	if len(res.Conflicts) != 0 {
		t.Fatalf("handover between verdicts must dissolve the conflict: %+v", res.Conflicts)
	}
	if res.Packages[0].Result != "CLEAR" {
		t.Fatalf("latest segment verdict must win, got %s", res.Packages[0].Result)
	}
}

func TestFuseReviewRulingResolves(t *testing.T) {
	c := evMt(TypeScreening, T(17, 5), T(17, 5), "PKG-A", "SHIP")
	c.Payload = map[string]any{"result": "REJECT"}
	rev := evMt(TypeManualReview, T(17, 6), T(17, 6), "PKG-A", "SHIP")
	rev.Payload = map[string]any{"conclusion": "CLEAR", "reviewer": "op"}
	res := Fuse(FusionInput{
		ShipmentKey: "SHIP", KnowledgeAt: T(17, 7), ActivePackages: []string{"PKG-A"},
		Events: []*Event{c, rev}, Dispositions: map[string]Disposition{},
		Weights: DefaultEvidenceWeights(), CompletenessFloor: 0.5, ConflictWindow: 2 * time.Hour,
	})
	if res.Packages[0].Result != "CLEAR" || !res.Packages[0].HumanRuled {
		t.Fatalf("human review must rule: %+v", res.Packages[0])
	}
}

func TestFuseCorrectionSupersedes(t *testing.T) {
	old := evMt(TypeScreening, T(17, 5), T(17, 5), "PKG-A", "SHIP")
	old.Payload = map[string]any{"result": "REJECT"}
	old.EventID = "old"
	corr := evMt(TypeManualReview, T(17, 6), T(17, 6), "PKG-A", "SHIP")
	corr.Payload = map[string]any{"conclusion": "CLEAR"}
	corr.SupersedesEventID = "old"
	res := Fuse(FusionInput{
		ShipmentKey: "SHIP", KnowledgeAt: T(17, 7), ActivePackages: []string{"PKG-A"},
		Events: []*Event{old, corr}, Dispositions: map[string]Disposition{},
		Weights: DefaultEvidenceWeights(), CompletenessFloor: 0.5, ConflictWindow: 2 * time.Hour,
	})
	if len(res.Corrections) != 1 {
		t.Fatalf("want 1 correction, got %d", len(res.Corrections))
	}
	if res.Packages[0].Result != "CLEAR" {
		t.Fatalf("correction must win, got %s", res.Packages[0].Result)
	}
}

func TestFuseManifestEvidenceGapBlocksConfirmation(t *testing.T) {
	scr := evMt(TypeScreening, T(17, 5), T(17, 5), "PKG-A", "SHIP")
	scr.Payload = map[string]any{"result": "CLEAR"}
	scr.EvidenceIndexes = []string{"IMG-1"}
	res := Fuse(FusionInput{
		ShipmentKey: "SHIP", KnowledgeAt: T(17, 7), ActivePackages: []string{"PKG-A"},
		Events: []*Event{scr}, Dispositions: map[string]Disposition{},
		ManifestEvidence:  []string{"IMG-MISSING"},
		Weights:           DefaultEvidenceWeights(),
		CompletenessFloor: 0.5,
		ConflictWindow:    2 * time.Hour,
	})
	if res.Status == StatusVerified {
		t.Fatal("manifest required evidence gap must prevent verified")
	}
	found := false
	for _, p := range res.PendingItems {
		if p.Code == "manifest_required_evidence_missing" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing required evidence must produce a pending item")
	}
}

func foldEvents(t *testing.T, evs []*Event) ([]Link, []Retraction) {
	t.Helper()
	var links []Link
	var rets []Retraction
	version := map[string]int{}
	// Naive in-memory folding mirroring the store semantics for tests only.
	for _, ev := range evs {
		d := Derive(ev)
		for _, key := range d.RetireKeys {
			for i := len(links) - 1; i >= 0; i-- {
				if links[i].LinkKey == key && links[i].RetractedAt == nil {
					links[i].RetractedAt = &ev.OccurredAt
					lk := ev.ReceivedAt
					links[i].RetractionKnowledgeAt = &lk
					rets = append(rets, Retraction{LinkKey: key, LinkVersion: links[i].LinkVersion,
						EffectiveAt: ev.OccurredAt, KnowledgeAt: ev.ReceivedAt})
					break
				}
			}
		}
		for _, up := range d.Upserts {
			version[up.LinkKey]++
			links = append(links, Link{
				LinkKey: up.LinkKey, LinkVersion: version[up.LinkKey], Kind: up.Kind,
				SubjectType: up.SubjectType, SubjectKey: up.SubjectKey,
				ObjectType: up.ObjectType, ObjectKey: up.ObjectKey,
				ValidFrom: up.ValidFrom, KnowledgeAt: up.KnowledgeAt,
				SourceEventID: up.SourceEventID, Attributes: up.Attributes,
			})
		}
	}
	return links, rets
}

func applyRetractions(links []Link, _ []Retraction) []Link { return links }
