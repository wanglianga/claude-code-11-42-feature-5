package api_test

import (
	"context"
	"testing"
	"time"
)

// reviewFixtures builds a scope and a case through the HTTP API and returns the
// case/scope ids. It exercises the real schema and append-only review tables.
func setupReview(t *testing.T, base, shipment string, cutoff time.Time, systems ...string) (string, string) {
	t.Helper()
	scope := post(t, base, "/v1/reviews/scopes", map[string]any{
		"shipment_key": shipment, "included_systems": systems,
		"cutoff_received_at": cutoff.Format(time.RFC3339),
	}, 201)
	scopeID, _ := scope["scope_id"].(string)
	c := post(t, base, "/v1/reviews/cases", map[string]any{
		"scope_id": scopeID, "title": "screenshot reconciliation",
	}, 201)
	caseID, _ := c["case_id"].(string)
	return scopeID, caseID
}

func TestReviewClockSkewEndToEnd(t *testing.T) {
	srv, _ := newServer(t)
	base := srv.URL
	ship := "AWB10000000001"
	cutoff := time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)
	scopeID, caseID := setupReview(t, base, ship, cutoff, "platform", "reading", "declaration")

	in := map[string]any{
		"events": []map[string]any{{
			"ref": "decl", "system": "declaration",
			"occurred": map[string]any{"earliest": "2026-11-17T08:00:00Z", "latest": "2026-11-17T08:00:00Z"},
			"membership": map[string]any{"link_kind": "shipment_package",
				"subjects": []string{ship}, "objects": []string{"PKG-1"}},
		}},
		"publications": []map[string]any{
			{"ref": "pub-a", "snapshot_id": "SNAPA", "subject": map[string]any{"type": "shipment", "key": ship},
				"version": 1, "snapshot_kind": "formal", "status": "verified",
				"published_at": "2026-11-17T10:00:00Z", "input_event_refs": []string{"decl"}},
			{"ref": "pub-b", "snapshot_id": "SNAPB", "subject": map[string]any{"type": "shipment", "key": ship},
				"version": 2, "snapshot_kind": "formal", "status": "pending_evidence",
				"published_at": "2026-11-17T10:40:00Z", "supersedes_snapshot_id": "SNAPA",
				"input_event_refs": []string{"decl"}},
		},
		"reads": []map[string]any{{
			"ref": "read-x", "system": "reading",
			"occurred":               map[string]any{"earliest": "2026-11-17T10:20:00Z", "latest": "2026-11-17T11:00:00Z"},
			"received":               map[string]any{"earliest": "2026-11-17T10:55:00Z", "latest": "2026-11-17T10:55:00Z"},
			"referenced_snapshot_id": "SNAPA",
			"observed":               map[string]string{"status": "verified"},
		}},
	}
	imp := post(t, base, "/v1/reviews/scopes/"+scopeID+"/imports", in, 201)
	if n, _ := imp["imported_count"].(float64); n < 4 {
		t.Fatalf("expected 4 imported records, got %v", imp)
	}

	run := post(t, base, "/v1/reviews/cases/"+caseID+"/runs", map[string]any{
		"target": map[string]any{
			"shipment_key": ship, "levels": []string{"shipment"},
			"instant":            map[string]any{"earliest": "2026-11-17T10:20:00Z", "latest": "2026-11-17T11:00:00Z"},
			"cutoff_received_at": "2026-11-17T22:00:00Z",
			"include_read_logs":  true,
			"claims":             []map[string]any{{"level": "shipment", "key": ship, "status": "verified"}},
		},
		"cross_check": true,
	}, 201)
	summary, _ := run["run"].(map[string]any)["analysis"].(map[string]any)["summary"].(map[string]any)
	if summary == nil {
		t.Fatalf("run has no analysis summary: %v", run)
	}
	if v, _ := summary["verdict"].(string); v != "both_explain" {
		t.Fatalf("clock skew should leave both readings open, got %q", v)
	}
	// Cross-check agreement must be reported and true.
	checks, _ := run["cross_check"].([]any)
	if len(checks) == 0 {
		t.Fatal("cross_check requested but missing")
	}
	for _, c := range checks {
		m := c.(map[string]any)
		if agree, _ := m["agreement"].(bool); !agree {
			t.Fatalf("targeted and exhaustive disagree: %v", m)
		}
	}
	runID := run["run"].(map[string]any)["run_id"].(string)

	// Examples are retrievable.
	ex := get(t, base, "/v1/reviews/runs/"+runID+"/examples", 200)
	if n, _ := ex["count"].(float64); n < 2 {
		t.Fatalf("expected both order examples, got %v", ex)
	}

	// Reproduce reproduces the verdict from the pinned material.
	rep := post(t, base, "/v1/reviews/runs/"+runID+"/reproduce", map[string]any{}, 200)
	repAn := rep["run"].(map[string]any)["analysis"].(map[string]any)
	repVerdict := repAn["summary"].(map[string]any)["verdict"].(string)
	if repVerdict != "both_explain" {
		t.Fatalf("reproduction changed the verdict: %q", repVerdict)
	}

	// Export with narrowed fields keeps the verdict.
	view := post(t, base, "/v1/reviews/runs/"+runID+"/export", map[string]any{
		"allowed_fields": []string{"record_id", "kind", "status"},
	}, 200)
	if view["verdict"] != "both_explain" {
		t.Fatalf("projection changed the verdict: %v", view)
	}
	if view["projection_integrity"] != "verdict_recomputed_from_same_material" {
		t.Fatalf("projection integrity not reported: %v", view)
	}
}

