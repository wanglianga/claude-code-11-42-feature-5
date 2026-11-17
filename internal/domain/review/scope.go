package review

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Scope policy. A review only ever reads material its registered scope allows:
// the included systems, the shipment's authorised relationship levels, and the
// cutoff. Records outside any of these are recorded as excluded with a reason,
// never silently dropped — a boundary that hides a constraint would let a
// conclusion look cleaner than the material supports.

// ImportResult is what one import produced before any case is analysed.
type ImportResult struct {
	Scope        ScopeRegistry   `json:"scope"`
	Imported     []*Record       `json:"imported"`
	Excluded     []ScopeDecision `json:"excluded"`
	LateArrivals []ScopeDecision `json:"late_arrivals"` // received after the scope cutoff
	Warnings     []string        `json:"warnings,omitempty"`
}

// ApplyScope converts drafts to records and filters them by the scope. The
// cutoff is an import-level fact: a record received after it cannot be reviewed
// in this scope at all. A record whose occurrence is before the cutoff but
// whose receipt is after it goes to late_arrivals and must open a new import.
func ApplyScope(scope ScopeRegistry, in LogImport) (*ImportResult, error) {
	cutoff := scope.Cutoff
	if cutoff.IsZero() {
		cutoff = time.Now().UTC()
	}
	res := &ImportResult{Scope: scope}
	incSystems := map[string]bool{}
	for _, s := range scope.IncludedSystems {
		incSystems[s] = true
	}
	excSystems := map[string]bool{}
	for _, s := range scope.ExcludedSystems {
		excSystems[s] = true
	}
	allow := func(system string) (bool, string) {
		if excSystems[system] {
			return false, "system excluded by scope: " + system
		}
		if len(incSystems) > 0 && !incSystems[system] && system != SystemPlatform {
			return false, "system not included in scope: " + system
		}
		return true, ""
	}
	add := func(r *Record, err error) {
		if err != nil {
			res.Excluded = append(res.Excluded, ScopeDecision{RecordID: draftName(r), Reason: err.Error()})
			return
		}
		// Late-arrival classification comes first: a record received after the
		// cutoff may enter a NEW review, but in this scope it is not reviewed at
		// all — including when its source is also outside the scope.
		if r.Received != nil && r.Received.Earliest.After(cutoff) {
			res.LateArrivals = append(res.LateArrivals, ScopeDecision{
				RecordID: r.RecordID,
				Reason: fmt.Sprintf("received at %s, after the scope cutoff %s; it may only enter a new review opened against the later material",
					r.Received.Earliest.UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339)),
			})
			return
		}
		if r.Kind == KindPublication && r.PublishedAt.After(cutoff) {
			res.LateArrivals = append(res.LateArrivals, ScopeDecision{
				RecordID: r.RecordID,
				Reason: fmt.Sprintf("published at %s, after the scope cutoff %s",
					r.PublishedAt.UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339)),
			})
			return
		}
		if ok, why := allow(r.System); !ok {
			res.Excluded = append(res.Excluded, ScopeDecision{RecordID: r.RecordID, Reason: why})
			return
		}
		// An exact occurrence after the cutoff with no separate receipt is also
		// not reviewable in this scope.
		if r.Kind != KindPublication && r.Occurred.Latest.After(cutoff) && r.Received == nil {
			res.LateArrivals = append(res.LateArrivals, ScopeDecision{
				RecordID: r.RecordID,
				Reason: fmt.Sprintf("occurred strictly after the scope cutoff %s",
					cutoff.UTC().Format(time.RFC3339)),
			})
			return
		}
		res.Imported = append(res.Imported, r)
	}
	for _, d := range in.Facts {
		add(NewRecordFromFacts(d))
	}
	for _, d := range in.Events {
		add(NewRecordFromEvent(d, cutoff))
	}
	for _, d := range in.Publications {
		add(NewRecordFromPublication(d))
	}
	for _, d := range in.Reads {
		r, err := NewRecordFromRead(d)
		if err == nil && r.ReferencedSnapshot != "" && r.Received != nil && r.Received.Earliest.After(cutoff) {
			// A read received after the cutoff cannot have been observed by the
			// review; the late-arrival rule below handles it.
		}
		add(r, err)
	}
	// Invalid drafts are reported, not fatal: one bad row must not silently
	// discard the rest of a dossier.
	sort.SliceStable(res.Imported, func(i, j int) bool { return res.Imported[i].RecordID < res.Imported[j].RecordID })
	sort.SliceStable(res.Excluded, func(i, j int) bool { return res.Excluded[i].RecordID < res.Excluded[j].RecordID })
	sort.SliceStable(res.LateArrivals, func(i, j int) bool { return res.LateArrivals[i].RecordID < res.LateArrivals[j].RecordID })
	return res, nil
}

