package review

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// World is the in-scope material of one review: the records that survived the
// cutoff and the scope filter, indexed for constraint assembly. Building it is
// pure; a review can be re-run from the pinned material alone.
type World struct {
	Target     Target
	Records    []*Record
	ByID       map[string]*Record
	Excluded   []ScopeDecision
	graph      *Closure
	levels     map[string]bool
	versions   map[string][]*Record // "type:key" -> publications sorted by version
	unresolved []string
	guardID    string
	// extraShipments are other shipments the scope authorises for comparison.
	// Their relationship closure is joined into the world so a multi-shipment
	// claim set can be tested against one common order.
	extraShipments []string
}

// NewWorld indexes records for a target. Records are expected to be the
// effective (cutoff-bounded, scope-filtered) set; excluded lists what was left
// out and why.
func NewWorld(target Target, records []*Record, excluded []ScopeDecision) *World {
	w := &World{
		Target: target, ByID: map[string]*Record{}, Excluded: excluded,
		levels: map[string]bool{}, versions: map[string][]*Record{},
		guardID: fmt.Sprintf("case-cutoff:%s@%s", target.CaseID, target.Cutoff.UTC().Format("20060102T150405Z")),
	}
	for _, lv := range target.Levels {
		w.levels[lv] = true
	}
	for _, as := range target.AuthorizedSubjects {
		if as.Type == "shipment" && as.Key != target.ShipmentKey {
			w.extraShipments = append(w.extraShipments, as.Key)
		}
	}
	w.extraShipments = uniqueSorted(w.extraShipments)
	if len(w.levels) == 0 {
		w.levels["shipment"] = true
	}
	sorted := append([]*Record(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RecordID < sorted[j].RecordID })
	w.Records = sorted
	for _, r := range sorted {
		w.ByID[r.RecordID] = r
		if r.Kind == KindPublication && r.Subject != nil {
			k := subjectKey(r.Subject.Type, r.Subject.Key)
			w.versions[k] = append(w.versions[k], r)
		}
	}
	// A publication log whose version numbers are not monotone with the
	// platform publication time is not silently re-sorted: the newer version is
	// what "the last effective version" means, so a non-monotone chain must be
	// reported rather than guessed at.
	for k := range w.versions {
		vs := w.versions[k]
		for i := 1; i < len(vs); i++ {
			if vs[i].Version > vs[i-1].Version && vs[i].PublishedAt.Before(vs[i-1].PublishedAt) {
				w.unresolved = append(w.unresolved, fmt.Sprintf(
					"subject %s: version %d is published at %s, before version %d at %s; "+
						"effective-version resolution is not well ordered for this chain",
					k, vs[i].Version, vs[i].PublishedAt.UTC().Format(time.RFC3339),
					vs[i-1].Version, vs[i-1].PublishedAt.UTC().Format(time.RFC3339)))
			}
		}
	}
	for k := range w.versions {
		vs := w.versions[k]
		sort.SliceStable(vs, func(i, j int) bool {
			if vs[i].Version != vs[j].Version {
				return vs[i].Version < vs[j].Version
			}
			if !vs[i].PublishedAt.Equal(vs[j].PublishedAt) {
				return vs[i].PublishedAt.Before(vs[j].PublishedAt)
			}
			return vs[i].RecordID < vs[j].RecordID
		})
	}
	w.graph = buildClosure(sorted, target.ShipmentKey)
	return w
}

// Unresolved lists material gaps found while assembling the world.
func (w *World) Unresolved() []string { return uniqueSorted(w.unresolved) }

// GuardID names the cutoff constraint's guard.
func (w *World) GuardID() string { return w.guardID }

func subjectKey(typ, key string) string { return typ + ":" + key }

// Versions returns the publication versions of one subject, ordered by version.
func (w *World) Versions(typ, key string) []*Record {
	return w.versions[subjectKey(typ, key)]
}

