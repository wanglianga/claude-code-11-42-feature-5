package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"cargo.example/secstate/internal/domain/review"
	"cargo.example/secstate/internal/store"
)

// ReviewScopeRequest registers a review scope.
type ReviewScopeRequest struct {
	review.ScopeRequest
	ScopeID string `json:"scope_id,omitempty"`
}

// RegisterReviewScope validates and stores a scope registry entry.
func (s *Service) RegisterReviewScope(ctx context.Context, req ReviewScopeRequest) (review.ScopeRegistry, error) {
	sc, err := review.ScopeRegistration(req.ScopeRequest)
	if err != nil {
		return review.ScopeRegistry{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if sc.ScopeID == "" {
		sc.ScopeID = "scope-" + uuid.NewString()[:8]
	}
	return s.st.SaveReviewScope(ctx, sc)
}

// ListReviewScopes lists registered scopes.
func (s *Service) ListReviewScopes(ctx context.Context) ([]review.ScopeRegistry, error) {
	return s.st.ListReviewScopes(ctx)
}

// GetReviewScope loads one scope.
func (s *Service) GetReviewScope(ctx context.Context, id string) (review.ScopeRegistry, error) {
	return s.st.GetReviewScope(ctx, id)
}

// ReviewImportRequest imports a bundle of logs into a scope.
type ReviewImportRequest struct {
	ScopeID string `json:"scope_id"`
	review.LogImport
	BatchKey string `json:"batch_key,omitempty"`
}

// ReviewImportResult reports what an import did.
type ReviewImportResult struct {
	ScopeID      string                 `json:"scope_id"`
	Imported     int                    `json:"imported_count"`
	Excluded     []review.ScopeDecision `json:"excluded,omitempty"`
	LateArrivals []review.ScopeDecision `json:"late_arrivals,omitempty"`
	BatchKey     string                 `json:"batch_key"`
	Warnings     []string               `json:"warnings,omitempty"`
}

// ImportReviewLogs applies the scope to a bundle and appends the records.
//
// A record received after the scope's cutoff is not silently folded into the
// old dossier: it is reported as a late arrival and must be imported into a new
// scope opened against the later cutoff. The same applies to a publication log
// stamped after the cutoff.
func (s *Service) ImportReviewLogs(ctx context.Context, req ReviewImportRequest) (ReviewImportResult, error) {
	sc, err := s.st.GetReviewScope(ctx, req.ScopeID)
	if err != nil {
		return ReviewImportResult{}, err
	}
	res, err := review.ApplyScope(sc, req.LogImport)
	if err != nil {
		return ReviewImportResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	batch := strings.TrimSpace(req.BatchKey)
	if batch == "" {
		batch = "batch-" + uuid.NewString()[:8]
	}
	out := ReviewImportResult{
		ScopeID: sc.ScopeID, Excluded: res.Excluded, LateArrivals: res.LateArrivals,
		BatchKey: batch, Warnings: res.Warnings,
	}
	saved, err := s.st.SaveReviewRecords(ctx, sc.ScopeID, batch, res.Imported, res)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return out, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return out, err
	}
	out.Imported = saved
	// Report drafts that failed construction as excluded with a reason.
	for _, d := range res.Excluded {
		if strings.HasPrefix(d.Reason, "event ") || strings.HasPrefix(d.Reason, "publication ") ||
			strings.HasPrefix(d.Reason, "read ") || strings.HasPrefix(d.Reason, "facts ") {
			out.Warnings = append(out.Warnings, d.RecordID+": "+d.Reason)
		}
	}
	return out, nil
}

// CreateReviewCaseRequest creates a case in a scope.
type CreateReviewCaseRequest struct {
	CaseID  string `json:"case_id,omitempty"`
	ScopeID string `json:"scope_id"`
	Title   string `json:"title,omitempty"`
}

// CreateReviewCase registers a case.
func (s *Service) CreateReviewCase(ctx context.Context, req CreateReviewCaseRequest) (store.CaseRecord, error) {
	if strings.TrimSpace(req.ScopeID) == "" {
		return store.CaseRecord{}, fmt.Errorf("%w: scope_id is required", ErrInvalid)
	}
	if _, err := s.st.GetReviewScope(ctx, req.ScopeID); err != nil {
		return store.CaseRecord{}, err
	}
	c := store.CaseRecord{CaseID: req.CaseID, ScopeID: req.ScopeID, Title: req.Title}
	if c.CaseID == "" {
		c.CaseID = "case-" + uuid.NewString()[:8]
	}
	return s.st.SaveReviewCase(ctx, c)
}

// AnalyzeCaseRequest runs a targeted review for a case.
type AnalyzeCaseRequest struct {
	CaseID        string        `json:"case_id"`
	Target        review.Target `json:"target"`
	Budget        int           `json:"budget,omitempty"`
	CrossCheck    bool          `json:"cross_check,omitempty"`
	CrossCheckCap int           `json:"cross_check_cap,omitempty"`
	AllowedFields []string      `json:"allowed_fields,omitempty"`
}

// AnalyzeCaseResult is the response of a run.
type AnalyzeCaseResult struct {
	Run        store.RunRecord             `json:"run"`
	CrossCheck []review.CrossCheck         `json:"cross_check,omitempty"`
	Restricted []review.RestrictedMaterial `json:"restricted_material,omitempty"`
}

// AnalyzeCase builds the world from the case's imported material at the target's
// cutoff, runs the targeted solver, and pins the result together with the exact
// record set and hash it read.
func (s *Service) AnalyzeCase(ctx context.Context, req AnalyzeCaseRequest) (AnalyzeCaseResult, error) {
	if strings.TrimSpace(req.CaseID) == "" {
		return AnalyzeCaseResult{}, fmt.Errorf("%w: case_id is required", ErrInvalid)
	}
	cse, err := s.st.GetReviewCase(ctx, req.CaseID)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	sc, err := s.st.GetReviewScope(ctx, cse.ScopeID)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	recs, excluded, err := s.st.LoadReviewRecords(ctx, sc.ScopeID)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	target := req.Target
	target.CaseID = cse.CaseID
	if target.ShipmentKey == "" {
		target.ShipmentKey = sc.ShipmentKey
	}
	if target.ShipmentKey != sc.ShipmentKey {
		return AnalyzeCaseResult{}, fmt.Errorf(
			"%w: target shipment %s is outside scope %s (registered for %s)",
			ErrInvalid, target.ShipmentKey, sc.ScopeID, sc.ShipmentKey)
	}
	if len(target.Levels) == 0 {
		target.Levels = []string{"shipment"}
	}
	// The scope's registered authorised subjects are the only extra cargo a
	// review may read beyond its target shipment. The service never widens them.
	if len(target.AuthorizedSubjects) == 0 {
		target.AuthorizedSubjects = sc.AuthorizedSubjects
	} else {
		for _, as := range target.AuthorizedSubjects {
			if !scopeAuthorises(sc, as) {
				return AnalyzeCaseResult{}, fmt.Errorf(
					"%w: subject %s %s is not authorised by scope %s",
					ErrInvalid, as.Type, as.Key, sc.ScopeID)
			}
		}
	}
	if target.Cutoff.IsZero() {
		target.Cutoff = sc.Cutoff
	}
	if target.Cutoff.After(sc.Cutoff) {
		return AnalyzeCaseResult{}, fmt.Errorf(
			"%w: target cutoff %s is after the scope cutoff %s; open a new scope for later material",
			ErrInvalid, target.Cutoff.Format(time.RFC3339), sc.Cutoff.Format(time.RFC3339))
	}
	// The review reads only records the scope holds at the target cutoff. A
	// record that arrived after the cutoff cannot influence this case even if
	// the same scope holds it — the cutoff is re-applied at analysis time.
	inScope := make([]*review.Record, 0, len(recs))
	var cutoffExcluded []review.ScopeDecision
	for _, r := range recs {
		if receivedAfter(r, target.Cutoff) {
			cutoffExcluded = append(cutoffExcluded, review.ScopeDecision{
				RecordID: r.RecordID,
				Reason: fmt.Sprintf("received or published after the review cutoff %s; a new review must be opened",
					target.Cutoff.UTC().Format(time.RFC3339)),
			})
			continue
		}
		inScope = append(inScope, r)
	}
	world := review.NewWorld(target, inScope, append(excluded, cutoffExcluded...))
	an, err := review.Analyze(world, review.AnalyzeOptions{
		BudgetLimit: req.Budget, Scope: sc, Now: s.nowFn().UTC(),
		EvaluateCombination: true,
	})
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	run := store.RunRecord{
		RunID: uuid.NewString(), CaseID: cse.CaseID, ScopeID: sc.ScopeID,
		Cutoff: target.Cutoff, Target: target,
		SummaryVerdict: an.Summary.Verdict, BudgetExhausted: an.Summary.BudgetExhausted,
		MaterialIDs: an.Material.RecordIDs, MaterialHash: an.Material.Hash,
		Analysis: an,
	}
	out := AnalyzeCaseResult{}
	if req.CrossCheck {
		out.CrossCheck = review.VerifyAgainstExhaustive(world, an.Assertions, req.CrossCheckCap)
		run.CrossCheck = out.CrossCheck
	}
	saved, err := s.st.SaveReviewRun(ctx, run)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	out.Run = saved
	// Restricted material: anything the target levels do not authorise is
	// exported as a boundary record, never as detail and never deleted.
	out.Restricted = review.BoundaryRecords(inScope, sc, target.Levels)
	return out, nil
}

// scopeAuthorises reports whether the scope registry lists a subject.
func scopeAuthorises(sc review.ScopeRegistry, sub review.SubjectRef) bool {
	for _, a := range sc.AuthorizedSubjects {
		if a.Type == sub.Type && a.Key == sub.Key {
			return true
		}
	}
	return false
}

func receivedAfter(r *review.Record, cutoff time.Time) bool {
	if r.Received != nil && r.Received.Earliest.After(cutoff) {
		return true
	}
	if r.Kind == review.KindPublication && r.PublishedAt.After(cutoff) {
		return true
	}
	return false
}

// GetReviewRun loads a stored run.
func (s *Service) GetReviewRun(ctx context.Context, runID string) (store.RunRecord, error) {
	return s.st.GetReviewRun(ctx, runID)
}

// ListReviewRuns lists a case's runs.
func (s *Service) ListReviewRuns(ctx context.Context, caseID string) ([]store.RunRecord, error) {
	return s.st.ListReviewRuns(ctx, caseID)
}

// GetReviewExamples returns the stored order examples of a run.
func (s *Service) GetReviewExamples(ctx context.Context, runID string) ([]review.OrderExample, error) {
	if _, err := s.st.GetReviewRun(ctx, runID); err != nil {
		return nil, err
	}
	return s.st.ListReviewExamples(ctx, runID)
}

// ReproduceRun re-runs a stored run against the material it pinned and confirms
// the verdict still follows from exactly that material. It fails loudly if a
// pinned record is missing — an old review cannot be silently reinterpreted
// from a newer dossier.
func (s *Service) ReproduceRun(ctx context.Context, runID string, budget int) (AnalyzeCaseResult, error) {
	old, err := s.st.GetReviewRun(ctx, runID)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	sc, err := s.st.GetReviewScope(ctx, old.ScopeID)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	recs, excluded, err := s.st.LoadReviewRecords(ctx, old.ScopeID)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	byID := map[string]*review.Record{}
	for _, r := range recs {
		byID[r.RecordID] = r
	}
	pinned := make([]*review.Record, 0, len(old.MaterialIDs))
	for _, id := range old.MaterialIDs {
		r, ok := byID[id]
		if !ok {
			return AnalyzeCaseResult{}, fmt.Errorf(
				"%w: pinned record %s is missing; the old review cannot be reproduced from the current material",
				store.ErrNotFound, id)
		}
		pinned = append(pinned, r)
	}
	world := review.NewWorld(old.Target, pinned, excluded)
	if err := review.ReproCheck(old.Analysis, pinned); err != nil {
		return AnalyzeCaseResult{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	an, err := review.Analyze(world, review.AnalyzeOptions{
		BudgetLimit: budget, Scope: sc, Now: s.nowFn().UTC(), EvaluateCombination: true,
	})
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	run := store.RunRecord{
		RunID: uuid.NewString(), CaseID: old.CaseID, ScopeID: old.ScopeID,
		Cutoff: old.Cutoff, Target: old.Target, SummaryVerdict: an.Summary.Verdict,
		BudgetExhausted: an.Summary.BudgetExhausted,
		MaterialIDs:     an.Material.RecordIDs, MaterialHash: an.Material.Hash, Analysis: an,
	}
	saved, err := s.st.SaveReviewRun(ctx, run)
	if err != nil {
		return AnalyzeCaseResult{}, err
	}
	out := AnalyzeCaseResult{Run: saved}
	out.Restricted = review.BoundaryRecords(pinned, sc, old.Target.Levels)
	return out, nil
}

// ExportReviewRequest produces a restricted material export.
type ExportReviewRequest struct {
	RunID         string   `json:"run_id"`
	AllowedFields []string `json:"allowed_fields,omitempty"`
}

// ExportReview applies field projection and boundary restriction to a stored
// run without changing its verdict. The internal result stays in the run; only
// the export view is narrowed.
func (s *Service) ExportReview(ctx context.Context, req ExportReviewRequest) (review.ExportView, error) {
	run, err := s.st.GetReviewRun(ctx, req.RunID)
	if err != nil {
		return review.ExportView{}, err
	}
	sc, err := s.st.GetReviewScope(ctx, run.ScopeID)
	if err != nil {
		return review.ExportView{}, err
	}
	allowed := review.FieldsFromName(req.AllowedFields)
	verdict, projected := review.Project(run.Analysis.Summary.Verdict, run.Analysis.Assertions, allowed)
	// The projected verdict must equal the internally computed one; projection
	// only hides fields.
	integrity := "verdict_recomputed_from_same_material"
	if verdict != run.Analysis.Summary.Verdict {
		integrity = "projection_mismatch"
	}
	recs, _, err := s.st.LoadReviewRecords(ctx, run.ScopeID)
	if err != nil {
		return review.ExportView{}, err
	}
	byID := map[string]*review.Record{}
	for _, r := range recs {
		byID[r.RecordID] = r
	}
	var pinned []*review.Record
	for _, id := range run.MaterialIDs {
		if r, ok := byID[id]; ok {
			pinned = append(pinned, r)
		}
	}
	boundary := review.BoundaryRecords(pinned, sc, run.Target.Levels)
	var redactions []string
	for _, b := range boundary {
		redactions = append(redactions, fmt.Sprintf("%s: %s", b.RecordID, b.Hidden))
	}
	return review.ExportView{
		CaseID: run.CaseID, Scope: sc, AllowedFields: sortedFields(allowed),
		Verdict: verdict, Assertions: projected, BoundaryRecords: boundary,
		Redactions: redactions, Integrity: integrity,
		Note: "field projection and boundary restriction hide fields and cargo details only; " +
			"the verdict is the same one computed from the full reviewed material",
	}, nil
}

func sortedFields(allowed map[string]bool) []string {
	out := make([]string, 0, len(allowed))
	for f := range allowed {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// MaterialSummary is a compact view of a case's imported material.
type MaterialSummary struct {
	ScopeID      string                 `json:"scope_id"`
	ShipmentKey  string                 `json:"shipment_key"`
	Cutoff       time.Time              `json:"cutoff_received_at"`
	RecordCount  int                    `json:"record_count"`
	ByKind       map[string]int         `json:"by_kind"`
	BySystem     map[string]int         `json:"by_system"`
	Hash         string                 `json:"material_hash"`
	LateArrivals []review.ScopeDecision `json:"late_arrivals,omitempty"`
}

// ReviewMaterial summarises what a scope currently holds.
func (s *Service) ReviewMaterial(ctx context.Context, scopeID string) (MaterialSummary, error) {
	sc, err := s.st.GetReviewScope(ctx, scopeID)
	if err != nil {
		return MaterialSummary{}, err
	}
	recs, _, err := s.st.LoadReviewRecords(ctx, scopeID)
	if err != nil {
		return MaterialSummary{}, err
	}
	sum := MaterialSummary{ScopeID: scopeID, ShipmentKey: sc.ShipmentKey, Cutoff: sc.Cutoff,
		ByKind: map[string]int{}, BySystem: map[string]int{}, RecordCount: len(recs)}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		sum.ByKind[r.Kind]++
		sum.BySystem[r.System]++
		ids = append(ids, r.RecordID)
	}
	sort.Strings(ids)
	canon, _ := json.Marshal(struct {
		Records []string
		Count   int
	}{ids, len(ids)})
	h := sha256.Sum256(canon)
	sum.Hash = hex.EncodeToString(h[:])
	return sum, nil
}

// ConstraintCheckRequest asks whether a case's base material is consistent at a
// cutoff, before any claim is posed.
type ConstraintCheckRequest struct {
	CaseID string    `json:"case_id"`
	Cutoff time.Time `json:"cutoff_received_at,omitempty"`
	Budget int       `json:"budget,omitempty"`
}

// ConstraintCheckResult reports base-material feasibility and the conflict, if
// any, that already exists before any claim is stated.
type ConstraintCheckResult struct {
	CaseID          string                  `json:"case_id"`
	Cutoff          time.Time               `json:"cutoff_received_at"`
	Feasible        bool                    `json:"feasible"`
	BudgetExhausted bool                    `json:"budget_exhausted"`
	ChecksUsed      int                     `json:"checks_used"`
	ConstraintCount int                     `json:"constraint_count"`
	Conflict        *review.ConflictSet     `json:"conflict,omitempty"`
	Unresolved      []string                `json:"unresolved,omitempty"`
	Material        review.MaterialManifest `json:"material"`
	Note            string                  `json:"note"`
}

// CheckReviewConstraints runs only the shared constraint system (windows,
// receipt semantics, per-source sequence order, publication prerequisites) and
// reports whether it can be satisfied at all. It is the "constraint check"
// endpoint: a fast, claim-free sanity pass an operator can run before deciding
// which questions to ask.
func (s *Service) CheckReviewConstraints(ctx context.Context, req ConstraintCheckRequest) (ConstraintCheckResult, error) {
	if strings.TrimSpace(req.CaseID) == "" {
		return ConstraintCheckResult{}, fmt.Errorf("%w: case_id is required", ErrInvalid)
	}
	cse, err := s.st.GetReviewCase(ctx, req.CaseID)
	if err != nil {
		return ConstraintCheckResult{}, err
	}
	sc, err := s.st.GetReviewScope(ctx, cse.ScopeID)
	if err != nil {
		return ConstraintCheckResult{}, err
	}
	cutoff := req.Cutoff
	if cutoff.IsZero() {
		cutoff = sc.Cutoff
	}
	if cutoff.After(sc.Cutoff) {
		return ConstraintCheckResult{}, fmt.Errorf("%w: cutoff %s is after the scope cutoff %s",
			ErrInvalid, cutoff.Format(time.RFC3339), sc.Cutoff.Format(time.RFC3339))
	}
	recs, excluded, err := s.st.LoadReviewRecords(ctx, sc.ScopeID)
	if err != nil {
		return ConstraintCheckResult{}, err
	}
	inScope := make([]*review.Record, 0, len(recs))
	for _, r := range recs {
		if receivedAfter(r, cutoff) {
			continue
		}
		inScope = append(inScope, r)
	}
	target := review.Target{CaseID: cse.CaseID, ShipmentKey: sc.ShipmentKey,
		Levels: []string{"shipment"}, Cutoff: cutoff}
	world := review.NewWorld(target, inScope, excluded)
	cs := world.BaseConstraints(req.Budget)
	sol := cs.Solve(nil)
	out := ConstraintCheckResult{
		CaseID: cse.CaseID, Cutoff: cutoff, ChecksUsed: cs.Budget().Used,
		ConstraintCount: len(cs.Constraints()), BudgetExhausted: cs.Budget().Exhausted,
		Material: review.MaterialManifest{},
	}
	out.Feasible = sol.Feasible
	out.Conflict = sol.Conflict
	out.Unresolved = world.Unresolved()
	ids := make([]string, 0, len(inScope))
	for _, r := range inScope {
		ids = append(ids, r.RecordID)
	}
	sort.Strings(ids)
	out.Material.RecordIDs = ids
	out.Material.Count = len(ids)
	if sol.Skipped {
		out.Note = "budget exhausted before the base material could be checked"
	} else if !sol.Feasible {
		out.Note = "the material's own constraints cannot hold simultaneously; the conflict set is sufficient but not claimed minimal"
	} else {
		out.Note = "the reviewed material is internally consistent; this says nothing about any particular claim"
	}
	return out, nil
}
