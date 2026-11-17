// Package api exposes the platform over HTTP with an OpenAPI 3 document and
// Swagger UI. The platform is a backend service, so the UI is documentation
// only; every business operation is a documented JSON endpoint.
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"cargo.example/secstate/internal/domain"
	"cargo.example/secstate/internal/service"
	"cargo.example/secstate/internal/store"
)

//go:embed openapi.json
var openapiSpec []byte

// Server wires routes to the service.
type Server struct {
	svc     *service.Service
	pool    *pgxpool.Pool
	mux     *http.ServeMux
	openapi []byte
}

// New builds the API server.
func New(svc *service.Service) *Server {
	s := &Server{svc: svc, pool: svc.Store().Pool(), mux: http.NewServeMux(), openapi: openapiSpec}
	s.routes()
	return s
}

// Handler returns the root handler (with logging and recovery).
func (s *Server) Handler() http.Handler {
	return recoverMW(logMW(s.mux))
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	m.HandleFunc("GET /docs", s.handleDocs)
	m.HandleFunc("GET /docs/", s.handleDocs)
	m.HandleFunc("GET /docs/swagger-ui.css", s.serveStatic("swagger-ui.css", "text/css"))
	m.HandleFunc("GET /docs/swagger-ui-bundle.js", s.serveStatic("swagger-ui-bundle.js", "application/javascript"))
	m.HandleFunc("GET /docs/swagger-ui-standalone-preset.js", s.serveStatic("swagger-ui-standalone-preset.js", "application/javascript"))
	m.HandleFunc("GET /docs/favicon-32x32.png", s.serveStatic("favicon-32x32.png", "image/png"))
	m.HandleFunc("GET /docs/favicon-16x16.png", s.serveStatic("favicon-16x16.png", "image/png"))

	m.HandleFunc("POST /v1/contracts", s.handleRegisterContract)
	m.HandleFunc("GET /v1/contracts", s.handleListContracts)

	m.HandleFunc("POST /v1/sources/{source}/events", s.handleIngest)
	m.HandleFunc("GET /v1/events", s.handleListEvents)
	m.HandleFunc("GET /v1/events/{id}", s.handleGetEvent)
	m.HandleFunc("GET /v1/events/{id}/attempts", s.handleEventAttempts)
	m.HandleFunc("GET /v1/intake-attempts", s.handleAttempts)

	m.HandleFunc("GET /v1/graph", s.handleGraph)
	m.HandleFunc("GET /v1/graph/shipments/{key}", s.handleShipmentGraph)
	m.HandleFunc("GET /v1/graph/shipments/{key}/composition", s.handleComposition)
	m.HandleFunc("GET /v1/graph/containers/{key}", s.handleContainerGraph)
	m.HandleFunc("GET /v1/graph/legs/{key}", s.handleLegGraph)

	m.HandleFunc("GET /v1/shipments/{key}/timeline", s.handleTimeline)
	m.HandleFunc("GET /v1/shipments/{key}/quality", s.handleQuality)
	m.HandleFunc("GET /v1/shipments/{key}/dispositions", s.handleDispositions)
	m.HandleFunc("GET /v1/shipments/{key}/assessment", s.handleAssessPreview)

	m.HandleFunc("POST /v1/watermarks", s.handleCreateWatermark)
	m.HandleFunc("GET /v1/watermarks", s.handleListWatermarks)
	m.HandleFunc("GET /v1/watermarks/{id}", s.handleGetWatermark)
	m.HandleFunc("POST /v1/watermarks/{id}/close", s.handleCloseWatermark)

	m.HandleFunc("POST /v1/publications", s.handlePublish)
	m.HandleFunc("GET /v1/snapshots", s.handleListSnapshots)
	m.HandleFunc("GET /v1/snapshots/{id}", s.handleGetSnapshot)
	m.HandleFunc("GET /v1/snapshots/{id}/replay", s.handleReplay)
	m.HandleFunc("GET /v1/publications/{scopeType}/{key}/latest", s.handleLatestSnapshot)
	m.HandleFunc("GET /v1/publications/{scopeType}/{key}/history", s.handleSnapshotHistory)
	m.HandleFunc("GET /v1/publications/{scopeType}/{key}/as-of", s.handleSnapshotAsOf)

	m.HandleFunc("GET /v1/operations/overview", s.handleOverview)

	s.reviewRoutes()
}

// ---- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

