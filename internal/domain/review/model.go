// Package review implements historical state-consistency review. It asks
// whether the shipment, container and leg statuses seen in one query can all
// hold simultaneously under one set of history facts, and it separates
// provable contradictions from cases where the records simply do not decide.
//
// Three ideas keep the review honest:
//
//   - Occurrence time and receipt time are different clocks. A record proves
//     what some system had when it ARRIVED; it says nothing about when what it
//     describes happened, beyond the source's declared clock interval.
//   - Numeric sequence numbers only order records inside the same source and
//     the same boot segment. Numbers are never compared across a restart, and
//     never used across sources.
//   - A status that was visible at one instant must be reproduced from the
//     version that was actually effective then, with all of its prerequisites.
//     Later snapshots cannot be substituted, and a shipment's membership in one
//     snapshot cannot be recombined with another snapshot's verdict counts.
//
// The package is pure: every check is a constraint query over imported records,
// so a review can be recomputed from its pinned material without the live
// tables.
package review

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---- records ---------------------------------------------------------------

// Record kinds. Every imported row is one of these.
const (
	KindEvent       = "event"       // business/log record from an operational source
	KindFacts       = "facts"       // single-system occurrence-order bootstrap
	KindPublication = "publication" // platform snapshot publication log entry
	KindRead        = "read"        // a consumer system's actual read log entry
)

// SystemPlatform identifies the publishing platform in records and scopes.
const SystemPlatform = "platform"

// TimeBound is the known interval for an instant. Earliest == Latest means the
// source reported an exact instant. Basis records how the bounds were derived
// ("declared", "synchronizer_offset", "receipt_exact", "occurrence_order_only").
type TimeBound struct {
	Earliest time.Time `json:"earliest"`
	Latest   time.Time `json:"latest"`
	Basis    string    `json:"basis"`
}

// Known reports whether the bound pins an exact instant.
func (b TimeBound) Known() bool { return b.Earliest.Equal(b.Latest) }

// Validate rejects inverted or missing bounds.
func (b TimeBound) Validate() bool {
	if b.Earliest.IsZero() || b.Latest.IsZero() {
		return false
	}
	return !b.Latest.Before(b.Earliest)
}

// SubjectRef identifies a shipment, container or leg.
type SubjectRef struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

func (s SubjectRef) Empty() bool { return s.Type == "" || s.Key == "" }

// MembershipClaim is a versioned relationship assertion: the subjects were
// composed of the objects under linkKind at the claiming record's instant.
type MembershipClaim struct {
	LinkKind string   `json:"link_kind"` // shipment_package|package_container|container_leg
	Subjects []string `json:"subjects"`
	Objects  []string `json:"objects"`
}

