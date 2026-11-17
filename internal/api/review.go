package api

import (
	"net/http"

	"cargo.example/secstate/internal/domain/review"
	"cargo.example/secstate/internal/service"
)

// Historical consistency review endpoints. These serve the research question:
// can the shipment, container and leg states in one query all hold under one
// set of history facts, and which of them are provably contradictory versus
// merely undecided.

func (s *Server) reviewRoutes() {
	m := s.mux
	m.HandleFunc("POST /v1/reviews/scopes", s.handleReviewScopeRegister)
	m.HandleFunc("GET /v1/reviews/scopes", s.handleReviewScopeList)
	m.HandleFunc("GET /v1/reviews/scopes/{id}", s.handleReviewScopeGet)
	m.HandleFunc("GET /v1/reviews/scopes/{id}/material", s.handleReviewMaterial)

	m.HandleFunc("POST /v1/reviews/scopes/{id}/imports", s.handleReviewImport)

	m.HandleFunc("POST /v1/reviews/cases", s.handleReviewCaseCreate)
	m.HandleFunc("POST /v1/reviews/cases/{id}/runs", s.handleReviewRun)
	m.HandleFunc("POST /v1/reviews/cases/{id}/constraints", s.handleReviewConstraintCheck)
	m.HandleFunc("GET /v1/reviews/cases/{id}/runs", s.handleReviewRunList)

	m.HandleFunc("GET /v1/reviews/runs/{id}", s.handleReviewRunGet)
	m.HandleFunc("GET /v1/reviews/runs/{id}/examples", s.handleReviewRunExamples)
	m.HandleFunc("POST /v1/reviews/runs/{id}/reproduce", s.handleReviewReproduce)
	m.HandleFunc("POST /v1/reviews/runs/{id}/export", s.handleReviewExport)
}

func (s *Server) handleReviewScopeRegister(w http.ResponseWriter, r *http.Request) {
	var req service.ReviewScopeRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	sc, err := s.svc.RegisterReviewScope(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Server) handleReviewScopeList(w http.ResponseWriter, r *http.Request) {
	scopes, err := s.svc.ListReviewScopes(r.Context())
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes, "count": len(scopes)})
}

func (s *Server) handleReviewScopeGet(w http.ResponseWriter, r *http.Request) {
	sc, err := s.svc.GetReviewScope(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) handleReviewMaterial(w http.ResponseWriter, r *http.Request) {
	sum, err := s.svc.ReviewMaterial(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) handleReviewImport(w http.ResponseWriter, r *http.Request) {
	var body review.LogImport
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	req := service.ReviewImportRequest{ScopeID: r.PathValue("id"), LogImport: body}
	if k := r.URL.Query().Get("batch_key"); k != "" {
		req.BatchKey = k
	}
	out, err := s.svc.ImportReviewLogs(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	status := http.StatusCreated
	if out.Imported == 0 && len(out.LateArrivals) > 0 {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

func (s *Server) handleReviewCaseCreate(w http.ResponseWriter, r *http.Request) {
	var req service.CreateReviewCaseRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	c, err := s.svc.CreateReviewCase(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleReviewRun(w http.ResponseWriter, r *http.Request) {
	var req service.AnalyzeCaseRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	req.CaseID = r.PathValue("id")
	res, err := s.svc.AnalyzeCase(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleReviewConstraintCheck(w http.ResponseWriter, r *http.Request) {
	var req service.ConstraintCheckRequest
	_ = decode(r, &req)
	req.CaseID = r.PathValue("id")
	out, err := s.svc.CheckReviewConstraints(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReviewRunList(w http.ResponseWriter, r *http.Request) {
	runs, err := s.svc.ListReviewRuns(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"case_id": r.PathValue("id"), "runs": runs, "count": len(runs)})
}

func (s *Server) handleReviewRunGet(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.GetReviewRun(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleReviewRunExamples(w http.ResponseWriter, r *http.Request) {
	ex, err := s.svc.GetReviewExamples(r.Context(), r.PathValue("id"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": r.PathValue("id"), "examples": ex, "count": len(ex)})
}

type reproduceReq struct {
	Budget int `json:"budget,omitempty"`
}

func (s *Server) handleReviewReproduce(w http.ResponseWriter, r *http.Request) {
	var req reproduceReq
	_ = decode(r, &req)
	res, err := s.svc.ReproduceRun(r.Context(), r.PathValue("id"), req.Budget)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleReviewExport(w http.ResponseWriter, r *http.Request) {
	var req service.ExportReviewRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	req.RunID = r.PathValue("id")
	view, err := s.svc.ExportReview(r.Context(), req)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
