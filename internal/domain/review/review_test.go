package review

import (
	"testing"
	"time"
)

func utc(h, m int) time.Time {
	return time.Date(2026, 11, 17, h, m, 0, 0, time.UTC)
}

func exact(t time.Time) *TimeBound { return &TimeBound{Earliest: t, Latest: t, Basis: "test"} }

func win(a, b time.Time) *TimeBound { return &TimeBound{Earliest: a, Latest: b, Basis: "test"} }

func mustScope(t *testing.T, systems ...string) ScopeRegistry {
	t.Helper()
	s, err := ScopeRegistration(ScopeRequest{
		ShipmentKey: "AWB10000000001", IncludedSystems: systems,
		Cutoff: utc(23, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// buildCase imports drafts, applies the scope and builds a world.
func buildCase(t *testing.T, scope ScopeRegistry, in LogImport, target Target) *World {
	t.Helper()
	res, err := ApplyScope(scope, in)
	if err != nil {
		t.Fatalf("apply scope: %v", err)
	}
	for _, e := range res.Excluded {
		if e.Reason != "" && len(e.Reason) > 6 && e.Reason[:6] == "event " {
			// construction errors are fine in this test scaffold
		}
	}
	target.CaseID = "case-1"
	if target.Cutoff.IsZero() {
		target.Cutoff = scope.Cutoff
	}
	return NewWorld(target, res.Imported, res.Excluded)
}

func analyze(t *testing.T, w *World, opts AnalyzeOptions) *Analysis {
	t.Helper()
	an, err := Analyze(w, opts)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return an
}

func findAssertion(an *Analysis, id string) *AssertionResult {
	for i := range an.Assertions {
		if an.Assertions[i].AssertionID == id {
			return &an.Assertions[i]
		}
	}
	return nil
}

// ---- 1. cross-device clock error: both readings survive --------------------

func TestBothExplainFromClockSkew(t *testing.T) {
	ship := "AWB10000000001"
	pubA := utc(10, 0) // verified publication for the shipment
	pubB := utc(10, 40)
	scope := mustScope(t, "platform", "screening", "custody", "reading")
	in := LogImport{
		Events: []EventDraft{
			{Ref: "decl", System: "declaration", Occurred: exact(utc(8, 0)),
				Membership: &MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{ship}, Objects: []string{"PKG-1"}}},
		},
		Publications: []PublicationDraft{
			{Ref: "pub-a", SnapshotID: "SNAPA", Subject: SubjectRef{"shipment", ship},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: pubA,
				InputRefs: []string{"decl"}},
			{Ref: "pub-b", SnapshotID: "SNAPB", Subject: SubjectRef{"shipment", ship},
				Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: pubB,
				Supersedes: "SNAPA"},
		},
		Reads: []ReadDraft{
			// Two consumer systems' clocks disagree by 45 minutes. One saw the
			// earlier version; one saw the later one. The review must not claim
			// which reading the "real" screen showed; both are possible.
			{Ref: "read-x", System: "reading", Occurred: win(utc(9, 50), utc(10, 10)),
				Received: exact(utc(10, 10)), ReferencedSnapshot: "SNAPA",
				Observed: map[string]string{"status": "verified"}},
			{Ref: "read-y", System: "reading", Occurred: win(utc(10, 50), utc(11, 20)),
				Received: exact(utc(11, 20)), ReferencedSnapshot: "SNAPB",
				Observed: map[string]string{"status": "pending_evidence"}},
		},
	}
	target := Target{
		ShipmentKey: ship, Levels: []string{"shipment"},
		Instant: win(utc(10, 20), utc(11, 0)),
		Cutoff:  utc(22, 0), IncludeReadLogs: true,
		Claims: []Claim{{Level: "shipment", Key: ship, Status: "verified"}},
	}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope, EvaluateCombination: true})
	res := findAssertion(an, "claim:0:shipment:"+ship)
	if res == nil {
		t.Fatalf("missing claim assertion; have %v", assertionIDs(an))
	}
	if res.Verdict != VerdictBothExplain {
		t.Fatalf("expected both_explain (clock skew leaves two readings), got %s (%+v)", res.Verdict, res)
	}
	if res.Positive == nil || res.Negative == nil {
		t.Fatalf("both examples required for both_explain: %+v", res)
	}
	if res.Positive.ExampleID == res.Negative.ExampleID {
		t.Fatal("positive and negative examples must differ")
	}
}

func assertionIDs(an *Analysis) []string {
	var out []string
	for _, a := range an.Assertions {
		out = append(out, a.AssertionID+"="+a.Verdict)
	}
	return out
}

// ---- 2. each claim individually possible, combination impossible ----------

// Two screenshots state different statuses for the same shipment; the instant
// is uncertain enough that each reading is individually possible, but a single
// instant cannot resolve to two different published versions. Individually:
// consistent. Together: contradicted.
func TestCombinationImpossibleThoughEachExplains(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform", "declaration")
	in := LogImport{
		Events: []EventDraft{
			{Ref: "decl", System: "declaration", Occurred: exact(utc(7, 0)),
				Membership: &MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{ship}, Objects: []string{"PKG-1"}}},
		},
		Publications: []PublicationDraft{
			{Ref: "ship-v1", SnapshotID: "SHIPV1", Subject: SubjectRef{"shipment", ship},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0),
				InputRefs: []string{"decl"}},
			{Ref: "ship-v2", SnapshotID: "SHIPV2", Subject: SubjectRef{"shipment", ship},
				Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(10, 40),
				Supersedes: "SHIPV1"},
		},
	}
	// The same uncertain instant [10:20, 11:00] straddles both versions.
	at := win(utc(10, 20), utc(11, 0))
	target := Target{
		ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0),
		Instant: at,
		Claims: []Claim{
			{Level: "shipment", Key: ship, Status: "verified", At: at},
			{Level: "shipment", Key: ship, Status: "pending_evidence", At: at},
		},
	}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope, EvaluateCombination: true})
	a := findAssertion(an, "claim:0:shipment:"+ship)
	b := findAssertion(an, "claim:1:shipment:"+ship)
	combo := findAssertion(an, "combination")
	if a == nil || b == nil || combo == nil {
		t.Fatalf("missing assertions: %v", assertionIDs(an))
	}
	if a.Verdict != VerdictBothExplain || b.Verdict != VerdictBothExplain {
		t.Fatalf("each claim alone should have both readings (clock uncertainty): verified=%s pending=%s",
			a.Verdict, b.Verdict)
	}
	if a.Positive == nil || a.Negative == nil {
		t.Fatalf("both_explain must carry both examples: %+v", a)
	}
	if combo.Verdict != VerdictContradicted {
		t.Fatalf("the two claims cannot hold at one instant; combination should be contradicted, got %s",
			combo.Verdict)
	}
	if combo.Conflict == nil || len(combo.Conflict.Records) < 2 {
		t.Fatalf("a joint contradiction must cite the original version records: %+v", combo.Conflict)
	}
	if combo.Conflict.Message == "" {
		t.Fatal("a conflict must explain that it is not minimal / not a single root cause")
	}
}

