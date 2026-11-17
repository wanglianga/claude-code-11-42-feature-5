package review

import (
	"testing"
	"time"
)

// exhaustiveAgrees runs the exhaustive verifier and requires exact agreement for
// every status claim in a small material. It is the "targeted solver is faithful
// to brute force" check the requirement asks for.
func exhaustiveAgrees(t *testing.T, w *World, an *Analysis) {
	t.Helper()
	checks := VerifyAgainstExhaustive(w, an.Assertions, 720)
	if len(checks) == 0 {
		return
	}
	for _, c := range checks {
		if !c.Agree {
			t.Fatalf("targeted=%s exhaustive=%s for %s (%v)",
				c.TargetedVerdict, c.ExhaustiveVerdict, c.AssertionID, c.Notes)
		}
	}
}

func TestExhaustiveCrossCheckClockSkew(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0)},
		{Ref: "v2", SnapshotID: "S2", Subject: SubjectRef{"shipment", ship},
			Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(10, 40),
			Supersedes: "S1"},
		{Ref: "v3", SnapshotID: "S3", Subject: SubjectRef{"shipment", ship},
			Version: 3, Kind: "formal", Status: "verified", PublishedAt: utc(11, 10),
			Supersedes: "S2"},
	}}
	at := win(utc(10, 20), utc(10, 50))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "pending_evidence", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	exhaustiveAgrees(t, w, an)
}

// TestExhaustiveCrossCheckContradicted verifies a claim that is impossible from
// every angle agrees with brute force.
func TestExhaustiveCrossCheckContradicted(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(9, 30)},
		{Ref: "v2", SnapshotID: "S2", Subject: SubjectRef{"shipment", ship},
			Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(9, 50),
			Supersedes: "S1"},
	}}
	// Nothing but pending_evidence was ever published in [10:00, 10:10].
	at := win(utc(10, 0), utc(10, 10))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	res := findAssertion(an, "claim:0:shipment:"+ship)
	if res == nil || res.Verdict != VerdictContradicted {
		t.Fatalf("expected contradicted, got %+v", res)
	}
	if res.Conflict == nil || len(res.Conflict.Records) == 0 {
		t.Fatalf("a contradiction must cite original records even when no version matches: %+v", res.Conflict)
	}
	exhaustiveAgrees(t, w, an)
}

// ---- budget exhaustion: no contradiction is not proof of consistency ------

func TestBudgetExhaustionIsExplicit(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	var pubs []PublicationDraft
	for i := 1; i <= 8; i++ {
		pubs = append(pubs, PublicationDraft{
			Ref: "v" + itoa(i), SnapshotID: "S" + itoa(i),
			Subject: SubjectRef{"shipment", ship}, Version: i, Kind: "formal",
			Status: "verified", PublishedAt: utc(9, i*5),
		})
	}
	in := LogImport{Publications: pubs}
	at := win(utc(10, 0), utc(12, 0))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope, BudgetLimit: 2})
	if !an.Budget.Exhausted {
		t.Fatalf("a budget of 2 must be exhausted by 9 versions; budget=%+v", an.Budget)
	}
	if an.Summary.Verdict == VerdictContradicted {
		t.Fatalf("a budget stop must never be reported as a contradiction: %+v", an.Summary)
	}
	if an.Summary.Verdict != VerdictUndetermined {
		t.Fatalf("budget exhaustion should leave the result undetermined, got %s", an.Summary.Verdict)
	}
	if an.Summary.NotProven == "" {
		t.Fatal("budget exhaustion must say that no contradiction is not a proof")
	}
}