// draftName returns a stable identifier even when a draft failed construction.
func draftName(r *Record) string {
	if r != nil {
		return r.RecordID
	}
	return "(invalid draft)"
}

// AuthorizeSubject reports whether a scope permits a subject to be displayed.
// The default (no explicit authorised subjects) follows the review's own
// relationship levels: the shipment plus whichever of its containers and legs
// the caller selected.
func (s ScopeRegistry) AuthorizeSubject(sub SubjectRef) bool {
	// An empty explicit list means the caller opted into the default of "the
	// shipment + whatever relationship levels this review studies", not "any
	// subject at all". A container or leg is only authorised when the registry
	// names it explicitly: a shipment authorisation never grants its ULDs.
	if len(s.AuthorizedSubjects) == 0 {
		return sub.Type == "shipment" && sub.Key == s.ShipmentKey
	}
	for _, a := range s.AuthorizedSubjects {
		if a.Type == sub.Type && a.Key == sub.Key {
			return true
		}
		if a.Type == "shipment" && sub.Type != "package" {
			// A shipment authorisation does not grant its container or leg rows,
			// which are separate cargo lines; it only grants the waybill itself.
			continue
		}
	}
	return false
}

// RestrictedMaterial is the boundary-only view used when a review touches
// cargo the caller is not authorised to see. It preserves the necessary
// constraint (the membership edge or the existence of a subject) while hiding
// the details, so a conclusion stays recomputable at the boundary.
type RestrictedMaterial struct {
	RecordID     string `json:"record_id"`
	System       string `json:"system"`
	Kind         string `json:"kind"`
	Boundary     string `json:"boundary"` // the necessary relation exposed
	Hidden       string `json:"hidden"`   // what was withheld
	Recomputable bool   `json:"independently_recomputable"`
}

// BoundaryRecords returns the boundary projection for records that touch
// unauthorised subjects. It never deletes the constraint: it replaces the
// detail with the minimal relation that still supports the conclusion. When
// even the relation cannot be disclosed the record is flagged as not
// independently recomputable rather than dropped.
func BoundaryRecords(records []*Record, scope ScopeRegistry, levels []string) []RestrictedMaterial {
	levelSet := map[string]bool{}
	for _, l := range levels {
		levelSet[l] = true
	}
	var out []RestrictedMaterial
	for _, r := range records {
		hidden := false
		switch {
		case r.Subject != nil:
			hidden = r.Subject.Type == "container" && !levelSet["container"] ||
				r.Subject.Type == "leg" && !levelSet["leg"]
		case r.Membership != nil:
			for _, o := range r.Membership.Objects {
				if r.Membership.LinkKind == "package_container" && !levelSet["container"] {
					hidden = true
				}
				if r.Membership.LinkKind == "container_leg" && !levelSet["leg"] {
					hidden = true
				}
				if r.Membership.LinkKind == "shipment_package" {
					// Package identifiers of a shipment the caller may see are
					// not a separate cargo line; the relation is disclosable.
					_ = o
				}
			}
		}
		if !hidden {
			continue
		}
		rec := RestrictedMaterial{
			RecordID: r.RecordID, System: r.System, Kind: r.Kind,
			Hidden: "subject details beyond the authorised relationship levels",
		}
		if r.Membership != nil {
			rec.Boundary = fmt.Sprintf("%s relation exists (endpoints withheld)", r.Membership.LinkKind)
			rec.Recomputable = true
		} else if r.Subject != nil {
			// The subject key is itself cargo detail; only print it when the
			// scope authorises that subject. Otherwise the boundary keeps the
			// type and withholds the identifier, and says the record is not
			// independently recomputable.
			if scope.AuthorizeSubject(*r.Subject) {
				rec.Boundary = fmt.Sprintf("%s %s exists; its published state is withheld", r.Subject.Type, r.Subject.Key)
				rec.Recomputable = true
			} else {
				rec.Boundary = fmt.Sprintf("%s exists; identifier and published state withheld", r.Subject.Type)
				rec.Recomputable = false
			}
		} else {
			rec.Boundary = "record touches an unauthorised subject"
			rec.Recomputable = false
		}
		// Withholding the subject key entirely makes the row impossible to
		// recompute; say so instead of pretending the boundary is independent.
		if r.Subject != nil && !containsStr(scope.IncludedSystems, r.System) && r.System != SystemPlatform {
			rec.Recomputable = false
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RecordID < out[j].RecordID })
	return out
}

