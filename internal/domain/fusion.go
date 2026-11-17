package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Data statuses the platform publishes. They describe evidence quality, never
// physical clearance: this platform does not release cargo.
const (
	StatusVerified        = "verified"         // 已验证
	StatusPendingEvidence = "pending_evidence" // 待补证
	StatusConflictPending = "conflict_pending" // 冲突待裁定
	StatusNotPublishable  = "not_publishable"  // 不可发布
)

// Screening verdicts.
const (
	VerdictClear        = "CLEAR"
	VerdictReject       = "REJECT"
	VerdictHold         = "HOLD"
	VerdictInconclusive = "INCONCLUSIVE"
)

// Component is one scored completeness dimension with its explanation.
type Component struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Weight       float64  `json:"weight"`
	Score        float64  `json:"score"`
	Contribution float64  `json:"contribution"`
	Detail       string   `json:"detail"`
	Missing      []string `json:"missing,omitempty"`
	SourceEvents []string `json:"source_events"`
}

// ConflictSide is one participant of a contradiction.
type ConflictSide struct {
	EventID    string    `json:"event_id"`
	Source     string    `json:"source"`
	Result     string    `json:"result"`
	OccurredAt time.Time `json:"occurred_at"`
	ReceivedAt time.Time `json:"received_at"`
}

// Conflict is a set of contradictory device results no evidence resolves.
type Conflict struct {
	PackageKey  string         `json:"package_key,omitempty"`
	WindowStart time.Time      `json:"window_start"`
	WindowEnd   time.Time      `json:"window_end"`
	Results     []ConflictSide `json:"results"`
	Reason      string         `json:"reason"`
}

// Correction is one explicit supersede relationship that was applied.
type Correction struct {
	CorrectingEventID string    `json:"correcting_event_id"`
	SupersededEventID string    `json:"superseded_event_id"`
	AppliedAt         time.Time `json:"applied_at"`
	FromResult        string    `json:"from_result,omitempty"`
	ToResult          string    `json:"to_result"`
	Source            string    `json:"source"`
	Note              string    `json:"note,omitempty"`
}

// PackageState is the per-package fused result.
type PackageState struct {
	PackageKey        string    `json:"package_key"`
	Result            string    `json:"result,omitempty"`
	ResultEventID     string    `json:"result_event_id,omitempty"`
	ResultSource      string    `json:"result_source,omitempty"`
	ResultAt          time.Time `json:"result_at,omitempty"`
	HumanRuled        bool      `json:"human_ruled"`
	EvidenceEligible  bool      `json:"evidence_eligible"`
	Disposition       string    `json:"disposition"`
	DispositionReason string    `json:"disposition_reason,omitempty"`
	ScreeningEvents   []string  `json:"screening_event_ids"`
	ReviewEvents      []string  `json:"review_event_ids"`
	ResegmentCount    int       `json:"resegment_count"`
	InConflict        bool      `json:"in_conflict"`
}

// FusionInput is the pure input to the fusion engine.
type FusionInput struct {
	ShipmentKey       string
	KnowledgeAt       time.Time
	ActivePackages    []string
	Events            []*Event
	Dispositions      map[string]Disposition
	Violations        []RuleViolation
	ManifestEvidence  []string // evidence indexes a manifest claims to rely on
	Weights           EvidenceWeights
	CompletenessFloor float64
	// ConflictWindow bounds what counts as a contradiction: two differing
	// verdicts within this window and not separated by a handover are a
	// conflict. Differing verdicts further apart are treated as ordinary
	// re-screening drift (the later one wins). Zero means "no window bound".
	ConflictWindow time.Duration
}

// FusionResult is the fused status with full provenance.
type FusionResult struct {
	Status               string         `json:"status"`
	SecurityResult       string         `json:"security_result,omitempty"`
	EvidenceCompleteness float64        `json:"evidence_completeness"`
	Components           []Component    `json:"completeness_components"`
	Packages             []PackageState `json:"packages"`
	PendingItems         []PendingItem  `json:"pending_items"`
	Conflicts            []Conflict     `json:"conflicts"`
	Corrections          []Correction   `json:"corrections"`
	BlockingRuleCodes    []string       `json:"blocking_rule_codes"`
	BlockingReasons      []string       `json:"blocking_reasons"`
	InputEventIDs        []string       `json:"input_event_ids"`
	InputGraphLinks      []string       `json:"input_graph_links"`
	InputRuleKeys        []string       `json:"input_rule_keys"`
	Decision             string         `json:"decision_explanation"`
}

