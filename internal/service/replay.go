package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/store"
)

// ReplayStep is one verified stage of the audit walk.
type ReplayStep struct {
	Name       string         `json:"name"`
	Status     string         `json:"status"` // ok|mismatch|missing
	Detail     string         `json:"detail"`
	Facts      map[string]any `json:"facts,omitempty"`
	Mismatches []string       `json:"mismatches,omitempty"`
}

// ReplayResult is a full audit reconstruction of one publication.
type ReplayResult struct {
	SnapshotID          string                      `json:"snapshot_id"`
	Scope               domain.ScopeRef             `json:"scope"`
	Version             int                         `json:"version"`
	SnapshotKind        string                      `json:"snapshot_kind"`
	PublishedAt         time.Time                   `json:"published_at"`
	CutoffReceivedAt    time.Time                   `json:"cutoff_received_at"`
	PublishedStatus     string                      `json:"published_status"`
	RecomputedStatus    string                      `json:"recomputed_status"`
	PublishedHash       string                      `json:"published_result_hash"`
	RecomputedHash      string                      `json:"recomputed_result_hash"`
	Match               bool                        `json:"deterministic_match"`
	Steps               []ReplayStep                `json:"steps"`
	InputEvents         []domain.LineageEvent       `json:"input_events"`
	GraphLinks          []domain.GraphPin           `json:"input_graph_links"`
	CompositionVersions []domain.CompositionVersion `json:"composition_versions,omitempty"`
	Rules               []domain.RuleViolation      `json:"rule_findings"`
	Dispositions        []store.DispositionRow      `json:"evidence_dispositions"`
	Corrections         []domain.Correction         `json:"corrections"`
	Watermark           *domain.Watermark           `json:"watermark,omitempty"`
	Diff                domain.SnapshotDiff         `json:"diff_vs_previous,omitempty"`
	Summary             string                      `json:"summary"`
}

