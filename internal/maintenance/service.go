package maintenance

import (
	"context"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

// Begin registers a maintenance window. The node stops receiving new leases
// immediately; an existing claimed lease converges per the drain policy.
func (s *Service) Begin(
	ctx context.Context,
	nodeID string,
	input domain.MaintenanceStartInput,
) (domain.MaintenanceView, error) {
	if err := domain.ValidateMaintenanceInput(input); err != nil {
		return domain.MaintenanceView{}, err
	}
	input = domain.NormalizeMaintenanceInput(input)
	now := s.clock.Now().UTC()
	var view domain.MaintenanceView
	err := s.store.Write(ctx, func(state *domain.State) error {
		record, err := domain.BeginMaintenance(state, nodeID, s.ids.New("mnt"), input, now)
		if err != nil {
			return err
		}
		store.AddAudit(state, s.ids, "node_maintenance", record.ID, "begun", now, map[string]any{
			"node_id":      nodeID,
			"drain_policy": input.DrainPolicy,
		})
		view, err = domain.BuildMaintenanceView(state, nodeID)
		return err
	})
	return view, err
}

// Cancel aborts a maintenance window that is not still draining a waited lease.
func (s *Service) Cancel(
	ctx context.Context,
	nodeID string,
	expectedVersion uint64,
) (domain.MaintenanceView, error) {
	now := s.clock.Now().UTC()
	var view domain.MaintenanceView
	err := s.store.Write(ctx, func(state *domain.State) error {
		record, err := domain.CancelMaintenance(state, nodeID, expectedVersion, now)
		if err != nil {
			return err
		}
		store.AddAudit(state, s.ids, "node_maintenance", record.ID, "canceled", now, map[string]any{
			"node_id": nodeID,
		})
		view, err = domain.BuildMaintenanceView(state, nodeID)
		return err
	})
	return view, err
}

// Resume returns a fully drained maintained node to service.
func (s *Service) Resume(
	ctx context.Context,
	nodeID string,
	expectedVersion uint64,
) (domain.MaintenanceView, error) {
	now := s.clock.Now().UTC()
	var view domain.MaintenanceView
	err := s.store.Write(ctx, func(state *domain.State) error {
		record, err := domain.ResumeMaintenance(state, nodeID, expectedVersion, now)
		if err != nil {
			return err
		}
		store.AddAudit(state, s.ids, "node_maintenance", record.ID, "resumed", now, map[string]any{
			"node_id": nodeID,
		})
		view, err = domain.BuildMaintenanceView(state, nodeID)
		return err
	})
	return view, err
}

// Get is the recovery read: node, latest maintenance record and the single
// participation conclusion shared with allocation and the lease list.
func (s *Service) Get(ctx context.Context, nodeID string) (domain.MaintenanceView, error) {
	var view domain.MaintenanceView
	err := s.store.Read(ctx, func(state *domain.State) error {
		var err error
		view, err = domain.BuildMaintenanceView(state, nodeID)
		return err
	})
	return view, err
}
