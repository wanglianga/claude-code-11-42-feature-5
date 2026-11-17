package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// AnalyzeOptions carries settings the caller controls.
type AnalyzeOptions struct {
	BudgetLimit int
	Scope       ScopeRegistry
	Now         time.Time
	// EvaluateCombination tests the claim set as one question: is there a
	// single instant and a single order in which every claim holds at once?
	EvaluateCombination bool
}

// Analyze runs the targeted review. It builds one difference-constraint system
// from the in-scope material and, for each claim, asks a small number of
// question-shaped queries instead of enumerating every order:
//
//	positive branch — is there a reading in which the instant is inside the
//	                  claim's interval and the effective version states the
//	                  claimed status?
//	negative branch — is there a reading in which the instant is inside the
//	                  interval and no effective version states it (either
//	                  nothing was published yet, or the effective version says
//	                  something else)?
//
// A claim is proven contradictory only when every positive shape is
// unsatisfiable while the material itself is satisfiable. It is "both explain"
// when a positive and a negative shape are each satisfiable — which is exactly
// what a source-clock error produces. Only the examples necessary to answer the
// questions are kept; no full permutation is exported.
func Analyze(w *World, opts AnalyzeOptions) (*Analysis, error) {
	cs := w.BaseConstraints(opts.BudgetLimit)
	an := &Analysis{
		CaseID: w.Target.CaseID, ShipmentKey: w.Target.ShipmentKey,
		Cutoff: w.Target.Cutoff, Levels: w.Target.Levels,
		TargetInstant: w.Target.Instant, Scope: opts.Scope,
		Unresolved: w.Unresolved(),
	}
	base := cs.Solve(nil)
	switch {
	case base.Skipped:
		an.Assertions = append(an.Assertions, AssertionResult{
			AssertionID: "base", Kind: "material", Verdict: VerdictUndetermined,
			Description: "search budget exhausted before the base material could be checked",
		})
		an.Summary = Summary{
			Verdict: VerdictUndetermined, BudgetExhausted: true,
			NotProven: "no conclusion: search budget exhausted",
			Findings:  []string{"budget exhausted before a single feasibility query completed"},
		}
		an.Unresolved = append(an.Unresolved, "search budget exhausted: no conclusion is claimed")
		return an, finalize(an, w, cs)
	case !base.Feasible:
		an.Assertions = append(an.Assertions, AssertionResult{
			AssertionID: "base", Kind: "material", Verdict: VerdictContradicted,
			Conflict:    base.Conflict,
			Description: "the reviewed material's own constraints cannot hold simultaneously",
		})
		an.Summary = Summary{
			Verdict: VerdictContradicted, Contradicted: 1,
			NotProven: "no contradiction found is not a proof of consistency",
			Findings:  []string{base.Conflict.Message},
		}
		return an, finalize(an, w, cs)
	}

	assertions := w.BuildAssertions()
	for _, a := range assertions {
		an.Assertions = append(an.Assertions, an.evaluate(w, cs, a))
	}
	if opts.EvaluateCombination {
		an.evaluateCombination(w, cs, assertions)
	}
	an.Summary = summarize(an.Assertions, cs)
	an.Examples = SortExamples(an.Examples)
	return an, finalize(an, w, cs)
}

func finalize(an *Analysis, w *World, cs *ConstraintSet) error {
	an.Constraints = cs.Constraints()
	an.Material = manifestOf(w.Records)
	an.Budget = cs.Budget()
	an.Excluded = w.Excluded
	return nil
}

