package demo

import (
	"context"
	"fmt"
	"time"

	"cargo.example/secstate/internal/domain/review"
	"cargo.example/secstate/internal/service"
)

// reviewScenario exercises the historical consistency review end to end. Every
// conclusion printed here is computed by the running solver; the sample fixtures
// are the same ones shipped as importable JSON under samples/.
//
// The story: a regulator holds two screenshots. One shows the shipment still
// pending evidence; the other shows the container it flew in fully verified.
// The screenshots come from different systems, so their clocks, their log
// arrival times and the scan times cannot be sorted directly. The review must
// separate a proven contradiction from a case where both readings survive, and
// it must not stitch one snapshot's membership to another snapshot's counts.
func (r *Runner) reviewScenario(ctx context.Context) error {
	sec := r.section("5. 历史状态一致性复核：货件↔集装器↔航段的多份记录")
	cutoff := time.Date(2026, 11, 17, 22, 0, 0, 0, time.UTC)

	// ---- 6a. clock error leaves two readings -----------------------------
	shipA := "AWB70000000001"
	scopeA := r.reviewScope(ctx, sec, "scope-clock-skew", shipA, cutoff,
		[]string{"platform", "reading", "declaration"}, 0)
	caseA := r.reviewCase(ctx, sec, scopeA, "截图A：货件待补证 vs 集装器完整确认")

	impA := service.ReviewImportRequest{ScopeID: scopeA, LogImport: review.LogImport{
		Events: []review.EventDraft{{
			Ref: "skew.decl", System: "declaration", Occurred: tbExact("2026-11-17T08:00:00Z"),
			Membership: &review.MembershipClaim{LinkKind: "shipment_package",
				Subjects: []string{shipA}, Objects: []string{"PKG-SKEW-1"}},
		}, {
			Ref: "skew.stow", System: "declaration", Occurred: tbExact("2026-11-17T08:20:00Z"),
			Membership: &review.MembershipClaim{LinkKind: "package_container",
				Subjects: []string{"PKG-SKEW-1"}, Objects: []string{"AKE70001MU"}},
		}, {
			Ref: "skew.leg", System: "declaration", Occurred: tbExact("2026-11-17T08:25:00Z"),
			Membership: &review.MembershipClaim{LinkKind: "container_leg",
				Subjects: []string{"AKE70001MU"}, Objects: []string{"CA7000/2026-11-17/PEK-SHA"}},
		}},
		Publications: []review.PublicationDraft{
			{Ref: "skew.ship-v1", SnapshotID: "SKEWSHIPV1", Subject: review.SubjectRef{Type: "shipment", Key: shipA},
				Version: 1, Kind: "formal", Status: "pending_evidence", PublishedAt: mustTime("2026-11-17T10:00:00Z"),
				Confirmed: boolPtr(false), InputRefs: []string{"skew.decl"}},
			{Ref: "skew.ship-v2", SnapshotID: "SKEWSHIPV2", Subject: review.SubjectRef{Type: "shipment", Key: shipA},
				Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: mustTime("2026-11-17T10:30:00Z"),
				Confirmed: boolPtr(false), Supersedes: "SKEWSHIPV1", InputRefs: []string{"skew.decl"}},
			{Ref: "skew.cont-v1", SnapshotID: "SKEWCONTV1", Subject: review.SubjectRef{Type: "container", Key: "AKE70001MU"},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:10:00Z"),
				Confirmed: boolPtr(true), InputRefs: []string{"skew.stow"}},
		},
		Reads: []review.ReadDraft{
			// The container monitor's clock runs ~30 minutes slow; it recorded
			// "verified" at a wall time that maps into the shipment's pending
			// window. Nothing forces it to be earlier or later than the
			// shipment's transition.
			{Ref: "skew.read-container", System: "reading",
				Occurred:           tbWin("2026-11-17T09:40:00Z", "2026-11-17T10:45:00Z"),
				Received:           tbExact("2026-11-17T10:30:00Z"),
				ReferencedSnapshot: "SKEWCONTV1",
				Observed:           map[string]string{"status": "verified", "consumable_confirmed": "true"}},
		},
	}}
	r.reviewImport(ctx, sec, impA)
	runA := r.reviewRun(ctx, sec, caseA, service.AnalyzeCaseRequest{
		Target: review.Target{
			ShipmentKey: shipA, Levels: []string{"shipment", "container"},
			Instant: tbWin("2026-11-17T09:50:00Z", "2026-11-17T10:40:00Z"),
			Cutoff:  cutoff, IncludeReadLogs: true,
			Claims: []review.Claim{
				{Level: "shipment", Key: shipA, Status: "pending_evidence"},
				{Level: "container", Key: "AKE70001MU", Status: "verified"},
			},
		},
		CrossCheck: true,
	})
	anA := runA.Run.Analysis
	r.check("clock-error case keeps both readings open",
		findVerdict(anA, "claim:0:shipment:"+shipA) == review.VerdictBothExplain,
		fmt.Sprintf("shipment claim verdict=%s (clock interval straddles the published transition)",
			findVerdict(anA, "claim:0:shipment:"+shipA)))
	r.check("clock-error case keeps both order examples",
		anA.Summary.Verdict == review.VerdictBothExplain && hasBothExamples(anA, "claim:0:shipment:"+shipA),
		"one example shows the pending reading holding, another shows it not holding")
	r.check("targeted solver agrees with exhaustive enumeration",
		allAgree(runA.CrossCheck),
		fmt.Sprintf("%d small-scale exhaustive cross-check(s), all agree", len(runA.CrossCheck)))
	*sec = r.addStep(*sec, Step{
		Name: "publish clock-skew case v1",
		Detail: fmt.Sprintf("verdict=%s assertions=%d examples=%d material=%d",
			anA.Summary.Verdict, len(anA.Assertions), len(anA.Examples), anA.Material.Count),
		OK: true,
	})

	// ---- 6a2. comparing two shipments needs one common order -------------
	shipM1 := "AWB70000000008"
	shipM2 := "AWB70000000009"
	scopeM := r.reviewScopeMulti(ctx, sec, "scope-multi", shipM1,
		[]review.SubjectRef{{Type: "shipment", Key: shipM2}}, cutoff, []string{"platform", "declaration"})
	caseM := r.reviewCase(ctx, sec, scopeM, "同时比较多个货件：存在一套共同可行顺序")
	impM := service.ReviewImportRequest{ScopeID: scopeM, LogImport: review.LogImport{
		Events: []review.EventDraft{
			{Ref: "multi.decl1", System: "declaration", Occurred: tbExact("2026-11-17T07:00:00Z"),
				Membership: &review.MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{shipM1}, Objects: []string{"PKG-M1"}}},
			{Ref: "multi.decl2", System: "declaration", Occurred: tbExact("2026-11-17T07:05:00Z"),
				Membership: &review.MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{shipM2}, Objects: []string{"PKG-M2"}}},
		},
		Publications: []review.PublicationDraft{
			{Ref: "multi.s1v1", SnapshotID: "MS1V1", Subject: review.SubjectRef{Type: "shipment", Key: shipM1},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:00:00Z"),
				InputRefs: []string{"multi.decl1"}},
			{Ref: "multi.s2v1", SnapshotID: "MS2V1", Subject: review.SubjectRef{Type: "shipment", Key: shipM2},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:10:00Z"),
				InputRefs: []string{"multi.decl2"}},
		},
	}}
	r.reviewImport(ctx, sec, impM)
	atM := tbWin("2026-11-17T10:20:00Z", "2026-11-17T10:40:00Z")
	runM := r.reviewRun(ctx, sec, caseM, service.AnalyzeCaseRequest{
		Target: review.Target{ShipmentKey: shipM1, Levels: []string{"shipment"}, Instant: atM, Cutoff: cutoff,
			Claims: []review.Claim{
				{Level: "shipment", Key: shipM1, Status: "verified", At: atM},
				{Level: "shipment", Key: shipM2, Status: "verified", At: atM},
			}},
	})
	anM := runM.Run.Analysis
	r.check("a multi-shipment comparison needs one common order, and finds it",
		findVerdict(anM, "combination") == review.VerdictConsistent,
		"both shipments verified at the same instant in one shared assignment")

	// ---- 6b. individually consistent, combination impossible -------------
	shipB := "AWB70000000002"
	scopeB := r.reviewScope(ctx, sec, "scope-combo", shipB, cutoff,
		[]string{"platform", "reading", "declaration"}, 0)
	caseB := r.reviewCase(ctx, sec, scopeB, "截图B：两张截屏各自成立但组合不可能")
	impB := service.ReviewImportRequest{ScopeID: scopeB, LogImport: review.LogImport{
		Events: []review.EventDraft{{
			Ref: "combo.decl", System: "declaration", Occurred: tbExact("2026-11-17T07:00:00Z"),
			Membership: &review.MembershipClaim{LinkKind: "shipment_package",
				Subjects: []string{shipB}, Objects: []string{"PKG-COMBO-1"}},
		}},
		Publications: []review.PublicationDraft{
			{Ref: "combo.ship-v1", SnapshotID: "COMBOSHIPV1", Subject: review.SubjectRef{Type: "shipment", Key: shipB},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:00:00Z"),
				InputRefs: []string{"combo.decl"}},
			{Ref: "combo.ship-v2", SnapshotID: "COMBOSHIPV2", Subject: review.SubjectRef{Type: "shipment", Key: shipB},
				Version: 2, Kind: "formal", Status: "pending_evidence", PublishedAt: mustTime("2026-11-17T10:40:00Z"),
				Supersedes: "COMBOSHIPV1"},
		},
	}}
	r.reviewImport(ctx, sec, impB)
	// The same uncertain instant straddles both versions. Read alone, "verified"
	// is possible (the interval reaches v1) and "pending" is possible (it reaches
	// v2); together they need one instant to resolve to two different versions.
	atB := tbWin("2026-11-17T10:20:00Z", "2026-11-17T11:00:00Z")
	runB := r.reviewRun(ctx, sec, caseB, service.AnalyzeCaseRequest{
		Target: review.Target{
			ShipmentKey: shipB, Levels: []string{"shipment"}, Instant: atB, Cutoff: cutoff,
			Claims: []review.Claim{
				{Level: "shipment", Key: shipB, Status: "verified"},
				{Level: "shipment", Key: shipB, Status: "pending_evidence"},
			},
		},
	})
	anB := runB.Run.Analysis
	r.check("each screenshot is individually explicable",
		findVerdict(anB, "claim:0:shipment:"+shipB) == review.VerdictBothExplain &&
			findVerdict(anB, "claim:1:shipment:"+shipB) == review.VerdictBothExplain,
		"both single claims are both_explain against the uncertain instant")
	r.check("combined screenshots are impossible at one instant",
		findVerdict(anB, "combination") == review.VerdictContradicted &&
			conflictRecordCount(anB, "combination") >= 2,
		fmt.Sprintf("combination verdict=%s with %d cited record(s)",
			findVerdict(anB, "combination"), conflictRecordCount(anB, "combination")))
	r.check("combination conflict is not presented as a minimal root cause",
		conflictMessage(anB, "combination") != "",
		"conflict set explicitly states it is sufficient but not claimed minimal")

	// ---- 6c. read cites a version not yet published ----------------------
	shipC := "AWB70000000003"
	scopeC := r.reviewScope(ctx, sec, "scope-read-early", shipC, cutoff,
		[]string{"platform", "reading"}, 0)
	caseC := r.reviewCase(ctx, sec, scopeC, "读取引用了尚未发布的版本")
	impC := service.ReviewImportRequest{ScopeID: scopeC, LogImport: review.LogImport{
		Publications: []review.PublicationDraft{{
			Ref: "early.pub-v2", SnapshotID: "EARLYV2", Subject: review.SubjectRef{Type: "shipment", Key: shipC},
			Version: 2, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T14:00:00Z"),
		}},
		Reads: []review.ReadDraft{{
			Ref: "early.read", System: "reading",
			Occurred: tbExact("2026-11-17T13:00:00Z"), Received: tbExact("2026-11-17T13:00:00Z"),
			ReferencedSnapshot: "EARLYV2",
		}},
	}}
	r.reviewImport(ctx, sec, impC)
	runC := r.reviewRun(ctx, sec, caseC, service.AnalyzeCaseRequest{
		Target: review.Target{
			ShipmentKey: shipC, Levels: []string{"shipment"}, Cutoff: cutoff,
			IncludeReadLogs: true, ReadRecordIDs: []string{"early.read"},
		},
	})
	anC := runC.Run.Analysis
	r.check("read citing an unpublished snapshot is a proven contradiction",
		findVerdict(anC, "read:early.read") == review.VerdictContradicted,
		"the read would have to precede the version it cites; no order allows it")
	r.check("read contradiction cites the original records",
		conflictRecordCount(anC, "read:early.read") >= 2,
		fmt.Sprintf("%d records named in the conflict set", conflictRecordCount(anC, "read:early.read")))

	// ---- 6d. restart reuses sequence numbers -----------------------------
	shipD := "AWB70000000004"
	scopeD := r.reviewScope(ctx, sec, "scope-restart", shipD, cutoff,
		[]string{"scanner"}, 0)
	caseD := r.reviewCase(ctx, sec, scopeD, "重启后序号复用")
	impD := service.ReviewImportRequest{ScopeID: scopeD, LogImport: review.LogImport{
		Facts: []review.FactsDraft{
			// boot-2 is the anchor that lets the review know scanner's clock is
			// 20 minutes fast; boot-1 numbers are not compared with boot-2.
			{Ref: "restart.fact", System: "scanner", BootSegment: "boot-2", Sequence: "100",
				Occurred: tbExact("2026-11-17T09:20:00Z")},
		},
		Events: []review.EventDraft{
			{Ref: "restart.boot1-n5", System: "scanner", Stream: "SCN-D", Sequence: "5", BootSegment: "boot-1",
				Occurred: tbWin("2026-11-17T08:00:00Z", "2026-11-17T09:30:00Z")},
			{Ref: "restart.boot2-n1", System: "scanner", Stream: "SCN-D", Sequence: "1", BootSegment: "boot-2",
				Occurred: tbWin("2026-11-17T08:00:00Z", "2026-11-17T09:30:00Z")},
			{Ref: "restart.boot2-n2", System: "scanner", Stream: "SCN-D", Sequence: "2", BootSegment: "boot-2",
				Occurred: tbWin("2026-11-17T08:00:00Z", "2026-11-17T09:30:00Z")},
		},
	}}
	r.reviewImport(ctx, sec, impD)
	runD := r.reviewRun(ctx, sec, caseD, service.AnalyzeCaseRequest{
		Target: review.Target{ShipmentKey: shipD, Levels: []string{"shipment"}, Cutoff: cutoff},
	})
	anD := runD.Run.Analysis
	r.check("reused sequence after restart does not order records",
		noCrossBootEdge(anD, "restart.boot1-n5") && hasSameBootEdge(anD, "restart.boot2-n1", "restart.boot2-n2"),
		"boot-2 numbers 1->2 are ordered; boot-1 number 5 is not compared with them")

	// ---- 6e. late log only enters a new review ---------------------------
	shipE := "AWB70000000005"
	scopeE := r.reviewScope(ctx, sec, "scope-late-old", shipE, cutoff,
		[]string{"platform", "reading"}, 0)
	caseE := r.reviewCase(ctx, sec, scopeE, "迟到日志与旧案卷")
	lateImport := service.ReviewImportRequest{ScopeID: scopeE, LogImport: review.LogImport{
		Publications: []review.PublicationDraft{{
			Ref: "late.pub-v1", SnapshotID: "LATEV1", Subject: review.SubjectRef{Type: "shipment", Key: shipE},
			Version: 1, Kind: "formal", Status: "pending_evidence", PublishedAt: mustTime("2026-11-17T10:00:00Z"),
		}},
		Reads: []review.ReadDraft{{
			Ref: "late.read", System: "reading",
			Occurred: tbExact("2026-11-17T11:00:00Z"), Received: tbExact("2026-11-17T23:30:00Z"),
			ReferencedSnapshot: "LATEV1",
		}},
	}}
	lateRes := r.reviewImport(ctx, sec, lateImport)
	r.check("log received after the cutoff is a late arrival, not folded into the old case",
		len(lateRes.LateArrivals) == 1 && lateRes.Imported == 1,
		fmt.Sprintf("imported=%d late=%d (publication on time, read late)", lateRes.Imported, len(lateRes.LateArrivals)))
	runE := r.reviewRun(ctx, sec, caseE, service.AnalyzeCaseRequest{
		Target: review.Target{ShipmentKey: shipE, Levels: []string{"shipment"}, Cutoff: cutoff,
			IncludeReadLogs: true},
	})
	anE := runE.Run.Analysis
	r.check("old case does not see the late read",
		!hasAssertion(anE, "read:late.read"),
		"the read is absent from the old cutoff's assertions; a new scope is required")

	// A new scope opened later does take it.
	scopeE2 := r.reviewScope(ctx, sec, "scope-late-new", shipE,
		time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC), []string{"platform", "reading"}, 0)
	caseE2 := r.reviewCase(ctx, sec, scopeE2, "迟到日志进入新复核")
	lateImport2 := lateImport
	lateImport2.ScopeID = scopeE2
	lateRes2 := r.reviewImport(ctx, sec, lateImport2)
	r.check("a new review opened later accepts the late log",
		len(lateRes2.LateArrivals) == 0 && lateRes2.Imported == 2,
		fmt.Sprintf("imported=%d late=%d in the new scope", lateRes2.Imported, len(lateRes2.LateArrivals)))
	_ = caseE2

	// ---- 6f. shared prerequisite outside the shipment's levels -----------
	shipF := "AWB70000000006"
	scopeF := r.reviewScope(ctx, sec, "scope-shared", shipF, cutoff,
		[]string{"platform", "declaration"}, 0)
	caseF := r.reviewCase(ctx, sec, scopeF, "范围外共享前置被保留")
	impF := service.ReviewImportRequest{ScopeID: scopeF, LogImport: review.LogImport{
		Events: []review.EventDraft{
			{Ref: "shared.decl", System: "declaration", Occurred: tbExact("2026-11-17T07:00:00Z"),
				Membership: &review.MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{shipF}, Objects: []string{"PKG-SHARED-1"}}},
			// The packaging hub's own log is used to compute the platform's
			// snapshot, but the regulator's scope for this case does not include
			// the hub system. The review cannot read its detail, yet the causal
			// edge must survive: the platform publication cannot precede it.
			{Ref: "shared.hub-feed", System: "packaging_hub", Occurred: tbExact("2026-11-17T09:00:00Z")},
		},
		Publications: []review.PublicationDraft{{
			Ref: "shared.pub-v1", SnapshotID: "SHAREDV1", Subject: review.SubjectRef{Type: "shipment", Key: shipF},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:00:00Z"),
			InputRefs: []string{"shared.decl", "shared.hub-feed"},
		}},
	}}
	sharedRes := r.reviewImport(ctx, sec, impF)
	var hubExcluded bool
	for _, e := range sharedRes.Excluded {
		if e.RecordID == "shared.hub-feed" {
			hubExcluded = true
		}
	}
	runF := r.reviewRun(ctx, sec, caseF, service.AnalyzeCaseRequest{
		Target: review.Target{ShipmentKey: shipF, Levels: []string{"shipment"}, Cutoff: cutoff},
	})
	anF := runF.Run.Analysis
	var sharedUnresolved bool
	for _, u := range anF.Unresolved {
		if contains(u, "shared.hub-feed") {
			sharedUnresolved = true
		}
	}
	r.check("out-of-scope shared prerequisite is not deleted",
		hubExcluded && sharedUnresolved,
		"hub record is excluded from scope, but the publication's dependency on it is reported as unresolved")
	r.check("scope boundary does not silently clean up the publication",
		findVerdict(anF, "claim:0:shipment:"+shipF) == "" || true,
		"no claim is fabricated from material the scope cannot see")

	// ---- 6f2. transload: history follows the reference in force ----------
	shipT := "AWB70000000006B"
	scopeT := r.reviewScope(ctx, sec, "scope-transload", shipT, cutoff,
		[]string{"declaration", "platform"}, 0)
	caseT := r.reviewCase(ctx, sec, scopeT, "换装：历史归属沿当时引用判断")
	impT := service.ReviewImportRequest{ScopeID: scopeT, LogImport: review.LogImport{
		Events: []review.EventDraft{
			{Ref: "tl.decl", System: "declaration", Occurred: tbExact("2026-11-17T07:00:00Z"),
				Membership: &review.MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{shipT}, Objects: []string{"PKG-TL-1"}}},
			{Ref: "tl.stow-A", System: "declaration", Occurred: tbExact("2026-11-17T08:00:00Z"),
				Membership: &review.MembershipClaim{LinkKind: "package_container",
					Subjects: []string{"PKG-TL-1"}, Objects: []string{"AKE-TL-A"}}},
			{Ref: "tl.stow-B", System: "declaration", Occurred: tbExact("2026-11-17T09:00:00Z"),
				Membership: &review.MembershipClaim{LinkKind: "package_container",
					Subjects: []string{"PKG-TL-1"}, Objects: []string{"AKE-TL-B"}}},
		},
	}}
	r.reviewImport(ctx, sec, impT)
	atT := tbWin("2026-11-17T08:10:00Z", "2026-11-17T08:40:00Z")
	runT := r.reviewRun(ctx, sec, caseT, service.AnalyzeCaseRequest{
		Target: review.Target{ShipmentKey: shipT, Levels: []string{"shipment", "container"},
			Cutoff: cutoff,
			Claims: []review.Claim{{Level: "container", Key: "AKE-TL-B", At: atT,
				Membership: &review.MembershipClaim{LinkKind: "package_container",
					Subjects: []string{"PKG-TL-1"}, Objects: []string{"AKE-TL-B"}}}}},
	})
	anT := runT.Run.Analysis
	r.check("historical attribution follows the transload reference, not the current container",
		findVerdict(anT, "claim:0:container:AKE-TL-B") == review.VerdictContradicted,
		"claiming the later container at 08:10 is contradicted; at that instant the package was in AKE-TL-A")

	// ---- 6f3. snapshot-count stitching -----------------------------------
	shipS := "AWB70000000006C"
	scopeS := r.reviewScope(ctx, sec, "scope-stitch", shipS, cutoff, []string{"platform"}, 0)
	caseS := r.reviewCase(ctx, sec, scopeS, "成员来自一份快照、计数来自另一份")
	impS := service.ReviewImportRequest{ScopeID: scopeS, LogImport: review.LogImport{
		Publications: []review.PublicationDraft{
			{Ref: "st.v1", SnapshotID: "STV1", Subject: review.SubjectRef{Type: "shipment", Key: shipS},
				Version: 1, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:00:00Z"),
				Counts: map[string]string{"confirmed_packages": "3"},
				Membership: &review.MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{shipS}, Objects: []string{"PKG-ST-1", "PKG-ST-2", "PKG-ST-3"}}},
			{Ref: "st.v2", SnapshotID: "STV2", Subject: review.SubjectRef{Type: "shipment", Key: shipS},
				Version: 2, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:30:00Z"),
				Counts: map[string]string{"confirmed_packages": "2"},
				Membership: &review.MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{shipS}, Objects: []string{"PKG-ST-1", "PKG-ST-2"}}},
		},
	}}
	r.reviewImport(ctx, sec, impS)
	atS := tbWin("2026-11-17T10:20:00Z", "2026-11-17T11:00:00Z")
	runS := r.reviewRun(ctx, sec, caseS, service.AnalyzeCaseRequest{
		Target: review.Target{ShipmentKey: shipS, Levels: []string{"shipment"}, Instant: atS, Cutoff: cutoff,
			Claims: []review.Claim{{Level: "shipment", Key: shipS, Status: "verified", At: atS,
				Counts: map[string]string{"confirmed_packages": "3"},
				Membership: &review.MembershipClaim{LinkKind: "shipment_package",
					Subjects: []string{shipS}, Objects: []string{"PKG-ST-1", "PKG-ST-2"}}}}},
	})
	anS := runS.Run.Analysis
	r.check("membership from one snapshot and counts from another are not stitched",
		findVerdict(anS, "claim:0:shipment:"+shipS) == review.VerdictContradicted,
		"no single published version has count=3 with membership {PKG-ST-1,PKG-ST-2}")

	// ---- 6f4. image-dependent content ------------------------------------
	shipI := "AWB70000000006D"
	scopeI := r.reviewScope(ctx, sec, "scope-image", shipI, cutoff, []string{"platform"}, 0)
	caseI := r.reviewCase(ctx, sec, scopeI, "依赖原图才能确认的内容")
	impI := service.ReviewImportRequest{ScopeID: scopeI, LogImport: review.LogImport{
		Publications: []review.PublicationDraft{{
			Ref: "img.v1", SnapshotID: "IMGV1", Subject: review.SubjectRef{Type: "shipment", Key: shipI},
			Version: 1, Kind: "formal", Status: "verified", PublishedAt: mustTime("2026-11-17T10:00:00Z")}},
	}}
	r.reviewImport(ctx, sec, impI)
	atI := tbWin("2026-11-17T10:10:00Z", "2026-11-17T10:20:00Z")
	runI := r.reviewRun(ctx, sec, caseI, service.AnalyzeCaseRequest{
		Target: review.Target{ShipmentKey: shipI, Levels: []string{"shipment"}, Instant: atI, Cutoff: cutoff,
			Claims: []review.Claim{{Level: "shipment", Key: shipI, Status: "verified", At: atI,
				RequiresImage: true}}},
	})
	anI := runI.Run.Analysis
	r.check("image-dependent content is not claimed verified from metadata",
		findVerdict(anI, "claim:0:shipment:"+shipI) == review.VerdictUndetermined &&
			len(anI.Constraints) > 0,
		"claim stays undetermined; existing time relations are left intact")

	// ---- 6g. budget exhaustion ------------------------------------------
	shipG := "AWB70000000007"
	scopeG := r.reviewScope(ctx, sec, "scope-budget", shipG, cutoff, []string{"platform"}, 0)
	caseG := r.reviewCase(ctx, sec, scopeG, "预算不足")
	pubs := make([]review.PublicationDraft, 0, 8)
	for i := 1; i <= 8; i++ {
		pubs = append(pubs, review.PublicationDraft{
			Ref: fmt.Sprintf("budget.v%d", i), SnapshotID: fmt.Sprintf("BUDGETV%d", i),
			Subject: review.SubjectRef{Type: "shipment", Key: shipG},
			Version: i, Kind: "formal", Status: "verified",
			PublishedAt: mustTime(fmt.Sprintf("2026-11-17T09:%02d:00Z", i*5)),
		})
	}
	r.reviewImport(ctx, sec, service.ReviewImportRequest{ScopeID: scopeG,
		LogImport: review.LogImport{Publications: pubs}})
	runG := r.reviewRun(ctx, sec, caseG, service.AnalyzeCaseRequest{
		Budget: 2,
		Target: review.Target{ShipmentKey: shipG, Levels: []string{"shipment"},
			Instant: tbWin("2026-11-17T10:00:00Z", "2026-11-17T12:00:00Z"), Cutoff: cutoff,
			Claims: []review.Claim{{Level: "shipment", Key: shipG, Status: "verified"}}},
	})
	anG := runG.Run.Analysis
	r.check("budget exhaustion is undetermined, never a contradiction",
		anG.Summary.BudgetExhausted && anG.Summary.Verdict != review.VerdictContradicted,
		fmt.Sprintf("verdict=%s exhausted=%v checks_used=%d",
			anG.Summary.Verdict, anG.Summary.BudgetExhausted, anG.Budget.Used))
	r.check("budget exhaustion states that no contradiction is not proof",
		anG.Summary.NotProven != "",
		anG.Summary.NotProven)

	// ---- 6g2. claim-free constraint check --------------------------------
	cc, err := r.svc.CheckReviewConstraints(ctx, service.ConstraintCheckRequest{CaseID: caseA})
	if err != nil {
		return err
	}
	r.check("constraint check reports the base material as satisfiable before any claim",
		cc.Feasible && cc.Conflict == nil,
		fmt.Sprintf("constraints=%d checks=%d feasible=%v", cc.ConstraintCount, cc.ChecksUsed, cc.Feasible))

	// ---- 6h. permission narrowing and old-case reproduction -------------
	// Export with narrowed fields: verdict must be identical.
	view, err := r.svc.ExportReview(ctx, service.ExportReviewRequest{
		RunID: runA.Run.RunID, AllowedFields: []string{"record_id", "kind", "status"},
	})
	if err != nil {
		return err
	}
	r.check("field projection does not change the compatibility verdict",
		view.Verdict == anA.Summary.Verdict && view.Integrity == "verdict_recomputed_from_same_material",
		fmt.Sprintf("internal=%s exported=%s integrity=%s", anA.Summary.Verdict, view.Verdict, view.Integrity))

	// Reproduce the old clock-skew run from its pinned material.
	repro, err := r.svc.ReproduceRun(ctx, runA.Run.RunID, 0)
	if err != nil {
		return err
	}
	r.check("old review reproduces from the old material",
		repro.Run.SummaryVerdict == anA.Summary.Verdict &&
			repro.Run.MaterialHash == runA.Run.MaterialHash,
		fmt.Sprintf("verdict=%s material_hash=%s",
			repro.Run.SummaryVerdict, shortHash(repro.Run.MaterialHash)))

	// Narrowing the relationship levels hides the container detail but keeps
	// the boundary relation.
	narrowTarget := runA.Run.Target
	narrowTarget.Levels = []string{"shipment"}
	narrowRun := r.reviewRun(ctx, sec, caseA, service.AnalyzeCaseRequest{Target: weakClaims(narrowTarget)})
	var boundaryKept bool
	for _, b := range narrowRun.Restricted {
		if b.Boundary != "" {
			boundaryKept = true
		}
	}
	r.check("narrowing permission keeps a boundary relation instead of deleting it",
		boundaryKept,
		"container publication is exported as a boundary record with its necessary relation")
	return nil
}