// AllSubjects returns every subject that has publications in scope.
func (w *World) AllSubjects() []SubjectRef {
	var out []SubjectRef
	for k := range w.versions {
		for _, v := range w.versions[k] {
			out = append(out, *v.Subject)
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// closure is the relationship closure the target's levels authorise, derived
// from in-scope membership records only.
type closure struct {
	shipmentPkgs  map[string][]string
	pkgContainers map[string][]string
	containerLegs map[string][]string
}

// Closure rebuilds the relationship closure from membership records.
type Closure struct{ *closure }

func buildClosure(records []*Record, shipment string) *Closure {
	c := &closure{
		shipmentPkgs: map[string][]string{}, pkgContainers: map[string][]string{},
		containerLegs: map[string][]string{},
	}
	for _, r := range records {
		if r.Membership == nil {
			continue
		}
		for _, s := range r.Membership.Subjects {
			for _, o := range r.Membership.Objects {
				switch r.Membership.LinkKind {
				case "shipment_package":
					c.shipmentPkgs[s] = appendUnique(c.shipmentPkgs[s], o)
				case "package_container":
					c.pkgContainers[s] = appendUnique(c.pkgContainers[s], o)
				case "container_leg":
					c.containerLegs[s] = appendUnique(c.containerLegs[s], o)
				}
			}
		}
	}
	return &Closure{c}
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// ShipmentPackages returns the packages ever linked to the shipment in scope.
func (w *World) ShipmentPackages() []string {
	return sortedCopy(w.graph.shipmentPkgs[w.Target.ShipmentKey])
}

// PackagesOfShipment returns the packages of any shipment the review may see
// (the target plus explicitly authorised comparison shipments).
func (w *World) PackagesOfShipment(shipment string) []string {
	return sortedCopy(w.graph.shipmentPkgs[shipment])
}

// VisibleShipments lists the target plus authorised comparison shipments.
func (w *World) VisibleShipments() []string {
	return uniqueSorted(append([]string{w.Target.ShipmentKey}, w.extraShipments...))
}

// ContainersOfShipment returns containers of the visible shipments' packages.
func (w *World) ContainersOfShipment() []string {
	var out []string
	for _, ship := range w.VisibleShipments() {
		for _, p := range w.graph.shipmentPkgs[ship] {
			out = append(out, w.graph.pkgContainers[p]...)
		}
	}
	return uniqueSorted(out)
}

// LegsOfShipment returns legs of the visible shipments' containers.
func (w *World) LegsOfShipment() []string {
	var out []string
	for _, c := range w.ContainersOfShipment() {
		out = append(out, w.graph.containerLegs[c]...)
	}
	return uniqueSorted(out)
}

// PackagesOfContainer returns the packages stowed in a container.
func (w *World) PackagesOfContainer(container string) []string {
	var out []string
	for _, p := range w.ShipmentPackages() {
		if containsStr(w.graph.pkgContainers[p], container) {
			out = append(out, p)
		}
	}
	return uniqueSorted(out)
}

// ShipmentsOfContainer: the review only knows the target shipment, so a
// container claim's shipment membership is the target when linked.
func (w *World) ShipmentsOfContainer(container string) []string {
	var out []string
	for p, conts := range w.graph.pkgContainers {
		if !containsStr(conts, container) {
			continue
		}
		for ship, pkgs := range w.graph.shipmentPkgs {
			if containsStr(pkgs, p) {
				out = append(out, ship)
			}
		}
	}
	return uniqueSorted(out)
}

// scopeSubjects returns the subjects the target may see at its levels.
func (w *World) scopeSubjects() []SubjectRef {
	var out []SubjectRef
	if w.levels["shipment"] {
		for _, ship := range w.VisibleShipments() {
			out = append(out, SubjectRef{Type: "shipment", Key: ship})
		}
	}
	if w.levels["container"] {
		for _, c := range w.ContainersOfShipment() {
			out = append(out, SubjectRef{Type: "container", Key: c})
		}
	}
	if w.levels["leg"] {
		for _, l := range w.LegsOfShipment() {
			out = append(out, SubjectRef{Type: "leg", Key: l})
		}
	}
	return out
}

// ---- base constraint assembly ---------------------------------------------

// BaseConstraints builds the system every branch shares: time windows from the
// declared source clocks, receipt semantics, per-source sequence order inside a
// boot segment, and publication prerequisites.
func (w *World) BaseConstraints(limit int) *ConstraintSet {
	cs := NewConstraintSet(limit)
	// Some fields are derived while assembling (an occurrence interval inferred
	// from an exact receipt, a publication anchored to its platform timestamp).
	// Derive on a copy so Build is repeatable and pure.
	for _, r := range w.Records {
		cp := *r
		r = &cp
		w.ByID[r.RecordID] = r
		w.addTimeConstraints(cs, r)
	}
	w.addSequenceConstraints(cs)
	w.addCausalConstraints(cs)
	return cs
}

func (w *World) addTimeConstraints(cs *ConstraintSet, r *Record) {
	occ := RecVar(r.RecordID)
	hasOcc := r.Occurred.Validate()
	if r.Kind == KindPublication && !r.PublishedAt.IsZero() {
		b := TimeBound{Earliest: r.PublishedAt, Latest: r.PublishedAt, Basis: "platform_clock"}
		r.Occurred = b
		hasOcc = true
	}
	if hasOcc {
		Window(cs, occ, r.Occurred, []string{r.RecordID},
			fmt.Sprintf("%s occurrence interval (%s)", r.RecordID, r.Occurred.Basis),
			PrefixWindow+r.RecordID)
	} else if r.Received != nil && r.Received.Known() {
		// No source occurrence instant: the receipt is the only anchor.
		r.Occurred = TimeBound{Earliest: r.Received.Earliest, Latest: r.Received.Latest, Basis: "receipt_exact"}
		hasOcc = true
		Window(cs, occ, r.Occurred, []string{r.RecordID},
			fmt.Sprintf("%s occurrence anchored to exact receipt", r.RecordID),
			PrefixWindow+r.RecordID)
	} else {
		w.unresolved = append(w.unresolved,
			fmt.Sprintf("record %s has no usable occurrence interval; it cannot participate in ordering", r.RecordID))
	}

	if r.Received != nil && r.Received.Validate() {
		rv := "recv:" + r.RecordID
		Window(cs, rv, *r.Received, []string{r.RecordID},
			fmt.Sprintf("%s receipt interval", r.RecordID), PrefixReceived+r.RecordID)
		if hasOcc {
			// A record cannot be received before it occurred.
			Before(cs, occ, rv, []string{r.RecordID},
				fmt.Sprintf("%s occurred before it was received", r.RecordID), PrefixCausal+r.RecordID+".occ<=recv")
		}
		// Nothing received after the case cutoff may influence the review.
		cs.Add(Constraint{
			ID: PrefixCutoff + r.RecordID, Kind: "scope", From: rv, To: zeroNode,
			MaxGapNs:  w.Target.Cutoff.UnixNano(),
			Reason:    fmt.Sprintf("%s must have been received by the case cutoff %s", r.RecordID, w.Target.Cutoff.Format("2006-01-02T15:04:05Z")),
			RecordIDs: []string{r.RecordID}, Guard: []string{w.guardID},
		})
	} else if hasOcc && r.Kind != KindPublication {
		// Occurrence alone still must lie before the cutoff's knowledge horizon.
		cs.Add(Constraint{
			ID: PrefixCutoff + r.RecordID, Kind: "scope", From: occ, To: zeroNode,
			MaxGapNs:  w.Target.Cutoff.UnixNano(),
			Reason:    fmt.Sprintf("%s occurrence must precede the case cutoff", r.RecordID),
			RecordIDs: []string{r.RecordID}, Guard: []string{w.guardID},
		})
	}
	if r.Kind == KindRead && r.Received != nil && r.Received.Known() {
		// Consumer read logs are the observed receipt of the read itself.
		rv := "recv:" + r.RecordID
		Window(cs, rv, TimeBound{Earliest: r.Received.Earliest, Latest: r.Received.Latest, Basis: "read_receipt"},
			[]string{r.RecordID}, fmt.Sprintf("%s read receipt is exact", r.RecordID), PrefixReceived+r.RecordID+"#read")
	}
	// Publication version timestamps are the platform clock: constrain the
	// publication instant to the platform's own reported published_at.
	if r.Kind == KindPublication && !r.PublishedAt.IsZero() && r.Received == nil {
		rv := "recv:" + r.RecordID
		Window(cs, rv, TimeBound{Earliest: r.PublishedAt, Latest: r.PublishedAt, Basis: "platform_clock"},
			[]string{r.RecordID}, fmt.Sprintf("%s publication receipt", r.RecordID), PrefixReceived+r.RecordID+"#pub")
	}
}

func (w *World) addSequenceConstraints(cs *ConstraintSet) {
	type group struct {
		valid   []*Record
		numbers map[int64][]*Record
	}
	groups := map[string]*group{}
	for _, r := range w.Records {
		if !r.ValidSequence || r.SourceSequence == "" {
			continue
		}
		n, err := strconv.ParseInt(r.SourceSequence, 10, 64)
		if err != nil {
			continue
		}
		gk := r.System + "/" + r.Stream + "/" + r.BootSegment
		g := groups[gk]
		if g == nil {
			g = &group{numbers: map[int64][]*Record{}}
			groups[gk] = g
		}
		g.numbers[n] = append(g.numbers[n], r)
	}
	for gk, g := range groups {
		nums := make([]int64, 0, len(g.numbers))
		for n := range g.numbers {
			nums = append(nums, n)
		}
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		var ordered []*Record
		for _, n := range nums {
			rs := g.numbers[n]
			if len(rs) > 1 {
				for _, r := range rs {
					r.ValidSequence = false
					r.SeqConflict = fmt.Sprintf("sequence %d is reused inside boot segment %s; numeric order is not evidence", n, gk)
					w.unresolved = append(w.unresolved, r.SeqConflict)
				}
				// Everything from this boot segment loses the numeric edge at the
				// reused number; keep the unambiguous prefix only.
				ordered = nil
				break
			}
			ordered = append(ordered, rs[0])
		}
		for i := 0; i+1 < len(ordered); i++ {
			a, b := ordered[i], ordered[i+1]
			if a.BootSegment != b.BootSegment {
				continue
			}
			StrictBefore(cs, RecVar(a.RecordID), RecVar(b.RecordID),
				[]string{a.RecordID, b.RecordID},
				fmt.Sprintf("same source %s boot %s: sequence %s precedes %s", gk, a.BootSegment, a.SourceSequence, b.SourceSequence),
				PrefixSeq+a.RecordID+"<"+b.RecordID)
		}
	}
	// Records that reuse a number inside one boot segment never order each
	// other, but their own windows still apply. Reused numbers across different
	// boot segments are unrelated by construction (the group key includes boot).
}

func (w *World) addCausalConstraints(cs *ConstraintSet) {
	for _, r := range w.Records {
		if r.Kind != KindPublication {
			continue
		}
		for _, ref := range r.InputRefs {
			in, ok := w.ByID[ref]
			if !ok {
				w.unresolved = append(w.unresolved,
					fmt.Sprintf("publication %s cites input %s that is outside the reviewed material; "+
						"its prerequisite cannot be independently recomputed", r.RecordID, ref))
				continue
			}
			Before(cs, RecVar(in.RecordID), RecVar(r.RecordID), []string{in.RecordID, r.RecordID},
				fmt.Sprintf("publication %s depends on input %s, so the input must precede the publication", r.RecordID, ref),
				PrefixCausal+r.RecordID+".in."+in.RecordID)
		}
	}
}

// ---- per-assertion shapes -------------------------------------------------

// effectiveConstraints express "version v is the effective version at instant
// at", given the ordered version list. It pins the version's own instant and
// pushes every later version strictly past the instant's earliest bound.
func effectiveConstraints(w *World, v *Record, later []*Record, at TimeBound, assertID string) []Constraint {
	out := []Constraint{{
		ID: PrefixEffective + assertID + "." + v.RecordID + "#before", Kind: "effective",
		From: RecVar(v.RecordID), To: zeroNode, MaxGapNs: at.Latest.UnixNano(),
		Reason:    fmt.Sprintf("version %s (v%d) must already be published by the latest bound of the queried instant", v.RecordID, v.Version),
		RecordIDs: []string{v.RecordID}, Guard: []string{w.guardID},
	}}
	for _, laterV := range later {
		out = append(out, Constraint{
			ID: PrefixEffective + assertID + "." + laterV.RecordID + "#after", Kind: "effective",
			From: RecVar(laterV.RecordID), To: zeroNode, MaxGapNs: at.Earliest.UnixNano() - 1,
			Reason:    fmt.Sprintf("version %s (v%d) must still be unpublished at the earliest bound of the queried instant", laterV.RecordID, laterV.Version),
			RecordIDs: []string{laterV.RecordID}, Guard: []string{w.guardID},
		})
	}
	for _, ref := range v.InputRefs {
		if in, ok := w.ByID[ref]; ok {
			out = append(out, Constraint{
				ID: PrefixCausal + assertID + "." + v.RecordID + ".prereq." + in.RecordID, Kind: "causal",
				From: RecVar(in.RecordID), To: RecVar(v.RecordID), MaxGapNs: 0,
				Reason:    fmt.Sprintf("visible status must include its prerequisite: input %s precedes publication %s", in.RecordID, v.RecordID),
				RecordIDs: []string{in.RecordID, v.RecordID}, Guard: []string{w.guardID},
			})
		}
	}
	return out
}

// noVisibleConstraints express "no version of the subject was effective at at".
func noVisibleConstraints(w *World, versions []*Record, at TimeBound, assertID string) []Constraint {
	if len(versions) == 0 {
		return nil
	}
	first := versions[0]
	return []Constraint{{
		ID: PrefixEffective + assertID + ".none", Kind: "effective",
		From: RecVar(first.RecordID), To: zeroNode, MaxGapNs: at.Latest.UnixNano(),
		Reason:    fmt.Sprintf("the subject had no published version by the instant, so version %s must still be unpublished", first.RecordID),
		RecordIDs: []string{first.RecordID}, Guard: []string{w.guardID},
	}}
}