func summarize(rs []AssertionResult, cs *ConstraintSet) Summary {
	s := Summary{NotProven: "no contradiction found is not a proof of consistency"}
	verdict := VerdictConsistent
	for _, r := range rs {
		switch r.Verdict {
		case VerdictContradicted:
			s.Contradicted++
		case VerdictBothExplain:
			s.BothExplain++
		case VerdictUndetermined:
			s.Undetermined++
		case VerdictConsistent:
			s.ConsistentChecks++
		}
		verdict = HigherVerdict(verdict, r.Verdict)
		if r.Conflict != nil && r.Verdict == VerdictContradicted {
			s.Findings = append(s.Findings, fmt.Sprintf("%s: %s", r.AssertionID, r.Conflict.Message))
		}
	}
	s.Verdict = verdict
	s.BudgetExhausted = cs.Budget().Exhausted
	if s.BudgetExhausted {
		s.Findings = append(s.Findings, fmt.Sprintf(
			"search budget exhausted after %d feasibility checks: unchecked assertions stay open",
			cs.Budget().Used))
	}
	return s
}

// evaluate dispatches to the right question shape for one assertion.
func (an *Analysis) evaluate(w *World, cs *ConstraintSet, a Assertion) AssertionResult {
	res := AssertionResult{
		AssertionID: a.ID, Kind: a.Kind, Origin: a.Origin, Description: a.Description,
		Coverage: a.coverage(w),
	}
	switch a.Kind {
	case AssertionEvidence:
		res.Verdict = an.evidenceVerdict(w, a)
	case AssertionRead:
		return an.readAssertion(w, cs, a)
	case AssertionSubjectExists, AssertionNoMembership:
		res.Verdict = an.presenceVerdict(w, a)
	case AssertionMembership:
		an.membershipAssertion(w, cs, a, &res)
	case AssertionStatus, AssertionConsumable, AssertionClaim:
		an.statusAssertion(w, cs, a, &res)
	case AssertionBoundary:
		res.Verdict = VerdictUndetermined
		res.Unresolved = append(res.Unresolved, a.BoundaryNote)
	default:
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"unknown assertion kind " + a.Kind}
	}
	return res
}

// ---- presence / evidence / read -------------------------------------------

func (an *Analysis) presenceVerdict(w *World, a Assertion) string {
	if a.Subject == nil {
		return VerdictConsistent
	}
	t, k := a.Subject.Type, a.Subject.Key
	switch t {
	case "shipment":
		if containsStr(w.VisibleShipments(), k) {
			return VerdictConsistent
		}
	case "package":
		if containsStr(w.ShipmentPackages(), k) {
			return VerdictConsistent
		}
	case "container":
		if containsStr(w.ContainersOfShipment(), k) || len(w.Versions("container", k)) > 0 {
			return VerdictConsistent
		}
	case "leg":
		if containsStr(w.LegsOfShipment(), k) || len(w.Versions("leg", k)) > 0 {
			return VerdictConsistent
		}
	}
	return VerdictUndetermined
}

func (an *Analysis) evidenceVerdict(w *World, a Assertion) string {
	var citing, archiving int
	for _, r := range w.Records {
		if !containsStr(r.Evidence, a.Evidence) {
			continue
		}
		if r.Classification == "archive" {
			archiving++
		} else {
			citing++
		}
	}
	if citing == 0 || archiving == 0 {
		return VerdictUndetermined
	}
	return VerdictConsistent
}