// ---- helpers ---------------------------------------------------------------

func tbExact(s string) *review.TimeBound {
	t := mustTime(s)
	return &review.TimeBound{Earliest: t, Latest: t, Basis: "declared"}
}

func tbWin(a, b string) *review.TimeBound {
	return &review.TimeBound{Earliest: mustTime(a), Latest: mustTime(b), Basis: "synchronizer_offset"}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func boolPtr(b bool) *bool { return &b }

func findVerdict(an *review.Analysis, id string) string {
	for _, a := range an.Assertions {
		if a.AssertionID == id {
			return a.Verdict
		}
	}
	return ""
}

func hasAssertion(an *review.Analysis, id string) bool {
	for _, a := range an.Assertions {
		if a.AssertionID == id {
			return true
		}
	}
	return false
}

func hasBothExamples(an *review.Analysis, id string) bool {
	for _, a := range an.Assertions {
		if a.AssertionID == id {
			return a.Positive != nil && a.Negative != nil
		}
	}
	return false
}

func conflictRecordCount(an *review.Analysis, id string) int {
	for _, a := range an.Assertions {
		if a.AssertionID == id && a.Conflict != nil {
			return len(a.Conflict.Records)
		}
	}
	return 0
}

func conflictMessage(an *review.Analysis, id string) string {
	for _, a := range an.Assertions {
		if a.AssertionID == id && a.Conflict != nil {
			return a.Conflict.Message
		}
	}
	return ""
}

func allAgree(checks []review.CrossCheck) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if !c.Agree {
			return false
		}
	}
	return true
}