// A shipment and its container can legitimately differ: the container
// aggregates several shipments and may lag. Together they are consistent, and
// the review says so rather than inventing a contradiction from a status
// difference.
func TestCrossLevelDifferenceIsNotAContradiction(t *testing.T) {
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
				Version: 1, Kind: "formal", Status: "pending_evidence", PublishedAt: utc(10, 0),
				InputRefs: []string{"decl"}},
			{Ref: "cont-v1", SnapshotID: "CONTV1", Subject: SubjectRef{"container", cont},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 30),
				InputRefs: []string{"stow"}},
		},
	}
	at := win(utc(10, 10), utc(10, 50))
	target := Target{
		ShipmentKey: ship, Levels: []string{"shipment", "container"},
		Instant: at, Cutoff: utc(22, 0),
		Claims: []Claim{
			{Level: "shipment", Key: ship, Status: "pending_evidence", At: at},
			{Level: "container", Key: cont, Status: "verified", At: at},
		},
	}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope, EvaluateCombination: true})
	combo := findAssertion(an, "combination")
	if combo == nil {
		t.Fatalf("missing combination: %v", assertionIDs(an))
	}
	if combo.Verdict != VerdictConsistent {
		t.Fatalf("shipment and container can hold different statuses at one instant, got %s", combo.Verdict)
	}
}

// ---- 3. read references a version not yet published ----------------------

func TestReadReferencesUnpublishedVersion(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform", "reading")
	in := LogImport{
		Publications: []PublicationDraft{
			{Ref: "pub-v2", SnapshotID: "SNAP-V2", Subject: SubjectRef{"shipment", ship},
				Version: 2, Kind: "formal", Status: "verified", PublishedAt: utc(14, 0)},
		},
		Reads: []ReadDraft{
			// The read claims to have seen v2 at 13:00, an hour before the only
			// publication of v2 exists. Force the causal edge and it breaks.
			{Ref: "read-early", System: "reading",
				Occurred: exact(utc(13, 0)), Received: exact(utc(13, 0)),
				ReferencedSnapshot: "SNAP-V2"},
		},
	}
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"},
		Cutoff: utc(22, 0), IncludeReadLogs: true, ReadRecordIDs: []string{"read-early"}}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	res := findAssertion(an, "read:read-early")
	if res == nil {
		t.Fatalf("missing read assertion: %v", assertionIDs(an))
	}
	if res.Verdict != VerdictContradicted {
		t.Fatalf("read citing an unpublished version must be contradicted, got %s", res.Verdict)
	}
	if res.Conflict == nil || len(res.Conflict.Records) == 0 {
		t.Fatalf("a contradiction must cite the original records: %+v", res.Conflict)
	}
}

// ---- 4. restart reuses numbers: no cross-boot ordering -------------------

