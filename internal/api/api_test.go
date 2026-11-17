package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"cargo.example/secstate/internal/api"
	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/service"
	"cargo.example/secstate/internal/store"
)

// dsn returns the test database. Tests are skipped when none is reachable so
// `go test ./...` still works in a bare checkout.
func dsn() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://ccdev:ccdev@127.0.0.1:5432/ccdev?sslmode=disable"
}

func newServer(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn())
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := st.Reset(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(st.Close)
	svc := service.New(st, service.DefaultConfig())
	srv := httptest.NewServer(api.New(svc).Handler())
	t.Cleanup(srv.Close)
	return srv, svc
}

func post(t *testing.T, base, path string, body any, wantStatus int) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s: status %d want %d body=%s", path, resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func get(t *testing.T, base, path string, wantStatus int) map[string]any {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d want %d body=%s", path, resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestOpenAPIDocumentServed(t *testing.T) {
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("openapi status %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("openapi is not valid JSON: %v", err)
	}
	if doc["openapi"] == nil {
		t.Fatal("missing openapi version field")
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) < 20 {
		t.Fatalf("openapi paths too thin: %d", len(paths))
	}
	for _, must := range []string{
		"/v1/sources/{source}/events", "/v1/contracts", "/v1/publications",
		"/v1/snapshots/{id}/replay", "/v1/watermarks", "/v1/graph/shipments/{key}/composition",
	} {
		if _, ok := paths[must]; !ok {
			t.Errorf("openapi missing documented path %s", must)
		}
	}
	// Swagger UI page and assets
	ui, err := http.Get(srv.URL + "/docs")
	if err != nil || ui.StatusCode != 200 {
		t.Fatalf("docs page unavailable: %v %d", err, ui.StatusCode)
	}
	ui.Body.Close()
	for _, asset := range []string{"/docs/swagger-ui.css", "/docs/swagger-ui-bundle.js"} {
		r, err := http.Get(srv.URL + asset)
		if err != nil || r.StatusCode != 200 {
			t.Errorf("asset %s unavailable: %v %d", asset, err, r.StatusCode)
			continue
		}
		r.Body.Close()
	}
}

func TestIngestAndIdempotencyOverHTTP(t *testing.T) {
	srv, _ := newServer(t)
	post(t, srv.URL, "/v1/contracts", map[string]any{
		"source": "scanner", "message_type": "scan", "compat": "backward",
		"definition": map[string]any{"policy": "backward", "fields": []any{
			map[string]any{"name": "mode", "spec": map[string]any{"kind": "enum", "required": true,
				"enum": []string{"arrival", "transfer", "loading"}}},
		}},
	}, 201)

	ev := map[string]any{
		"message_type": "scan", "idempotency_key": "k1",
		"occurred_at": "2026-11-17T02:00:00Z", "received_at": "2026-11-17T02:00:00Z",
		"device_id": "SCN-1", "device_event_no": "0001",
		"package_id":    "PKG-1",
		"business_keys": map[string]string{"awb": "123-45678901"},
		"payload":       map[string]any{"mode": "arrival"},
	}
	out := post(t, srv.URL, "/v1/sources/scanner/events", ev, 201)
	if out["outcome"] != "accepted" {
		t.Fatalf("want accepted, got %v", out["outcome"])
	}
	dup := post(t, srv.URL, "/v1/sources/scanner/events", ev, 200)
	if dup["outcome"] != "duplicate_identical" {
		t.Fatalf("identical retransmission must be suppressed, got %v", dup["outcome"])
	}
	conflict := map[string]any{}
	for k, v := range ev {
		conflict[k] = v
	}
	conflict["payload"] = map[string]any{"mode": "transfer"}
	post(t, srv.URL, "/v1/sources/scanner/events", conflict, 409)

	// A payload violating the contract enum is rejected with 422.
	bad := map[string]any{
		"message_type": "scan", "idempotency_key": "k2",
		"occurred_at": "2026-11-17T02:01:00Z", "package_id": "PKG-1",
		"payload": map[string]any{"mode": "nonsense"},
	}
	post(t, srv.URL, "/v1/sources/scanner/events", bad, 422)
}

func TestPublicationLifecycleOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	// Contracts
	post(t, srv.URL, "/v1/contracts", map[string]any{
		"source": "declaration", "message_type": "declaration", "compat": "strict",
		"definition": map[string]any{"policy": "strict", "fields": []any{
			map[string]any{"name": "packages", "spec": map[string]any{"kind": "array", "required": true}},
		}},
	}, 201)
	post(t, srv.URL, "/v1/contracts", map[string]any{
		"source": "screening", "message_type": "screening", "compat": "strict",
		"definition": map[string]any{"policy": "strict", "fields": []any{
			map[string]any{"name": "result", "spec": map[string]any{"kind": "enum", "required": true,
				"enum": []string{"CLEAR", "REJECT", "HOLD", "INCONCLUSIVE"}}},
		}},
	}, 201)

	post(t, srv.URL, "/v1/sources/declaration/events", map[string]any{
		"message_type": "declaration", "idempotency_key": "d1",
		"occurred_at": "2026-11-17T02:00:00Z", "business_keys": map[string]string{"awb": "12345678901"},
		"payload": map[string]any{"packages": []string{"PKG-1"}},
	}, 201)
	post(t, srv.URL, "/v1/sources/screening/events", map[string]any{
		"message_type": "screening", "idempotency_key": "s1", "package_id": "PKG-1",
		"occurred_at": "2026-11-17T02:10:00Z", "evidence_indexes": []string{"IMG-1"},
		"payload": map[string]any{"result": "CLEAR"},
	}, 201)

	// Graph shows the shipment/package relationship.
	g := get(t, srv.URL, "/v1/graph/shipments/AWB12345678901", 200)
	if g["packages"] == nil {
		t.Fatal("graph missing packages")
	}

	// Create a watermark in wait_complete mode; not closed -> publish skips.
	wm := post(t, srv.URL, "/v1/watermarks", map[string]any{
		"scope_type": "shipment", "scope_key": "AWB12345678901",
		"cutoff_received_at": "2026-11-17T02:20:00Z",
		"required_sources":   []string{"declaration", "screening"},
		"require_handover":   false, "mode": "wait_complete",
	}, 201)
	wmID := wm["id"].(string)
	skipped := post(t, srv.URL, "/v1/publications", map[string]any{
		"scope_type": "shipment", "scope_key": "AWB12345678901", "watermark_id": wmID,
	}, 202)
	if skipped["skipped"] != true {
		t.Fatalf("wait_complete must skip while sources are open: %v", skipped)
	}

	post(t, srv.URL, "/v1/watermarks/"+wmID+"/close", map[string]any{"source": "declaration"}, 200)
	post(t, srv.URL, "/v1/watermarks/"+wmID+"/close", map[string]any{"source": "screening"}, 200)
	res := post(t, srv.URL, "/v1/publications", map[string]any{
		"scope_type": "shipment", "scope_key": "AWB12345678901", "watermark_id": wmID,
	}, 201)
	snap, _ := res["snapshot"].(map[string]any)
	if snap == nil {
		t.Fatalf("expected a snapshot, got %v", res)
	}
	snapID := snap["id"].(string)

	// The snapshot is readable, and its replay reproduces it.
	got := get(t, srv.URL, "/v1/snapshots/"+snapID, 200)
	if got["snapshot_kind"] != "formal" {
		t.Fatalf("want formal, got %v", got["snapshot_kind"])
	}
	rep := get(t, srv.URL, "/v1/snapshots/"+snapID+"/replay", 200)
	if rep["deterministic_match"] != true {
		t.Fatalf("replay must match: %v", rep["summary"])
	}
	// History endpoint lists it.
	hist := get(t, srv.URL, "/v1/publications/shipment/AWB12345678901/history", 200)
	if n, _ := hist["count"].(float64); n < 1 {
		t.Fatalf("history should include the snapshot: %v", hist)
	}
	// As-of requires the timestamp parameter.
	get(t, srv.URL, "/v1/publications/shipment/AWB12345678901/as-of", 400)
	asOf := get(t, srv.URL, "/v1/publications/shipment/AWB12345678901/as-of?cutoff_received_at=2026-11-17T02:20:00Z", 200)
	if asOf["id"] != snapID {
		t.Fatalf("as-of should return the same snapshot: %v", asOf["id"])
	}
	_ = svc
}

func TestAppendOnlyLedgerOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	post(t, srv.URL, "/v1/contracts", map[string]any{
		"source": "review", "message_type": "manual_review", "compat": "strict",
		"definition": map[string]any{"policy": "strict", "fields": []any{
			map[string]any{"name": "conclusion", "spec": map[string]any{"kind": "enum", "required": true,
				"enum": []string{"CLEAR", "REJECT", "HOLD"}}},
			map[string]any{"name": "reviewer", "spec": map[string]any{"kind": "string", "required": true}},
		}},
	}, 201)
	out := post(t, srv.URL, "/v1/sources/review/events", map[string]any{
		"message_type": "manual_review", "idempotency_key": "r1", "package_id": "PKG-1",
		"occurred_at": "2026-11-17T02:00:00Z",
		"payload":     map[string]any{"conclusion": "CLEAR", "reviewer": "op"},
	}, 201)
	id := out["event_id"].(string)
	// Direct UPDATE must be rejected by the DB trigger.
	pool := svc.Store().Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `UPDATE raw_events SET payload='{}' WHERE event_id=$1`, id)
	if err == nil {
		t.Fatal("append-only trigger did not block an UPDATE")
	}
	// The event is still readable unchanged.
	ev := get(t, srv.URL, "/v1/events/"+id, 200)
	if ev["EventID"] != id {
		t.Fatalf("event mutated: %v", ev["EventID"])
	}
	_ = domain.TypeManualReview
	_ = fmt.Sprintf
}
