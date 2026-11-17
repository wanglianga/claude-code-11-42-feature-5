package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/store"
)

// RegisterContract validates and appends a contract version.
func (s *Service) RegisterContract(ctx context.Context, c store.Contract) (store.Contract, error) {
	if c.Compat == "" {
		c.Compat = c.Definition.Policy
	}
	if c.Compat == "" {
		c.Compat = "backward"
	}
	if c.Definition.Policy == "" {
		c.Definition.Policy = c.Compat
	}
	if c.Definition.Policy != c.Compat {
		return store.Contract{}, fmt.Errorf("%w: definition.policy and compat must agree", ErrInvalid)
	}
	if len(c.Definition.Fields) == 0 {
		return store.Contract{}, fmt.Errorf("%w: definition.fields must not be empty", ErrInvalid)
	}
	return s.st.RegisterContract(ctx, c)
}

// TimelineView is the API response for a shipment timeline.
type TimelineView struct {
	ShipmentKey         string                      `json:"shipment_key"`
	CutoffReceivedAt    time.Time                   `json:"cutoff_received_at"`
	Entries             []domain.TimelineEntry      `json:"entries"`
	Anomalies           map[string]int              `json:"anomaly_counts"`
	SequenceBasis       map[string]int              `json:"sequence_basis_counts"`
	CompositionVersions []domain.CompositionVersion `json:"composition_versions"`
}

// Timeline builds the explained merged timeline for a shipment.
func (s *Service) Timeline(ctx context.Context, shipmentKey string, cutoff *time.Time) (TimelineView, error) {
	kc := s.cutoffOrNow(cutoff)
	links, _, err := s.st.LoadLinks(ctx, &kc)
	if err != nil {
		return TimelineView{}, err
	}
	graph := domain.NewGraph(links)
	all, err := s.st.ListEvents(ctx, store.EventFilter{UpToReceived: &kc})
	if err != nil {
		return TimelineView{}, err
	}
	evs := filterEventsForShipment(graph, all, shipmentKey, kc)
	if len(evs) == 0 {
		return TimelineView{}, fmt.Errorf("%w: no events for shipment %s known at %s",
			store.ErrNotFound, shipmentKey, kc.Format(time.RFC3339))
	}
	disp := dispositionMap(graph, evs, shipmentKey, kc)
	entries := domain.BuildTimeline(evs, disp, kc)
	view := TimelineView{
		ShipmentKey: shipmentKey, CutoffReceivedAt: kc, Entries: entries,
		Anomalies: map[string]int{}, SequenceBasis: map[string]int{},
		CompositionVersions: graph.MembershipVersions(shipmentKey),
	}
	for _, e := range entries {
		for _, a := range e.Anomalies {
			view.Anomalies[a]++
		}
		view.SequenceBasis[e.SequenceBasis]++
	}
	return view, nil
}

// QualityView is the API response for shipment quality.
type QualityView struct {
	ShipmentKey      string                 `json:"shipment_key"`
	CutoffReceivedAt time.Time              `json:"cutoff_received_at"`
	Summary          domain.QualitySummary  `json:"summary"`
	Violations       []domain.RuleViolation `json:"violations"`
	Dispositions     []store.DispositionRow `json:"evidence_dispositions"`
}

// Quality evaluates and lists the rule findings for a shipment.
func (s *Service) Quality(ctx context.Context, shipmentKey string, cutoff *time.Time) (QualityView, error) {
	kc := s.cutoffOrNow(cutoff)
	links, _, err := s.st.LoadLinks(ctx, &kc)
	if err != nil {
		return QualityView{}, err
	}
	graph := domain.NewGraph(links)
	all, err := s.st.ListEvents(ctx, store.EventFilter{UpToReceived: &kc})
	if err != nil {
		return QualityView{}, err
	}
	evs := filterEventsForShipment(graph, all, shipmentKey, kc)
	if len(evs) == 0 {
		return QualityView{}, fmt.Errorf("%w: no events for shipment %s", store.ErrNotFound, shipmentKey)
	}
	disp := dispositionMap(graph, evs, shipmentKey, kc)
	attempts, err := s.attemptsFor(ctx, evs, kc)
	if err != nil {
		return QualityView{}, err
	}
	timeline := domain.BuildTimeline(evs, disp, kc)
	violations := domain.EvaluateRules(domain.RuleInput{
		ShipmentKey: shipmentKey, Events: evs, Timeline: timeline,
		Dispositions: disp, Attempts: attempts, Now: s.nowFn().UTC(),
		Config: domain.RuleConfig{
			ClockDriftThreshold: s.cfg.ClockDriftThreshold,
			ConflictWindow:      s.cfg.ConflictWindow,
		},
	})
	drows, err := s.st.DispositionsForShipment(ctx, shipmentKey)
	if err != nil {
		return QualityView{}, err
	}
	var cur []store.DispositionRow
	for _, d := range drows {
		if !d.KnowledgeAt.After(kc) {
			cur = append(cur, d)
		}
	}
	return QualityView{
		ShipmentKey: shipmentKey, CutoffReceivedAt: kc,
		Summary: buildQualitySummary(evs, violations), Violations: violations,
		Dispositions: cur,
	}, nil
}

