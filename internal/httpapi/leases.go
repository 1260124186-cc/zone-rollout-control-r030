package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) allocateLeases(w http.ResponseWriter, r *http.Request) {
	var input domain.LeaseInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	group, err := s.allocations.Allocate(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, group)
}

func (s *Server) listLeases(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		limit = parsed
	}
	leaseState := domain.LeaseState(strings.TrimSpace(r.URL.Query().Get("state")))
	switch leaseState {
	case "",
		domain.LeaseClaimed,
		domain.LeaseReported,
		domain.LeaseReleased,
		domain.LeaseExpired:
	default:
		sendError(w, r, domain.Validation("lease state is not supported", map[string]any{"state": leaseState}))
		return
	}
	page, err := s.allocations.List(
		r.Context(),
		strings.TrimSpace(r.URL.Query().Get("plan_id")),
		strings.TrimSpace(r.URL.Query().Get("node_id")),
		leaseState,
		limit,
		r.URL.Query().Get("cursor"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) getLease(w http.ResponseWriter, r *http.Request) {
	view, err := s.allocations.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) releaseLease(w http.ResponseWriter, r *http.Request) {
	lease, err := s.allocations.Release(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, lease)
}
