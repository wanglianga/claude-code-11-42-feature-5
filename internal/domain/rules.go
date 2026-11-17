package domain

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Rule codes. Each maps to a human explanation and a severity that feeds the
// data status.
const (
	RuleDuplicateRetransmission = "duplicate_retransmission"
	RuleCrossPackageReference   = "cross_package_reference"
	RuleMissingHandover         = "missing_handover"
	RuleDeviceClockDrift        = "device_clock_drift"
	RuleStatusRegression        = "status_regression"
	RuleInconsistentEncoding    = "inconsistent_source_encoding"
)

// Severities.
const (
	SevInfo    = "info"
	SevWarning = "warning"
	SevError   = "error"
	SevBlocker = "blocker"
)

// RuleViolation is one finding, keyed for idempotent append.
type RuleViolation struct {
	Code          string         `json:"code"`
	Severity      string         `json:"severity"`
	SubjectType   string         `json:"subject_type"`
	SubjectKey    string         `json:"subject_key"`
	DedupeKey     string         `json:"dedupe_key"`
	KnowledgeAt   time.Time      `json:"knowledge_at"`
	Detail        map[string]any `json:"detail"`
	RelatedEvents []string       `json:"related_event_ids"`
	FirstSeenAt   time.Time      `json:"first_detected_at"`
}

// Blocking reports whether the finding prevents a confirmed publication.
func (r RuleViolation) Blocking() bool {
	return r.Severity == SevError || r.Severity == SevBlocker
}

// AttemptInfo is the intake audit fact a duplicate rule needs.
type AttemptInfo struct {
	Source         string
	MessageType    string
	IdempotencyKey string
	Outcome        string
	ContentHash    string
	OccurredAt     *time.Time
	ReceivedAt     time.Time
	OriginalEvent  string
	Message        string
}

// RuleConfig tunes thresholds.
type RuleConfig struct {
	ClockDriftThreshold time.Duration
	ConflictWindow      time.Duration
}

// DefaultRuleConfig is the platform default.
func DefaultRuleConfig() RuleConfig {
	return RuleConfig{ClockDriftThreshold: 15 * time.Minute, ConflictWindow: 2 * time.Hour}
}

// RuleInput is everything the pure rule engine may inspect.
type RuleInput struct {
	ShipmentKey  string
	Events       []*Event
	Timeline     []TimelineEntry
	Dispositions map[string]Disposition
	Attempts     []AttemptInfo
	Now          time.Time
	Config       RuleConfig
}

// EvaluateRules runs all six rules for one shipment. Findings are returned in
// a stable order with deterministic dedupe keys so re-running at a later
// watermark appends only genuinely new findings.
func EvaluateRules(in RuleInput) []RuleViolation {
	var out []RuleViolation
	out = append(out, ruleDuplicates(in)...)
	out = append(out, ruleCrossPackage(in)...)
	out = append(out, ruleMissingHandover(in)...)
	out = append(out, ruleClockDrift(in)...)
	out = append(out, ruleStatusRegression(in)...)
	out = append(out, ruleEncoding(in)...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].DedupeKey < out[j].DedupeKey
	})
	return out
}

func mkRule(code, severity, subjectType, subjectKey, dedupe string, at time.Time,
	detail map[string]any, related []string) RuleViolation {
	if related == nil {
		related = []string{}
	}
	return RuleViolation{
		Code: code, Severity: severity, SubjectType: subjectType, SubjectKey: subjectKey,
		DedupeKey: dedupe, KnowledgeAt: at, Detail: detail, RelatedEvents: related,
	}
}