// Replay reconstructs why a published status was published, walking from the
// published version back to the raw messages, graph relations, rules and
// human rulings. It re-runs the same engine over exactly the pinned inputs and
// compares the deterministic result hash, so a drifting engine or a mutated
// ledger is detectable.
func (s *Service) Replay(ctx context.Context, snapshotID string) (ReplayResult, error) {
	sn, err := s.st.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return ReplayResult{}, err
	}
	if sn.Payload == nil || sn.Payload.Lineage.ResultHash == "" {
		return ReplayResult{}, fmt.Errorf("%w: snapshot carries no replayable lineage", store.ErrNotFound)
	}
	lin := sn.Payload.Lineage
	out := ReplayResult{
		SnapshotID: sn.ID, Scope: domain.ScopeRef{Type: sn.ScopeType, Key: sn.ScopeKey},
		Version: sn.Version, SnapshotKind: sn.Kind, PublishedAt: sn.PublishedAt,
		CutoffReceivedAt: sn.CutoffReceivedAt, PublishedStatus: sn.Status,
		InputEvents: lin.InputEvents, GraphLinks: lin.InputGraphLinks,
		CompositionVersions: sn.Payload.CompositionVersions,
		Corrections:         sn.Payload.Corrections, Diff: sn.Diff,
		PublishedHash: lin.ResultHash,
		Rules:         []domain.RuleViolation{}, Dispositions: []store.DispositionRow{},
	}

	// Step 1: every pinned input event must still exist byte-for-byte.
	var missing, changed []string
	pinned := make([]*domain.Event, 0, len(lin.InputEvents))
	for _, le := range lin.InputEvents {
		ev, err := s.st.EventByID(ctx, le.EventID)
		if err != nil {
			missing = append(missing, le.EventID)
			continue
		}
		if ev.Seq != le.Seq || ev.ContentHash != le.ContentHash {
			changed = append(changed, le.EventID)
			continue
		}
		pinned = append(pinned, ev)
	}
	st := ReplayStep{Name: "raw_ledger",
		Facts: map[string]any{"pinned_events": len(lin.InputEvents), "max_seq": lin.MaxSeq}}
	if len(missing)+len(changed) > 0 {
		st.Status = "mismatch"
		st.Detail = "the append-only ledger no longer matches the pinned input versions"
		st.Mismatches = append(missing, changed...)
	} else {
		st.Status = "ok"
		st.Detail = fmt.Sprintf("%d pinned events verified by seq + content hash",
			len(lin.InputEvents))
	}
	out.Steps = append(out.Steps, st)

	// Step 2: rebuild the identity graph at the cutoff and reconcile with pins.
	// The projection is bounded by BOTH the knowledge cutoff and the pinned
	// append sequence so a backdated late arrival cannot leak into history.
	links, _, err := s.st.LoadLinksAt(ctx, sn.CutoffReceivedAt, lin.MaxSeq)
	if err != nil {
		return ReplayResult{}, err
	}
	graph := domain.NewGraph(links)
	scopeKeysFirst := derivedShipmentKeys(sn.Payload)
	if len(scopeKeysFirst) == 0 {
		scopeKeysFirst = []string{sn.ScopeKey}
	}
	rebuilt := scopePins(graph, scopeKeysFirst, sn.CutoffReceivedAt)
	st = ReplayStep{Name: "identity_graph",
		Facts: map[string]any{"pinned_links": len(lin.InputGraphLinks), "rebuilt_links": len(rebuilt)}}
	if pinsEqual(lin.InputGraphLinks, rebuilt) {
		st.Status = "ok"
		st.Detail = "graph link versions at the knowledge cutoff match the published lineage"
	} else {
		st.Status = "mismatch"
		st.Detail = "graph projection differs from the published lineage"
		st.Mismatches = diffPins(lin.InputGraphLinks, rebuilt)
	}
	out.Steps = append(out.Steps, st)

	// Step 3: re-evaluate the six quality rules over the pinned events.
	attempts, err := s.attemptsFor(ctx, pinned, sn.CutoffReceivedAt)
	if err != nil {
		return ReplayResult{}, err
	}
	// For a multi-shipment scope each shipment is evaluated separately and then
	// folded; the pinned event list is partitioned the same way.
	var bundles []shipmentBundle
	scopeKeys := derivedShipmentKeys(sn.Payload)
	if len(scopeKeys) == 0 {
		scopeKeys = []string{sn.ScopeKey}
	}
	for _, sk := range scopeKeys {
		sub := filterEventsForShipment(graph, pinned, sk, sn.CutoffReceivedAt)
		disp := dispositionMap(graph, sub, sk, sn.CutoffReceivedAt)
		timeline := domain.BuildTimeline(sub, disp, sn.CutoffReceivedAt)
		subAttempts := filterAttempts(attempts, sub)
		rules := domain.EvaluateRules(domain.RuleInput{
			ShipmentKey: sk, Events: sub, Timeline: timeline, Dispositions: disp,
			Attempts: subAttempts, Now: sn.CutoffReceivedAt,
			Config: domain.RuleConfig{
				ClockDriftThreshold: s.cfg.ClockDriftThreshold,
				ConflictWindow:      s.cfg.ConflictWindow,
			},
		})
		out.Rules = append(out.Rules, rules...)
		for _, d := range disp {
			out.Dispositions = append(out.Dispositions, store.DispositionRow{
				EventID: d.EventID, ShipmentKey: d.ShipmentKey, PackageKey: d.PackageKey,
				Disposition: d.Disposition, Reason: d.Reason, KnowledgeAt: d.KnowledgeAt,
			})
		}
		bundles = append(bundles, shipmentBundle{
			key: sk, events: sub, violations: rules,
			fusion: s.fuseShipment(graph, sub, sk, sn.CutoffReceivedAt, rules),
		})
	}
	blocking := map[string]bool{}
	for _, v := range out.Rules {
		if v.Blocking() {
			blocking[v.Code] = true
		}
	}
	blockingCodes := make([]string, 0, len(blocking))
	for c := range blocking {
		blockingCodes = append(blockingCodes, c)
	}
	sort.Strings(blockingCodes)
	out.Steps = append(out.Steps, ReplayStep{
		Name: "quality_rules", Status: "ok",
		Detail: fmt.Sprintf("%d rule findings recomputed from pinned events (%d blocking)",
			len(out.Rules), len(blockingCodes)),
		Facts: map[string]any{"blocking_codes": blockingCodes},
	})

	// Step 4: evidence alignment recomputed from the graph.
	misaligned := 0
	for _, d := range out.Dispositions {
		if !d.EvidenceEligible() {
			misaligned++
		}
	}
	out.Steps = append(out.Steps, ReplayStep{
		Name: "evidence_alignment", Status: "ok",
		Detail: fmt.Sprintf("%d evidence classifications recomputed, %d misaligned",
			len(out.Dispositions), misaligned),
	})

	// Step 5: re-fuse and compare the deterministic result hash.
	var fused domain.FusionResult
	if len(bundles) == 1 {
		fused = bundles[0].fusion
	} else {
		fused = aggregateFusion(bundles)
	}
	fused.InputRuleKeys = ruleKeysOf(out.Rules)
	fused.InputEventIDs = idsOfEvents(pinned)
	fused.InputGraphLinks = pinStrings(rebuilt)

	reLin := lin
	reLin.InputEvents = lineageEvents(pinned)
	reLin.InputGraphLinks = rebuilt
	reLin.InputRuleKeys = fused.InputRuleKeys
	reLin.ResultHash = domain.HashResult(fused, reLin)

	out.RecomputedHash = reLin.ResultHash
	out.RecomputedStatus = fused.Status
	out.Match = out.RecomputedHash == out.PublishedHash && fused.Status == sn.Status
	status := "ok"
	detail := "recomputed result hash and status equal the published values"
	if !out.Match {
		status = "mismatch"
		detail = "recomputed result does not equal the publication"
	}
	out.Steps = append(out.Steps, ReplayStep{
		Name: "status_fusion", Status: status, Detail: detail,
		Facts: map[string]any{
			"published_hash": out.PublishedHash, "recomputed_hash": out.RecomputedHash,
			"published_status": sn.Status, "recomputed_status": fused.Status,
		},
	})

	// Step 6: the version chain, for "why did this differ from the last one".
	if sn.Supersedes != "" {
		if prev, err := s.st.GetSnapshot(ctx, sn.Supersedes); err == nil {
			out.Steps = append(out.Steps, ReplayStep{
				Name: "version_history", Status: "ok",
				Detail: fmt.Sprintf("version %d supersedes version %d (%s/%s)",
					sn.Version, prev.Version, prev.Status, prev.Kind),
				Facts: map[string]any{"previous_snapshot_id": prev.ID, "diff": sn.Diff},
			})
		}
	}

	if wm, err := s.st.GetWatermark(ctx, sn.WatermarkID); err == nil {
		out.Watermark = &wm
	}
	if out.Match {
		out.Summary = fmt.Sprintf("replayed %s %s v%d (%s) from %d pinned events and %d graph links: "+
			"published status %q reproduced deterministically",
			sn.ScopeType, sn.ScopeKey, sn.Version, sn.Kind,
			len(lin.InputEvents), len(lin.InputGraphLinks), sn.Status)
	} else {
		out.Summary = fmt.Sprintf("replay of %s %s v%d did NOT reproduce the published result "+
			"(published %q / recomputed %q)", sn.ScopeType, sn.ScopeKey, sn.Version,
			sn.Status, fused.Status)
	}
	return out, nil
}