type errBody struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details any    `json:"details,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string, details any) {
	writeJSON(w, status, errBody{Error: msg, Code: code, Details: details})
}

func mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, "invalid", err.Error(), nil)
	case errors.Is(err, service.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error(), nil)
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.pool.Ping(ctx); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "db_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(s.openapi)
}

const docsHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>航空货运安检事件数据质量与状态服务平台 · API</title>
  <link rel="stylesheet" href="/docs/swagger-ui.css"/>
  <style>
    body { margin:0; font-family: system-ui, -apple-system, "Segoe UI", sans-serif; }
    .banner { padding: 14px 24px; background:#0f172a; color:#e2e8f0; font-size:13px; line-height:1.6 }
    .banner b { color:#7dd3fc }
    .banner code { background:#1e293b; padding:1px 6px; border-radius:4px }
  </style>
</head>
<body>
  <div class="banner">
    <b>航空货运安检事件数据质量与状态服务平台</b> · 后端服务，详见 <code>/openapi.json</code> 与下方交互式文档。<br/>
    本平台只发布安检数据状态与证据链，不执行物理安检、放行、配载或异常处置。
  </div>
  <div id="swagger-ui"></div>
  <script src="/docs/swagger-ui-bundle.js"></script>
  <script src="/docs/swagger-ui-standalone-preset.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/openapi.json",
      dom_id: "#swagger-ui",
      deepLinking: true,
      presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
      layout: "StandaloneLayout",
      defaultModelsExpandDepth: 0,
      docExpansion: "list",
      tryItOutEnabled: true
    });
  </script>
</body>
</html>`

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, docsHTML)
}

// ---- contracts -------------------------------------------------------------

type registerContractReq struct {
	Source      string                    `json:"source"`
	MessageType string                    `json:"message_type"`
	Version     int                       `json:"version,omitempty"`
	Compat      string                    `json:"compat"`
	Definition  domain.ContractDefinition `json:"definition"`
}

func (s *Server) handleRegisterContract(w http.ResponseWriter, r *http.Request) {
	var req registerContractReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Source) == "" || !domain.IsKnownMessageType(req.MessageType) {
		writeErr(w, http.StatusUnprocessableEntity, "invalid",
			"source is required and message_type must be a known type", nil)
		return
	}
	c, err := s.svc.RegisterContract(r.Context(), store.Contract{
		Source: req.Source, MessageType: req.MessageType, Version: req.Version,
		Compat: req.Compat, Definition: req.Definition,
	})
	if err != nil {
		if errors.Is(err, service.ErrInvalid) || errors.Is(err, store.ErrDuplicate) {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_contract", err.Error(), nil)
			return
		}
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleListContracts(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.Store().ListContracts(r.Context())
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contracts": list, "count": len(list)})
}

// ---- events ----------------------------------------------------------------

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	var env domain.EventEnvelope
	if err := decode(r, &env); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	out, err := s.svc.Ingest(r.Context(), service.IngestRequest{Source: source, Env: env})
	if err != nil {
		status := http.StatusInternalServerError
		code := "internal"
		switch {
		case errors.Is(err, service.ErrInvalid):
			status, code = http.StatusUnprocessableEntity, "invalid"
		case errors.Is(err, service.ErrConflict):
			status, code = http.StatusConflict, "duplicate_conflicting"
		}
		writeErr(w, status, code, err.Error(), out)
		return
	}
	status := http.StatusCreated
	if out.Outcome == "duplicate_identical" {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EventFilter{
		Source: q.Get("source"), MessageType: q.Get("message_type"),
		ShipmentKey: q.Get("shipment_key"), PackageKey: q.Get("package_key"),
	}
	if v := q.Get("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if f.Limit == 0 {
		f.Limit = 200
	}
	if v := q.Get("up_to_received"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.UpToReceived = &t
		}
	}
	evs, err := s.svc.Store().ListEvents(r.Context(), f)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "count": len(evs)})
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.svc.Store().EventByID(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleEventAttempts(w http.ResponseWriter, r *http.Request) {
	ev, err := s.svc.Store().EventByID(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	rows, err := s.svc.Store().Attempts(r.Context(), ev.Source, ev.IdempotencyKey, 50)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_id": ev.EventID, "attempts": rows})
}

func (s *Server) handleAttempts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.svc.Store().Attempts(r.Context(), r.URL.Query().Get("source"),
		r.URL.Query().Get("idempotency_key"), limit)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": rows, "count": len(rows)})
}

// ---- graph -----------------------------------------------------------------

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	subs, err := s.svc.Store().Subjects(r.Context())
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, subs)
}

type graphLinkView struct {
	domain.Link
	Source string `json:"source,omitempty"`
}

func (s *Server) handleShipmentGraph(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	links, _, err := s.svc.Store().LoadLinks(r.Context(), nil)
	if err != nil {
		mapErr(w, err)
		return
	}
	g := domain.NewGraph(links)
	pkgs := g.ShipmentPackagesEver(key)
	versions := g.MembershipVersions(key)
	now := s.svc.KnowledgeHorizon(r.Context())
	var filtered []domain.Link
	for _, l := range links {
		if l.SubjectKey == key || (l.Kind == domain.LinkPackageContainer && contains(pkgs, l.SubjectKey)) {
			filtered = append(filtered, l)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shipment_key": key,
		"packages":     pkgs,
		"versions":     versions,
		"links":        filtered,
		"containers":   g.ContainersOfShipment(key, now, now),
		"as_of":        now,
	})
}

func (s *Server) handleComposition(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	links, _, err := s.svc.Store().LoadLinks(r.Context(), nil)
	if err != nil {
		mapErr(w, err)
		return
	}
	g := domain.NewGraph(links)
	now := s.svc.KnowledgeHorizon(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"shipment_key":     key,
		"versions":         g.MembershipVersions(key),
		"current_packages": g.ActivePackages(key, now, now),
		"as_of":            now,
	})
}