func TestSequenceReuseAcrossBootIsNotOrdering(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "scanner")
	in := LogImport{
		Events: []EventDraft{
			{Ref: "boot1-n5", System: "scanner", Stream: "dev1", Sequence: "5", BootSegment: "boot-1",
				Occurred: win(utc(9, 0), utc(9, 5))},
			{Ref: "boot2-n1", System: "scanner", Stream: "dev1", Sequence: "1", BootSegment: "boot-2",
				Occurred: win(utc(8, 0), utc(9, 59))},
			{Ref: "boot2-n2", System: "scanner", Stream: "dev1", Sequence: "2", BootSegment: "boot-2",
				Occurred: win(utc(8, 0), utc(9, 59))},
		},
	}
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	// No sequence constraint may link boot-1 number 5 with boot-2 number 1/2.
	for _, c := range an.Constraints {
		if c.Kind != "distinct" {
			continue
		}
		for _, r := range c.RecordIDs {
			if r == "boot1-n5" {
				t.Fatalf("cross-boot sequence constraint leaked: %+v", c)
			}
		}
	}
	// Boot-2 records do order each other.
	found := false
	for _, c := range an.Constraints {
		if c.Kind == "distinct" && len(c.RecordIDs) == 2 &&
			c.RecordIDs[0] == "boot2-n1" && c.RecordIDs[1] == "boot2-n2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("same-boot sequence order must be asserted; constraints=%+v", an.Constraints)
	}
}

func TestSequenceReuseWithinBootIsNotOrdering(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "scanner")
	in := LogImport{
		Events: []EventDraft{
			{Ref: "a", System: "scanner", Stream: "dev1", Sequence: "7", BootSegment: "boot-1",
				Occurred: win(utc(9, 0), utc(9, 10))},
			{Ref: "b", System: "scanner", Stream: "dev1", Sequence: "7", BootSegment: "boot-1",
				Occurred: win(utc(9, 11), utc(9, 20))},
		},
	}
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	for _, c := range an.Constraints {
		if c.Kind == "distinct" {
			t.Fatalf("reused numbers must not order records: %+v", c)
		}
	}
	found := false
	for _, u := range an.Unresolved {
		if len(u) > 0 && (containsStr([]string{u}, u)) && (indexOfStr(u, "reused") >= 0) {
			found = true
		}
	}
	if !found {
		t.Fatalf("reused sequence numbers must be surfaced as unresolved; got %v", an.Unresolved)
	}
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---- 5. late logs only enter a new review --------------------------------

func TestLateLogExcludedFromOldCutoff(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform", "reading")
	in := LogImport{Reads: []ReadDraft{
		{Ref: "late-read", System: "reading", Occurred: exact(utc(11, 0)),
			Received: exact(utc(23, 30)), ReferencedSnapshot: "SNAP-V1"},
		{Ref: "on-time-read", System: "reading", Occurred: exact(utc(11, 0)),
			Received: exact(utc(11, 5)), ReferencedSnapshot: "SNAP-V1"},
	}}
	res, err := ApplyScope(scope, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.LateArrivals) != 1 || res.LateArrivals[0].RecordID != "late-read" {
		t.Fatalf("a read received after the cutoff must be a late arrival, got %+v", res.LateArrivals)
	}
	for _, r := range res.Imported {
		if r.RecordID == "late-read" {
			t.Fatal("late read leaked into the reviewed material")
		}
	}
	if len(res.Imported) != 1 || res.Imported[0].RecordID != "on-time-read" {
		t.Fatalf("on-time read should be reviewed; imported=%v", res.Imported)
	}
	// A publication log whose published_at is after the cutoff is late too.
	in2 := LogImport{Publications: []PublicationDraft{
		{Ref: "late-pub", SnapshotID: "SNAP-V9", Subject: SubjectRef{"shipment", ship},
			Version: 9, Kind: "formal", Status: "verified", PublishedAt: utc(23, 15)},
	}}
	res2, _ := ApplyScope(scope, in2)
	if len(res2.LateArrivals) != 1 {
		t.Fatalf("late publication must be a late arrival, got %+v", res2)
	}
}

// ---- 6. combined claims: a shared prerequisite outside scope -------------

func TestSharedPrerequisiteIsPreserved(t *testing.T) {
	ship := "AWB10000000001"
	scope := mustScope(t, "platform", "screening")
	in := LogImport{
		Events: []EventDraft{
			{Ref: "shared-input", System: "screening", Occurred: exact(utc(8, 0)),
				Evidence: []string{"IMG-1"}},
		},
		Publications: []PublicationDraft{
			{Ref: "pub-v1", SnapshotID: "SNAP-V1", Subject: SubjectRef{"shipment", ship},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: utc(10, 0),
				InputRefs: []string{"shared-input"}},
		},
	}
	target := Target{ShipmentKey: ship, Levels: []string{"shipment"}, Cutoff: utc(22, 0)}
	w := buildCase(t, scope, in, target)
	an := analyze(t, w, AnalyzeOptions{Scope: scope})
	// The causal edge shared-input -> pub-v1 must exist even though the input
	// carries no status of its own.
	found := false
	for _, c := range an.Constraints {
		if c.Kind == "causal" && c.From == RecVar("shared-input") && c.To == RecVar("pub-v1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("shared publication prerequisite must remain a constraint: %+v", an.Constraints)
	}
	if len(an.Material.RecordIDs) == 0 {
		t.Fatal("material manifest must pin the reviewed records")
	}
}