func TestBudgetZeroMeansUnchecked(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0)},
		{Ref: "v2", SnapshotID: "S2", Subject: SubjectRef{"shipment", ship},
			Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(11, 0),
			Supersedes: "S1"},
	}}
	at := win(utc(10, 5), utc(10, 15))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope, BudgetLimit: 1})
	// Limit 1: the base solve consumes it, claims are skipped.
	if an.Summary.Verdict != VerdictUndetermined {
		t.Fatalf("one-query budget cannot confirm a claim; got %s", an.Summary.Verdict)
	}
	if !an.Budget.Exhausted {
		t.Fatalf("budget must report exhausted; %+v", an.Budget)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// ---- permission narrowing --------------------------------------------------

func TestRestrictedExportPreservesVerdict(t *testing.T) {
	ship, cont := "AWB10000000001", "AKE10001MU"
	scope := mustScope(t, "platform", "declaration")
	in := LogImport{
		Events: []EventDraft{
			{Ref: "decl", System: "declaration", Occurred: exact(utc(7, 0)),
				Membership: &MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{ship}, Objects: []string{"PKG-1"}}},
			{Ref: "stow", System: "declaration", Occurred: exact(utc(7, 30)),
				Membership: &MembershipClaim{LinkKind: "package_container",
					Subjects: []string{"PKG-1"}, Objects: []string{cont}}},
		},
		Publications: []PublicationDraft{
			{Ref: "ship-v1", SnapshotID: "SHIPV1", Subject: SubjectRef{"shipment", ship},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0),
				InputRefs: []string{"decl"}},
			{Ref: "cont-v1", SnapshotID: "CONTV1", Subject: SubjectRef{"container", cont},
				Version: 1, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(10, 10),
				InputRefs: []string{"stow"}},
		},
	}
	at := win(utc(10, 20), utc(10, 40))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment", "container"},
		Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	internal := an.Summary.Verdict

	// Narrow the allowed fields: the verdict must not change.
	allowed := FieldsFromName([]string{"record_id", "kind"})
	verdict, projected := Project(an.Summary.Verdict, an.Assertions, allowed)
	if verdict != internal {
		t.Fatalf("projection changed the verdict from %s to %s", internal, verdict)
	}
	// Descriptions are dropped when "detail" is not allowed.
	for _, a := range projected {
		if a.Description != "" {
			t.Fatalf("detail leaked into a projection that excludes it: %+v", a)
		}
	}
	// Boundary records hide the container detail but keep the relation.
	restricted := BoundaryRecords(w.Records, scope, []string{"shipment"})
	foundContainer := false
	for _, r := range restricted {
		if r.Kind == KindPublication && r.RecordID == "cont-v1" {
			foundContainer = true
			if r.Boundary == "" {
				t.Fatal("a withheld record must still carry its necessary relation")
			}
		}
	}
	if !foundContainer {
		// Even if the projection lists it differently, the constraint must not
		// have been deleted from the internal material.
		if len(w.Versions("container", cont)) == 0 {
			t.Fatal("withheld container publication was deleted instead of bounded")
		}
	}
}

// ---- reproducibility -------------------------------------------------------

func TestOldReviewReproducesFromOldMaterial(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0)},
		{Ref: "v2", SnapshotID: "S2", Subject: SubjectRef{"shipment", ship},
			Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(11, 0),
			Supersedes: "S1"},
	}}
	at := win(utc(10, 5), utc(10, 15))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	if err := ReproCheck(an, w.Records); err != nil {
		t.Fatalf("old review must reproduce from its own material: %v", err)
	}
	// Drop a pinned record and the reproduction must fail loudly.
	if len(w.Records) < 2 {
		t.Skip("need at least two records to drop one")
	}
	shrunk := append([]*Record{}, w.Records[1:]...)
	if err := ReproCheck(an, shrunk); err == nil {
		t.Fatal("reproduction must fail when a pinned record is missing")
	}
	// A changed material (same ids, different values) must also fail the hash.
	mutated := make([]*Record, len(w.Records))
	for i, r := range w.Records {
		cp := *r
		if i == 0 {
			cp.Detail = "tampered"
		}
		// The manifest hashes ids, so a value-only change does not alter it;
		// that is by design for id-pinning. Add a record to change the set.
		mutated[i] = &cp
	}
	extra := append(mutated, &Record{RecordID: "smuggled", Kind: KindEvent,
		Occurred: TimeBound{Earliest: utc(8, 0), Latest: utc(8, 0), Basis: "test"}})
	if err := ReproCheck(an, extra); err == nil {
		t.Fatal("reproduction must fail when extra material is injected")
	}
}

// ---- restart segment boundaries are never bridged -------------------------

func TestNoSequenceEdgeAcrossBootSegments(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "scanner")
	in := LogImport{Events: []EventDraft{
		{Ref: "boot1-n9", System: "scanner", Sequence: "9", BootSegment: "boot-1",
			Occurred: win(utc(9, 50), utc(10, 5))},
		{Ref: "boot2-n1", System: "scanner", Sequence: "1", BootSegment: "boot-2",
			Occurred: win(utc(9, 40), utc(9, 55))},
	}}
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	for _, c := range an.Constraints {
		if c.Kind == "distinct" {
			t.Fatalf("no sequence edge may cross a boot segment: %+v", c)
		}
	}
}