// Fuse computes the data status. The algorithm:
//  1. Supersede chains are resolved first; a superseded verdict never counts.
//  2. Device verdicts are segmented by screening handovers. Two different
//     verdicts inside one segment are a conflict — never silently resolved.
//  3. A human review is a ruling: it resolves its package and is recorded as a
//     correction when it explicitly supersedes a device event.
//  4. Completeness is a weighted sum over six evidence dimensions.
//  5. Status is verified only with complete, consistent, unblocked evidence.
func Fuse(in FusionInput) FusionResult {
	res := FusionResult{
		Components: []Component{}, PendingItems: []PendingItem{},
		Conflicts: []Conflict{}, Corrections: []Correction{},
		Packages: []PackageState{}, BlockingReasons: []string{},
	}
	w := in.Weights
	byID := map[string]*Event{}
	for _, ev := range in.Events {
		byID[ev.EventID] = ev
	}
	supersededBy := map[string]*Event{}
	for _, ev := range in.Events {
		if ev.SupersedesEventID != "" {
			supersededBy[ev.SupersedesEventID] = ev
		}
	}
	oldIDs := make([]string, 0, len(supersededBy))
	for id := range supersededBy {
		oldIDs = append(oldIDs, id)
	}
	sort.Strings(oldIDs)
	for _, oldID := range oldIDs {
		corrector := supersededBy[oldID]
		c := Correction{
			CorrectingEventID: corrector.EventID, SupersededEventID: oldID,
			AppliedAt: corrector.ReceivedAt, ToResult: corrector.Result(),
			Source: corrector.Source, Note: str(corrector.Payload, "note"),
		}
		if old, ok := byID[oldID]; ok {
			c.FromResult = old.Result()
		}
		res.Corrections = append(res.Corrections, c)
	}

	eligible := func(ev *Event) bool {
		d, ok := in.Dispositions[ev.EventID]
		return !ok || d.EvidenceEligible()
	}

	// ---- per-package fusion -------------------------------------------
	pkgEvents := map[string][]*Event{}
	var shipmentLevel []*Event
	for _, ev := range in.Events {
		if !eligible(ev) {
			continue
		}
		switch ev.MessageType {
		case TypeScreening:
			if ev.PackageKey == "" {
				shipmentLevel = append(shipmentLevel, ev)
				continue
			}
			pkgEvents[ev.PackageKey] = append(pkgEvents[ev.PackageKey], ev)
		case TypeManualReview:
			if ev.PackageKey != "" {
				pkgEvents[ev.PackageKey] = append(pkgEvents[ev.PackageKey], ev)
			}
		}
	}
	packages := append([]string(nil), in.ActivePackages...)
	if len(packages) == 0 {
		for p := range pkgEvents {
			packages = append(packages, p)
		}
	}
	sort.Strings(packages)
	activeSet := map[string]bool{}
	for _, p := range packages {
		activeSet[p] = true
	}
	// Verdicts recorded against a package that a split/merge has retired are
	// NOT carried over to any successor version. They remain visible as history
	// (and as a pending item), but the new physical packages need their own
	// screening.
	var notCarried []string
	for p, evs := range pkgEvents {
		if activeSet[p] {
			continue
		}
		for _, ev := range evs {
			if ev.MessageType == TypeScreening || ev.MessageType == TypeManualReview {
				notCarried = append(notCarried, ev.EventID)
			}
		}
		delete(pkgEvents, p)
	}
	if len(notCarried) > 0 {
		sort.Strings(notCarried)
		res.PendingItems = append(res.PendingItems, PendingItem{
			Code: "prior_verdict_not_carried_over", Severity: SevInfo,
			Detail: "packages not active at the cutoff (retired by split/merge): their verdicts are kept as " +
				"history but are not transferred to any successor package",
		})
	}

	// dispositions grouped by package (for the per-package explanation)
	pkgDisp := map[string]Disposition{}
	for _, d := range in.Dispositions {
		if d.Disposition != DispositionOK {
			pkgDisp[d.PackageKey] = d
		}
	}

	var screeningIDs, reviewIDs []string
	withResult := 0
	var noResultPkgs []string
	results := map[string]bool{}

	for _, p := range packages {
		ps := PackageState{
			PackageKey: p, Disposition: DispositionOK,
			EvidenceEligible: true,
			ScreeningEvents:  []string{}, ReviewEvents: []string{},
		}
		if d, ok := pkgDisp[p]; ok {
			ps.Disposition = d.Disposition
			ps.DispositionReason = d.Reason
		} else {
			ps.EvidenceEligible = true
		}
		evs := append([]*Event(nil), pkgEvents[p]...)
		// Drop superseded events: they are historical, not effective.
		effective := evs[:0]
		for _, ev := range evs {
			if _, isOld := supersededBy[ev.EventID]; isOld {
				continue
			}
			effective = append(effective, ev)
		}
		evs = effective
		sort.SliceStable(evs, func(i, j int) bool {
			if evs[i].OccurredAt.Equal(evs[j].OccurredAt) {
				return evs[i].ReceivedAt.Before(evs[j].ReceivedAt)
			}
			return evs[i].OccurredAt.Before(evs[j].OccurredAt)
		})

		// Segment device verdicts by screening handovers.
		segments := [][]*Event{}
		var cur []*Event
		var lastDevice *Event
		for _, ev := range evs {
			switch ev.MessageType {
			case TypeScreening:
				if lastDevice != nil && handoverBetween(in, p, lastDevice, ev) && len(cur) > 0 {
					segments = append(segments, cur)
					cur = nil
				}
				cur = append(cur, ev)
				lastDevice = ev
			case TypeManualReview:
				ps.ReviewEvents = append(ps.ReviewEvents, ev.EventID)
				reviewIDs = append(reviewIDs, ev.EventID)
			}
		}
		if len(cur) > 0 {
			segments = append(segments, cur)
		}

		var conflictSides []ConflictSide
		for si, seg := range segments {
			// Walk the segment in occurrence order, tracking the current
			// effective verdict. A differing verdict inside the conflict window
			// is a contradiction; outside the window it is ordinary
			// re-screening drift and the newer verdict simply wins.
			distinctInSeg := map[string][]*Event{}
			for _, ev := range seg {
				distinctInSeg[ev.Result()] = append(distinctInSeg[ev.Result()], ev)
			}
			if len(distinctInSeg) > 1 && in.ConflictWindow > 0 {
				// Windowed interpretation: split the segment into runs and
				// only call a conflict when two different verdicts are close.
				var effective *Event
				for _, ev := range seg {
					if effective == nil {
						effective = ev
						continue
					}
					if effective.Result() == ev.Result() {
						effective = ev
						continue
					}
					gap := ev.OccurredAt.Sub(effective.OccurredAt)
					if gap < 0 {
						gap = -gap
					}
					if gap <= in.ConflictWindow {
						ps.InConflict = true
						conflictSides = append(conflictSides, sideOf(effective), sideOf(ev))
						// Keep both; a human must rule.
					} else {
						effective = ev // re-screening drift
					}
				}
				if !ps.InConflict && effective != nil {
					ps.Result = effective.Result()
					ps.ResultEventID = effective.EventID
					ps.ResultSource = effective.Source
					ps.ResultAt = effective.OccurredAt
				}
				ps.ResegmentCount = si
				continue
			}
			var resultNames []string
			for name := range distinctInSeg {
				resultNames = append(resultNames, name)
			}
			sort.Strings(resultNames)
			if len(distinctInSeg) > 1 {
				ps.InConflict = true
				// Every distinct verdict in this segment participates, in a
				// stable order so a replay produces the same conflict set.
				for _, name := range resultNames {
					conflictSides = append(conflictSides, sideOf(distinctInSeg[name][0]))
				}
				continue
			}
			// A segment with one verdict; keep its latest event.
			for _, name := range resultNames {
				if si == len(segments)-1 || ps.Result == "" {
					sameResult := distinctInSeg[name]
					latest := sameResult[len(sameResult)-1]
					ps.Result = latest.Result()
					ps.ResultEventID = latest.EventID
					ps.ResultSource = latest.Source
					ps.ResultAt = latest.OccurredAt
				}
			}
			ps.ResegmentCount = si
		}
		for _, ev := range evs {
			if ev.MessageType == TypeScreening {
				ps.ScreeningEvents = append(ps.ScreeningEvents, ev.EventID)
				screeningIDs = append(screeningIDs, ev.EventID)
			}
		}

		// Human ruling: the latest eligible review overrides device verdicts.
		if len(ps.ReviewEvents) > 0 {
			var latestReview *Event
			for _, ev := range evs {
				if ev.MessageType == TypeManualReview {
					latestReview = ev
				}
			}
			if latestReview != nil && latestReview.Result() != "" {
				ps.Result = latestReview.Result()
				ps.ResultEventID = latestReview.EventID
				ps.ResultSource = latestReview.Source
				ps.ResultAt = latestReview.OccurredAt
				ps.HumanRuled = true
				ps.InConflict = false
				conflictSides = nil
			}
		}

		if ps.InConflict && len(conflictSides) > 0 {
			start, end := conflictSides[0].OccurredAt, conflictSides[0].OccurredAt
			for _, c := range conflictSides[1:] {
				if c.OccurredAt.Before(start) {
					start = c.OccurredAt
				}
				if c.OccurredAt.After(end) {
					end = c.OccurredAt
				}
			}
			res.Conflicts = append(res.Conflicts, Conflict{
				PackageKey: p, WindowStart: start, WindowEnd: end, Results: conflictSides,
				Reason: "contradictory device results arrived for this package with no handover evidence " +
					"separating them; a human ruling is required, the platform will not pick one",
			})
		}

		// A verdict that needs human confirmation but has none.
		if !ps.InConflict && ps.Result != "" {
			needReview := ps.Result != VerdictClear
			if needReview && !ps.HumanRuled {
				res.PendingItems = append(res.PendingItems, PendingItem{
					Code: "review_missing", Severity: SevWarning, PackageKey: p,
					Detail: fmt.Sprintf("device verdict %s for package %s has no human review conclusion",
						ps.Result, p),
				})
			}
		}
		if ps.Result != "" {
			withResult++
			results[ps.Result] = true
		} else if !ps.InConflict {
			noResultPkgs = append(noResultPkgs, p)
		}
		ps.ScreeningEvents = SortedUnique(ps.ScreeningEvents)
		ps.ReviewEvents = SortedUnique(ps.ReviewEvents)
		res.Packages = append(res.Packages, ps)
	}
	if len(packages) == 0 && len(shipmentLevel) > 0 {
		// No package identities yet: a shipment-level verdict still counts as
		// evidence, but it cannot be attributed per package.
		for _, ev := range shipmentLevel {
			screeningIDs = append(screeningIDs, ev.EventID)
			results[ev.Result()] = true
		}
	}

	// ---- completeness components --------------------------------------
	declaredPkgs := map[string]bool{}
	handoverScreening := false
	handoverDeparture := false
	for _, ev := range in.Events {
		switch ev.MessageType {
		case TypeDeclaration:
			for _, raw := range strs(ev.Payload, "packages") {
				declaredPkgs[CanonicalKeyOrRaw(KeyPackage, raw)] = true
			}
		case TypeHandover:
			switch ev.Mode() {
			case ModeScreening:
				handoverScreening = true
			case ModeDeparture, ModeLoading:
				handoverDeparture = true
			}
		}
	}

	var declMissing []string
	for _, p := range packages {
		if !declaredPkgs[p] {
			declMissing = append(declMissing, p)
		}
	}
	declScore := 0.0
	if len(packages) > 0 {
		declScore = float64(len(packages)-len(declMissing)) / float64(len(packages))
	}
	res.Components = append(res.Components, Component{
		Key: "declaration", Label: "运单申报覆盖", Weight: w.Declaration, Score: round3(declScore),
		Contribution: round3(w.Declaration * declScore),
		Detail:       fmt.Sprintf("%d/%d 个在效包装件有申报记录", len(packages)-len(declMissing), len(packages)),
		Missing:      declMissing, SourceEvents: idsOfType(in.Events, TypeDeclaration),
	})

	hsScore := 0.0
	var hsMissing []string
	if handoverScreening {
		hsScore = 1
	} else {
		hsMissing = []string{"handover(screening)"}
		res.PendingItems = append(res.PendingItems, PendingItem{
			Code: "missing_handover", Severity: SevWarning,
			Detail: "no screening handover record; receipt order cannot be corroborated",
		})
	}
	res.Components = append(res.Components, Component{
		Key: "handover", Label: "交接记录", Weight: w.Handover, Score: round3(hsScore),
		Contribution: round3(w.Handover * hsScore),
		Detail:       boolDetail(handoverScreening, "已收到送检交接", "未收到送检交接"),
		Missing:      hsMissing, SourceEvents: idsOfType(in.Events, TypeHandover),
	})

	scScore := 0.0
	if len(res.Packages) > 0 {
		scScore = float64(withResult) / float64(len(res.Packages))
	} else if len(shipmentLevel) > 0 {
		scScore = 1
	}
	if len(noResultPkgs) > 0 {
		res.PendingItems = append(res.PendingItems, PendingItem{
			Code: "screening_result_missing", Severity: SevError,
			Detail: fmt.Sprintf("packages without a usable screening verdict: %v", noResultPkgs),
		})
	}
	res.Components = append(res.Components, Component{
		Key: "screening", Label: "设备安检结论", Weight: w.Screening, Score: round3(scScore),
		Contribution: round3(w.Screening * scScore),
		Detail:       fmt.Sprintf("%d/%d 个包装件有可用安检结论", withResult, len(res.Packages)),
		Missing:      noResultPkgs, SourceEvents: SortedUnique(screeningIDs),
	})

	// Evidence index completeness. Two sub-scores: the verdict carries its
	// declared index, and the image index library actually archived it. A
	// verdict with a bag of indexes that no archive event confirms is not
	// complete evidence, even though the index string is present.
	var checkEvents []*Event
	for _, ev := range in.Events {
		if (ev.MessageType == TypeScreening || ev.MessageType == TypeManualReview) && eligible(ev) {
			checkEvents = append(checkEvents, ev)
		}
	}
	archived := map[string]bool{}
	for _, ev := range in.Events {
		if ev.MessageType == TypeEvidence {
			for _, idx := range ev.EvidenceIndexes {
				archived[idx] = true
			}
		}
	}
	var evMissing, referenced, archivedCount []string
	refSeen := map[string]bool{}
	for _, ev := range checkEvents {
		need := 1
		if n, ok := num(ev.Payload, "required_evidence_count"); ok && n > 0 {
			need = int(n)
		}
		if len(ev.EvidenceIndexes) < need {
			evMissing = append(evMissing, ev.EventID)
		}
		for _, idx := range ev.EvidenceIndexes {
			if !refSeen[idx] {
				refSeen[idx] = true
				referenced = append(referenced, idx)
				if archived[idx] {
					archivedCount = append(archivedCount, idx)
				}
			}
		}
	}
	var manifestMissing []string
	for _, want := range in.ManifestEvidence {
		found := false
		for _, ev := range in.Events {
			if contains(ev.EvidenceIndexes, want) {
				found = true
				break
			}
		}
		if !found {
			manifestMissing = append(manifestMissing, want)
			res.PendingItems = append(res.PendingItems, PendingItem{
				Code: "manifest_required_evidence_missing", Severity: SevError,
				Detail: fmt.Sprintf("舱单引用的必要证据索引 %s 在本账本中不存在", want),
			})
		}
	}
	presence := 0.0
	if len(checkEvents) > 0 {
		presence = float64(len(checkEvents)-len(evMissing)) / float64(len(checkEvents))
	}
	archiveScore := 0.0
	if len(referenced) > 0 {
		archiveScore = float64(len(archivedCount)) / float64(len(referenced))
	}
	evScore := 0.7*presence + 0.3*archiveScore
	if len(checkEvents) == 0 {
		evScore = 0
	}
	// A manifest that names required evidence the ledger never received makes
	// the evidence dimension incomplete: downstream may only see quality
	// warnings and pending-evidence items, never a confirmed status.
	if len(manifestMissing) > 0 {
		manifestPenalty := float64(len(manifestMissing)) /
			float64(len(manifestMissing)+len(referenced)+1)
		evScore *= (1 - manifestPenalty)
	}
	if len(evMissing) > 0 {
		res.PendingItems = append(res.PendingItems, PendingItem{
			Code: "evidence_index_missing", Severity: SevError,
			Detail: fmt.Sprintf("%d 条安检/复核事件缺少声明的证据索引: %v", len(evMissing), evMissing),
		})
	}
	if len(referenced) > len(archivedCount) {
		missing := make([]string, 0)
		for _, idx := range referenced {
			if !archived[idx] {
				missing = append(missing, idx)
			}
		}
		sort.Strings(missing)
		res.PendingItems = append(res.PendingItems, PendingItem{
			Code: "evidence_not_archived", Severity: SevError,
			Detail: fmt.Sprintf("证据索引未被图像索引库归档确认: %v", missing),
		})
	}
	res.Components = append(res.Components, Component{
		Key: "evidence_index", Label: "证据索引完整度", Weight: w.EvidenceIndex, Score: round3(evScore),
		Contribution: round3(w.EvidenceIndex * evScore),
		Detail: fmt.Sprintf("%d/%d 条结论事件携带证据索引，%d/%d 个索引已归档",
			len(checkEvents)-len(evMissing), len(checkEvents),
			len(archivedCount), len(referenced)),
		Missing: evMissing, SourceEvents: idsOf(checkEvents),
	})

	// Review coverage.
	reviewRequired, reviewSatisfied := 0, 0
	for _, ps := range res.Packages {
		if ps.Result == "" || ps.Result == VerdictClear || ps.HumanRuled {
			continue
		}
		reviewRequired++
	}
	rvScore := 1.0
	if reviewRequired > 0 {
		rvScore = float64(reviewSatisfied) / float64(reviewRequired)
	}
	res.Components = append(res.Components, Component{
		Key: "review", Label: "人工复核", Weight: w.Review, Score: round3(rvScore),
		Contribution: round3(w.Review * rvScore),
		Detail:       fmt.Sprintf("%d/%d 条非放行结论已有人工复核", reviewSatisfied, reviewRequired),
		SourceEvents: SortedUnique(reviewIDs),
	})

	// Identity consistency.
	identityTotal, identityOK := 0, 0
	var ineligible []string
	for _, ev := range in.Events {
		switch ev.MessageType {
		case TypeHandover, TypePackageChange, TypeDeclaration:
			continue
		}
		identityTotal++
		if d, found := in.Dispositions[ev.EventID]; found && !d.EvidenceEligible() {
			ineligible = append(ineligible, ev.EventID)
			continue
		}
		identityOK++
	}
	idScore := 1.0
	if identityTotal > 0 {
		idScore = float64(identityOK) / float64(identityTotal)
	}
	res.Components = append(res.Components, Component{
		Key: "identity", Label: "身份一致性", Weight: w.Identity, Score: round3(idScore),
		Contribution: round3(w.Identity * idScore),
		Detail:       fmt.Sprintf("%d/%d 条事件引用在效包装件版本", identityOK, identityTotal),
		Missing:      ineligible, SourceEvents: ineligible,
	})

	total := 0.0
	for _, c := range res.Components {
		total += c.Contribution
	}
	res.EvidenceCompleteness = round3(total)

	if len(in.ManifestEvidence) > 0 && !handoverDeparture {
		res.PendingItems = append(res.PendingItems, PendingItem{
			Code: "departure_handover_missing", Severity: SevWarning,
			Detail: "已收到舱单，但没有放行/装机交接关闭该航段",
		})
	}

	// ---- blocking rules -----------------------------------------------
	blocking := map[string]bool{}
	for _, v := range in.Violations {
		if !v.Blocking() || blocking[v.Code] {
			continue
		}
		blocking[v.Code] = true
		res.BlockingRuleCodes = append(res.BlockingRuleCodes, v.Code)
		expl := fmt.Sprint(v.Detail["explanation"])
		res.BlockingReasons = append(res.BlockingReasons, v.Code+": "+expl)
		if v.Code == RuleCrossPackageReference {
			res.PendingItems = append(res.PendingItems, PendingItem{
				Code: "cross_package_evidence", Severity: SevError,
				Detail: "证据与被拆并后的在效包装件失配，不会自动转移给任何子件；需重新扫描/复核",
			})
		}
	}
	sort.Strings(res.BlockingRuleCodes)

	// ---- decision -----------------------------------------------------
	if len(results) == 1 {
		for r := range results {
			res.SecurityResult = r
		}
	}
	hasBlocking := len(res.BlockingRuleCodes) > 0
	// A blocker is unfixable-by-supplement (e.g. evidence misaligned with a
	// retired package, an unruled conflict). An error-severity pending item is
	// fixable by supplementing evidence (missing archive, missing index) and
	// therefore yields pending_evidence, not not_publishable.
	blockerPending := false
	for _, p := range res.PendingItems {
		if p.Severity == SevBlocker {
			blockerPending = true
		}
	}
	switch {
	case len(res.Conflicts) > 0:
		res.Status = StatusConflictPending
		res.Decision = "存在相互矛盾的设备结论且无交接证据分隔：需人工裁定，平台不会代选结果"
	case len(res.Packages) == 0 && len(shipmentLevel) == 0:
		res.Status = StatusNotPublishable
		res.Decision = "没有任何可归属到在效包装件的安检证据"
	case withResult == 0 && len(shipmentLevel) == 0:
		res.Status = StatusNotPublishable
		res.Decision = "没有任何可用的安检结论，无法生成下游可消费的确认状态"
	case hasBlocking || blockerPending:
		res.Status = StatusNotPublishable
		res.Decision = "阻断性质量问题禁止签发确认状态：" + strings.Join(res.BlockingReasons, "; ")
	case res.EvidenceCompleteness < in.CompletenessFloor || hasMaterialPending(res.PendingItems):
		res.Status = StatusPendingEvidence
		codes := make([]string, 0, len(res.PendingItems))
		for _, p := range res.PendingItems {
			if p.Severity != SevInfo {
				codes = append(codes, p.Code)
			}
		}
		res.Decision = fmt.Sprintf("证据完整度 %.3f（阈值 %.3f），仍有待补证项：%s",
			res.EvidenceCompleteness, in.CompletenessFloor, strings.Join(SortedUnique(codes), ","))
	case len(results) > 1:
		res.Status = StatusConflictPending
		res.Decision = "不同包装件结论不一致且无人工裁定统一"
	default:
		res.Status = StatusVerified
		res.Decision = fmt.Sprintf("%d 个包装件结论一致，证据完整度 %.3f，无阻断项",
			len(res.Packages), res.EvidenceCompleteness)
	}

	res.InputEventIDs = idsOf(in.Events)
	res.InputRuleKeys = ruleKeys(in.Violations)
	return res
}