// Record is one imported log record in canonical form.
type Record struct {
	RecordID string `json:"record_id"`
	System   string `json:"system"`
	Stream   string `json:"stream"`
	Kind     string `json:"kind"`

	// SourceSequence is the raw number as recorded. ValidSequence is false when
	// the number cannot be used for ordering (missing, non-numeric, or reused
	// inside one boot segment). BootSegment disambiguates a restart; two
	// records are only sequence-comparable when system, stream and boot segment
	// all match.
	SourceSequence string `json:"source_sequence,omitempty"`
	ValidSequence  bool   `json:"sequence_valid"`
	BootSegment    string `json:"boot_segment,omitempty"`
	SeqConflict    string `json:"sequence_conflict,omitempty"` // set when a number was reused

	// Occurred is the occurrence-time interval as proven by the source clock.
	Occurred TimeBound `json:"occurred"`
	// Received is the platform receipt instant. Nil for records that were not
	// directly observed by the review (usually external consumer logs).
	Received *TimeBound `json:"received,omitempty"`

	Subject    *SubjectRef      `json:"subject,omitempty"`
	Membership *MembershipClaim `json:"membership,omitempty"`
	Evidence   []string         `json:"evidence_indexes,omitempty"`

	// Publication facts.
	SnapshotID     string    `json:"snapshot_id,omitempty"`
	Version        int       `json:"version,omitempty"`
	SnapshotKind   string    `json:"snapshot_kind,omitempty"`
	Status         string    `json:"status,omitempty"`
	SecurityResult string    `json:"security_result,omitempty"`
	Confirmed      *bool     `json:"consumable_confirmed,omitempty"`
	PublishedAt    time.Time `json:"published_at,omitempty"`

	// Read facts.
	ReferencedSnapshot string            `json:"referenced_snapshot_id,omitempty"`
	Observed           map[string]string `json:"observed,omitempty"`

	// InputRefs pin the records a publication was computed from (event ids from
	// the same import). They are causal prerequisites: a publication may not
	// precede them.
	InputRefs  []string `json:"input_event_refs,omitempty"`
	Supersedes string   `json:"supersedes_snapshot_id,omitempty"`

	// Classification marks evidence events used by the target dossier.
	Classification string `json:"classification,omitempty"`
	// Tags are stable labels used to name dossiers and conflicts.
	Tags   []string `json:"tags,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// ---- review request --------------------------------------------------------

// Claim is one state stated by the query (a screenshot's content, or a field a
// reviewer wants tested). Several claims in one target are tested both
// individually and as one combination.
type Claim struct {
	Level      string     `json:"level"` // shipment|container|leg
	Key        string     `json:"key"`
	Status     string     `json:"status"`
	Consumable *bool      `json:"consumable_confirmed,omitempty"`
	At         *TimeBound `json:"at,omitempty"` // overrides the target instant
	Source     string     `json:"source,omitempty"`
	// Counts pins numeric fields a screenshot shows (for example
	// "confirmed_packages": "3"). They must come from the SAME published
	// version as the status: a membership list from one snapshot cannot be
	// combined with a confirmation count from another.
	Counts map[string]string `json:"counts,omitempty"`
	// Membership states a relationship claim (package in which container at
	// the instant). Historical attribution follows the reference in force at
	// the instant; the current container never substitutes for the old one.
	Membership *MembershipClaim `json:"membership,omitempty"`
	// RequiresImage marks content that can only be confirmed from the original
	// image. The review will not claim it verified from metadata alone.
	RequiresImage bool `json:"requires_image,omitempty"`
}

// Target is the fixed subject of a review. The request pins the shipment, the
// relationship levels, the instant or reads to explain, the knowledge cutoff,
// and the fields the caller is allowed to see. A review never widens these.
type Target struct {
	CaseID      string   `json:"case_id"`
	ShipmentKey string   `json:"shipment_key"`
	Levels      []string `json:"levels"` // shipment|container|leg
	// AuthorizedSubjects lists extra cargo the scope lets this review read
	// (for example a second shipment being compared). Unlisted cargo stays
	// boundary-only. The service fills this from the scope's registry.
	AuthorizedSubjects []SubjectRef `json:"authorized_subjects,omitempty"`
	Instant            *TimeBound   `json:"instant,omitempty"`
	ReadRecordIDs      []string     `json:"read_record_ids,omitempty"`
	Claims             []Claim      `json:"claims,omitempty"`
	Cutoff             time.Time    `json:"cutoff_received_at"`
	AllowedFields      []string     `json:"allowed_fields,omitempty"`
	IncludeReadLogs    bool         `json:"include_read_logs"`
}

// DefaultAllowedFields is the projection used when a caller does not restrict
// fields: enough to see the claim and the evidence, never unrelated cargo.
func DefaultAllowedFields() []string {
	return []string{
		"record_id", "system", "kind", "subject", "status", "sequence",
		"occurred", "received", "classification", "detail",
	}
}

// EventDraft is one incoming raw event record for import.
type EventDraft struct {
	Ref            string            `json:"ref"` // caller handle, used by publication inputs
	System         string            `json:"system"`
	Stream         string            `json:"stream,omitempty"`
	Kind           string            `json:"kind"` // defaults to event
	Subject        *SubjectRef       `json:"subject,omitempty"`
	Membership     *MembershipClaim  `json:"membership,omitempty"`
	Evidence       []string          `json:"evidence_indexes,omitempty"`
	Sequence       string            `json:"source_sequence,omitempty"`
	BootSegment    string            `json:"boot_segment,omitempty"`
	Occurred       *TimeBound        `json:"occurred,omitempty"`
	Received       *TimeBound        `json:"received,omitempty"`
	Classification string            `json:"classification,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	Detail         string            `json:"detail,omitempty"`
	Extra          map[string]string `json:"extra,omitempty"`
}

