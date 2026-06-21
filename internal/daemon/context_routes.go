package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/ctxstore"
)

// ctxSetRequest is the body for PUT /context/{key}.
type ctxSetRequest struct {
	Value string `json:"value"`
	By    string `json:"by"` // writer identity; "" -> "human"
}

// ctxCASRequest is the body for POST /context/{key}/cas. Expected "" means "the
// key must be absent".
type ctxCASRequest struct {
	Expected string `json:"expected"`
	Value    string `json:"value"`
	By       string `json:"by"` // writer identity; "" -> "human"
}

// ctxAppendRequest is the body for POST /context/{key}/append.
type ctxAppendRequest struct {
	Value string `json:"value"`
	Sep   string `json:"sep"` // separator inserted before value when the key exists
	By    string `json:"by"`  // writer identity; "" -> "human"
}

// ctxListResponse is the body for GET /context.
type ctxListResponse struct {
	Entries []ctxstore.Entry `json:"entries"`
}

func (s *Server) registerContextRoutes(r chi.Router) {
	r.Put("/context/{key}", s.handleCtxSet)
	r.Post("/context/{key}/cas", s.handleCtxCAS)
	r.Post("/context/{key}/append", s.handleCtxAppend)
	r.Get("/context/{key}", s.handleCtxGet)
	r.Delete("/context/{key}", s.handleCtxDel)
	r.Get("/context", s.handleCtxList)
}

func (s *Server) handleCtxCAS(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	var req ctxCASRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	by := req.By
	if by == "" {
		by = "human"
	}
	e, err := s.cstore.CompareAndSet(key, req.Expected, req.Value, by)
	if errors.Is(err, ctxstore.ErrBadKey) {
		writeErr(w, http.StatusBadRequest, "invalid key")
		return
	}
	if errors.Is(err, ctxstore.ErrConflict) {
		writeErr(w, http.StatusConflict, "value conflict: current value does not match expected")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleCtxAppend(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	var req ctxAppendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	by := req.By
	if by == "" {
		by = "human"
	}
	e, err := s.cstore.Append(key, req.Value, req.Sep, by)
	if errors.Is(err, ctxstore.ErrBadKey) {
		writeErr(w, http.StatusBadRequest, "invalid key")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleCtxSet(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	var req ctxSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	by := req.By
	if by == "" {
		by = "human"
	}
	e, err := s.cstore.Set(key, req.Value, by)
	if errors.Is(err, ctxstore.ErrBadKey) {
		writeErr(w, http.StatusBadRequest, "invalid key")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleCtxGet(w http.ResponseWriter, r *http.Request) {
	e, err := s.cstore.Get(chi.URLParam(r, "key"))
	if errors.Is(err, ctxstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "context key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleCtxDel(w http.ResponseWriter, r *http.Request) {
	err := s.cstore.Del(chi.URLParam(r, "key"))
	if errors.Is(err, ctxstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "context key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleCtxList(w http.ResponseWriter, r *http.Request) {
	entries, err := s.cstore.List(r.URL.Query().Get("prefix"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ctxListResponse{Entries: entries})
}
