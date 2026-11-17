// Package demo drives one deterministic end-to-end scenario against a fresh
// database. It exercises every case the platform must handle:
//
//	SHIP-1  clean path: declaration -> scan -> screening -> evidence archive ->
//	        handover -> stow -> manifest; publish provisional then formal.
//	SHIP-2  duplicate retransmission (identical + conflicting), a result
//	        correction with supersedes, evidence misalignment after a split,
//	        a late scan still citing the parent package.
//	SHIP-3  a transload between containers and a conflict_pending shipment.
//	SHIP-4  a manifest citing a required evidence index the ledger never got:
//	        the publication must refuse to emit a confirmed status.
//
// Every scenario ends with an audit replay of each published version to prove
// the status can be reproduced from history.
package demo

import (
	"context"
	"fmt"
	"time"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/service"
	"cargo.example/secstate/internal/store"
)

// Step is one recorded action and its outcome (rendered in the report).
type Step struct {
	Name    string         `json:"name"`
	Detail  string         `json:"detail"`
	Outcome map[string]any `json:"outcome,omitempty"`
	OK      bool           `json:"ok"`
}

// Section groups steps for reporting.
type Section struct {
	Title string `json:"title"`
	Steps []Step `json:"steps"`
}

// Report is the whole scenario run.
type Report struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Sections   []Section `json:"sections"`
	Checks     []Check   `json:"checks"`
	Summary    string    `json:"summary"`
}

// Check is a hard assertion with an explanation.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Runner drives the scenario.
type Runner struct {
	svc *service.Service
	now time.Time
	rep *Report
	ids map[string]string // logical name -> event id
	// ResetBeforeRun wipes the schema so the run is deterministic. Disable it
	// to layer demo data onto an existing ledger.
	ResetBeforeRun bool
}

const (
	ShipClean    = "AWB12345678901"
	ShipDupSplit = "AWB12345678902"
	ShipConflict = "AWB65432100003"
	ShipManifest = "AWB98765400004"

	PkgClean   = "PKG-CLEAN-1"
	PkgDupA    = "PKG-DUP-1"
	PkgDupB    = "PKG-DUP-2"
	PkgSplitA1 = "PKG-SPLIT-1A"
	PkgSplitA2 = "PKG-SPLIT-1B"
	PkgConfA   = "PKG-CONF-1"
	PkgManif   = "PKG-MANIF-1"

	ContA = "AKE12345MU"
	ContB = "AKE67890MU"
	Leg1  = "CA1234/2026-11-17/PEK-SHA"
)

// NewRunner builds a runner with a frozen base time. It resets the schema by
// default so the run is deterministic and self-contained.
func NewRunner(svc *service.Service) *Runner {
	base := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	return &Runner{
		svc: svc, now: base, rep: &Report{StartedAt: base},
		ids: map[string]string{}, ResetBeforeRun: true,
	}
}

func (r *Runner) tick(d time.Duration) { r.now = r.now.Add(d) }

// run executes fn with the service clock set to the runner's current time.
func (r *Runner) at(offset time.Duration) time.Time { return r.now.Add(offset) }

// addStep appends to the current section.
func (r *Runner) addStep(s Section, step Step) Section {
	s.Steps = append(s.Steps, step)
	return s
}

// Sections is the running list being assembled.
func (r *Runner) section(title string) *Section {
	r.rep.Sections = append(r.rep.Sections, Section{Title: title})
	return &r.rep.Sections[len(r.rep.Sections)-1]
}

func (r *Runner) check(name string, ok bool, detail string) {
	r.rep.Checks = append(r.rep.Checks, Check{Name: name, OK: ok, Detail: detail})
}

// Run executes the full scenario and returns the report.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	r.svc.SetClock(func() time.Time { return r.now })
	defer r.svc.SetClock(time.Now)

	// Clean the workspace for a deterministic run.
	if r.ResetBeforeRun {
		if err := r.svc.Store().Reset(ctx); err != nil {
			return nil, err
		}
	}
	if err := r.registerContracts(ctx); err != nil {
		return nil, err
	}
	if err := r.cleanScenario(ctx); err != nil {
		return nil, err
	}
	if err := r.duplicateSplitScenario(ctx); err != nil {
		return nil, err
	}
	if err := r.conflictScenario(ctx); err != nil {
		return nil, err
	}
	if err := r.manifestGapScenario(ctx); err != nil {
		return nil, err
	}
	if err := r.reviewScenario(ctx); err != nil {
		return nil, err
	}
	r.verifyAtEnd(ctx)

	r.rep.FinishedAt = r.now
	total, ok := 0, 0
	for _, c := range r.rep.Checks {
		total++
		if c.OK {
			ok++
		}
	}
	r.rep.Summary = fmt.Sprintf("%d/%d checks passed", ok, total)
	if ok != total {
		r.rep.Summary += " (some checks failed — see details)"
	}
	return r.rep, nil
}