func TestReviewLateImportNotFolded(t *testing.T) {
	srv, _ := newServer(t)
	base := srv.URL
	ship := "AWB10000000002"
	cutoff := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	scopeID, _ := setupReview(t, base, ship, cutoff, "reading")
	out := post(t, base, "/v1/reviews/scopes/"+scopeID+"/imports", map[string]any{
		"events": []map[string]any{{
			"ref": "late", "system": "reading",
			"occurred": map[string]any{"earliest": "2026-11-17T11:00:00Z", "latest": "2026-11-17T11:00:00Z"},
			"received": map[string]any{"earliest": "2026-11-17T13:00:00Z", "latest": "2026-11-17T13:00:00Z"},
		}},
	}, 200)
	late, _ := out["late_arrivals"].([]any)
	if len(late) != 1 {
		t.Fatalf("late record must be reported, not folded: %v", out)
	}
	if n, _ := out["imported_count"].(float64); n != 0 {
		t.Fatalf("late record must not be imported into the old scope: %v", out)
	}
}

func TestReviewScopeRejectsUnauthorizedShipment(t *testing.T) {
	srv, _ := newServer(t)
	base := srv.URL
	cutoff := time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)
	_, caseID := setupReview(t, base, "AWB10000000001", cutoff, "platform")
	// A case cannot be analysed for a different shipment than its scope covers.
	post(t, base, "/v1/reviews/cases/"+caseID+"/runs", map[string]any{
		"target": map[string]any{"shipment_key": "AWB99999999999",
			"levels": []string{"shipment"}, "cutoff_received_at": "2026-11-17T22:00:00Z"},
	}, 422)
}

func TestReviewReadCitedUnpublishedVersionEndToEnd(t *testing.T) {
	srv, _ := newServer(t)
	base := srv.URL
	ship := "AWB10000000003"
	cutoff := time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)
	scopeID, caseID := setupReview(t, base, ship, cutoff, "platform", "reading", "declaration")
	post(t, base, "/v1/reviews/scopes/"+scopeID+"/imports", map[string]any{
		"publications": []map[string]any{{
			"ref": "pub-v2", "snapshot_id": "SNAP-V2",
			"subject": map[string]any{"type": "shipment", "key": ship},
			"version": 2, "snapshot_kind": "formal", "status": "verified",
			"published_at": "2026-11-17T14:00:00Z",
		}},
		"reads": []map[string]any{{
			"ref": "read-early", "system": "reading",
			"occurred":               map[string]any{"earliest": "2026-11-17T13:00:00Z", "latest": "2026-11-17T13:00:00Z"},
			"received":               map[string]any{"earliest": "2026-11-17T13:00:00Z", "latest": "2026-11-17T13:00:00Z"},
			"referenced_snapshot_id": "SNAP-V2",
		}},
	}, 201)
	run := post(t, base, "/v1/reviews/cases/"+caseID+"/runs", map[string]any{
		"target": map[string]any{
			"shipment_key": ship, "levels": []string{"shipment"},
			"cutoff_received_at": "2026-11-17T22:00:00Z",
			"include_read_logs":  true, "read_record_ids": []string{"read-early"},
		},
	}, 201)
	an := run["run"].(map[string]any)["analysis"].(map[string]any)
	res := findAssertionIn(an, "read:read-early")
	if res == nil || res["verdict"] != "contradicted" {
		t.Fatalf("read citing an unpublished snapshot must be contradicted: %v", an["assertions"])
	}
}

func findAssertionIn(an map[string]any, id string) map[string]any {
	arr, _ := an["assertions"].([]any)
	for _, a := range arr {
		m, _ := a.(map[string]any)
		if m["assertion_id"] == id {
			return m
		}
	}
	return nil
}