// PublicationDraft is one snapshot publication log entry. Publication times
// are the platform's own clock; they are evidence only of what was published,
// not of what the referenced facts' sources claimed happened.
type PublicationDraft struct {
	Ref            string            `json:"ref"`
	SnapshotID     string            `json:"snapshot_id"`
	Subject        SubjectRef        `json:"subject"`
	Version        int               `json:"version"`
	Kind           string            `json:"snapshot_kind"` // provisional|formal
	Status         string            `json:"status"`
	SecurityResult string            `json:"security_result,omitempty"`
	Confirmed      *bool             `json:"consumable_confirmed,omitempty"`
	PublishedAt    time.Time         `json:"published_at"`
	Sequence       string            `json:"source_sequence,omitempty"`
	BootSegment    string            `json:"boot_segment,omitempty"`
	InputRefs      []string          `json:"input_event_refs,omitempty"`
	Supersedes     string            `json:"supersedes_snapshot_id,omitempty"`
	Counts         map[string]string `json:"counts,omitempty"`
	Membership     *MembershipClaim  `json:"membership,omitempty"`
}

// ReadDraft is one consumer read log entry.
type ReadDraft struct {
	Ref                string            `json:"ref"`
	System             string            `json:"system"`
	Stream             string            `json:"stream,omitempty"`
	Sequence           string            `json:"source_sequence,omitempty"`
	BootSegment        string            `json:"boot_segment,omitempty"`
	Occurred           *TimeBound        `json:"occurred,omitempty"`
	Received           *TimeBound        `json:"received,omitempty"`
	ReferencedSnapshot string            `json:"referenced_snapshot_id,omitempty"`
	Observed           map[string]string `json:"observed,omitempty"`
	Tags               []string          `json:"tags,omitempty"`
	Detail             string            `json:"detail,omitempty"`
}

// FactsDraft is one single-system facts batch: order of arrival is its
// sequence. It pins a zero-point per equipment clock. It is not treated as a
// business record and is never promoted into an assertion by itself.
type FactsDraft struct {
	Ref         string     `json:"ref"`
	System      string     `json:"system"`
	Stream      string     `json:"stream,omitempty"`
	BootSegment string     `json:"boot_segment,omitempty"`
	Sequence    string     `json:"source_sequence,omitempty"`
	Occurred    *TimeBound `json:"occurred,omitempty"`
	Received    *TimeBound `json:"received,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

// ScopeRequest registers what material the review is allowed to use.
type ScopeRequest struct {
	ScopeID            string       `json:"scope_id,omitempty"`
	Label              string       `json:"label,omitempty"`
	ShipmentKey        string       `json:"shipment_key"`
	IncludedSystems    []string     `json:"included_systems"`
	IncludedStreams    []string     `json:"included_streams,omitempty"`
	AuthorizedSubjects []SubjectRef `json:"authorized_subjects,omitempty"`
	ExcludedSystems    []string     `json:"excluded_systems,omitempty"`
	Cutoff             time.Time    `json:"cutoff_received_at"`
	Note               string       `json:"note,omitempty"`
}

// LogImport bundles every record a caller wants reviewed.
type LogImport struct {
	ScopeID      string             `json:"scope_id"`
	Events       []EventDraft       `json:"events"`
	Facts        []FactsDraft       `json:"facts"`
	Publications []PublicationDraft `json:"publications"`
	Reads        []ReadDraft        `json:"reads"`
	Cutoff       *time.Time         `json:"cutoff_received_at,omitempty"`
}

// ---- analysis output -------------------------------------------------------

// Verdicts. The first three are the answer to "can these claims hold together";
// the fourth is reported for claims that were checked and never contradicted.
const (
	VerdictContradicted = "contradicted" // a conflict is proven: no common order satisfies the material
	VerdictBothExplain  = "both_explain" // a compatible and an incompatible reading both survive
	VerdictUndetermined = "undetermined" // the material cannot decide either way yet
	VerdictConsistent   = "consistent"   // every checked arrangement holds; not a proof of the real world
)

// Verdict ranks: contradicted dominates both_explain dominates undetermined
// dominates consistent.
var verdictRank = map[string]int{
	VerdictConsistent: 0, VerdictUndetermined: 1, VerdictBothExplain: 2, VerdictContradicted: 3,
}

// Assertion kinds.
const (
	AssertionSubjectExists = "subject_exists"
	AssertionNoMembership  = "no_membership"
	AssertionMembership    = "membership_at_instant"
	AssertionStatus        = "status_at_instant"
	AssertionConsumable    = "consumable_at_instant"
	AssertionEvidence      = "evidence_published"
	AssertionRead          = "read_outcome"
	AssertionClaim         = "claim_status"
	// AssertionBoundary marks a claim about cargo the scope cannot disclose.
	// It is reported as undetermined (not silently dropped and not fabricated).
	AssertionBoundary = "boundary_subject"
)

// Assertion is one claim the analysis tests before a branch is posed. A status
// assertion means "some published version is effective at instant I, and none
// of them states a different status than the required one". Membership and
// subject assertions are positive-only: they describe the object a claim is
// about, so they are asserted, not alternated.
type Assertion struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"`
	Origin     string           `json:"origin"` // requirement|claim|candidate|boundary
	Subject    *SubjectRef      `json:"subject,omitempty"`
	Membership *MembershipClaim `json:"membership,omitempty"`
	// Counts pins numeric fields that must come from the same version as the
	// status (e.g. confirmed package count). A membership list from one
	// snapshot cannot be paired with a count from another.
	Counts map[string]string `json:"counts,omitempty"`
	// RequiresImage marks content only confirmable from the original image.
	RequiresImage bool              `json:"requires_image,omitempty"`
	BoundaryNote  string            `json:"boundary_note,omitempty"`
	Status        string            `json:"status,omitempty"`
	Evidence      string            `json:"evidence_index,omitempty"`
	ReadRecord    string            `json:"read_record_id,omitempty"`
	Observed      map[string]string `json:"observed,omitempty"`
	At            *TimeBound        `json:"at,omitempty"`
	Description   string            `json:"description,omitempty"`
	// GuardRecord is the review request's cutoff-bearing guardian (the case's
	// knowledge cutoff); its instant appears in every publication-backing set
	// so a conclusion cannot silently depend on a later-setting record.
	GuardRecord string `json:"guard_record_id,omitempty"`
}