func (an *Analysis) readAssertion(w *World, cs *ConstraintSet, a Assertion) AssertionResult {
	res := AssertionResult{
		AssertionID: a.ID, Kind: a.Kind, Origin: a.Origin, Description: a.Description,
		Coverage: a.coverage(w),
	}
	r, ok := w.ByID[a.ReadRecord]
	if !ok {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"read record " + a.ReadRecord + " is not in the reviewed material"}
		return res
	}
	if r.ReferencedSnapshot == "" {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"read " + r.RecordID + " does not name the snapshot version it read"}
		return res
	}
	var pub *Record
	for _, c := range w.Records {
		if c.Kind == KindPublication && c.SnapshotID == r.ReferencedSnapshot {
			pub = c
			break
		}
	}
	if pub == nil {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{fmt.Sprintf(
			"read %s references snapshot %s, which is outside the reviewed material; "+
				"whether it existed at read time cannot be recomputed", r.RecordID, r.ReferencedSnapshot)}
		return res
	}
	// Force "publication precedes read" and see whether the material admits it.
	extra := []Constraint{{
		ID: PrefixRead + r.RecordID, Kind: causal,
		From: RecVar(pub.RecordID), To: RecVar(r.RecordID), MaxGapNs: 0,
		Reason: fmt.Sprintf("read %s references snapshot %s, so that version must have been published before the read",
			r.RecordID, r.ReferencedSnapshot),
		RecordIDs: []string{r.RecordID, pub.RecordID}, Guard: []string{w.GuardID()},
	}}
	got := cs.Solve(extra)
	res.AlternativeShapes = 1
	res.ShapesChecked = 1
	switch {
	case got.Skipped:
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"budget exhausted while checking the read reference"}
	case got.Feasible:
		res.Verdict = VerdictConsistent
		ex := an.storeExample(w, got, "compatible",
			fmt.Sprintf("read %s can be placed after snapshot %s", r.RecordID, r.ReferencedSnapshot), nil)
		res.Positive = refFor(ex)
		an.Examples = append(an.Examples, *ex)
	default:
		res.Verdict = VerdictContradicted
		res.Conflict = got.Conflict
	}
	return res
}

// ---- membership ------------------------------------------------------------

// membershipAssertion asks whether the claimed relation (subjects -> objects)
// can be the effective membership at the claim's instant, given every imported
// stow/membership record for the same subject side. A later re-stow must be
// strictly after the instant; an earlier stow may be before the effective one.
func (an *Analysis) membershipAssertion(w *World, cs *ConstraintSet, a Assertion, res *AssertionResult) {
	at, ok := statusInstant(a, w.Target)
	if !ok {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"membership claim has no queried instant"}
		return
	}
	m := a.Membership
	if m == nil || len(m.Subjects) == 0 || len(m.Objects) == 0 {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"membership claim does not name both sides of the relation"}
		return
	}
	subj, wantObj := m.Subjects[0], m.Objects[0]
	var rels []*Record
	for _, r := range w.Records {
		if r.Membership == nil || r.Membership.LinkKind != m.LinkKind {
			continue
		}
		if containsStr(r.Membership.Subjects, subj) {
			rels = append(rels, r)
		}
	}
	if len(rels) == 0 {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{fmt.Sprintf(
			"no %s relation for %s was imported, so membership at the instant cannot be recomputed",
			m.LinkKind, subj)}
		return
	}
	atv := addInstant(cs, "membership:"+a.ID, at)
	var pos, neg [][]Constraint
	for _, s := range rels {
		shape := effectiveAt(w, s, rels, atv, a.ID+"/"+s.RecordID)
		if containsStr(s.Membership.Objects, wantObj) {
			pos = append(pos, shape)
		} else {
			neg = append(neg, shape)
		}
	}
	neg = append(neg, noVisibleAt(w, rels[0], atv, a.ID))
	an.applyBranches(w, cs, res, pos, neg, at)
}

// ---- status ----------------------------------------------------------------