func (s *Server) handleContainerGraph(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	links, _, err := s.svc.Store().LoadLinks(r.Context(), nil)
	if err != nil {
		mapErr(w, err)
		return
	}
	g := domain.NewGraph(links)
	now := s.svc.KnowledgeHorizon(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"container_key": key,
		"packages":      g.PackagesOfContainer(key, now, now),
		"legs":          g.LegsOfContainer(key, now),
		"as_of":         now,
	})
}

func (s *Server) handleLegGraph(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	links, _, err := s.svc.Store().LoadLinks(r.Context(), nil)
	if err != nil {
		mapErr(w, err)
		return
	}
	g := domain.NewGraph(links)
	now := s.svc.KnowledgeHorizon(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"leg_key":   key,
		"shipments": g.ShipmentsOnLeg(key, now, now),
		"as_of":     now,
	})
}

// ---- shipment views --------------------------------------------------------

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	res, err := s.svc.Timeline(r.Context(), key, cutoffParam(r))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleQuality(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	res, err := s.svc.Quality(r.Context(), key, cutoffParam(r))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDispositions(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	rows, err := s.svc.Store().DispositionsForShipment(r.Context(), key)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shipment_key": key, "dispositions": rows, "count": len(rows)})
}

func (s *Server) handleAssessPreview(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	res, err := s.svc.PreviewAssessment(r.Context(), key, cutoffParam(r))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func cutoffParam(r *http.Request) *time.Time {
	v := r.URL.Query().Get("cutoff_received_at")
	if v == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		t = t.UTC()
		return &t
	}
	return nil
}

// ---- watermarks ------------------------------------------------------------

func (s *Server) handleCreateWatermark(w http.ResponseWriter, r *http.Request) {
	var req service.CreateWatermarkRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	wm, err := s.svc.CreateWatermark(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wm)
}

func (s *Server) handleListWatermarks(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.Store().ListWatermarks(r.Context())
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"watermarks": list, "count": len(list)})
}

func (s *Server) handleGetWatermark(w http.ResponseWriter, r *http.Request) {
	wm, err := s.svc.Store().GetWatermark(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wm)
}

type closeWMReq struct {
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
}

func (s *Server) handleCloseWatermark(w http.ResponseWriter, r *http.Request) {
	var req closeWMReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	wm, err := s.svc.CloseWatermarkSource(r.Context(), r.PathValue("id"), req.Source, req.Note)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wm)
}

// ---- publication -----------------------------------------------------------

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var req service.PublishRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	res, err := s.svc.Publish(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	status := http.StatusCreated
	if res.Skipped {
		status = http.StatusAccepted
	}
	writeJSON(w, status, res)
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.svc.Store().ListAllSnapshots(r.Context(), limit)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": list, "count": len(list)})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	sn, err := s.svc.Store().GetSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Replay(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) scopeFromPath(r *http.Request) (string, string) {
	st := r.PathValue("scopeType")
	key := r.PathValue("key")
	return st, key
}

func (s *Server) handleLatestSnapshot(w http.ResponseWriter, r *http.Request) {
	st, key := s.scopeFromPath(r)
	sn, err := s.svc.Store().LatestSnapshot(r.Context(), store.SnapshotQuery{
		ScopeType: st, ScopeKey: key, Kind: r.URL.Query().Get("kind"),
	})
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

func (s *Server) handleSnapshotHistory(w http.ResponseWriter, r *http.Request) {
	st, key := s.scopeFromPath(r)
	list, err := s.svc.Store().ListSnapshots(r.Context(), st, key)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope_type": st, "scope_key": key,
		"snapshots": list, "count": len(list)})
}

func (s *Server) handleSnapshotAsOf(w http.ResponseWriter, r *http.Request) {
	st, key := s.scopeFromPath(r)
	asOf := cutoffParam(r)
	if asOf == nil {
		writeErr(w, http.StatusBadRequest, "missing_as_of",
			"cutoff_received_at query parameter (RFC3339) is required for point-in-time queries", nil)
		return
	}
	q := store.SnapshotQuery{ScopeType: st, ScopeKey: key, AsOf: asOf, Kind: r.URL.Query().Get("kind")}
	sn, err := s.svc.Store().LatestSnapshot(r.Context(), q)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

// ---- operations overview ---------------------------------------------------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := s.svc.Store().EventCounts(ctx)
	if err != nil {
		mapErr(w, err)
		return
	}
	spans, err := s.svc.Store().SourceSpans(ctx)
	if err != nil {
		mapErr(w, err)
		return
	}
	subs, err := s.svc.Store().Subjects(ctx)
	if err != nil {
		mapErr(w, err)
		return
	}
	snaps, err := s.svc.Store().ListAllSnapshots(ctx, 20)
	if err != nil {
		mapErr(w, err)
		return
	}
	wms, err := s.svc.Store().ListWatermarks(ctx)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event_counts": counts, "sources": spans, "subjects": subs,
		"recent_snapshots": snaps, "watermarks": wms,
	})
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