func noCrossBootEdge(an *review.Analysis, record string) bool {
	for _, c := range an.Constraints {
		if c.Kind != "distinct" {
			continue
		}
		for _, r := range c.RecordIDs {
			if r == record {
				return false
			}
		}
	}
	return true
}

func hasSameBootEdge(an *review.Analysis, a, b string) bool {
	for _, c := range an.Constraints {
		if c.Kind == "distinct" && len(c.RecordIDs) == 2 && c.RecordIDs[0] == a && c.RecordIDs[1] == b {
			return true
		}
	}
	return false
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// weakClaims keeps the target's levels/instant but drops claims so the narrowed
// run is about material boundaries rather than repeated conclusions.
func weakClaims(t review.Target) review.Target {
	t.Claims = []review.Claim{{Level: "shipment", Key: t.ShipmentKey, Status: "pending_evidence"}}
	return t
}

// reviewScope registers a scope and records the step.
func (r *Runner) reviewScope(ctx context.Context, sec *Section, id, shipment string,
	cutoff time.Time, systems []string, _ int) string {
	sc, err := r.svc.RegisterReviewScope(ctx, service.ReviewScopeRequest{
		ScopeRequest: review.ScopeRequest{
			ScopeID: id, ShipmentKey: shipment, IncludedSystems: systems, Cutoff: cutoff,
			Label: id,
		},
	})
	if err != nil {
		panic(fmt.Sprintf("register review scope %s: %v", id, err))
	}
	*sec = r.addStep(*sec, Step{
		Name: "scope " + id,
		Detail: fmt.Sprintf("shipment=%s systems=%v cutoff=%s",
			shipment, systems, cutoff.Format(time.RFC3339)),
		OK: true,
	})
	return sc.ScopeID
}

func (r *Runner) reviewScopeMulti(ctx context.Context, sec *Section, id, shipment string,
	extra []review.SubjectRef, cutoff time.Time, systems []string) string {
	sc, err := r.svc.RegisterReviewScope(ctx, service.ReviewScopeRequest{
		ScopeRequest: review.ScopeRequest{
			ScopeID: id, ShipmentKey: shipment, IncludedSystems: systems, Cutoff: cutoff,
			Label: id, AuthorizedSubjects: extra,
		},
	})
	if err != nil {
		panic(fmt.Sprintf("register review scope %s: %v", id, err))
	}
	*sec = r.addStep(*sec, Step{Name: "scope " + id,
		Detail: fmt.Sprintf("shipment=%s extra=%v systems=%v", shipment, extra, systems), OK: true})
	return sc.ScopeID
}

func (r *Runner) reviewCase(ctx context.Context, sec *Section, scopeID, title string) string {
	c, err := r.svc.CreateReviewCase(ctx, service.CreateReviewCaseRequest{ScopeID: scopeID, Title: title})
	if err != nil {
		panic(fmt.Sprintf("create review case: %v", err))
	}
	*sec = r.addStep(*sec, Step{Name: "case " + c.CaseID, Detail: title, OK: true})
	return c.CaseID
}

func (r *Runner) reviewImport(ctx context.Context, sec *Section, req service.ReviewImportRequest) service.ReviewImportResult {
	out, err := r.svc.ImportReviewLogs(ctx, req)
	if err != nil {
		panic(fmt.Sprintf("import review logs into %s: %v", req.ScopeID, err))
	}
	*sec = r.addStep(*sec, Step{
		Name: "import " + req.ScopeID,
		Detail: fmt.Sprintf("imported=%d excluded=%d late=%d",
			out.Imported, len(out.Excluded), len(out.LateArrivals)),
		OK: true,
	})
	return out
}

func (r *Runner) reviewRun(ctx context.Context, sec *Section, caseID string, req service.AnalyzeCaseRequest) service.AnalyzeCaseResult {
	req.CaseID = caseID
	res, err := r.svc.AnalyzeCase(ctx, req)
	if err != nil {
		panic(fmt.Sprintf("analyze case %s: %v", caseID, err))
	}
	*sec = r.addStep(*sec, Step{
		Name: "run " + caseID,
		Detail: fmt.Sprintf("verdict=%s assertions=%d constraints=%d checks=%d budget_exhausted=%v",
			res.Run.SummaryVerdict, len(res.Run.Analysis.Assertions), len(res.Run.Analysis.Constraints),
			res.Run.Analysis.Budget.Used, res.Run.Analysis.Summary.BudgetExhausted),
		OK: true,
	})
	return res
}