// statusAssertion evaluates a status claim. The claim's instant is a variable in
// the interval; the effective version is whatever version is published at that
// variable and has no later published version at it. Claims that share an
// interval share the variable, which is what makes "these two screenshots show
// states that cannot both be true at one instant" expressible.
func (an *Analysis) statusAssertion(w *World, cs *ConstraintSet, a Assertion, res *AssertionResult) {
	at, ok := statusInstant(a, w.Target)
	if !ok {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{
			"no queried instant: a status consistency question must name the instant it is asked about " +
				"(a screenshot time alone is a source-clock interval; bound it first)"}
		return
	}
	if a.Subject == nil || a.Subject.Empty() {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"status claim does not name its subject"}
		return
	}
	if a.RequiresImage {
		// Image-dependent content cannot be confirmed from metadata; the
		// absence of the image does not overturn existing time relations, but
		// it does stop the review from claiming this content verified.
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"claim depends on the original image, which the review did not read; " +
			"existing time relations are untouched, but this content is not claimed verified"}
		return
	}
	versions := w.Versions(a.Subject.Type, a.Subject.Key)
	if len(versions) == 0 {
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{fmt.Sprintf(
			"no publication log for %s %s is in scope; the material cannot show what was published at the instant",
			a.Subject.Type, a.Subject.Key)}
		return
	}
	atv := addInstant(cs, instantKey(at), at)
	var pos, neg [][]Constraint
	for _, v := range versions {
		shape := effectiveAt(w, v, versions, atv, a.ID+"/"+v.RecordID)
		if statusMatches(v, a) {
			pos = append(pos, shape)
		} else {
			neg = append(neg, shape)
		}
	}
	neg = append(neg, noVisibleAt(w, versions[0], atv, a.ID))
	an.applyBranches(w, cs, res, pos, neg, at)
}

// instantKey gives every claim with the same interval the same instant
// variable, so claims stated about "the same moment" really share a moment.
func instantKey(at TimeBound) string {
	return fmt.Sprintf("at@%d..%d", at.Earliest.UnixNano(), at.Latest.UnixNano())
}

func addInstant(cs *ConstraintSet, name string, at TimeBound) string {
	v := "instant:" + name
	Window(cs, v, at, nil,
		fmt.Sprintf("queried instant must lie in [%s, %s]",
			at.Earliest.UTC().Format(time.RFC3339Nano), at.Latest.UTC().Format(time.RFC3339Nano)),
		PrefixWindow+"instant:"+name)
	return v
}

// effectiveAt encodes "v is the version a query at the instant resolves to":
// v is published by the instant (v <= at) and every later version is still
// unpublished at the instant (at < later). Prerequisites stay in the base
// system, so a visible status always carries its causal inputs.
func effectiveAt(w *World, v *Record, versions []*Record, atv, id string) []Constraint {
	out := []Constraint{{
		ID: PrefixEffective + id + "#v", Kind: "effective",
		From: RecVar(v.RecordID), To: atv, MaxGapNs: 0,
		Reason:    fmt.Sprintf("version %s (v%d) must already be published at the queried instant", v.RecordID, v.Version),
		RecordIDs: []string{v.RecordID}, Guard: []string{w.GuardID()},
	}}
	found := false
	for _, o := range versions {
		if o.RecordID == v.RecordID {
			found = true
			continue
		}
		if !found {
			continue // versions before v are free to precede it
		}
		out = append(out, Constraint{
			ID: PrefixEffective + id + "#next." + o.RecordID, Kind: "effective",
			From: atv, To: RecVar(o.RecordID), MaxGapNs: -1,
			Reason:    fmt.Sprintf("version %s (v%d) must still be unpublished at the queried instant", o.RecordID, o.Version),
			RecordIDs: []string{o.RecordID}, Guard: []string{w.GuardID()},
		})
	}
	return out
}

// noVisibleAt encodes "the subject had no published version yet at the
// instant": the first version is published strictly after the instant.
func noVisibleAt(w *World, first *Record, atv, id string) []Constraint {
	return []Constraint{{
		ID: PrefixEffective + id + "#none", Kind: "effective",
		From: atv, To: RecVar(first.RecordID), MaxGapNs: -1,
		Reason:    fmt.Sprintf("no version of the subject was published at the queried instant; the earliest is %s", first.RecordID),
		RecordIDs: []string{first.RecordID}, Guard: []string{w.GuardID()},
	}}
}

