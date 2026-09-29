// Package maintenance registers node maintenance windows, drains outstanding
// leases through deterministic policies, and brings recovered nodes back into
// rollout participation.
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

// Begin registers a maintenance window for a node. Under wait the node stops
// receiving new leases while its outstanding claimed leases are allowed to
// converge naturally; release terminates them immediately as released and
// expire as expired.
func (s *Service) Begin(
	ctx context.Context,
	nodeID string,
	input domain.MaintenanceBeginInput,
) (domain.NodeView, error) {
	if err := domain.ValidateMaintenanceBeginInput(input); err != nil {
		return domain.NodeView{}, err
	}
	input = domain.NormalizeMaintenanceBeginInput(input)
	fingerprint := domain.MaintenanceFingerprint(nodeID, input)
	requestIndex := nodeID + "\x00" + input.RequestKey
	now := s.clock.Now().UTC()
	var view domain.NodeView

	err := s.store.Write(ctx, func(state *domain.State) error {
		node, ok := state.Nodes[nodeID]
		if !ok {
			return domain.NotFound("node", nodeID)
		}
		if existingID, exists := state.MaintenanceRequestKeys[requestIndex]; exists {
			existing, found := state.Maintenances[existingID]
			if !found {
				return domain.NewError(domain.CodeStorageFailure, "maintenance request index is inconsistent", map[string]any{
					"maintenance_id": existingID,
				})
			}
			if existing.RequestFingerprint != fingerprint {
				return domain.NewError(domain.CodeIdempotencyConflict, "request key was reused with different maintenance parameters", map[string]any{
					"request_key": input.RequestKey,
				})
			}
			view = state.NodeView(node)
			return nil
		}

		record := domain.NodeMaintenance{
			ID:                 s.ids.New("mnt"),
			NodeID:             nodeID,
			State:              domain.MaintenanceActive,
			Reason:             input.Reason,
			RecoveryCondition:  input.RecoveryCondition,
			DrainMode:          domain.DrainMode(input.DrainMode),
			RequestKey:         input.RequestKey,
			RequestFingerprint: fingerprint,
		}
		updatedNode, record, err := domain.EnterMaintenance(state, node, record, now)
		if err != nil {
			return err
		}
		state.MaintenanceRequestKeys[requestIndex] = record.ID
		store.AddAudit(state, s.ids, "maintenance", record.ID, "begun", now, map[string]any{
			"node_id":              nodeID,
			"reason":               record.Reason,
			"drain_mode":           string(record.DrainMode),
			"affected_lease_count": record.AffectedLeaseCount,
		})
		for _, leaseID := range record.LeaseIDs {
			lease := state.Leases[leaseID]
			store.AddAudit(state, s.ids, "lease", leaseID, "drained_"+string(lease.State), now, map[string]any{
				"maintenance_id": record.ID,
			})
		}
		view = state.NodeView(updatedNode)
		return nil
	})
	return view, err
}

// Cancel aborts an active maintenance window. A wait-mode node whose leases
// are still outstanding goes back to leased; a drained node returns to ready.
func (s *Service) Cancel(
	ctx context.Context,
	nodeID string,
	input domain.MaintenanceCancelInput,
) (domain.NodeView, error) {
	now := s.clock.Now().UTC()
	var view domain.NodeView
	err := s.store.Write(ctx, func(state *domain.State) error {
		node, ok := state.Nodes[nodeID]
		if !ok {
			return domain.NotFound("node", nodeID)
		}
		if node.MaintenanceID == "" {
			return domain.InvalidState("node has no active maintenance", map[string]any{
				"node_id": nodeID,
			})
		}
		record, found := state.Maintenances[node.MaintenanceID]
		if !found {
			return domain.NewError(domain.CodeStorageFailure, "node references a missing maintenance record", map[string]any{
				"maintenance_id": node.MaintenanceID,
			})
		}
		if input.ExpectedVersion != 0 && node.Version != input.ExpectedVersion {
			return versionConflict(nodeID, input.ExpectedVersion, node.Version)
		}
		updatedNode, updatedRecord, err := domain.CancelMaintenance(state, node, record, now)
		if err != nil {
			return err
		}
		store.AddAudit(state, s.ids, "maintenance", record.ID, "canceled", now, map[string]any{
			"node_id": nodeID,
			"state":   string(updatedNode.State),
		})
		view = state.NodeView(updatedNode)
		_ = updatedRecord
		return nil
	})
	return view, err
}

