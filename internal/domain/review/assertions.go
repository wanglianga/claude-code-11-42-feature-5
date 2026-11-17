package review

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BuildAssertions turns the fixed target into the set of questions the
// analysis must answer. Three sources:
//
//  1. Candidate claims — the statuses the query states for the target's
//     authorised subjects at the queried instant, plus any explicit claims.
//  2. Imported reads — a read's observed status is only a candidate; it is
//     accepted when a version effective at the read instant carries that status.
//  3. Object facts — the subjects and membership relations the claims are about
//     are asserted as positive facts with their own witnesses, so a claim is
//     never proven about an object that cannot exist in the material.
func (w *World) BuildAssertions() []Assertion {
	var out []Assertion
	instant := w.Target.Instant
	// Only claims supplied by the query or observed by an external consumer are
	// tested. Re-deriving a claim from the platform's own publication log and
	// then "confirming" it would be circular: the question is whether an
	// independently stated screenshot/read can have been true, not whether the
	// platform can restate what it published.
	for i, c := range w.Target.Claims {
		sub := SubjectRef{Type: c.Level, Key: c.Key}
		if sub.Empty() {
			continue
		}
		if !w.authorized(sub) {
			// The claim touches cargo the scope cannot disclose. Report it as
			// a boundary question so the reviewer knows it was not silently
			// dropped; do not fabricate a conclusion from hidden material.
			out = append(out, Assertion{
				ID: fmt.Sprintf("claim:%d:%s:%s", i, sub.Type, sub.Key), Kind: AssertionBoundary,
				Origin: "boundary", Subject: &sub, Status: c.Status,
				BoundaryNote: "claimed subject is outside the authorised relationship levels; " +
					"the claim cannot be recomputed from disclosable material",
				Description: fmt.Sprintf("claim about %s %s is boundary-only", sub.Type, sub.Key),
			})
			continue
		}
		at := c.At
		if at == nil || !at.Validate() {
			at = instant
		}
		// A claim with no instant is still listed and reported undetermined, so
		// the caller sees the question was received but cannot be decided.
		if at == nil || !at.Validate() {
			out = append(out, Assertion{
				ID: fmt.Sprintf("claim:%d:%s:%s", i, sub.Type, sub.Key), Kind: AssertionStatus,
				Origin: "claim", Subject: &sub, Status: c.Status,
				Description: fmt.Sprintf("requested claim: %s %s is %q, but no queried instant was supplied",
					sub.Type, sub.Key, c.Status),
			})
			continue
		}
		// A claim that states only a relationship (no status) is a membership
		// question: "was this the attribution in force at the instant?". That is
		// how a transload is reviewed without letting the current container
		// stand in for the historical one.
		if c.Membership != nil && strings.TrimSpace(c.Status) == "" {
			m := *c.Membership
			out = append(out, Assertion{
				ID: fmt.Sprintf("claim:%d:%s:%s", i, sub.Type, sub.Key), Kind: AssertionMembership,
				Origin: "claim", Membership: &m, At: at,
				Description: fmt.Sprintf("requested relationship: %s %v -> %v at %s",
					m.LinkKind, m.Subjects, m.Objects, at.Earliest.UTC().Format("2006-01-02T15:04:05Z")),
			})
			continue
		}
		a := Assertion{
			ID: fmt.Sprintf("claim:%d:%s:%s", i, sub.Type, sub.Key), Kind: AssertionStatus,
			Origin: "claim", Subject: &sub, Status: c.Status, At: at,
			Description: fmt.Sprintf("requested claim: %s %s is %q at %s", sub.Type, sub.Key,
				c.Status, at.Earliest.UTC().Format("2006-01-02T15:04:05Z")),
		}
		if c.Consumable != nil {
			a.Kind = AssertionClaim
			a.Observed = map[string]string{"consumable_confirmed": boolString(*c.Consumable)}
		}
		if len(c.Counts) > 0 {
			a.Counts = c.Counts
		}
		if c.Membership != nil {
			a.Membership = c.Membership
		}
		if c.RequiresImage {
			a.RequiresImage = true
		}
		out = w.dedupeClaims(out, a)
	}
	// Imported consumer reads: their observed status is a candidate reading.
	for _, r := range w.Records {
		if r.Kind != KindRead || !w.Target.IncludeReadLogs {
			continue
		}
		if len(w.Target.ReadRecordIDs) > 0 && !containsStr(w.Target.ReadRecordIDs, r.RecordID) {
			continue
		}
		if r.Subject != nil && !w.authorized(*r.Subject) {
			continue
		}
		out = append(out, Assertion{
			ID: "read:" + r.RecordID, Kind: AssertionRead, Origin: "candidate",
			ReadRecord: r.RecordID, At: timeBoundPtr(r.Occurred),
			Description: fmt.Sprintf("consumer read %s records the observed state it saw", r.RecordID),
		})
		if r.Subject != nil && r.Received != nil {
			if st := r.Observed["status"]; st != "" {
				at := TimeBound{Earliest: r.Received.Earliest, Latest: r.Received.Latest, Basis: "read_receipt"}
				a := Assertion{
					ID: "read-claim:" + r.RecordID, Kind: AssertionClaim, Origin: "candidate",
					Subject: r.Subject, Status: st, At: &at,
					Description: fmt.Sprintf("consumer read %s recorded %s %s as %q at read time",
						r.RecordID, r.Subject.Type, r.Subject.Key, st),
				}
				if cs := r.Observed["consumable_confirmed"]; cs != "" {
					a.Observed = map[string]string{"consumable_confirmed": cs}
				}
				out = w.dedupeClaims(out, a)
			}
		}
	}
	// Object facts: subjects and memberships the claims refer to.
	seen := map[string]bool{}
	for _, a := range out {
		if a.Subject != nil {
			id := "exist:" + a.Subject.Type + ":" + a.Subject.Key
			if !seen[id] {
				seen[id] = true
				sub := *a.Subject
				out = append(out, Assertion{
					ID: id, Kind: AssertionSubjectExists, Origin: "requirement", Subject: &sub,
					Description: fmt.Sprintf("%s %s must exist in the reviewed material", sub.Type, sub.Key),
				})
			}
		}
	}
	for _, r := range w.Records {
		if r.Membership == nil {
			continue
		}
		if !w.membershipTouchesScope(r) {
			continue
		}
		at := timeBoundPtr(r.Occurred)
		id := "membership:" + r.RecordID
		if seen[id] {
			continue
		}
		seen[id] = true
		m := *r.Membership
		out = append(out, Assertion{
			ID: id, Kind: AssertionMembership, Origin: "requirement", Membership: &m, At: at,
			Description: fmt.Sprintf("relationship %s %v -> %v must be recomputable at the claiming instant",
				m.LinkKind, m.Subjects, m.Objects),
		})
	}
	// Evidence prerequisites: whenever a publication or verdict in scope cites
	// an evidence index, that index must have been archived. The prerequisite
	// is surfaced whether or not a claim was stated, because a publication that
	// silently outruns its own evidence is exactly the kind of gap the review
	// exists to expose.
	evSeen := map[string]bool{}
	addEvidence := func(id string) {
		if id == "" || evSeen[id] {
			return
		}
		evSeen[id] = true
		out = append(out, Assertion{
			ID: "evidence:" + id, Kind: AssertionEvidence, Origin: "requirement",
			Evidence: id, Description: fmt.Sprintf("evidence index %s must be archived in scope", id),
		})
	}
	for _, a := range out {
		for _, id := range w.evidenceRequiredBy(a) {
			addEvidence(id)
		}
	}
	for _, r := range w.Records {
		if r.Kind != KindPublication {
			continue
		}
		for _, ref := range r.InputRefs {
			if in, ok := w.ByID[ref]; ok {
				for _, idx := range in.Evidence {
					addEvidence(idx)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// dedupeClaims replaces an earlier generated candidate about the same subject
// with an explicit claim, so a reviewer-supplied claim is not shadowed.
func (w *World) dedupeClaims(existing []Assertion, a Assertion) []Assertion {
	for i := range existing {
		e := &existing[i]
		if e.Kind != AssertionStatus && e.Kind != AssertionClaim {
			continue
		}
		if e.Subject == nil || a.Subject == nil || *e.Subject != *a.Subject || e.At != nil && a.At != nil && !e.At.Earliest.Equal(a.At.Earliest) {
			continue
		}
		// Two explicit claims about the same subject are allowed to coexist:
		// they are different assertions and may conflict with each other. Only a
		// generated candidate is replaced by an explicit claim.
		if e.Origin == "claim" {
			continue
		}
		existing[i] = a
		return existing
	}
	return append(existing, a)
}

// candidateSubjects returns the authorised subjects at the target's levels,
// preferring an explicit instant reading.
func (w *World) candidateSubjects() []SubjectRef {
	var out []SubjectRef
	for _, sub := range w.scopeSubjects() {
		out = append(out, sub)
	}
	return out
}

// candidateStatus is the status a query at the instant would read for a subject
// from the imported publication log alone — used only to pick which claim to
// test when the caller states one through an imported screen. It is never
// asserted as truth.
func (w *World) candidateStatus(sub SubjectRef, at TimeBound, versions []*Record) string {
	best := ""
	bestVer := -1
	for _, v := range versions {
		if v.PublishedAt.IsZero() {
			continue
		}
		if !v.PublishedAt.After(at.Latest) && v.Version > bestVer {
			best, bestVer = v.Status, v.Version
		}
	}
	return best
}

func (w *World) authorized(sub SubjectRef) bool {
	switch sub.Type {
	case "shipment":
		return w.levels["shipment"] && containsStr(w.VisibleShipments(), sub.Key)
	case "container":
		return w.levels["container"] && containsStr(w.ContainersOfShipment(), sub.Key)
	case "leg":
		return w.levels["leg"] && containsStr(w.LegsOfShipment(), sub.Key)
	case "package":
		for _, ship := range w.VisibleShipments() {
			if containsStr(w.graph.shipmentPkgs[ship], sub.Key) {
				return true
			}
		}
		return false
	}
	return false
}

func (w *World) membershipTouchesScope(r *Record) bool {
	if r.Membership == nil {
		return false
	}
	for _, s := range r.Membership.Subjects {
		for _, o := range r.Membership.Objects {
			switch r.Membership.LinkKind {
			case "shipment_package":
				if containsStr(w.VisibleShipments(), s) && w.levels["shipment"] {
					return true
				}
			case "package_container":
				if containsStr(w.ShipmentPackages(), s) && (w.levels["container"] || w.levels["leg"]) {
					return true
				}
				if containsStr(w.ContainersOfShipment(), o) && w.levels["container"] {
					return true
				}
			case "container_leg":
				if containsStr(w.ContainersOfShipment(), s) && w.levels["leg"] {
					return true
				}
			}
		}
	}
	return false
}

// evidenceRequiredBy returns index ids the assertion's records depend on, so
// evidence prerequisites of a published status are made explicit.
func (w *World) evidenceRequiredBy(a Assertion) []string {
	var out []string
	if a.Subject == nil {
		return out
	}
	for _, v := range w.Versions(a.Subject.Type, a.Subject.Key) {
		for _, ref := range v.InputRefs {
			if in, ok := w.ByID[ref]; ok {
				out = append(out, in.Evidence...)
			}
		}
	}
	return uniqueSorted(out)
}

// coverage explains, per assertion, which records back the claim and why an
// apparent candidate is not counted. It is a transparency aid; the verdict is
// always recomputed by the solver, never read off this list.
func (a Assertion) coverage(w *World) []CandidateCoverage {
	var out []CandidateCoverage
	if a.Subject == nil {
		return out
	}
	type cand struct {
		r       *Record
		counted bool
		reason  string
	}
	var cands []cand
	var mentions []*Record
	for _, r := range w.Records {
		if r.Kind != KindPublication || r.Subject == nil {
			continue
		}
		if *r.Subject != *a.Subject {
			continue
		}
		mentions = append(mentions, r)
	}
	at, ok := statusInstant(a, w.Target)
	if !ok {
		for _, r := range mentions {
			out = append(out, CandidateCoverage{RecordID: r.RecordID, Counted: false,
				Reason: "no queried instant; this publication cannot be placed relative to the claim"})
		}
		return out
	}
	versions := mentions
	for i, v := range versions {
		matches := statusMatches(v, a)
		switch {
		case v.PublishedAt.IsZero():
			cands = append(cands, cand{v, false, "publication has no platform timestamp; it cannot be placed in order"})
		case v.PublishedAt.After(at.Latest):
			cands = append(cands, cand{v, false, "published strictly after the queried instant's latest bound; not visible then"})
		default:
			// Check whether a later version was already effective at earliest.
			laterEffective := false
			for _, o := range versions[i+1:] {
				if !o.PublishedAt.After(at.Latest) {
					laterEffective = true
				}
			}
			_ = laterEffective
			if matches {
				cands = append(cands, cand{v, true,
					"published by the instant and states the claimed status"})
			} else {
				cands = append(cands, cand{v, false,
					fmt.Sprintf("published by the instant but states %q, not the claimed status", v.Status)})
			}
		}
	}
	// A status that a later, after-cutoff version would have shown is not
	// counted: "a value matches today" is not "this value was visible then".
	if len(cands) == 0 {
		for _, r := range mentions {
			cands = append(cands, cand{r, false, "version exists in scope but not at the queried instant"})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].r.RecordID < cands[j].r.RecordID })
	for _, c := range cands {
		out = append(out, CandidateCoverage{RecordID: c.r.RecordID, Counted: c.counted, Reason: c.reason})
	}
	return out
}

// ---- record construction --------------------------------------------------

// NewRecordFromEvent normalises an imported event draft.
func NewRecordFromEvent(d EventDraft, cutoff time.Time) (*Record, error) {
	if d.Ref == "" {
		return nil, fmt.Errorf("event draft needs a ref")
	}
	if d.Occurred == nil || !d.Occurred.Validate() {
		return nil, fmt.Errorf("event %s needs an occurred interval with earliest <= latest", d.Ref)
	}
	r := &Record{
		RecordID: d.Ref, System: d.System, Stream: defaultStream(d.Stream), Kind: KindEvent,
		Subject: d.Subject, Membership: d.Membership, Evidence: sortedCopy(d.Evidence),
		SourceSequence: d.Sequence, BootSegment: defaultBoot(d.BootSegment),
		Classification: d.Classification, Tags: sortedCopy(d.Tags), Detail: d.Detail,
	}
	if d.Kind != "" {
		switch d.Kind {
		case KindEvent, KindFacts, KindPublication, KindRead:
			r.Kind = d.Kind
		default:
			return nil, fmt.Errorf("event %s has unknown kind %q", d.Ref, d.Kind)
		}
	}
	r.Occurred = *d.Occurred
	r.Received = d.Received
	if d.Received != nil && !d.Received.Validate() {
		return nil, fmt.Errorf("event %s has an inverted received interval", d.Ref)
	}
	r.ValidSequence = d.Sequence != ""
	return r, nil
}

// NewRecordFromPublication normalises an imported publication log entry.
func NewRecordFromPublication(p PublicationDraft) (*Record, error) {
	if p.Ref == "" || p.SnapshotID == "" {
		return nil, fmt.Errorf("publication draft needs ref and snapshot_id")
	}
	if p.Subject.Empty() {
		return nil, fmt.Errorf("publication %s needs a subject", p.Ref)
	}
	if p.PublishedAt.IsZero() {
		return nil, fmt.Errorf("publication %s needs published_at", p.Ref)
	}
	b := TimeBound{Earliest: p.PublishedAt.UTC(), Latest: p.PublishedAt.UTC(), Basis: "platform_clock"}
	r := &Record{
		RecordID: p.Ref, System: SystemPlatform, Stream: "publication",
		Kind: KindPublication, Occurred: b, Subject: &SubjectRef{Type: p.Subject.Type, Key: p.Subject.Key},
		SnapshotID: p.SnapshotID, Version: p.Version, SnapshotKind: p.Kind,
		Status: p.Status, SecurityResult: p.SecurityResult, Confirmed: p.Confirmed,
		PublishedAt: p.PublishedAt.UTC(), SourceSequence: p.Sequence,
		BootSegment: defaultBoot(p.BootSegment), InputRefs: sortedCopy(p.InputRefs),
		Supersedes: p.Supersedes, Membership: p.Membership,
	}
	r.ValidSequence = p.Sequence != ""
	if len(p.Counts) > 0 {
		r.Observed = map[string]string{}
		for k, v := range p.Counts {
			r.Observed[k] = v
		}
	}
	// Autonomous publication ordering: version N+1 for one subject is published
	// no earlier than N, which the causal/version chain below makes explicit.
	return r, nil
}

// NewRecordFromRead normalises an imported consumer read.
func NewRecordFromRead(d ReadDraft) (*Record, error) {
	if d.Ref == "" || d.System == "" {
		return nil, fmt.Errorf("read draft needs ref and system")
	}
	if d.Occurred == nil || !d.Occurred.Validate() {
		return nil, fmt.Errorf("read %s needs an occurred interval", d.Ref)
	}
	r := &Record{
		RecordID: d.Ref, System: d.System, Stream: defaultStream(d.Stream), Kind: KindRead,
		Occurred: *d.Occurred, Received: d.Received, ReferencedSnapshot: d.ReferencedSnapshot,
		Observed: d.Observed, SourceSequence: d.Sequence, BootSegment: defaultBoot(d.BootSegment),
		Tags: sortedCopy(d.Tags), Detail: d.Detail,
	}
	r.ValidSequence = d.Sequence != ""
	if d.Received == nil {
		// When a read does not report a separate receipt, its occurrence is the
		// only instant the review has.
		r.Received = d.Occurred
	}
	return r, nil
}

// NewRecordFromFacts normalises a facts batch. It carries only an occurrence
// order within its own system and boot segment; it never becomes a claim.
func NewRecordFromFacts(d FactsDraft) (*Record, error) {
	if d.Ref == "" || d.System == "" {
		return nil, fmt.Errorf("facts draft needs ref and system")
	}
	occ := TimeBound{}
	if d.Occurred != nil && d.Occurred.Validate() {
		occ = *d.Occurred
	} else if d.Received != nil && d.Received.Validate() {
		occ = TimeBound{Earliest: d.Received.Earliest, Latest: d.Received.Latest, Basis: "facts_receipt"}
	} else {
		return nil, fmt.Errorf("facts %s needs an occurred or received interval", d.Ref)
	}
	n, err := strconv.ParseInt(d.Sequence, 10, 64)
	if d.Sequence == "" || err != nil || n < 0 {
		return nil, fmt.Errorf("facts %s needs a non-negative numeric source_sequence (its only ordering evidence)", d.Ref)
	}
	r := &Record{
		RecordID: d.Ref, System: d.System, Stream: defaultStream(d.Stream), Kind: KindFacts,
		Occurred: occ, Received: d.Received, SourceSequence: d.Sequence,
		BootSegment: defaultBoot(d.BootSegment), ValidSequence: true, Detail: d.Detail,
	}
	return r, nil
}

func defaultStream(s string) string {
	if s == "" {
		return "main"
	}
	return s
}

func defaultBoot(s string) string {
	if s == "" {
		return "boot-1"
	}
	return s
}

// timeBoundPtr returns a pointer to a copy of b.
func timeBoundPtr(b TimeBound) *TimeBound { return &b }
