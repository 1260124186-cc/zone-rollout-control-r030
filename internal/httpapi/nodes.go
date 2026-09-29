package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) enrollNode(w http.ResponseWriter, r *http.Request) {
	var input domain.NodeCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	node, err := s.fleet.Enroll(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, node)
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	view, err := s.fleet.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		limit = parsed
	}
	state := domain.NodeState(strings.TrimSpace(r.URL.Query().Get("state")))
	switch state {
	case "",
		domain.NodeReady,
		domain.NodeLeased,
		domain.NodeQuarantined,
		domain.NodeDraining,
		domain.NodeDrained:
	default:
		sendError(w, r, domain.Validation("node state is not supported", map[string]any{"state": state}))
		return
	}
	page, err := s.fleet.List(
		r.Context(),
		state,
		strings.TrimSpace(strings.ToLower(r.URL.Query().Get("region"))),
		limit,
		r.URL.Query().Get("cursor"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}