// Concrete constraint ids are prefixed so a status branch's positive,
// negative and impossible shapes are distinguishable in a conflict report.

const (
	PrefixSubject   = "subj:"
	PrefixStatus    = "stat:"
	PrefixConsum    = "cons:"
	PrefixClaim     = "claim:"
	PrefixReq       = "req:"
	PrefixRead      = "read:"
	PrefixScreen    = "screen:"
	PrefixBoot      = "boot:"
	PrefixCutoff    = "cutoff:"
	PrefixWindow    = "win:"
	PrefixReceived  = "recv:"
	PrefixSeq       = "seq:"
	PrefixCausal    = "causal:"
	PrefixEffective = "eff:"
	PrefixScope     = "scope:"
)

// Constraint is one difference constraint in the form
//
//	t[From] - t[To] <= MaxGap
//
// The whole timeline question is reduced to a system of these, solved with
// Bellman-Ford. Every constraint keeps the ids of the original records that
// produced it, so an unsatisfiable system can be reported as a set of records
// and conditions rather than an opaque "no".
type Constraint struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"` // window|received|sequence|causal|effective|distinct|scope
	From      string   `json:"from"`
	To        string   `json:"to"`
	MaxGapNs  int64    `json:"max_gap_ns"`
	Reason    string   `json:"reason"`
	RecordIDs []string `json:"record_ids"`
	Guard     []string `json:"guard_record_ids,omitempty"`
}

// Node naming.
const zeroNode = "$Z"

// RecVar is the occurrence instant variable of a record.
func RecVar(recordID string) string { return "rec:" + recordID }

// EffVar is the effective-resolution instant of a publication version at one
// scope instant: when this version became the reading a query at that instant
// resolves to. It is what makes "the last effective version" testable with
// difference constraints.
func EffVar(recordID, scopeKey string, at int64) string {
	return fmt.Sprintf("eff:%s|%s|%d", recordID, scopeKey, at)
}

// BackVar is the branch variable for a status assertion: the record that backs
// the assertion, or the sentinel for "some version says otherwise".
func BackVar(assertionID, recordID string) string { return "back:" + assertionID + ":" + recordID }

// RepVar is the version a claim is read to reproduce: exactly one publication
// rep per (assertion, scope instant).
func RepVar(assertionID, scopeKey string, at int64) string {
	return fmt.Sprintf("rep:%s|%s|%d", assertionID, scopeKey, at)
}