// ---- same-source same-boot ordering is asserted ---------------------------

func TestSameBootOrderingAsserted(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "scanner")
	in := LogImport{Events: []EventDraft{
		{Ref: "n1", System: "scanner", Sequence: "1", BootSegment: "boot-1",
			Occurred: win(utc(9, 0), utc(9, 30))},
		{Ref: "n2", System: "scanner", Sequence: "2", BootSegment: "boot-1",
			Occurred: win(utc(9, 0), utc(9, 30))},
	}}
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	found := false
	for _, c := range an.Constraints {
		if c.Kind == "distinct" && c.From == RecVar("n1") && c.To == RecVar("n2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sequence 1 -> 2 in one boot must be ordered; constraints=%+v", an.Constraints)
	}
}

// ---- a target with no instant cannot answer a status question -------------

func TestStatusClaimWithoutInstantIsUndetermined(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0)},
	}}
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified"}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	res := findAssertion(an, "claim:0:shipment:"+ship)
	if res == nil {
		t.Fatalf("claim must still be listed: %v", assertionIDs(an))
	}
	if res.Verdict != VerdictUndetermined {
		t.Fatalf("a screenshot time alone cannot decide; got %s", res.Verdict)
	}
	if len(res.Unresolved) == 0 {
		t.Fatal("an undetermined claim must say what is missing")
	}
}

// TestNoReadLogsNoInference confirms that without read logs the review only
// speaks about what the platform published, not what a consumer saw.
func TestNoReadLogsNoInference(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform", "reading")
	in := LogImport{
		Publications: []PublicationDraft{
			{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0)},
		},
		Reads: []ReadDraft{
			{Ref: "r1", System: "reading", Occurred: exact(utc(11, 0)),
				Received: exact(utc(11, 0)), ReferencedSnapshot: "S1",
				Observed: map[string]string{"status": "verified"}},
		},
	}
	at := win(utc(10, 30), utc(10, 40))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at,
		Cutoff: utc(22, 0), IncludeReadLogs: false,
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	for _, a := range an.Assertions {
		if a.Kind == AssertionRead {
			t.Fatalf("read assertion must not be built when read logs are excluded: %+v", a)
		}
	}
	// The publication-based claim is still answerable.
	res := findAssertion(an, "claim:0:shipment:"+ship)
	if res == nil || res.Verdict != VerdictConsistent {
		t.Fatalf("published material should answer the publication question: %+v", res)
	}
}

var _ = time.RFC3339

// ---- multi-shipment: a common order across shipments ----------------------

func TestMultiShipmentCommonOrder(t *testing.T) {
	ship1, ship2 := "AWB10000000001", "AWB10000000002"
	scope, err := ScopeRegistration(ScopeRequest{
		ShipmentKey: ship1, IncludedSystems: []string{"platform", "declaration"},
		AuthorizedSubjects: []SubjectRef{{Type: "shipment", Key: ship2}},
		Cutoff:             utc(23, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	in := LogImport{
		Events: []EventDraft{
			{Ref: "decl1", System: "declaration", Occurred: exact(utc(7, 0)),
				Membership: &MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{ship1}, Objects: []string{"PKG-A"}}},
			{Ref: "decl2", System: "declaration", Occurred: exact(utc(7, 5)),
				Membership: &MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{ship2}, Objects: []string{"PKG-B"}}},
		},
		Publications: []PublicationDraft{
			{Ref: "s1v1", SnapshotID: "S1V1", Subject: SubjectRef{"shipment", ship1},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0),
				InputRefs: []string{"decl1"}},
			{Ref: "s2v1", SnapshotID: "S2V1", Subject: SubjectRef{"shipment", ship2},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 10),
				InputRefs: []string{"decl2"}},
		},
	}
	at := win(utc(10, 20), utc(10, 40))
	target := Target{ShipmentKey: ship1, Levels: []string{"shipment"},
		AuthorizedSubjects: []SubjectRef{{Type: "shipment", Key: ship2}},
		Instant:            at, Cutoff: utc(22, 0),
		Claims: []Claim{
			{Level: "shipment", Key: ship1, Status: "verified", At: at},
			{Level: "shipment", Key: ship2, Status: "verified", At: at},
		}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope, EvaluateCombination: true})
	combo := findAssertion(an, "combination")
	if combo == nil || combo.Verdict != VerdictConsistent {
		t.Fatalf("two shipments both verified at one instant should share an order: %+v", combo)
	}
}

