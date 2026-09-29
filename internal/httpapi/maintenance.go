package httpapi

import (
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) beginMaintenance(w http.ResponseWriter, r *http.Request) {
	var input domain.MaintenanceBeginInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.maintenances.Begin(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, view)
}

func (s *Server) cancelMaintenance(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeOptionalBody[domain.MaintenanceCancelInput](w, r)
	if !ok {
		return
	}
	view, err := s.maintenances.Cancel(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) recoverNode(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeOptionalBody[domain.MaintenanceRecoverInput](w, r)
	if !ok {
		return
	}
	view, err := s.maintenances.Recover(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) getMaintenance(w http.ResponseWriter, r *http.Request) {
	record, err := s.maintenances.GetForNode(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, record)
}

func (s *Server) getMaintenanceByID(w http.ResponseWriter, r *http.Request) {
	record, err := s.maintenances.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, record)
}