// 1. Duplicate retransmission. Same dedupe key with different content, or the
// same content under different keys, both mean the feed is re-sending.
func ruleDuplicates(in RuleInput) []RuleViolation {
	var out []RuleViolation
	for _, a := range in.Attempts {
		switch a.Outcome {
		case "duplicate_identical":
			out = append(out, mkRule(RuleDuplicateRetransmission, SevInfo, "shipment",
				in.ShipmentKey, "identical:"+a.Source+":"+a.IdempotencyKey, a.ReceivedAt,
				map[string]any{
					"message_type": a.MessageType, "source": a.Source,
					"idempotency_key": a.IdempotencyKey, "outcome": a.Outcome,
					"original_event_id": a.OriginalEvent,
					"explanation":       "byte-identical retransmission was accepted once and suppressed",
				}, []string{a.OriginalEvent}))
		case "duplicate_conflicting":
			out = append(out, mkRule(RuleDuplicateRetransmission, SevWarning, "shipment",
				in.ShipmentKey, "conflicting:"+a.Source+":"+a.IdempotencyKey, a.ReceivedAt,
				map[string]any{
					"message_type": a.MessageType, "source": a.Source,
					"idempotency_key": a.IdempotencyKey, "outcome": a.Outcome,
					"original_event_id": a.OriginalEvent, "reported": a.Message,
					"explanation": "same idempotency key arrived with different content; original kept, new content quarantined",
				}, []string{a.OriginalEvent}))
		}
	}
	// Content-level: identical fingerprint under different keys within the
	// same source (a scanner replaying with a fresh key).
	groups := map[string][]*Event{}
	for _, ev := range in.Events {
		groups[ev.Source+"|"+ev.ContentHash] = append(groups[ev.Source+"|"+ev.ContentHash], ev)
	}
	for k, evs := range groups {
		if len(evs) < 2 {
			continue
		}
		keys := map[string]bool{}
		for _, ev := range evs {
			keys[ev.DedupeKey] = true
		}
		if len(keys) < 2 {
			continue
		}
		var ids []string
		var last time.Time
		for _, ev := range evs {
			ids = append(ids, ev.EventID)
			if ev.ReceivedAt.After(last) {
				last = ev.ReceivedAt
			}
		}
		sort.Strings(ids)
		out = append(out, mkRule(RuleDuplicateRetransmission, SevWarning, "shipment",
			in.ShipmentKey, "content:"+k, last,
			map[string]any{
				"explanation": "same content arrived under different idempotency/device keys",
				"event_ids":   ids, "event_count": len(ids),
			}, ids))
	}
	return out
}

// 2. Cross-package reference: evidence whose package identity belongs to
// another shipment, or misaligned evidence after a split.
func ruleCrossPackage(in RuleInput) []RuleViolation {
	var out []RuleViolation
	for _, ev := range in.Events {
		d, ok := in.Dispositions[ev.EventID]
		if !ok {
			continue
		}
		switch d.Disposition {
		case DispositionSupersededPackage, DispositionUnknownPackage:
			out = append(out, mkRule(RuleCrossPackageReference, SevWarning, "shipment", in.ShipmentKey,
				"pkgref:"+ev.EventID, ev.ReceivedAt,
				map[string]any{
					"package_key": ev.PackageKey, "disposition": d.Disposition,
					"event_type": ev.MessageType, "source": ev.Source,
					"reason": d.Reason,
					"explanation": "event references a package that is not an active version; " +
						"evidence is retained but not attributed to any other package",
				}, []string{ev.EventID}))
		}
	}
	// Evidence index shared by different package lines is suspicious.
	byEvidence := map[string]map[string][]string{} // evidence -> package -> events
	for _, ev := range in.Events {
		for _, idx := range ev.EvidenceIndexes {
			if byEvidence[idx] == nil {
				byEvidence[idx] = map[string][]string{}
			}
			byEvidence[idx][ev.PackageKey] = append(byEvidence[idx][ev.PackageKey], ev.EventID)
		}
	}
	for idx, pkgs := range byEvidence {
		if len(pkgs) < 2 {
			continue
		}
		var ids []string
		var names []string
		for p, evs := range pkgs {
			names = append(names, p)
			ids = append(ids, evs...)
		}
		sort.Strings(names)
		sort.Strings(ids)
		out = append(out, mkRule(RuleCrossPackageReference, SevWarning, "shipment", in.ShipmentKey,
			"shared-evidence:"+idx, in.Now,
			map[string]any{
				"evidence_index": idx, "packages": names, "related_events": ids,
				"explanation": "one image/evidence index is referenced by events of multiple packages",
			}, ids))
	}
	return out
}