// Recover returns a fully drained node to the ready pool once the recorded
// recovery conditions have been met.
func (s *Service) Recover(
	ctx context.Context,
	nodeID string,
	input domain.MaintenanceRecoverInput,
) (domain.NodeView, error) {
	now := s.clock.Now().UTC()
	var view domain.NodeView
	err := s.store.Write(ctx, func(state *domain.State) error {
		node, ok := state.Nodes[nodeID]
		if !ok {
			return domain.NotFound("node", nodeID)
		}
		if node.MaintenanceID == "" {
			return domain.InvalidState("node has no active maintenance", map[string]any{
				"node_id": nodeID,
			})
		}
		record, found := state.Maintenances[node.MaintenanceID]
		if !found {
			return domain.NewError(domain.CodeStorageFailure, "node references a missing maintenance record", map[string]any{
				"maintenance_id": node.MaintenanceID,
			})
		}
		if input.ExpectedVersion != 0 && node.Version != input.ExpectedVersion {
			return versionConflict(nodeID, input.ExpectedVersion, node.Version)
		}
		updatedNode, _, err := domain.RecoverNode(state, node, record, now)
		if err != nil {
			return err
		}
		store.AddAudit(state, s.ids, "maintenance", record.ID, "recovered", now, map[string]any{
			"node_id": nodeID,
		})
		view = state.NodeView(updatedNode)
		return nil
	})
	return view, err
}

// Get returns a maintenance record by its own identifier.
func (s *Service) Get(ctx context.Context, maintenanceID string) (domain.NodeMaintenance, error) {
	var record domain.NodeMaintenance
	err := s.store.Read(ctx, func(state *domain.State) error {
		found, ok := state.Maintenances[maintenanceID]
		if !ok {
			return domain.NotFound("maintenance", maintenanceID)
		}
		record = found
		return nil
	})
	return record, err
}

// GetForNode returns the active maintenance record for a node, or the most
// recent finished record when no window is active.
func (s *Service) GetForNode(ctx context.Context, nodeID string) (domain.NodeMaintenance, error) {
	var record domain.NodeMaintenance
	err := s.store.Read(ctx, func(state *domain.State) error {
		node, ok := state.Nodes[nodeID]
		if !ok {
			return domain.NotFound("node", nodeID)
		}
		if node.MaintenanceID != "" {
			found, exists := state.Maintenances[node.MaintenanceID]
			if !exists {
				return domain.NewError(domain.CodeStorageFailure, "node references a missing maintenance record", map[string]any{
					"maintenance_id": node.MaintenanceID,
				})
			}
			record = found
			return nil
		}
		var latest *domain.NodeMaintenance
		for _, candidate := range state.Maintenances {
			if candidate.NodeID != nodeID || candidate.State == domain.MaintenanceActive {
				continue
			}
			candidate := candidate
			if latest == nil ||
				candidate.CreatedAt.After(latest.CreatedAt) ||
				(candidate.CreatedAt.Equal(latest.CreatedAt) && candidate.ID > latest.ID) {
				latest = &candidate
			}
		}
		if latest == nil {
			return domain.NotFound("maintenance", nodeID)
		}
		record = *latest
		return nil
	})
	return record, err
}

func versionConflict(nodeID string, expected uint64, actual uint64) *domain.DomainError {
	return domain.NewError(domain.CodeVersionConflict, "node version does not match", map[string]any{
		"node_id":  nodeID,
		"expected": expected,
		"actual":   actual,
	})
}