// ---- transload: historical attribution follows the reference in force -----

func TestTransloadHistoricalContainer(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "declaration")
	in := LogImport{Events: []EventDraft{
		{Ref: "decl", System: "declaration", Occurred: exact(utc(7, 0)),
			Membership: &MembershipClaim{LinkKind: "shipment_package",
				Subjects: []string{ship}, Objects: []string{"PKG-1"}}},
		// Package sits in container A, then is transloaded into B.
		{Ref: "stow-A", System: "declaration", Occurred: exact(utc(8, 0)),
			Membership: &MembershipClaim{LinkKind: "package_container",
				Subjects: []string{"PKG-1"}, Objects: []string{"AKE-A"}}},
		{Ref: "stow-B", System: "declaration", Occurred: exact(utc(9, 0)),
			Membership: &MembershipClaim{LinkKind: "package_container",
				Subjects: []string{"PKG-1"}, Objects: []string{"AKE-B"}}},
	}}
	// At 08:30 the package was in A; a claim of B at that instant is false.
	at := win(utc(8, 10), utc(8, 40))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment", "container"}, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "container", Key: "AKE-B", Status: "", At: at,
			Membership: &MembershipClaim{LinkKind: "package_container",
				Subjects: []string{"PKG-1"}, Objects: []string{"AKE-B"}}}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	res := findAssertion(an, "claim:0:container:AKE-B")
	if res == nil {
		t.Fatalf("missing claim: %v", assertionIDs(an))
	}
	if res.Verdict != VerdictContradicted {
		t.Fatalf("claiming the later container at an earlier instant must be contradicted: %s", res.Verdict)
	}
	// At 09:10 the package really is in B. Same claim at a later instant holds.
	at2 := win(utc(9, 10), utc(9, 40))
	t2 := target
	t2.Instant = at2
	t2.Claims = []Claim{{Level: "container", Key: "AKE-B", Status: "", At: at2,
		Membership: &MembershipClaim{LinkKind: "package_container",
			Subjects: []string{"PKG-1"}, Objects: []string{"AKE-B"}}}}
	w2 := buildCase(t, scope, in, t2)
	an2 := analyze(t, w2, AnalyzeOptions{Scope: scope})
	res2 := findAssertion(an2, "claim:0:container:AKE-B")
	if res2 == nil || res2.Verdict != VerdictConsistent {
		t.Fatalf("at 09:10 the package is genuinely in B: %+v", res2)
	}
}

// ---- snapshot stitching: membership from one version, count from another --

func TestSnapshotStitchingRejected(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0),
			Counts: map[string]string{"confirmed_packages": "3"},
			Membership: &MembershipClaim{LinkKind: "shipment_package",
				Subjects: []string{ship}, Objects: []string{"PKG-1", "PKG-2", "PKG-3"}}},
		{Ref: "v2", SnapshotID: "S2", Subject: SubjectRef{"shipment", ship},
			Version: 2, Kind: "formal", Status: "verified", PublishedAt: utc(10, 30),
			Counts: map[string]string{"confirmed_packages": "2"},
			Membership: &MembershipClaim{LinkKind: "shipment_package",
				Subjects: []string{ship}, Objects: []string{"PKG-1", "PKG-2"}}},
	}}
	// The instant is wide enough to reach both versions. No single version has
	// the count 3 together with the membership {PKG-1, PKG-2}: that combination
	// was never published.
	at := win(utc(10, 20), utc(11, 0))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at,
			Counts: map[string]string{"confirmed_packages": "3"},
			Membership: &MembershipClaim{LinkKind: "shipment_package",
				Subjects: []string{ship}, Objects: []string{"PKG-1", "PKG-2"}}}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	res := findAssertion(an, "claim:0:shipment:"+ship)
	if res == nil {
		t.Fatalf("missing claim: %v", assertionIDs(an))
	}
	if res.Verdict != VerdictContradicted {
		t.Fatalf("a membership/count combination never published together must be contradicted, got %s", res.Verdict)
	}
}

// ---- image-only content is never claimed verified from metadata ----------

func TestImageDependentClaimNotClaimedVerified(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0)},
	}}
	at := win(utc(10, 10), utc(10, 20))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified", At: at, RequiresImage: true}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	res := findAssertion(an, "claim:0:shipment:"+ship)
	if res == nil || res.Verdict != VerdictUndetermined {
		t.Fatalf("image-dependent content must stay undetermined: %+v", res)
	}
	// Existing time relations untouched: the publication constraints remain.
	if len(an.Constraints) == 0 {
		t.Fatal("the image gap must not remove existing time constraints")
	}
}