// 3. Missing handover: screening/loading happened without the operational
// handover that should precede it.
func ruleMissingHandover(in RuleInput) []RuleViolation {
	var out []RuleViolation
	// Index handovers both by package and by shipment: some sources hand over
	// the entire consignment at once with no package reference.
	byPackage := map[string][]*Event{}
	byShipment := map[string][]*Event{}
	for _, ev := range in.Events {
		if ev.MessageType != TypeHandover {
			continue
		}
		if ev.PackageKey != "" {
			byPackage[ev.PackageKey] = append(byPackage[ev.PackageKey], ev)
		}
		if ev.ShipmentKey != "" {
			byShipment[ev.ShipmentKey] = append(byShipment[ev.ShipmentKey], ev)
		}
	}
	for _, ev := range in.Events {
		var neededMode string
		switch ev.MessageType {
		case TypeScreening:
			neededMode = ModeScreening
		case TypeScan:
			if ev.Mode() == ModeLoading {
				neededMode = ModeLoading
			} else {
				continue
			}
		case TypeFlightManifest:
			// A manifest does not itself need a handover; the departure
			// handover gap is reported by the fleet-level check.
			continue
		default:
			continue
		}
		found := false
		candidates := append(append([]*Event{}, byPackage[ev.PackageKey]...), byShipment[in.ShipmentKey]...)
		for _, h := range candidates {
			if h.Mode() != neededMode {
				continue
			}
			if h.ReceivedAt.After(ev.ReceivedAt) {
				continue
			}
			if h.OccurredAt.After(ev.OccurredAt) && !h.OccurredAt.Equal(ev.OccurredAt) {
				continue
			}
			found = true
			break
		}
		if !found {
			out = append(out, mkRule(RuleMissingHandover, SevWarning, "shipment", in.ShipmentKey,
				fmt.Sprintf("handover:%s:%s", neededMode, ev.EventID), ev.ReceivedAt,
				map[string]any{
					"event_id": ev.EventID, "event_type": ev.MessageType,
					"package_key": ev.PackageKey, "required_handover_mode": neededMode,
					"explanation": "no handover record precedes this event; time order is inferred from device sequence and stage",
				}, []string{ev.EventID}))
		}
	}
	return out
}

// 4. Device clock drift: arrival far from occurrence, negative drift, or a
// device reporting into the future.
func ruleClockDrift(in RuleInput) []RuleViolation {
	var out []RuleViolation
	th := in.Config.ClockDriftThreshold
	if th <= 0 {
		th = 15 * time.Minute
	}
	for _, ev := range in.Events {
		if ev.DeviceID == "" && ev.MessageType != TypeScan && ev.MessageType != TypeScreening {
			continue
		}
		delta := ev.ReceivedAt.Sub(ev.OccurredAt)
		var detail map[string]any
		switch {
		case delta < -time.Minute:
			detail = map[string]any{
				"drift_ms": delta.Milliseconds(), "direction": "negative",
				"explanation": "event reports an occurrence after the time it was received (clock ahead)",
			}
		case delta > th && ev.OccurredAt.Before(in.Now.Add(-th)):
			detail = map[string]any{
				"drift_ms": delta.Milliseconds(), "direction": "late_arrival",
				"explanation": "event arrived long after it occurred (late backfill or clock behind)",
			}
		}
		if detail == nil {
			continue
		}
		detail["event_id"] = ev.EventID
		detail["device_id"] = ev.DeviceID
		detail["occurred_at"] = ev.OccurredAt
		detail["received_at"] = ev.ReceivedAt
		out = append(out, mkRule(RuleDeviceClockDrift, SevWarning, "shipment", in.ShipmentKey,
			"drift:"+ev.EventID, ev.ReceivedAt, detail, []string{ev.EventID}))
	}
	return out
}