func sideOf(ev *Event) ConflictSide {
	return ConflictSide{
		EventID: ev.EventID, Source: ev.Source, Result: ev.Result(),
		OccurredAt: ev.OccurredAt, ReceivedAt: ev.ReceivedAt,
	}
}

// hasMaterialPending reports whether any pending item is more than an
// informational notice. Info items (such as "prior verdict not carried over")
// describe retained history and must not block a verified status.
func hasMaterialPending(items []PendingItem) bool {
	for _, p := range items {
		if p.Severity != SevInfo {
			return true
		}
	}
	return false
}

func boolDetail(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// handoverBetween reports whether a screening handover for the package or
// shipment arrived strictly between two verdict receipts. Only such a handover
// legally separates "re-screened after re-handover" from "two machines
// disagree".
func handoverBetween(in FusionInput, pkg string, a, b *Event) bool {
	lo, hi := a.ReceivedAt, b.ReceivedAt
	if hi.Before(lo) {
		lo, hi = hi, lo
	}
	for _, ev := range in.Events {
		if ev.MessageType != TypeHandover || ev.Mode() != ModeScreening {
			continue
		}
		if ev.PackageKey != "" && ev.PackageKey != pkg && ev.PackageKey != in.ShipmentKey {
			continue
		}
		if ev.ReceivedAt.After(lo) && !ev.ReceivedAt.After(hi) {
			return true
		}
	}
	return false
}

func idsOf(evs []*Event) []string {
	out := make([]string, 0, len(evs))
	seen := map[string]bool{}
	for _, e := range evs {
		if !seen[e.EventID] {
			seen[e.EventID] = true
			out = append(out, e.EventID)
		}
	}
	sort.Strings(out)
	return out
}

func idsOfType(evs []*Event, mt string) []string {
	var out []string
	for _, e := range evs {
		if e.MessageType == mt {
			out = append(out, e.EventID)
		}
	}
	sort.Strings(out)
	return out
}

func ruleKeys(vs []RuleViolation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Code+"|"+v.DedupeKey)
	}
	sort.Strings(out)
	return out
}
