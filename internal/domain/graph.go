package domain

import (
	"fmt"
	"sort"
	"time"
)

// Identity graph link kinds. A shipment (waybill consignment) is composed of
// package versions; packages are stowed in containers (ULDs); containers fly
// on flight legs. Every relationship is effective-dated and knowledge-dated so
// a snapshot can be projected from exactly what was known when it closed.
const (
	LinkShipmentPackage  = "shipment_package"
	LinkPackageContainer = "package_container"
	LinkContainerLeg     = "container_leg"
)

// Link is one immutable relationship row. Retirement is a separate row (see
// Retraction) so the original claim is never overwritten.
type Link struct {
	ID                    int64             `json:"id,omitempty"`
	LinkKey               string            `json:"link_key"`
	LinkVersion           int               `json:"link_version"`
	Kind                  string            `json:"kind"`
	SubjectType           string            `json:"subject_type"`
	SubjectKey            string            `json:"subject_key"`
	ObjectType            string            `json:"object_type"`
	ObjectKey             string            `json:"object_key"`
	ValidFrom             time.Time         `json:"valid_from"`
	KnowledgeAt           time.Time         `json:"knowledge_at"`
	PackageRevision       string            `json:"package_revision,omitempty"`
	SourceEventID         string            `json:"source_event_id"`
	Attributes            map[string]string `json:"attributes,omitempty"`
	RetractedAt           *time.Time        `json:"retracted_at,omitempty"`
	RetractionKnowledgeAt *time.Time        `json:"retraction_knowledge_at,omitempty"`
	RetractionReason      string            `json:"retraction_reason,omitempty"`
}

// Retraction records a link being closed by a later identity change. It never
// modifies the link row.
type Retraction struct {
	LinkKey       string    `json:"link_key"`
	LinkVersion   int       `json:"link_version"`
	EffectiveAt   time.Time `json:"effective_at"`
	KnowledgeAt   time.Time `json:"knowledge_at"`
	Reason        string    `json:"reason"`
	SourceEventID string    `json:"source_event_id"`
}

// ActiveAt is true when the link describes reality at occurred time t and was
// known at knowledge cutoff k. Retirement cannot affect a projection that
// closed before the retirement knowledge arrived — that is what makes replay
// of a past publication possible.
func (l Link) ActiveAt(t, k time.Time) bool {
	if l.KnowledgeAt.After(k) {
		return false
	}
	if l.ValidFrom.After(t) {
		return false
	}
	if l.RetiredAsOf(t, k) {
		return false
	}
	return true
}

// RetiredAsOf reports whether the link's retirement was effective by t and
// known by k.
func (l Link) RetiredAsOf(t, k time.Time) bool {
	if l.RetractedAt == nil || l.RetractionKnowledgeAt == nil {
		return false
	}
	return !l.RetractedAt.After(t) && !l.RetractionKnowledgeAt.After(k)
}

// Graph is the projected identity graph over a set of links.
type Graph struct {
	links []Link
}

// NewGraph indexes materialised links. The caller passes links with their
// retirement columns already joined in.
func NewGraph(links []Link) *Graph {
	cp := make([]Link, len(links))
	copy(cp, links)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].ValidFrom.Equal(cp[j].ValidFrom) {
			if cp[i].LinkKey == cp[j].LinkKey {
				return cp[i].LinkVersion < cp[j].LinkVersion
			}
			return cp[i].LinkKey < cp[j].LinkKey
		}
		return cp[i].ValidFrom.Before(cp[j].ValidFrom)
	})
	return &Graph{links: cp}
}

// Links returns the raw projected links (for audit).
func (g *Graph) Links() []Link { return g.links }

// ActiveMemberships returns the shipment's active package memberships at
// occurred time t as known at knowledge K.
func (g *Graph) ActiveMemberships(shipmentKey string, t, k time.Time) []Link {
	var out []Link
	for _, l := range g.links {
		if l.Kind != LinkShipmentPackage || l.SubjectKey != shipmentKey {
			continue
		}
		if l.ActiveAt(t, k) {
			out = append(out, l)
		}
	}
	return out
}

// ActivePackages returns the active package set of a shipment.
func (g *Graph) ActivePackages(shipmentKey string, t, k time.Time) []string {
	var out []string
	for _, l := range g.ActiveMemberships(shipmentKey, t, k) {
		out = append(out, l.ObjectKey)
	}
	return SortedUnique(out)
}