// applyBranches solves the positive and negative shape sets and derives the
// verdict, keeping one reviewable example per surviving side.
func (an *Analysis) applyBranches(w *World, cs *ConstraintSet, res *AssertionResult, pos, neg [][]Constraint, at TimeBound) {
	posFeasible, posConflict, posExample, skipped := an.tryShapes(w, cs, pos, "compatible", &at)
	if skipped {
		res.Verdict = VerdictUndetermined
		res.Unresolved = append(res.Unresolved, "budget exhausted while checking the positive branch")
		return
	}
	negFeasible, negConflict, negExample, skipped := an.tryShapes(w, cs, neg, "incompatible", &at)
	if skipped {
		res.Verdict = VerdictUndetermined
		res.Unresolved = append(res.Unresolved, "budget exhausted while checking the negative branch")
		return
	}
	res.AlternativeShapes = len(pos) + len(neg)
	res.ShapesChecked = len(pos) + len(neg)
	switch {
	case posFeasible && negFeasible:
		res.Verdict = VerdictBothExplain
		res.Positive = refFor(posExample)
		res.Negative = refFor(negExample)
	case posFeasible && !negFeasible:
		res.Verdict = VerdictConsistent
		res.Positive = refFor(posExample)
	case !posFeasible && negFeasible:
		res.Verdict = VerdictContradicted
		res.Negative = refFor(negExample)
		res.Conflict = posConflict
		if res.Conflict == nil {
			res.Conflict = negConflict
		}
		if res.Conflict == nil {
			// No version can state the claim at this instant: the incompatibility
			// is between the claim and the versions that define the reading.
			// Cite those original records and the conditions, never a bare "no".
			res.Conflict = definitionalConflict(res, w, at)
		}
	default:
		res.Verdict = VerdictUndetermined
		res.Unresolved = append(res.Unresolved,
			"neither branch could be solved; the material may be incomplete for this question")
	}
	if posExample != nil {
		an.Examples = append(an.Examples, *posExample)
	}
	if negExample != nil && !sameReading(posExample, negExample) {
		an.Examples = append(an.Examples, *negExample)
	}
}

// tryShapes solves every shape in a branch. The branch is feasible as soon as
// one shape holds; every shape is attempted so the budget accounting is honest,
// but only the first feasible assignment is kept as an example.
func (an *Analysis) tryShapes(w *World, cs *ConstraintSet, shapes [][]Constraint, kind string, at *TimeBound) (bool, *ConflictSet, *OrderExample, bool) {
	if len(shapes) == 0 {
		return false, nil, nil, false
	}
	var example *OrderExample
	var conflict *ConflictSet
	for _, shape := range shapes {
		got := cs.Solve(shape)
		if got.Skipped {
			return false, conflict, nil, true
		}
		if got.Feasible {
			if example == nil {
				note := "a mutually consistent assignment in which the claim holds"
				if kind == "incompatible" {
					note = "a mutually consistent assignment in which the claim does not hold"
				}
				example = an.storeExample(w, got, kind, note, at)
			}
			continue
		}
		if conflict == nil {
			conflict = got.Conflict
		}
	}
	return example != nil, conflict, example, false
}

func sameReading(a, b *OrderExample) bool {
	if a == nil || b == nil {
		return false
	}
	if len(a.Times) != len(b.Times) {
		return false
	}
	for k, v := range a.Times {
		if b.Times[k] != v {
			return false
		}
	}
	return true
}

func refFor(e *OrderExample) *ExampleRef {
	if e == nil {
		return nil
	}
	return &ExampleRef{ExampleID: e.Kind + "|" + e.Note, Kind: e.Kind, Summary: e.Note}
}

// storeExample materialises a solver assignment into a reviewable example. It
// keeps the resolved occurrence instants, the resulting order, and the reading
// at the queried instant for the subjects the target levels authorise.
func (an *Analysis) storeExample(w *World, sr SolveResult, kind, note string, at *TimeBound) *OrderExample {
	times := map[string]int64{}
	type pair struct {
		id string
		t  int64
	}
	var ps []pair
	for _, r := range w.Records {
		if t, ok := sr.Times[RecVar(r.RecordID)]; ok {
			times[r.RecordID] = t
			ps = append(ps, pair{r.RecordID, t})
		}
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].t != ps[j].t {
			return ps[i].t < ps[j].t
		}
		return ps[i].id < ps[j].id
	})
	seq := make([]string, 0, len(ps))
	for _, p := range ps {
		seq = append(seq, p.id)
	}
	readings := map[string]string{}
	if at != nil {
		readings = an.readingsAt(w, sr, *at)
	}
	return &OrderExample{Kind: kind, Times: times, Sequence: seq, Readings: readings, Note: note}
}