func derivedShipmentKeys(p *domain.SnapshotPayload) []string {
	var out []string
	for _, r := range p.DerivedFrom {
		if r.ScopeType == "shipment" && r.ScopeKey != "" {
			out = append(out, r.ScopeKey)
		}
	}
	return domain.SortedUnique(out)
}

func filterAttempts(all []domain.AttemptInfo, evs []*domain.Event) []domain.AttemptInfo {
	want := map[string]bool{}
	for _, ev := range evs {
		want[ev.EventID] = true
		if ev.IdempotencyKey != "" {
			want["key:"+ev.IdempotencyKey] = true
		}
	}
	var out []domain.AttemptInfo
	for _, a := range all {
		if want[a.OriginalEvent] || want["key:"+a.IdempotencyKey] {
			out = append(out, a)
		}
	}
	return out
}

func pinsEqual(a, b []domain.GraphPin) bool {
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

func diffPins(a, b []domain.GraphPin) []string {
	am := map[string]bool{}
	for _, p := range a {
		am[fmt.Sprintf("%s@v%d", p.LinkKey, p.LinkVersion)] = true
	}
	bm := map[string]bool{}
	for _, p := range b {
		bm[fmt.Sprintf("%s@v%d", p.LinkKey, p.LinkVersion)] = true
	}
	var out []string
	for k := range am {
		if !bm[k] {
			out = append(out, "published-only:"+k)
		}
	}
	for k := range bm {
		if !am[k] {
			out = append(out, "rebuilt-only:"+k)
		}
	}
	sort.Strings(out)
	return out
}
