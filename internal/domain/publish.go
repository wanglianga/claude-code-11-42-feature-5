package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Watermark modes.
const (
	// ModeWaitComplete publishes nothing until every required source has closed
	// and the handover condition holds. Downstream gets fewer provisional
	// values but later availability, and a late device message delays the whole
	// shipment.
	ModeWaitComplete = "wait_complete"
	// ModeEarlyProvisional publishes a provisional snapshot at the cutoff and a
	// formal snapshot later once sources close. Downstream can read early but
	// must distinguish provisional from formal.
	ModeEarlyProvisional = "early_provisional"
)

// Snapshot kinds.
const (
	SnapshotProvisional = "provisional"
	SnapshotFormal      = "formal"
)

// Watermark is the agreed closing line for a batch of shipments.
type Watermark struct {
	ID               string    `json:"id"`
	ScopeType        string    `json:"scope_type"` // shipment|container|leg|all
	ScopeKey         string    `json:"scope_key"`
	CutoffReceivedAt time.Time `json:"cutoff_received_at"`
	RequiredSources  []string  `json:"required_sources"`
	RequireHandover  bool      `json:"require_handover"`
	Mode             string    `json:"mode"`
	Label            string    `json:"label,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ClosedSources    []string  `json:"closed_sources,omitempty"`
	Complete         bool      `json:"complete"`
	MissingSources   []string  `json:"missing_sources,omitempty"`
}

// LineageEvent pins one input event by sequence and content hash.
type LineageEvent struct {
	Seq         int64     `json:"seq"`
	EventID     string    `json:"event_id"`
	Source      string    `json:"source"`
	MessageType string    `json:"message_type"`
	ReceivedAt  time.Time `json:"received_at"`
	ContentHash string    `json:"content_hash"`
}

// Lineage is the immutable input set a snapshot was computed from.
type Lineage struct {
	AssessmentID       string          `json:"assessment_id"`
	CutoffReceivedAt   time.Time       `json:"cutoff_received_at"`
	MaxSeq             int64           `json:"max_seq"`
	InputEvents        []LineageEvent  `json:"input_events"`
	InputGraphLinks    []GraphPin      `json:"input_graph_links"`
	InputRuleKeys      []string        `json:"input_rule_keys"`
	InputCorrections   []Correction    `json:"input_corrections"`
	Weights            EvidenceWeights `json:"evidence_weights"`
	CompletenessFloor  float64         `json:"completeness_floor"`
	PreviousSnapshotID string          `json:"previous_snapshot_id,omitempty"`
	ResultHash         string          `json:"result_hash"`
}

// GraphPin pins a graph relationship by key+version.
type GraphPin struct {
	LinkKey     string `json:"link_key"`
	LinkVersion int    `json:"link_version"`
	Kind        string `json:"kind"`
	SubjectKey  string `json:"subject_key"`
	ObjectKey   string `json:"object_key"`
}

// QualitySummary is the compact quality view carried on every snapshot.
type QualitySummary struct {
	EventCount        int            `json:"event_count"`
	Sources           []string       `json:"sources"`
	ViolationsByCode  map[string]int `json:"violations_by_code"`
	BlockingCodes     []string       `json:"blocking_codes"`
	Warnings          int            `json:"warning_count"`
	Info              int            `json:"info_count"`
	UnalignedEvidence int            `json:"unaligned_evidence_count"`
	MissingFields     int            `json:"missing_required_fields"`
}

// SnapshotDiff is the explicit difference between two versions of the same
// scope. Downstream reads it to know what changed instead of diffing payloads.
type SnapshotDiff struct {
	PreviousVersion   int      `json:"previous_version,omitempty"`
	StatusFrom        string   `json:"status_from,omitempty"`
	StatusTo          string   `json:"status_to"`
	ResultFrom        string   `json:"result_from,omitempty"`
	ResultTo          string   `json:"result_to,omitempty"`
	CompletenessFrom  float64  `json:"completeness_from"`
	CompletenessTo    float64  `json:"completeness_to"`
	PendingAdded      []string `json:"pending_added"`
	PendingResolved   []string `json:"pending_resolved"`
	ConflictsAdded    []string `json:"conflicts_added"`
	ConflictsResolved []string `json:"conflicts_resolved"`
	NewEventIDs       []string `json:"new_event_ids"`
	CorrectionsAdded  []string `json:"corrections_added"`
	ConfirmedChanged  bool     `json:"consumable_confirmed_changed"`
	Summary           string   `json:"summary"`
}

// Snapshot is a published versioned status dataset.
type Snapshot struct {
	ID                   string           `json:"id"`
	ScopeType            string           `json:"scope_type"`
	ScopeKey             string           `json:"scope_key"`
	Version              int              `json:"version"`
	Kind                 string           `json:"snapshot_kind"`
	WatermarkID          string           `json:"watermark_id"`
	AssessmentID         string           `json:"assessment_id"`
	CutoffReceivedAt     time.Time        `json:"cutoff_received_at"`
	Status               string           `json:"status"`
	SecurityResult       string           `json:"security_result,omitempty"`
	EvidenceCompleteness float64          `json:"evidence_completeness"`
	ConsumableConfirmed  bool             `json:"consumable_confirmed"`
	QualitySummary       QualitySummary   `json:"quality_summary"`
	Diff                 SnapshotDiff     `json:"diff"`
	Supersedes           string           `json:"supersedes_snapshot_id,omitempty"`
	PublishedAt          time.Time        `json:"published_at"`
	Payload              *SnapshotPayload `json:"dataset,omitempty"`
}

// SnapshotPayload is the full dataset stored with a snapshot. It is what an
// audit replay walks and what a downstream consumer renders.
type SnapshotPayload struct {
	Scope                  ScopeRef             `json:"scope"`
	Status                 string               `json:"status"`
	SecurityResult         string               `json:"security_result,omitempty"`
	ConsumableConfirmed    bool                 `json:"consumable_confirmed"`
	SnapshotKind           string               `json:"snapshot_kind"`
	Watermark              WatermarkRef         `json:"watermark"`
	EvidenceCompleteness   float64              `json:"evidence_completeness"`
	CompletenessComponents []Component          `json:"completeness_components"`
	Packages               []PackageState       `json:"packages"`
	PendingItems           []PendingItem        `json:"pending_items"`
	Conflicts              []Conflict           `json:"conflicts"`
	Corrections            []Correction         `json:"corrections"`
	CompositionVersions    []CompositionVersion `json:"composition_versions,omitempty"`
	Timeline               []TimelineEntry      `json:"timeline,omitempty"`
	ManifestRefs           []ManifestRef        `json:"manifest_references,omitempty"`
	Decision               string               `json:"decision_explanation"`
	BlockingReasons        []string             `json:"blocking_reasons,omitempty"`
	QualitySummary         QualitySummary       `json:"quality_summary"`
	Lineage                Lineage              `json:"lineage"`
	DerivedFrom            []SnapshotRef        `json:"derived_from"`
	Note                   string               `json:"note,omitempty"`
	Diff                   SnapshotDiff         `json:"diff_vs_previous,omitempty"`
}

// ScopeRef identifies a snapshot subject.
type ScopeRef struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// WatermarkRef is the watermark subset embedded in a payload.
type WatermarkRef struct {
	ID               string    `json:"id"`
	Mode             string    `json:"mode"`
	CutoffReceivedAt time.Time `json:"cutoff_received_at"`
	Complete         bool      `json:"source_complete"`
	Label            string    `json:"label,omitempty"`
}

// ManifestRef records a manifest referencing this shipment's status/evidence.
type ManifestRef struct {
	EventID          string   `json:"event_id"`
	Source           string   `json:"source"`
	ReferencedStatus string   `json:"referenced_status,omitempty"`
	RequiredEvidence []string `json:"required_evidence"`
	MissingEvidence  []string `json:"missing_evidence,omitempty"`
}

// SnapshotRef is a minimal reference used in lineage/aggregation.
type SnapshotRef struct {
	SnapshotID string `json:"snapshot_id"`
	ScopeType  string `json:"scope_type"`
	ScopeKey   string `json:"scope_key"`
	Version    int    `json:"version"`
	Kind       string `json:"snapshot_kind"`
	Status     string `json:"status"`
}

// HashResult computes the deterministic result hash over the fused result and
// the pinned input versions. Two replays of the same inputs must produce the
// same hash; a mismatch is surfaced as an audit finding.
func HashResult(res FusionResult, lineage Lineage) string {
	canon := struct {
		Status       string
		Result       string
		Completeness float64
		Packages     []PackageState
		Pending      []PendingItem
		Conflicts    []Conflict
		Corrections  []Correction
		Events       []LineageEvent
		Rules        []string
		Links        []GraphPin
	}{res.Status, res.SecurityResult, res.EvidenceCompleteness, res.Packages,
		res.PendingItems, res.Conflicts, res.Corrections, lineage.InputEvents,
		lineage.InputRuleKeys, lineage.InputGraphLinks}
	b, _ := json.Marshal(canon)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DiffSnapshots computes the explicit delta between two published versions.
func DiffSnapshots(prev *SnapshotPayload, prevVer int, next *SnapshotPayload) SnapshotDiff {
	d := SnapshotDiff{
		PendingAdded: []string{}, PendingResolved: []string{},
		ConflictsAdded: []string{}, ConflictsResolved: []string{},
		NewEventIDs: []string{}, CorrectionsAdded: []string{},
		StatusTo: next.Status, CompletenessTo: next.EvidenceCompleteness,
		ResultTo: next.SecurityResult,
	}
	if prev == nil {
		d.Summary = "first published version"
		for _, p := range next.PendingItems {
			d.PendingAdded = append(d.PendingAdded, p.Code)
		}
		for _, c := range next.Conflicts {
			d.ConflictsAdded = append(d.ConflictsAdded, conflictLabel(c))
		}
		d.NewEventIDs = lineageIDs(next.Lineage.InputEvents)
		d.Summary = "initial publication"
		return d
	}
	d.PreviousVersion = prevVer
	d.StatusFrom = prev.Status
	d.ResultFrom = prev.SecurityResult
	d.CompletenessFrom = prev.EvidenceCompleteness
	d.ConfirmedChanged = prev.ConsumableConfirmed != next.ConsumableConfirmed

	prevPending := map[string]bool{}
	for _, p := range prev.PendingItems {
		prevPending[p.Code+"|"+p.PackageKey] = true
	}
	nextPending := map[string]bool{}
	for _, p := range next.PendingItems {
		nextPending[p.Code+"|"+p.PackageKey] = true
		if !prevPending[p.Code+"|"+p.PackageKey] {
			d.PendingAdded = append(d.PendingAdded, p.Code)
		}
	}
	for k := range prevPending {
		if !nextPending[k] {
			d.PendingResolved = append(d.PendingResolved, k)
		}
	}
	prevConf := map[string]bool{}
	for _, c := range prev.Conflicts {
		prevConf[conflictLabel(c)] = true
	}
	nextConf := map[string]bool{}
	for _, c := range next.Conflicts {
		nextConf[conflictLabel(c)] = true
		if !prevConf[conflictLabel(c)] {
			d.ConflictsAdded = append(d.ConflictsAdded, conflictLabel(c))
		}
	}
	for k := range prevConf {
		if !nextConf[k] {
			d.ConflictsResolved = append(d.ConflictsResolved, k)
		}
	}
	prevEvents := map[string]bool{}
	for _, e := range prev.Lineage.InputEvents {
		prevEvents[e.EventID] = true
	}
	for _, e := range next.Lineage.InputEvents {
		if !prevEvents[e.EventID] {
			d.NewEventIDs = append(d.NewEventIDs, e.EventID)
		}
	}
	prevCorr := map[string]bool{}
	for _, c := range prev.Corrections {
		prevCorr[c.CorrectingEventID] = true
	}
	for _, c := range next.Corrections {
		if !prevCorr[c.CorrectingEventID] {
			d.CorrectionsAdded = append(d.CorrectionsAdded, c.CorrectingEventID)
		}
	}
	sort.Strings(d.PendingAdded)
	sort.Strings(d.PendingResolved)
	sort.Strings(d.ConflictsAdded)
	sort.Strings(d.ConflictsResolved)
	sort.Strings(d.NewEventIDs)
	sort.Strings(d.CorrectionsAdded)
	parts := []string{fmt.Sprintf("v%d->v%d", prevVer, prevVer+1)}
	if d.StatusFrom != d.StatusTo {
		parts = append(parts, "status "+d.StatusFrom+" -> "+d.StatusTo)
	}
	if d.ResultFrom != d.ResultTo {
		parts = append(parts, "result "+emptyDash(d.ResultFrom)+" -> "+emptyDash(d.ResultTo))
	}
	if len(d.PendingAdded) > 0 {
		parts = append(parts, fmt.Sprintf("%d pending added", len(d.PendingAdded)))
	}
	if len(d.PendingResolved) > 0 {
		parts = append(parts, fmt.Sprintf("%d pending resolved", len(d.PendingResolved)))
	}
	if len(d.ConflictsAdded) > 0 {
		parts = append(parts, fmt.Sprintf("%d conflicts added", len(d.ConflictsAdded)))
	}
	if len(d.ConflictsResolved) > 0 {
		parts = append(parts, fmt.Sprintf("%d conflicts resolved", len(d.ConflictsResolved)))
	}
	if len(d.NewEventIDs) > 0 {
		parts = append(parts, fmt.Sprintf("%d new inputs", len(d.NewEventIDs)))
	}
	d.Summary = joinComma(parts)
	return d
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func conflictLabel(c Conflict) string {
	return c.PackageKey + "@" + c.WindowStart.UTC().Format("20060102T150405Z")
}

func lineageIDs(in []LineageEvent) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.EventID)
	}
	sort.Strings(out)
	return out
}

// AggregateContainerStatus folds shipment statuses into a container status.
// The container is only "verified" when every shipment inside is verified; any
// conflict makes the container a ruling case. This is what prevents a
// downstream loader from reading a partially-confirmed ULD as confirmed.
func AggregateContainerStatus(statuses []string) string {
	if len(statuses) == 0 {
		return StatusNotPublishable
	}
	rank := map[string]int{
		StatusVerified: 0, StatusPendingEvidence: 1,
		StatusConflictPending: 2, StatusNotPublishable: 3,
	}
	worst := StatusVerified
	for _, s := range statuses {
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	if len(statuses) == 1 {
		return statuses[0]
	}
	return worst
}