func (an *Analysis) readingsAt(w *World, sr SolveResult, at TimeBound) map[string]string {
	out := map[string]string{}
	for _, sub := range w.AllSubjects() {
		if !w.authorized(sub) {
			continue
		}
		best, bestVer := "", -1
		for _, v := range w.Versions(sub.Type, sub.Key) {
			t, ok := sr.Times[RecVar(v.RecordID)]
			if !ok || t > at.Latest.UnixNano() {
				continue
			}
			if v.Version > bestVer {
				best, bestVer = v.Status, v.Version
			}
		}
		if best != "" {
			out[sub.Type+":"+sub.Key] = best
		}
	}
	return out
}

// ---- combination -----------------------------------------------------------

// evaluateCombination tests the fixed claim set as one question: is there a
// single order in which every claim holds at once? Each claim contributes its
// positive version shapes, using its own instant variable; two claims stated
// about the same moment share that variable, so they cannot be read from two
// different versions of the same subject. When no cross product is satisfiable
// the claims are individually possible but jointly impossible, and each claim's
// own examples remain available.
func (an *Analysis) evaluateCombination(w *World, cs *ConstraintSet, assertions []Assertion) {
	var combos [][]Constraint
	var contributes []Assertion
	for _, a := range assertions {
		if a.Origin != "claim" {
			continue
		}
		if a.Kind != AssertionStatus && a.Kind != AssertionConsumable && a.Kind != AssertionClaim {
			continue
		}
		if a.RequiresImage {
			continue // image-dependent content cannot be joined into a metadata conclusion
		}
		at, ok := statusInstant(a, w.Target)
		if !ok || a.Subject == nil {
			continue
		}
		atv := addInstant(cs, instantKey(at), at)
		versions := w.Versions(a.Subject.Type, a.Subject.Key)
		var shapes [][]Constraint
		for _, v := range versions {
			if statusMatches(v, a) {
				shapes = append(shapes, effectiveAt(w, v, versions, atv, "combo/"+a.ID+"/"+v.RecordID))
			}
		}
		if len(shapes) == 0 {
			an.Assertions = append(an.Assertions, AssertionResult{
				AssertionID: "combination", Kind: "claim_set", Origin: "claim",
				Verdict: VerdictContradicted,
				Description: "at least one claim in the set has no published version stating it, " +
					"so the set cannot hold in any order",
			})
			return
		}
		combos = productAppend(combos, shapes)
		contributes = append(contributes, a)
	}
	if len(contributes) == 0 {
		an.Assertions = append(an.Assertions, AssertionResult{
			AssertionID: "combination", Kind: "claim_set", Origin: "claim",
			Verdict:     VerdictUndetermined,
			Description: "no status claims with a queried instant were supplied; nothing to combine",
		})
		return
	}
	var example *OrderExample
	var conflict *ConflictSet
	var conflictRecords []string
	skipped := false
	for _, combo := range combos {
		got := cs.Solve(combo)
		if got.Skipped {
			skipped = true
			break
		}
		if got.Feasible {
			if example == nil {
				example = an.storeExample(w, got, "combination",
					"one common order satisfies every claimed status simultaneously", nil)
			}
			continue
		}
		if conflict == nil {
			conflict = got.Conflict
			// The combination's incompatibility is between the version records
			// this cross product pinned. Cite every record the combo mentions so
			// the conflict names the whole set, not a single edge.
			for _, c := range combo {
				conflictRecords = append(conflictRecords, c.RecordIDs...)
			}
			for _, cv := range combo {
				conflictRecords = append(conflictRecords, cv.Guard...)
			}
		}
	}
	if conflict != nil {
		conflict.Records = uniqueSorted(append(conflict.Records, conflictRecords...))
		conflict.Message = "each claim names a version that could be effective on its own, but no single " +
			"order resolves the shared instant to all of them at once; the cited records are sufficient " +
			"to prove the combination impossible, though the set is not claimed minimal"
	}
	res := AssertionResult{AssertionID: "combination", Kind: "claim_set", Origin: "claim",
		AlternativeShapes: len(combos), ShapesChecked: len(combos)}
	switch {
	case example != nil:
		res.Verdict = VerdictConsistent
		res.Description = fmt.Sprintf("all %d claims hold in one common order", len(contributes))
		res.Positive = refFor(example)
		an.Examples = append(an.Examples, *example)
	case skipped:
		res.Verdict = VerdictUndetermined
		res.Unresolved = []string{"budget exhausted before the claim set could be checked"}
	case conflict != nil:
		res.Verdict = VerdictContradicted
		res.Conflict = conflict
		res.Description = "each claim is individually possible, but there is no single order " +
			"in which they all hold; separately feasible versions must not be stitched together"
	default:
		res.Verdict = VerdictUndetermined
	}
	an.Assertions = append(an.Assertions, res)
}

