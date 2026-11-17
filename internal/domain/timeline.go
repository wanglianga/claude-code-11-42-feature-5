package domain

import (
	"fmt"
	"sort"
	"time"
)

// TimelineEntry is one event placed on the reconciled timeline with an
// explanation of how its position was determined.
type TimelineEntry struct {
	EventID       string    `json:"event_id"`
	Source        string    `json:"source"`
	MessageType   string    `json:"message_type"`
	SourceCode    string    `json:"source_code,omitempty"`
	PackageKey    string    `json:"package_key,omitempty"`
	Stage         int       `json:"stage"`
	StageName     string    `json:"stage_name"`
	OccurredAt    time.Time `json:"occurred_at"`
	ReceivedAt    time.Time `json:"received_at"`
	SortKey       string    `json:"sort_key"`
	SequenceBasis string    `json:"sequence_basis"`
	ClockDriftMs  int64     `json:"clock_drift_ms"`
	SequenceNo    string    `json:"sequence_no,omitempty"`
	Revision      string    `json:"package_revision,omitempty"`
	EvidenceCount int       `json:"evidence_count"`
	Explanation   string    `json:"explanation"`
	Anomalies     []string  `json:"anomalies,omitempty"`
}

// Timeline builds the merged, explained event order for one shipment.
//
// Ordering is (stage, occurred_at, device sequence, received_at, seq). The
// stage component encodes the business handover order (declared -> screened ->
// loaded -> manifested) so a device whose clock is wrong cannot make screening
// appear to happen after loading. The receipt order breaks ties only after the
// business order, which is what lets a late backfill land in the right place in
// history without rewriting past entries — entries are re-derived, never
// mutated.
func BuildTimeline(events []*Event, disp map[string]Disposition, now time.Time) []TimelineEntry {
	entries := make([]TimelineEntry, 0, len(events))
	for _, ev := range events {
		stage := ev.Stage()
		e := TimelineEntry{
			EventID: ev.EventID, Source: ev.Source, MessageType: ev.MessageType,
			SourceCode: ev.SourceCode, PackageKey: ev.PackageKey,
			Stage: stage, StageName: StageName(stage),
			OccurredAt: ev.OccurredAt, ReceivedAt: ev.ReceivedAt,
			SequenceNo: ev.DeviceEventNo, Revision: ev.PackageRevision,
			EvidenceCount: len(ev.EvidenceIndexes),
		}
		drift := ev.ReceivedAt.Sub(ev.OccurredAt)
		if drift < 0 {
			drift = -drift
		}
		e.ClockDriftMs = drift.Milliseconds()
		if ev.DeviceEventNo != "" {
			e.SequenceBasis = "device_event_no"
		} else {
			e.SequenceBasis = "occurred_at"
		}
		if d, ok := disp[ev.EventID]; ok && !d.EvidenceEligible() {
			e.Anomalies = append(e.Anomalies, d.Disposition)
			e.Explanation = fmt.Sprintf("event not usable as evidence: %s", d.Reason)
		} else {
			e.Explanation = fmt.Sprintf("%s at %s from %s (arrived %s)",
				StageName(stage), ev.OccurredAt.Format(time.RFC3339), ev.Source,
				ev.ReceivedAt.Format(time.RFC3339))
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Before(b.OccurredAt)
		}
		if a.SequenceNo != b.SequenceNo {
			return a.SequenceNo < b.SequenceNo
		}
		if !a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ReceivedAt.Before(b.ReceivedAt)
		}
		return a.EventID < b.EventID
	})
	for i := range entries {
		entries[i].SortKey = fmt.Sprintf("%03d|%s|%s", entries[i].Stage,
			entries[i].OccurredAt.Format("20060102T150405.000000000Z"), entries[i].SequenceNo)
	}
	return entries
}