func TestReviewBudgetExhaustionReported(t *testing.T) {
	srv, _ := newServer(t)
	base := srv.URL
	ship := "AWB10000000004"
	cutoff := time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)
	scopeID, caseID := setupReview(t, base, ship, cutoff, "platform")
	pubs := []map[string]any{}
	for i := 1; i <= 6; i++ {
		pubs = append(pubs, map[string]any{
			"ref": "v" + string(rune('0'+i)), "snapshot_id": "S" + string(rune('0'+i)),
			"subject": map[string]any{"type": "shipment", "key": ship},
			"version": i, "snapshot_kind": "formal", "status": "verified",
			"published_at": "2026-11-17T09:0" + string(rune('0'+i)) + ":00Z",
		})
	}
	post(t, base, "/v1/reviews/scopes/"+scopeID+"/imports", map[string]any{
		"publications": pubs,
	}, 201)
	run := post(t, base, "/v1/reviews/cases/"+caseID+"/runs", map[string]any{
		"budget": 2,
		"target": map[string]any{
			"shipment_key": ship, "levels": []string{"shipment"},
			"instant":            map[string]any{"earliest": "2026-11-17T10:00:00Z", "latest": "2026-11-17T12:00:00Z"},
			"cutoff_received_at": "2026-11-17T22:00:00Z",
			"claims":             []map[string]any{{"level": "shipment", "key": ship, "status": "verified"}},
		},
	}, 201)
	an := run["run"].(map[string]any)["analysis"].(map[string]any)
	summary := an["summary"].(map[string]any)
	if ex, _ := summary["budget_exhausted"].(bool); !ex {
		t.Fatalf("budget must be reported exhausted: %v", summary)
	}
	if v, _ := summary["verdict"].(string); v == "contradicted" {
		t.Fatalf("a budget stop must not be reported as a contradiction: %v", summary)
	}
}

func TestReviewTablesAppendOnly(t *testing.T) {
	srv, svc := newServer(t)
	base := srv.URL
	cutoff := time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)
	scopeID, _ := setupReview(t, base, "AWB10000000005", cutoff, "platform")
	post(t, base, "/v1/reviews/scopes/"+scopeID+"/imports", map[string]any{
		"publications": []map[string]any{{
			"ref": "ao.v1", "snapshot_id": "AOV1",
			"subject": map[string]any{"type": "shipment", "key": "AWB10000000005"},
			"version": 1, "snapshot_kind": "formal", "status": "verified",
			"published_at": "2026-11-17T10:00:00Z",
		}},
	}, 201)
	// Row-level triggers only fire on an actual row; each statement must touch
	// at least one existing row to prove the guard.
	rows := []struct{ table, sql string }{
		{"review_records", `UPDATE review_records SET body='{}'::jsonb WHERE scope_id=$1`},
		{"review_scopes", `UPDATE review_scopes SET label='x' WHERE scope_id=$1`},
		{"review_imports", `DELETE FROM review_imports WHERE scope_id=$1`},
	}
	for _, tc := range rows {
		if _, err := svc.Store().Pool().Exec(context.Background(), tc.sql, scopeID); err == nil {
			t.Fatalf("%s must reject mutation", tc.table)
		}
	}
}

func TestReviewRunPersistedAndRetrievable(t *testing.T) {
	srv, _ := newServer(t)
	base := srv.URL
	ship := "AWB10000000006"
	cutoff := time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)
	scopeID, caseID := setupReview(t, base, ship, cutoff, "platform")
	post(t, base, "/v1/reviews/scopes/"+scopeID+"/imports", map[string]any{
		"publications": []map[string]any{{
			"ref": "p.v1", "snapshot_id": "P1", "subject": map[string]any{"type": "shipment", "key": ship},
			"version": 1, "snapshot_kind": "formal", "status": "verified",
			"published_at": "2026-11-17T10:00:00Z",
		}},
	}, 201)
	at := map[string]any{"earliest": "2026-11-17T10:10:00Z", "latest": "2026-11-17T10:20:00Z"}
	run := post(t, base, "/v1/reviews/cases/"+caseID+"/runs", map[string]any{
		"target": map[string]any{"shipment_key": ship, "levels": []string{"shipment"},
			"instant": at, "cutoff_received_at": "2026-11-17T22:00:00Z",
			"claims": []map[string]any{{"level": "shipment", "key": ship, "status": "verified"}}},
	}, 201)
	runID := run["run"].(map[string]any)["run_id"].(string)

	// The stored run must reload identically from the database, including the
	// pinned material hash and the analysis summary.
	got := get(t, base, "/v1/reviews/runs/"+runID, 200)
	if got["material_hash"] == "" || got["summary_verdict"] != "consistent" {
		t.Fatalf("reloaded run lost its pinned result: %v", got)
	}
	an := got["analysis"].(map[string]any)
	if an["material"].(map[string]any)["hash"] != run["run"].(map[string]any)["material_hash"] {
		t.Fatal("analysis material hash differs from the run's pinned hash")
	}

	// Listing the case's runs must include it.
	list := get(t, base, "/v1/reviews/cases/"+caseID+"/runs", 200)
	if n, _ := list["count"].(float64); n < 1 {
		t.Fatalf("list missing the run: %v", list)
	}

	// Constraint check runs on the same case.
	cc := post(t, base, "/v1/reviews/cases/"+caseID+"/constraints", map[string]any{}, 200)
	if feasible, _ := cc["feasible"].(bool); !feasible {
		t.Fatalf("base material should be feasible: %v", cc)
	}
}