func productAppend(acc [][]Constraint, next [][]Constraint) [][]Constraint {
	if len(acc) == 0 {
		return next
	}
	var out [][]Constraint
	for _, a := range acc {
		for _, b := range next {
			merged := make([]Constraint, 0, len(a)+len(b))
			merged = append(merged, a...)
			merged = append(merged, b...)
			out = append(out, merged)
			if len(out) > 256 {
				return out
			}
		}
	}
	return out
}

// manifestOf pins the reviewed material by id and hash.
func manifestOf(records []*Record) MaterialManifest {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.RecordID)
	}
	sort.Strings(ids)
	canon, _ := json.Marshal(struct {
		Records []string
		Count   int
	}{ids, len(ids)})
	sum := sha256.Sum256(canon)
	return MaterialManifest{RecordIDs: ids, Count: len(ids), Hash: hex.EncodeToString(sum[:])}
}

// ReproCheck recomputes an analysis against the supplied material and confirms
// the pinned records are all present and the material hash matches, which is the
// "old review can be reproduced from the old material" guarantee.
func ReproCheck(an *Analysis, records []*Record) error {
	manifest := manifestOf(records)
	if manifest.Hash != an.Material.Hash {
		return fmt.Errorf("material hash %s does not match the pinned %s", manifest.Hash, an.Material.Hash)
	}
	byID := map[string]bool{}
	for _, r := range records {
		byID[r.RecordID] = true
	}
	for _, id := range an.Material.RecordIDs {
		if !byID[id] {
			return fmt.Errorf("pinned record %s is missing from the supplied material", id)
		}
	}
	return nil
}

// definitionalConflict builds a conflict set for the case where the claim never
// matches any version that could be effective at the instant. It cites the
// publication records whose effective shapes were posed and the condition each
// would require, so the reviewer can see exactly which original records make the
// claim impossible.
func definitionalConflict(res *AssertionResult, w *World, at TimeBound) *ConflictSet {
	if res == nil {
		return nil
	}
	var records []string
	var conds []string
	for _, cv := range res.Coverage {
		records = append(records, cv.RecordID)
		conds = append(conds, cv.Reason)
	}
	if len(records) == 0 {
		// Fall back to the subject's versions if coverage did not name any.
		return nil
	}
	sort.Strings(records)
	return &ConflictSet{
		Records: uniqueSorted(records), Conditions: conds,
		Message: "no published version that could be effective at the queried instant states the claimed " +
			"status (including any count/membership the claim pins); the cited records are sufficient " +
			"to show the incompatibility but the set is not claimed minimal",
	}
}
