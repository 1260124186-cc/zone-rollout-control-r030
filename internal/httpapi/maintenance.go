package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) beginMaintenance(w http.ResponseWriter, r *http.Request) {
	var input domain.MaintenanceStartInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.maintenance.Begin(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, view)
}

func (s *Server) cancelMaintenance(w http.ResponseWriter, r *http.Request) {
	expectedVersion, err := optionalExpectedVersion(w, r)
	if err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.maintenance.Cancel(r.Context(), r.PathValue("id"), expectedVersion)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) resumeNode(w http.ResponseWriter, r *http.Request) {
	expectedVersion, err := optionalExpectedVersion(w, r)
	if err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.maintenance.Resume(r.Context(), r.PathValue("id"), expectedVersion)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) getMaintenance(w http.ResponseWriter, r *http.Request) {
	view, err := s.maintenance.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

// optionalExpectedVersion reads expected_version from a JSON body that may be
// empty. Maintenance cancel/resume are safe to invoke with no body.
func optionalExpectedVersion(w http.ResponseWriter, r *http.Request) (uint64, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestLimit))
	if err != nil {
		return 0, domain.Validation("request body could not be read", map[string]any{"reason": err.Error()})
	}
	if len(body) == 0 {
		return 0, nil
	}
	var input versionInput
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return 0, domain.Validation("request body must contain valid JSON", map[string]any{"reason": err.Error()})
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return 0, domain.Validation("request body must contain one JSON value", nil)
	}
	if input.ExpectedVersion == 0 {
		return 0, domain.Validation("expected_version must be greater than zero", nil)
	}
	return input.ExpectedVersion, nil
}