// Project applies the allowed-field list to an assertion. Projection removes
// fields from the exported view but never changes a verdict: the verdict is
// recomputed from the same material before projection, so field removal cannot
// flip a conclusion.
func Project(verdict string, assertions []AssertionResult, allowed map[string]bool) (string, []AssertionResult) {
	out := make([]AssertionResult, 0, len(assertions))
	for _, a := range assertions {
		pa := AssertionResult{AssertionID: a.AssertionID, Kind: a.Kind, Verdict: a.Verdict}
		if allowed["detail"] {
			pa.Description = a.Description
		}
		if allowed["classification"] && len(a.Coverage) > 0 {
			pa.Coverage = a.Coverage
		}
		if allowed["status"] {
			pa.Positive = a.Positive
			pa.Negative = a.Negative
			pa.Conflict = a.Conflict
			pa.Unresolved = a.Unresolved
		}
		out = append(out, pa)
	}
	return verdict, out
}

// FieldsFromName normalises the requested field list.
func FieldsFromName(fields []string) map[string]bool {
	if len(fields) == 0 {
		fields = DefaultAllowedFields()
	}
	out := map[string]bool{}
	for _, f := range fields {
		out[strings.TrimSpace(strings.ToLower(f))] = true
	}
	// record_id, kind and verdict are structural: without them the export is
	// not a review at all.
	out["record_id"] = true
	out["kind"] = true
	out["verdict"] = true
	return out
}

// ScopeRegistration validates the registry entries a caller supplies.
func ScopeRegistration(req ScopeRequest) (ScopeRegistry, error) {
	if strings.TrimSpace(req.ShipmentKey) == "" {
		return ScopeRegistry{}, fmt.Errorf("scope shipment_key is required")
	}
	if len(req.IncludedSystems) == 0 {
		return ScopeRegistry{}, fmt.Errorf("scope included_systems must list at least one source system")
	}
	if req.Cutoff.IsZero() {
		return ScopeRegistry{}, fmt.Errorf("scope cutoff_received_at is required")
	}
	s := ScopeRegistry{
		ScopeID: req.ScopeID, Label: req.Label, ShipmentKey: req.ShipmentKey,
		IncludedSystems:    uniqueSorted(req.IncludedSystems),
		IncludedStreams:    uniqueSorted(req.IncludedStreams),
		AuthorizedSubjects: req.AuthorizedSubjects,
		ExcludedSystems:    uniqueSorted(req.ExcludedSystems),
		Cutoff:             req.Cutoff.UTC(), Note: req.Note,
	}
	seen := map[string]bool{}
	for _, inc := range s.IncludedSystems {
		if seen[inc] {
			return ScopeRegistry{}, fmt.Errorf("duplicate included system %q", inc)
		}
		seen[inc] = true
	}
	return s, nil
}