// MembershipVersions returns every distinct composition version of a shipment
// ordered oldest first. Each version is the FULL active package set at that
// effective instant, so a split renders as "before: {A,B} / after: {B,A1,A2}"
// rather than an opaque delta.
type CompositionVersion struct {
	Version       int       `json:"version"`
	PackageKeys   []string  `json:"package_keys"`
	EffectiveFrom time.Time `json:"effective_from"`
	KnowledgeAt   time.Time `json:"knowledge_at"`
	SourceEventID string    `json:"source_event_id"`
	Cause         string    `json:"cause"`
}

func (g *Graph) MembershipVersions(shipmentKey string) []CompositionVersion {
	type stamp struct {
		t     time.Time
		k     time.Time
		event string
		cause string
	}
	byTime := map[int64]*stamp{}
	var order []int64
	for _, l := range g.links {
		if l.Kind != LinkShipmentPackage || l.SubjectKey != shipmentKey {
			continue
		}
		id := l.ValidFrom.UnixNano()
		s, ok := byTime[id]
		if !ok {
			s = &stamp{t: l.ValidFrom, k: l.KnowledgeAt, event: l.SourceEventID, cause: l.Attributes["change"]}
			byTime[id] = s
			order = append(order, id)
		}
		if l.KnowledgeAt.After(s.k) {
			s.k = l.KnowledgeAt
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	raw := make([]CompositionVersion, 0, len(order))
	for _, id := range order {
		s := byTime[id]
		cause := s.cause
		if cause == "" {
			cause = "declared"
		}
		raw = append(raw, CompositionVersion{
			PackageKeys:   g.ActivePackages(shipmentKey, s.t, s.k),
			EffectiveFrom: s.t, KnowledgeAt: s.k,
			SourceEventID: s.event, Cause: cause,
		})
	}
	// A restatement that does not change the active package set is not a new
	// composition version: only transitions are versions.
	out := make([]CompositionVersion, 0, len(raw))
	var prev []string
	for _, v := range raw {
		cur := append([]string(nil), v.PackageKeys...)
		sort.Strings(cur)
		if prev != nil && equalStrings(cur, prev) {
			continue
		}
		v.Version = len(out) + 1
		out = append(out, v)
		prev = cur
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PackageWasMember reports whether a package was ever linked to a shipment
// (regardless of retirement). Used to distinguish "superseded after split" from
// "never known".
func (g *Graph) PackageWasMember(packageKey string) (string, bool) {
	for _, l := range g.links {
		if l.Kind == LinkShipmentPackage && l.ObjectKey == packageKey {
			return l.SubjectKey, true
		}
	}
	return "", false
}

// ShipmentPackagesEver returns every package key ever linked to the shipment,
// including retired ones. Keeps misaligned late evidence auditable.
func (g *Graph) ShipmentPackagesEver(shipmentKey string) []string {
	var out []string
	for _, l := range g.links {
		if l.Kind == LinkShipmentPackage && l.SubjectKey == shipmentKey {
			out = append(out, l.ObjectKey)
		}
	}
	return SortedUnique(out)
}

// ActiveContainerOf returns the container a package is stowed in at t/K.
// A package can legitimately have >1 when the shipper staged it in two ULDs
// before consolidation; callers treat >1 as a quality signal.
func (g *Graph) ActiveContainerOf(packageKey string, t, k time.Time) []string {
	var out []string
	for _, l := range g.links {
		if l.Kind != LinkPackageContainer || l.SubjectKey != packageKey {
			continue
		}
		if l.ActiveAt(t, k) {
			out = append(out, l.ObjectKey)
		}
	}
	return SortedUnique(out)
}

// CurrentContainerOf ignores validity windows and returns the newest stow
// claim known at K.
func (g *Graph) CurrentContainerOf(packageKey string, k time.Time) (string, bool) {
	var best Link
	found := false
	for _, l := range g.links {
		if l.Kind != LinkPackageContainer || l.SubjectKey != packageKey {
			continue
		}
		if l.KnowledgeAt.After(k) {
			continue
		}
		if l.RetiredAsOf(l.ValidFrom, k) {
			continue
		}
		if !found || l.ValidFrom.After(best.ValidFrom) ||
			(l.ValidFrom.Equal(best.ValidFrom) && l.LinkVersion > best.LinkVersion) {
			best = l
			found = true
		}
	}
	if !found {
		return "", false
	}
	return best.ObjectKey, true
}

// LegsOfContainer returns the legs a container is booked on, as known at K.
func (g *Graph) LegsOfContainer(containerKey string, k time.Time) []string {
	var out []string
	for _, l := range g.links {
		if l.Kind != LinkContainerLeg || l.SubjectKey != containerKey {
			continue
		}
		if l.KnowledgeAt.After(k) {
			continue
		}
		if l.RetractedAt != nil && l.RetractionKnowledgeAt != nil &&
			!l.RetractedAt.After(l.ValidFrom) && !l.RetractionKnowledgeAt.After(k) {
			continue
		}
		out = append(out, l.ObjectKey)
	}
	return SortedUnique(out)
}

// LegsOfShipment returns the union of legs reachable from the shipment's
// currently active packages via their current containers, as known at K.
func (g *Graph) LegsOfShipment(shipmentKey string, t, k time.Time) []string {
	var out []string
	for _, p := range g.ActivePackages(shipmentKey, t, k) {
		for _, c := range g.ActiveContainerOf(p, t, k) {
			out = append(out, g.LegsOfContainer(c, k)...)
		}
	}
	return SortedUnique(out)
}

// PackagesOfContainer returns packages currently stowed in a container at K.
func (g *Graph) PackagesOfContainer(containerKey string, t, k time.Time) []string {
	var out []string
	for _, l := range g.links {
		if l.Kind != LinkPackageContainer || l.ObjectKey != containerKey {
			continue
		}
		if l.ActiveAt(t, k) {
			out = append(out, l.SubjectKey)
		}
	}
	return SortedUnique(out)
}

// ContainersOfShipment returns active containers of the shipment's packages.
func (g *Graph) ContainersOfShipment(shipmentKey string, t, k time.Time) []string {
	var out []string
	for _, p := range g.ActivePackages(shipmentKey, t, k) {
		out = append(out, g.ActiveContainerOf(p, t, k)...)
	}
	return SortedUnique(out)
}

// ShipmentsOnLeg returns shipments with an active package in a container on
// the leg at time t/K.
func (g *Graph) ShipmentsOnLeg(legKey string, t, k time.Time) []string {
	var out []string
	for _, c := range g.containersOnLeg(legKey, k) {
		for _, p := range g.PackagesOfContainer(c, t, k) {
			if s, ok := g.shipmentOfActivePackage(p, t, k); ok {
				out = append(out, s)
			}
		}
	}
	return SortedUnique(out)
}

func (g *Graph) containersOnLeg(legKey string, k time.Time) []string {
	var out []string
	for _, l := range g.links {
		if l.Kind == LinkContainerLeg && l.ObjectKey == legKey && !l.KnowledgeAt.After(k) {
			out = append(out, l.SubjectKey)
		}
	}
	return SortedUnique(out)
}

func (g *Graph) shipmentOfActivePackage(pkg string, t, k time.Time) (string, bool) {
	for _, l := range g.links {
		if l.Kind == LinkShipmentPackage && l.ObjectKey == pkg && l.ActiveAt(t, k) {
			return l.SubjectKey, true
		}
	}
	return "", false
}

// ShipmentOfPackage returns the shipment owning a package as an active member
// at t/K.
func (g *Graph) ShipmentOfPackage(pkg string, t, k time.Time) (string, bool) {
	return g.shipmentOfActivePackage(pkg, t, k)
}

// ---- Derivation from ledger events ------------------------------------------

// LinkRequest is a graph mutation implied by an event, plus any retirement.
type LinkRequest struct {
	LinkKey         string
	Kind            string
	SubjectType     string
	SubjectKey      string
	ObjectType      string
	ObjectKey       string
	ValidFrom       time.Time
	KnowledgeAt     time.Time
	PackageRevision string
	SourceEventID   string
	Attributes      map[string]string
}

// Derivation is the deterministic graph effect of one event.
type Derivation struct {
	Upserts      []LinkRequest
	RetireKeys   []string // link keys to close at the event's effective time
	RetireReason string
}

// Slot keys. One stow slot per package (transload closes the previous), one
// membership slot per shipment+package pair (split retires the parent pair).
func MembershipKey(shipmentKey, packageKey string) string {
	return "shipment/" + shipmentKey + "/package/" + packageKey
}

func StowKey(packageKey string) string { return "package/" + packageKey + "/stow" }

func ContainerLegKey(containerKey, legKey string) string {
	return "container/" + containerKey + "/leg/" + legKey
}

// Derive computes the graph effect of an event. It is a pure function and the
// only place graph semantics live, which keeps replay deterministic.
func Derive(ev *Event) Derivation {
	switch ev.MessageType {
	case TypeDeclaration:
		var ups []LinkRequest
		var retire []string
		for _, raw := range strs(ev.Payload, "packages") {
			p := CanonicalKeyOrRaw(KeyPackage, raw)
			if p == "" || ev.ShipmentKey == "" {
				continue
			}
			ups = append(ups, LinkRequest{
				LinkKey: MembershipKey(ev.ShipmentKey, p), Kind: LinkShipmentPackage,
				SubjectType: "shipment", SubjectKey: ev.ShipmentKey,
				ObjectType: "package", ObjectKey: p,
				ValidFrom: ev.OccurredAt, KnowledgeAt: ev.ReceivedAt,
				SourceEventID: ev.EventID, Attributes: map[string]string{"change": "declared"},
			})
		}
		return Derivation{Upserts: ups, RetireKeys: retire}
	case TypePackageChange:
		action := dramaticallyLower(str(ev.Payload, "action"))
		var parents, children []string
		for _, pk := range strs(ev.Payload, "parent_packages") {
			parents = append(parents, CanonicalKeyOrRaw(KeyPackage, pk))
		}
		for _, ck := range strs(ev.Payload, "child_packages") {
			children = append(children, CanonicalKeyOrRaw(KeyPackage, ck))
		}
		shipment := ev.ShipmentKey
		if shipment == "" {
			return Derivation{}
		}
		var ups []LinkRequest
		var retire []string
		add := func(keys []string, change string) {
			for _, p := range keys {
				if p == "" {
					continue
				}
				ups = append(ups, LinkRequest{
					LinkKey: MembershipKey(shipment, p), Kind: LinkShipmentPackage,
					SubjectType: "shipment", SubjectKey: shipment,
					ObjectType: "package", ObjectKey: p,
					ValidFrom: ev.OccurredAt, KnowledgeAt: ev.ReceivedAt,
					SourceEventID: ev.EventID, Attributes: map[string]string{"change": change},
				})
			}
		}
		switch action {
		case "split":
			add(children, "split_child")
			for _, p := range parents {
				retire = append(retire, MembershipKey(shipment, p))
			}
		case "merge":
			add(children, "merge_child")
			for _, p := range parents {
				retire = append(retire, MembershipKey(shipment, p))
			}
		default:
			return Derivation{}
		}
		return Derivation{Upserts: ups, RetireKeys: retire, RetireReason: "package_" + action}
	case TypeContainerStow:
		containerRaw := str(ev.Payload, "container_id")
		container := CanonicalKeyOrRaw(KeyContainer, containerRaw)
		if container == "" || ev.PackageKey == "" {
			return Derivation{}
		}
		return Derivation{
			Upserts: []LinkRequest{{
				LinkKey: StowKey(ev.PackageKey), Kind: LinkPackageContainer,
				SubjectType: "package", SubjectKey: ev.PackageKey,
				ObjectType: "container", ObjectKey: container,
				ValidFrom: ev.OccurredAt, KnowledgeAt: ev.ReceivedAt,
				PackageRevision: ev.PackageRevision, SourceEventID: ev.EventID,
				Attributes: map[string]string{"slot": str(ev.Payload, "slot")},
			}},
			// The stow slot is single-valued: a new stow closes the previous
			// one for replay of earlier instants only if it is a different
			// container, which the store checks against the existing version.
			RetireReason: "transload",
		}
	case TypeFlightManifest:
		container := CanonicalKeyOrRaw(KeyContainer, str(ev.Payload, "container_id"))
		leg := CanonicalKeyOrRaw(KeyLeg, str(ev.Payload, "leg"))
		if container == "" || leg == "" {
			return Derivation{}
		}
		return Derivation{Upserts: []LinkRequest{{
			LinkKey: ContainerLegKey(container, leg), Kind: LinkContainerLeg,
			SubjectType: "container", SubjectKey: container,
			ObjectType: "leg", ObjectKey: leg,
			ValidFrom: ev.OccurredAt, KnowledgeAt: ev.ReceivedAt,
			SourceEventID: ev.EventID,
			Attributes:    map[string]string{"flight_no": str(ev.Payload, "flight_no")},
		}}}
	}
	return Derivation{}
}

func dramaticallyLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// ---- Evidence disposition -------------------------------------------------

// Dispositions classify whether an event may count as evidence for the active
// version of a shipment.
const (
	DispositionOK                = "ok"
	DispositionUnknownPackage    = "unknown_package"
	DispositionSupersededPackage = "superseded_package"
	DispositionRevisionMismatch  = "revision_mismatch"
	DispositionContainerMismatch = "container_mismatch"
)

// Disposition is one classification record for an event.
type Disposition struct {
	EventID     string    `json:"event_id"`
	ShipmentKey string    `json:"shipment_key"`
	PackageKey  string    `json:"package_key"`
	Disposition string    `json:"disposition"`
	Reason      string    `json:"reason"`
	KnowledgeAt time.Time `json:"knowledge_at"`
}

// EvidenceEligible is true when the event may contribute to fusion.
func (d Disposition) EvidenceEligible() bool { return d.Disposition == DispositionOK }

// MembershipActiveAtKnowledge answers "was this package an active version of
// the shipment at knowledge time k, judged only with what was known at k".
// This is the right question for evidence alignment: an event is acceptable
// when the package was genuine at the moment the event was RECEIVED, even if a
// later split retired it. A late re-transmission that arrives after the split
// is not acceptable and is never transferred to a child.
func (g *Graph) MembershipActiveAtKnowledge(packageKey, shipmentKey string, k time.Time) bool {
	for _, l := range g.links {
		if l.Kind != LinkShipmentPackage || l.ObjectKey != packageKey || l.SubjectKey != shipmentKey {
			continue
		}
		if l.ActiveAt(k, k) {
			return true
		}
	}
	return false
}

// RetirementOf returns when a package membership was retired and when that
// retirement became known, if it was ever retired.
func (g *Graph) RetirementOf(packageKey string) (effective, known time.Time, ok bool) {
	for _, l := range g.links {
		if l.Kind != LinkShipmentPackage || l.ObjectKey != packageKey {
			continue
		}
		if l.RetractedAt == nil || l.RetractionKnowledgeAt == nil {
			continue
		}
		if !ok || l.RetractionKnowledgeAt.Before(known) {
			effective, known, ok = *l.RetractedAt, *l.RetractionKnowledgeAt, true
		}
	}
	return effective, known, ok
}

// ClassifyEvent decides if an event is acceptable evidence for the identified
// shipment at knowledge cutoff k. The question is asked at the knowledge time
// the event ARRIVED (capped by the cutoff): was its package a genuine active
// version of the shipment then?
//
// This makes the split case exact: evidence that arrived while the package was
// still current was valid at that time and stays valid in snapshots that closed
// before the split. A late scan that arrives after the split is misaligned and
// is never transferred to a child.
func ClassifyEvent(g *Graph, ev *Event, shipmentKey string, k time.Time) Disposition {
	d := Disposition{
		EventID: ev.EventID, ShipmentKey: shipmentKey, PackageKey: ev.PackageKey, KnowledgeAt: k,
	}
	if ev.PackageKey == "" {
		d.Disposition = DispositionOK
		d.Reason = "event carries no package reference"
		return d
	}
	kk := ev.ReceivedAt
	if kk.After(k) {
		kk = k
	}
	if g.MembershipActiveAtKnowledge(ev.PackageKey, shipmentKey, kk) {
		d.Disposition = DispositionOK
		d.Reason = "package was an active version of the shipment when the event arrived"
		if ev.MessageType != TypeContainerStow && ev.MessageType != TypeFlightManifest &&
			ev.MessageType != TypePackageChange && ev.MessageType != TypeDeclaration {
			if container := str(ev.Payload, "container_id"); container != "" {
				want := CanonicalKeyOrRaw(KeyContainer, container)
				got := g.ActiveContainerOf(ev.PackageKey, ev.OccurredAt, kk)
				if len(got) > 0 && !contains(got, want) {
					d.Disposition = DispositionContainerMismatch
					d.Reason = fmt.Sprintf("event cites container %s but package %s is in %v at event time",
						want, ev.PackageKey, got)
				}
			}
		}
		return d
	}
	// The package is not active for this shipment at the arrival knowledge.
	if _, everMember := g.PackageWasMember(ev.PackageKey); everMember {
		d.Disposition = DispositionSupersededPackage
		if eff, known, ok := g.RetirementOf(ev.PackageKey); ok && !known.After(kk) {
			_ = eff
			d.Reason = "event arrived after the package was retired by a split/merge; evidence is misaligned and is not attributed to any child"
		} else {
			d.Reason = "package is no longer an active version of the shipment; evidence is retained but not transferred to any successor"
		}
		return d
	}
	d.Disposition = DispositionUnknownPackage
	d.Reason = "package was never an active version of this shipment"
	return d
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