// PreviewAssessment fuses a shipment's current status without publishing.
func (s *Service) PreviewAssessment(ctx context.Context, shipmentKey string, cutoff *time.Time) (dominantPreview, error) {
	kc := s.cutoffOrNow(cutoff)
	links, _, err := s.st.LoadLinks(ctx, &kc)
	if err != nil {
		return dominantPreview{}, err
	}
	graph := domain.NewGraph(links)
	all, err := s.st.ListEvents(ctx, store.EventFilter{UpToReceived: &kc})
	if err != nil {
		return dominantPreview{}, err
	}
	evs := filterEventsForShipment(graph, all, shipmentKey, kc)
	if len(evs) == 0 {
		return dominantPreview{}, fmt.Errorf("%w: no events for shipment %s", store.ErrNotFound, shipmentKey)
	}
	disp := dispositionMap(graph, evs, shipmentKey, kc)
	attempts, err := s.attemptsFor(ctx, evs, kc)
	if err != nil {
		return dominantPreview{}, err
	}
	timeline := domain.BuildTimeline(evs, disp, kc)
	violations := domain.EvaluateRules(domain.RuleInput{
		ShipmentKey: shipmentKey, Events: evs, Timeline: timeline,
		Dispositions: disp, Attempts: attempts, Now: kc,
		Config: domain.RuleConfig{
			ClockDriftThreshold: s.cfg.ClockDriftThreshold,
			ConflictWindow:      s.cfg.ConflictWindow,
		},
	})
	fusion := s.fuseShipment(graph, evs, shipmentKey, kc, violations)
	// A preview is not a publication; flag it clearly.
	return dominantPreview{
		ShipmentKey:       shipmentKey,
		CutoffReceivedAt:  kc,
		Preview:           true,
		Fusion:            fusion,
		Violations:        violations,
		Dispositions:      disp,
		Timeline:          timeline,
		CompletenessFloor: s.cfg.CompletenessFloor,
	}, nil
}

type dominantPreview struct {
	ShipmentKey       string                        `json:"shipment_key"`
	CutoffReceivedAt  time.Time                     `json:"cutoff_received_at"`
	Preview           bool                          `json:"preview"`
	Fusion            domain.FusionResult           `json:"fusion"`
	Violations        []domain.RuleViolation        `json:"rule_findings"`
	Dispositions      map[string]domain.Disposition `json:"dispositions"`
	Timeline          []domain.TimelineEntry        `json:"timeline"`
	CompletenessFloor float64                       `json:"completeness_floor"`
}

// KnowledgeHorizon returns the newest received_at currently in the ledger, or
// the wall clock if the ledger is empty. It is the sensible default "as of"
// for diagnostic reads on a batch platform where a source's clock may run
// ahead of the server's.
func (s *Service) KnowledgeHorizon(ctx context.Context) time.Time {
	var latest *time.Time
	if err := s.st.Pool().QueryRow(ctx, `SELECT max(received_at) FROM raw_events`).Scan(&latest); err == nil &&
		latest != nil && latest.After(time.Time{}) {
		return latest.UTC()
	}
	return s.nowFn().UTC()
}

func (s *Service) cutoffOrNow(c *time.Time) time.Time {
	if c != nil {
		return c.UTC()
	}
	return s.KnowledgeHorizon(context.Background())
}

// SortedViolations is a helper for stable API output.
func SortedViolations(vs []domain.RuleViolation) []domain.RuleViolation {
	out := append([]domain.RuleViolation{}, vs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].DedupeKey < out[j].DedupeKey
	})
	return out
}
