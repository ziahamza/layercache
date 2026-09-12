package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/cloud"
)

func (server *Server) cacheLimit(ctx context.Context) (int64, error) {
	if server.cloudStore != nil {
		quota, err := server.cloudStore.Quota(ctx)
		return quota.MaxBytes, err
	}
	return server.config.MaxBytes, nil
}

func (server *Server) cacheAdministrationRoutes() {
	server.mux.Handle("GET /v1/cache/quota", server.requireToken(http.HandlerFunc(server.readCacheQuota)))
	server.mux.Handle("PUT /v1/cache/quota", server.requireToken(http.HandlerFunc(server.changeCacheQuota)))
	server.mux.Handle("GET /v1/cache/pins", server.requireToken(http.HandlerFunc(server.listCachePins)))
	server.mux.Handle("PUT /v1/cache/pins/{owner}", server.requireToken(http.HandlerFunc(server.setCachePin)))
	server.mux.Handle("DELETE /v1/cache/pins/{owner}", server.requireToken(http.HandlerFunc(server.removeCachePin)))
}

func (server *Server) readCacheQuota(w http.ResponseWriter, r *http.Request) {
	quota, err := server.cloudStore.Quota(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "project quota unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

func (server *Server) changeCacheQuota(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MaxBytes int64 `json:"maxBytes"`
	}
	if err := decodeCloudJSON(w, r, &body); err != nil || body.MaxBytes <= 0 || body.MaxBytes == int64(^uint64(0)>>1) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "maxBytes must be a positive bounded integer"})
		return
	}
	quota, err := server.cloudStore.SetQuota(r.Context(), requestActor(r), body.MaxBytes)
	if errors.Is(err, cloud.ErrQuota) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "pins prevent the requested quota; quota and entries are unchanged"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "quota change could not be confirmed; read quota before retrying"})
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

func (server *Server) listCachePins(w http.ResponseWriter, r *http.Request) {
	limit, err := parseBoundedInteger(r.URL.Query().Get("limit"), 100, 1000)
	if err != nil || limit == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pin page size"})
		return
	}
	pins, err := server.cloudStore.AdministratorPins(r.Context(), r.URL.Query().Get("after"), int(limit))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read pin page"})
		return
	}
	next := ""
	if len(pins) == int(limit) {
		next = pins[len(pins)-1].Owner
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins, "nextCursor": next})
}

func (server *Server) setCachePin(w http.ResponseWriter, r *http.Request) {
	var pin artifact.Pin
	if err := decodeCloudJSON(w, r, &pin); err != nil || pin.Owner != "" && pin.Owner != r.PathValue("owner") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid administrator pin"})
		return
	}
	pin.Owner = r.PathValue("owner")
	if pin.Key.Project != server.config.ProjectID {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	err := server.cloudStore.SetAdministratorPin(r.Context(), requestActor(r), pin)
	if errors.Is(err, cloud.ErrNotFound) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if errors.Is(err, cloud.ErrConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "pin identity differs from the existing entry or pin"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "administrator pin could not be confirmed"})
		return
	}
	writeJSON(w, http.StatusOK, pin)
}

func (server *Server) removeCachePin(w http.ResponseWriter, r *http.Request) {
	if err := server.cloudStore.RemoveAdministratorPin(r.Context(), requestActor(r), r.PathValue("owner")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "administrator pin release could not be confirmed"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