// CandidateCoverage explains which record backs an assertion and why a record
// that mentions the same fact does not count (unauthorized write, non-effective
// version, cutoff-excluded, or a read that could have seen a different version).
type CandidateCoverage struct {
	RecordID string `json:"record_id"`
	Counted  bool   `json:"counted"`
	Reason   string `json:"reason"`
}

// EvidenceRef points at the material behind a conclusion.
type EvidenceRef struct {
	RecordID string `json:"record_id"`
	Role     string `json:"role"` // assertion|fact|constraint|publication|read
	Detail   string `json:"detail,omitempty"`
}

// ConflictSet is a set of original records and conditions that can no longer
// hold together. It is explicitly not claimed to be minimal or to name a single
// root cause.
type ConflictSet struct {
	Records    []string `json:"record_ids"`
	Conditions []string `json:"conditions"`
	Message    string   `json:"message"`
}

// OrderExample is one concrete arrangement supporting (or refusing) a claim.
type OrderExample struct {
	Kind     string            `json:"kind"`     // compatible|incompatible
	Times    map[string]int64  `json:"times_ns"` // record id -> resolved occurrence (unix ns)
	Sequence []string          `json:"sequence"`
	Readings map[string]string `json:"readings,omitempty"`
	Note     string            `json:"note,omitempty"`
}

// AssertionResult is the per-claim outcome.
type AssertionResult struct {
	AssertionID string              `json:"assertion_id"`
	Kind        string              `json:"kind"`
	Origin      string              `json:"origin"`
	Description string              `json:"description,omitempty"`
	Verdict     string              `json:"verdict"`
	Coverage    []CandidateCoverage `json:"candidate_coverage,omitempty"`
	Positive    *ExampleRef         `json:"positive_example,omitempty"`
	Negative    *ExampleRef         `json:"negative_example,omitempty"`
	Conflict    *ConflictSet        `json:"conflict,omitempty"`
	Unresolved  []string            `json:"unresolved,omitempty"`
	// AlternativeShapes and ShapesChecked record how many constraint shapes the
	// targeted solver actually posed, so a budget stop is visible: fewer
	// shapes checked than alternatives means the answer is not exhaustive.
	AlternativeShapes int `json:"alternative_shapes,omitempty"`
	ShapesChecked     int `json:"shapes_checked,omitempty"`
}

// ExampleRef points at an order example stored with the run.
type ExampleRef struct {
	ExampleID string `json:"example_id"`
	Kind      string `json:"kind"`
	Summary   string `json:"summary"`
}

// Budget records how much of the allowed search was consumed.
type Budget struct {
	Limit         int  `json:"limit"`
	Used          int  `json:"used"`
	Exhausted     bool `json:"exhausted"`
	SkippedChecks int  `json:"skipped_checks"`
}

// Summary is the case-level answer.
type Summary struct {
	Verdict          string   `json:"verdict"`
	ConsistentChecks int      `json:"consistent_checks"`
	Contradicted     int      `json:"contradicted"`
	BothExplain      int      `json:"both_explain"`
	Undetermined     int      `json:"undetermined"`
	BudgetExhausted  bool     `json:"budget_exhausted"`
	NotProven        string   `json:"no_contradiction_is_not_proof"`
	Findings         []string `json:"findings"`
}

// Analysis is the full targeted-solver result for one case at one cutoff.
type Analysis struct {
	CaseID        string            `json:"case_id"`
	ShipmentKey   string            `json:"shipment_key"`
	Cutoff        time.Time         `json:"cutoff_received_at"`
	Levels        []string          `json:"levels"`
	TargetInstant *TimeBound        `json:"target_instant,omitempty"`
	Summary       Summary           `json:"summary"`
	Assertions    []AssertionResult `json:"assertions"`
	Constraints   []Constraint      `json:"constraints"`
	Unresolved    []string          `json:"unresolved"`
	Examples      []OrderExample    `json:"examples,omitempty"`
	Budget        Budget            `json:"budget"`
	Scope         ScopeRegistry     `json:"scope"`
	Excluded      []ScopeDecision   `json:"excluded_records,omitempty"`
	Material      MaterialManifest  `json:"material"`
	GeneratedAt   time.Time         `json:"generated_at"`
}

// Example is a stored order example retrievable by id.
type Example struct {
	ID      string       `json:"id"`
	CaseID  string       `json:"case_id"`
	Example OrderExample `json:"example"`
}