// 5. Status regression: in receipt order, a package's workflow stage moves
// backwards without an explicit correction (supersedes reference).
func ruleStatusRegression(in RuleInput) []RuleViolation {
	var out []RuleViolation
	superseded := map[string]bool{}
	for _, ev := range in.Events {
		if ev.SupersedesEventID != "" {
			superseded[ev.SupersedesEventID] = true
		}
	}
	byPkg := map[string][]*Event{}
	for _, ev := range in.Events {
		if ev.PackageKey == "" {
			continue
		}
		byPkg[ev.PackageKey] = append(byPkg[ev.PackageKey], ev)
	}
	for pkg, evs := range byPkg {
		sort.SliceStable(evs, func(i, j int) bool {
			if evs[i].ReceivedAt.Equal(evs[j].ReceivedAt) {
				return evs[i].Seq < evs[j].Seq
			}
			return evs[i].ReceivedAt.Before(evs[j].ReceivedAt)
		})
		maxStage := 0
		var maxEvent *Event
		for _, ev := range evs {
			stage := ev.Stage()
			if stage < maxStage && !superseded[ev.EventID] && ev.MessageType != TypePackageChange {
				out = append(out, mkRule(RuleStatusRegression, SevWarning, "shipment", in.ShipmentKey,
					"regress:"+ev.EventID, ev.ReceivedAt,
					map[string]any{
						"package_key": pkg, "event_id": ev.EventID,
						"event_stage": StageName(stage), "previous_stage": StageName(maxStage),
						"previous_event_id": maxEvent.EventID,
						"explanation":       "a later-arriving event reports an earlier workflow stage without an explicit correction",
					}, []string{ev.EventID, maxEvent.EventID}))
			}
			if stage >= maxStage {
				maxStage = stage
				maxEvent = ev
			}
		}
	}
	return out
}

// 6. Inconsistent source encoding: the same logical key spelled differently
// across feeds, or non-canonical spelling.
func ruleEncoding(in RuleInput) []RuleViolation {
	var out []RuleViolation
	type rawUse struct {
		kind   string
		canon  string
		raw    string
		source string
		events []string
	}
	uses := map[string]*rawUse{}
	for _, ev := range in.Events {
		for kind, raw := range ev.RawBusinessKeys {
			if kind == KeyPackage {
				continue // package raw ids are already the canonical handle
			}
			canon := ev.BusinessKeys[kind]
			if canon == "" || raw == "" {
				continue
			}
			id := kind + "|" + canon + "|" + raw
			u, ok := uses[id]
			if !ok {
				u = &rawUse{kind: kind, canon: canon, raw: raw, source: ev.Source}
				uses[id] = u
			}
			u.events = append(u.events, ev.EventID)
		}
	}
	// Group by canonical key to see differing spellings.
	spellings := map[string]map[string]*rawUse{}
	for _, u := range uses {
		k := u.kind + "|" + u.canon
		if spellings[k] == nil {
			spellings[k] = map[string]*rawUse{}
		}
		spellings[k][u.raw] = u
	}
	for k, byRaw := range spellings {
		if len(byRaw) < 2 {
			continue
		}
		var raws, sources, ids []string
		for raw, u := range byRaw {
			raws = append(raws, raw)
			sources = append(sources, u.source)
			ids = append(ids, u.events...)
		}
		sort.Strings(raws)
		sort.Strings(sources)
		sort.Strings(ids)
		canon := ""
		for _, u := range byRaw {
			canon = u.canon
			break
		}
		out = append(out, mkRule(RuleInconsistentEncoding, SevWarning, "shipment", in.ShipmentKey,
			"encoding:"+k, in.Now,
			map[string]any{
				"canonical_key": canon, "spellings": raws, "sources": SortedUnique(sources),
				"explanation": "feeds encode the same business key differently; normalised for identity matching",
			}, SortedUnique(ids)))
	}
	return out
}

// EvidenceWeights defines the completeness model. The weights sum to 1.0.
type EvidenceWeights struct {
	Declaration   float64
	Handover      float64
	Screening     float64
	EvidenceIndex float64
	Review        float64
	Identity      float64
}

// DefaultEvidenceWeights is the shipped model.
func DefaultEvidenceWeights() EvidenceWeights {
	return EvidenceWeights{
		Declaration: 0.15, Handover: 0.15, Screening: 0.25,
		EvidenceIndex: 0.15, Review: 0.15, Identity: 0.15,
	}
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// PendingItem is a concrete "待补证" instruction for the operator.
type PendingItem struct {
	Code       string `json:"code"`
	Detail     string `json:"detail"`
	PackageKey string `json:"package_key,omitempty"`
	Severity   string `json:"severity"`
}