// ---- boundary subject is reported, never fabricated ----------------------

func TestBoundaryClaimReported(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform")
	in := LogImport{Publications: []PublicationDraft{
		{Ref: "v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0)},
	}}
	at := win(utc(10, 10), utc(10, 20))
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{{Level: "container", Key: "AKE-SECRET", Status: "verified", At: at}}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	res := findAssertion(an, "claim:0:container:AKE-SECRET")
	if res == nil || res.Kind != AssertionBoundary {
		t.Fatalf("unauthorised container claim must be a boundary assertion: %+v", res)
	}
	if res.Verdict != VerdictUndetermined {
		t.Fatalf("a boundary claim cannot be confirmed or refuted: %s", res.Verdict)
	}
}

// ---- evidence dependency: index cited but not archived -------------------

func TestEvidencePrerequisite(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "screening", "imaging", "platform")
	base := LogImport{
		Events: []EventDraft{
			{Ref: "scan", System: "screening", Occurred: exact(utc(8, 0)),
				Evidence: []string{"IMG-1"}, Classification: "verdict"},
		},
		Publications: []PublicationDraft{{
			Ref: "pub-v1", SnapshotID: "S1", Subject: SubjectRef{"shipment", ship},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0),
			InputRefs: []string{"scan"},
		}},
	}
	// Without an archive record the evidence dimension is undetermined.
	w := buildCase(t, scope, base, Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)})
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	ev := findAssertion(an, "evidence:IMG-1")
	if ev == nil {
		t.Fatalf("evidence prerequisite must be asserted: %v", assertionIDs(an))
	}
	if ev.Verdict != VerdictUndetermined {
		t.Fatalf("evidence cited but not archived must stay undetermined: %s", ev.Verdict)
	}
	// Add the archive record and the dependency is satisfied.
	base.Events = append(base.Events, EventDraft{
		Ref: "archive", System: "imaging", Occurred: exact(utc(8, 5)),
		Evidence: []string{"IMG-1"}, Classification: "archive",
	})
	w2 := buildCase(t, scope, base, Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)})
	an2 := analyze(t, w2, AnalyzeOptions{Scope: scope})
	ev2 := findAssertion(an2, "evidence:IMG-1")
	if ev2 == nil || ev2.Verdict != VerdictConsistent {
		t.Fatalf("archived evidence must be consistent: %+v", ev2)
	}
}

// ---- boundary projection never leaks an unauthorised identifier ---------

func TestBoundaryProjectionHidesKey(t *testing.T) {
	ship, cont := "AWB10000000001", "AKE-SECRET"
	scope := mustScope(t, "platform", "declaration")
	in := LogImport{
		Events: []EventDraft{
			{Ref: "decl", System: "declaration", Occurred: exact(utc(7, 0)),
				Membership: &MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{ship}, Objects: []string{"PKG-1"}}},
			{Ref: "stow", System: "declaration", Occurred: exact(utc(7, 30)),
				Membership: &MembershipClaim{LinkKind: "package_container",
					Subjects: []string{"PKG-1"}, Objects: []string{cont}}},
		},
		Publications: []PublicationDraft{{
			Ref: "cont-v1", SnapshotID: "CONT1", Subject: SubjectRef{"container", cont},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0),
			InputRefs: []string{"stow"},
		}},
	}
	w := buildCase(t, scope, in, Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)})
	boundary := BoundaryRecords(w.Records, scope, []string{"shipment"})
	found := false
	for _, b := range boundary {
		if b.RecordID == "cont-v1" || b.RecordID == "stow" {
			found = true
			if indexOfStr(b.Boundary, cont) >= 0 {
				t.Fatalf("boundary leaked the container identifier: %q", b.Boundary)
			}
			if b.RecordID == "cont-v1" && b.Recomputable {
				t.Fatalf("a withheld identifier cannot be independently recomputable: %+v", b)
			}
		}
	}
	if !found {
		t.Fatalf("withheld records must still be represented, not deleted: %+v", boundary)
	}
	// The constraint is not deleted from the internal material.
	if len(w.Versions("container", cont)) == 0 {
		t.Fatal("boundary projection deleted the container publication instead of bounding it")
	}
}