// ScopeRegistry is the recorded authority for a review: which systems and
// streams were imported, which subjects the reviewer may see, and the cutoff.
type ScopeRegistry struct {
	ScopeID            string       `json:"scope_id"`
	Label              string       `json:"label,omitempty"`
	ShipmentKey        string       `json:"shipment_key"`
	IncludedSystems    []string     `json:"included_systems"`
	IncludedStreams    []string     `json:"included_streams,omitempty"`
	AuthorizedSubjects []SubjectRef `json:"authorized_subjects"`
	ExcludedSystems    []string     `json:"excluded_systems,omitempty"`
	Cutoff             time.Time    `json:"cutoff_received_at"`
	Note               string       `json:"note,omitempty"`
	RegisteredAt       time.Time    `json:"registered_at"`
}

// ScopeDecision reports how one record was treated by the scope.
type ScopeDecision struct {
	RecordID string `json:"record_id"`
	Reason   string `json:"reason"`
}

// MaterialManifest pins exactly which records an analysis read, so an old
// review can be reproduced after the live material has moved on.
type MaterialManifest struct {
	RecordIDs []string `json:"record_ids"`
	Count     int      `json:"count"`
	Hash      string   `json:"hash"`
}

// ExportView is the field-projected, restricted view of an analysis result.
// The internal verdict is recomputed from the same material; projection only
// hides fields and boundary records, never flips a conclusion.
type ExportView struct {
	CaseID          string               `json:"case_id"`
	Scope           ScopeRegistry        `json:"scope"`
	AllowedFields   []string             `json:"allowed_fields"`
	Verdict         string               `json:"verdict"`
	Assertions      []AssertionResult    `json:"assertions"`
	BoundaryRecords []RestrictedMaterial `json:"boundary_records"`
	Redactions      []string             `json:"redactions"`
	Integrity       string               `json:"projection_integrity"`
	Note            string               `json:"note"`
}

// ---- helpers ---------------------------------------------------------------

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// HigherVerdict returns the more decisive of two verdicts.
func HigherVerdict(a, b string) string {
	if verdictRank[a] >= verdictRank[b] {
		return a
	}
	return b
}

// statusInstant resolves the instant a claim is asked about: the assertion's
// own override first, then the target's fixed instant.
func statusInstant(a Assertion, target Target) (TimeBound, bool) {
	if a.At != nil && a.At.Validate() {
		return *a.At, true
	}
	if target.Instant != nil && target.Instant.Validate() {
		return *target.Instant, true
	}
	return TimeBound{}, false
}

// statusMatches reports whether a published version states the status an
// assertion claims, including the consumable flag when one is observed.
func statusMatches(v *Record, a Assertion) bool {
	if a.Kind == AssertionConsumable {
		if v.Confirmed == nil {
			return false
		}
		want := a.Observed != nil && a.Observed["consumable_confirmed"] == "true"
		return *v.Confirmed == want
	}
	if lower(v.Status) != lower(a.Status) {
		return false
	}
	if a.Observed != nil {
		if cs, ok := a.Observed["consumable_confirmed"]; ok {
			if v.Confirmed == nil || boolString(*v.Confirmed) != cs {
				return false
			}
		}
	}
	// A count a screenshot shows must come from the same published version that
	// states the status; different snapshots' numbers are never stitched.
	for k, want := range a.Counts {
		got, ok := observedValue(v, k)
		if !ok || got != want {
			return false
		}
	}
	// If the claim also states a membership, the version must carry the same
	// membership; a status from one snapshot cannot be paired with a membership
	// list from another.
	if a.Membership != nil {
		if v.Membership == nil || !membershipEqual(*v.Membership, *a.Membership) {
			return false
		}
	}
	return true
}

// observedValue reads a publication's recorded numeric field.
func observedValue(v *Record, key string) (string, bool) {
	if v.Observed == nil {
		return "", false
	}
	s, ok := v.Observed[key]
	return s, ok && s != ""
}

func membershipEqual(a, b MembershipClaim) bool {
	if a.LinkKind != b.LinkKind {
		return false
	}
	return sameSet(a.Subjects, b.Subjects) && sameSet(a.Objects, b.Objects)
}

func sameSet(a, b []string) bool {
	x, y := uniqueSorted(a), uniqueSorted(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