// ---------------------------------------------------------------------------

func (r *Runner) registerContracts(ctx context.Context) error {
	sec := r.section("0. 报文契约注册")
	contracts := []struct {
		source string
		mt     string
		compat string
		fields []domain.FieldSpecBinding
	}{
		{"declaration", domain.TypeDeclaration, "strict", []domain.FieldSpecBinding{
			{Name: "packages", Spec: domain.FieldSpec{Kind: domain.KindArray, Required: true}},
			{Name: "awb", Spec: domain.FieldSpec{Kind: domain.KindString, Required: true}},
			{Name: "shipper", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"scanner", domain.TypeScan, "backward", []domain.FieldSpecBinding{
			{Name: "mode", Spec: domain.FieldSpec{Kind: domain.KindEnum, Required: true, Enum: []string{"arrival", "transfer", "loading"}}},
			{Name: "device_id", Spec: domain.FieldSpec{Kind: domain.KindString, Required: true}},
			{Name: "location", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"screening", domain.TypeScreening, "strict", []domain.FieldSpecBinding{
			{Name: "result", Spec: domain.FieldSpec{Kind: domain.KindEnum, Required: true, Enum: []string{"CLEAR", "REJECT", "HOLD", "INCONCLUSIVE"}}},
			{Name: "device_id", Spec: domain.FieldSpec{Kind: domain.KindString, Required: true}},
			{Name: "required_evidence_count", Spec: domain.FieldSpec{Kind: domain.KindNumber, Required: false}},
			{Name: "container_id", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"imaging", domain.TypeEvidence, "backward", []domain.FieldSpecBinding{
			{Name: "archive_state", Spec: domain.FieldSpec{Kind: domain.KindEnum, Required: true, Enum: []string{"archived", "failed"}}},
			{Name: "device_id", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"review", domain.TypeManualReview, "strict", []domain.FieldSpecBinding{
			{Name: "conclusion", Spec: domain.FieldSpec{Kind: domain.KindEnum, Required: true, Enum: []string{"CLEAR", "REJECT", "HOLD"}}},
			{Name: "reviewer", Spec: domain.FieldSpec{Kind: domain.KindString, Required: true}},
			{Name: "note", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"stowage", domain.TypeContainerStow, "backward", []domain.FieldSpecBinding{
			{Name: "container_id", Spec: domain.FieldSpec{Kind: domain.KindString, Required: true}},
			{Name: "slot", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"manifest", domain.TypeFlightManifest, "backward", []domain.FieldSpecBinding{
			{Name: "container_id", Spec: domain.FieldSpec{Kind: domain.KindString, Required: true}},
			{Name: "leg", Spec: domain.FieldSpec{Kind: domain.KindString, Required: true}},
			{Name: "required_evidence", Spec: domain.FieldSpec{Kind: domain.KindArray, Required: false}},
			{Name: "referenced_status", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"custody", domain.TypeHandover, "backward", []domain.FieldSpecBinding{
			{Name: "mode", Spec: domain.FieldSpec{Kind: domain.KindEnum, Required: true, Enum: []string{"screening", "departure", "loading"}}},
			{Name: "from_party", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
			{Name: "to_party", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
		{"custody", domain.TypePackageChange, "strict", []domain.FieldSpecBinding{
			{Name: "action", Spec: domain.FieldSpec{Kind: domain.KindEnum, Required: true, Enum: []string{"split", "merge"}}},
			{Name: "parent_packages", Spec: domain.FieldSpec{Kind: domain.KindArray, Required: true}},
			{Name: "child_packages", Spec: domain.FieldSpec{Kind: domain.KindArray, Required: true}},
			{Name: "reason", Spec: domain.FieldSpec{Kind: domain.KindString, Required: false}},
		}},
	}
	for _, c := range contracts {
		ct, err := r.svc.RegisterContract(ctx, store.Contract{
			Source: c.source, MessageType: c.mt, Compat: c.compat,
			Definition: domain.ContractDefinition{Fields: c.fields, Policy: c.compat},
		})
		if err != nil {
			return err
		}
		*sec = r.addStep(*sec, Step{
			Name: fmt.Sprintf("%s/%s", c.source, c.mt),
			Detail: fmt.Sprintf("registered v%d compat=%s fields=%d",
				ct.Version, ct.Compat, len(c.fields)),
			OK: true,
		})
	}
	return nil
}

// Fields is a tiny helper for the report line.
