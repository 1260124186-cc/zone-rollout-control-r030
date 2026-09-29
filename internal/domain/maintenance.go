package domain

import (
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxMaintenanceReasonRunes = 300
	MaxRecoveryConditionRunes = 300
)

// Stable reason codes returned by NodeParticipation.
const (
	ParticipationReady            = "eligible"
	ReasonNodeLeased              = "node_leased"
	ReasonNodeQuarantined         = "node_quarantined"
	ReasonNodeMaintenance         = "node_maintenance"
	ReasonNodeMaintenanceDraining = "node_maintenance_draining"
	ReasonNodeNotReady            = "node_not_ready"
)

// MaintenanceStartInput carries the registered reason and recovery condition.
type MaintenanceStartInput struct {
	Reason            string      `json:"reason"`
	RecoveryCondition string      `json:"recovery_condition"`
	DrainPolicy       DrainPolicy `json:"drain_policy"`
}

func NormalizeMaintenanceInput(input MaintenanceStartInput) MaintenanceStartInput {
	input.Reason = strings.TrimSpace(input.Reason)
	input.RecoveryCondition = strings.TrimSpace(input.RecoveryCondition)
	input.DrainPolicy = DrainPolicy(strings.TrimSpace(strings.ToLower(string(input.DrainPolicy))))
	return input
}

func ValidateMaintenanceInput(input MaintenanceStartInput) error {
	input = NormalizeMaintenanceInput(input)
	details := make(map[string]any)
	if count := utf8.RuneCountInString(input.Reason); count < 1 || count > MaxMaintenanceReasonRunes {
		details["reason"] = "must contain between 1 and 300 characters"
	}
	if count := utf8.RuneCountInString(input.RecoveryCondition); count < 1 || count > MaxRecoveryConditionRunes {
		details["recovery_condition"] = "must contain between 1 and 300 characters"
	}
	switch input.DrainPolicy {
	case DrainWait, DrainRelease, DrainExpire:
	default:
		details["drain_policy"] = "must be wait, release, or expire"
	}
	if len(details) > 0 {
		return Validation("maintenance input is invalid", details)
	}
	return nil
}

// ActiveLeaseForNode returns the claimed lease currently occupying a node and
// whether one exists. It is the single source of truth for node occupancy.
func ActiveLeaseForNode(state *State, nodeID string) (NodeLease, bool) {
	for _, lease := range state.Leases {
		if lease.NodeID == nodeID && lease.State == LeaseClaimed {
			return lease, true
		}
	}
	return NodeLease{}, false
}

// ActiveMaintenanceForNode returns the active maintenance record of a node.
func ActiveMaintenanceForNode(state *State, nodeID string) (NodeMaintenanceRecord, bool) {
	maintenance, ok := state.Maintenances[nodeID]
	if !ok || maintenance.Status != MaintenanceActive {
		return NodeMaintenanceRecord{}, false
	}
	return maintenance, true
}

// NodeParticipation decides whether a node can take a new lease. Every read
// path (recovery read, allocation read and lease list) must derive its answer
// from this function so they can never disagree.
func NodeParticipation(state *State, nodeID string) Participation {
	answer := Participation{NodeID: nodeID, Eligible: false}
	node, ok := state.Nodes[nodeID]
	if !ok {
		answer.NodeState = ""
		answer.Reason = ReasonNodeNotReady
		return answer
	}
	answer.NodeState = node.State
	if maintenance, active := ActiveMaintenanceForNode(state, nodeID); active {
		answer.InMaintenance = true
		answer.Drained = maintenance.Drained
		if maintenance.Drained {
			answer.Reason = ReasonNodeMaintenance
		} else {
			answer.Reason = ReasonNodeMaintenanceDraining
		}
		if lease, claimed := ActiveLeaseForNode(state, nodeID); claimed {
			answer.ActiveLeaseID = lease.ID
		}
		return answer
	}
	if lease, claimed := ActiveLeaseForNode(state, nodeID); claimed {
		answer.Reason = ReasonNodeLeased
		answer.ActiveLeaseID = lease.ID
		return answer
	}
	switch node.State {
	case NodeReady:
		answer.Eligible = true
		answer.Reason = ParticipationReady
	case NodeQuarantined:
		answer.Reason = ReasonNodeQuarantined
	case NodeMaintenance:
		answer.Reason = ReasonNodeMaintenance
	default:
		answer.Reason = ReasonNodeNotReady
	}
	return answer
}

// SettleLease converges a claimed lease to a terminal state. It is the only
// place that flips a lease to reported/released/expired and always applies the
// same node rule: a node with active maintenance stays under maintenance and
// has its drain progress recorded; otherwise it returns to ready. It never
// creates a second occupancy.
func SettleLease(
	state *State,
	lease NodeLease,
	target LeaseState,
	at time.Time,
	probeID string,
) {
	if lease.State != LeaseClaimed {
		return
	}
	finishedAt := at.UTC()
	lease.State = target
	lease.FinishedAt = &finishedAt
	if target == LeaseReported {
		lease.ProbeID = probeID
	}
	state.Leases[lease.ID] = lease

	node, ok := state.Nodes[lease.NodeID]
	if !ok {
		return
	}
	maintenance, underMaintenance := state.Maintenances[lease.NodeID]
	if underMaintenance && maintenance.Status == MaintenanceActive {
		maintenance.OutstandingLease = ""
		maintenance.Drained = true
		state.Maintenances[lease.NodeID] = maintenance
		node.State = NodeMaintenance
		node.Version++
		node.UpdatedAt = finishedAt
		state.Nodes[node.ID] = node
		return
	}
	if node.State == NodeLeased || node.State == NodeMaintenance {
		node.State = NodeReady
		node.Version++
		node.UpdatedAt = finishedAt
		state.Nodes[node.ID] = node
	}
}

// BeginMaintenance moves a node into maintenance, registering the reason and
// recovery condition and converging an existing claimed lease per policy.
func BeginMaintenance(
	state *State,
	nodeID string,
	maintenanceID string,
	input MaintenanceStartInput,
	now time.Time,
) (NodeMaintenanceRecord, error) {
	node, ok := state.Nodes[nodeID]
	if !ok {
		return NodeMaintenanceRecord{}, NotFound("node", nodeID)
	}
	if existing, active := ActiveMaintenanceForNode(state, nodeID); active {
		return NodeMaintenanceRecord{}, NewError(CodeMaintenanceActive, "node is already under maintenance", map[string]any{
			"node_id":        nodeID,
			"maintenance_id": existing.ID,
		})
	}
	if node.State == NodeQuarantined {
		return NodeMaintenanceRecord{}, InvalidState("quarantined node must be released before maintenance", map[string]any{
			"node_id": nodeID,
			"state":   node.State,
		})
	}

	at := now.UTC()
	maintenance := NodeMaintenanceRecord{
		ID:                maintenanceID,
		NodeID:            nodeID,
		Reason:            input.Reason,
		RecoveryCondition: input.RecoveryCondition,
		DrainPolicy:       input.DrainPolicy,
		Status:            MaintenanceActive,
		BegunAt:           at,
	}

	// Converge an existing claimed lease first. The maintenance record is not
	// visible yet, so SettleLease returns the node to ready inside this
	// transaction; it is then moved to maintenance below. No other goroutine
	// can observe the intermediate state because the whole change is serialized
	// under the store write lock.
	if lease, claimed := ActiveLeaseForNode(state, nodeID); claimed {
		switch input.DrainPolicy {
		case DrainWait:
			maintenance.Drained = false
			maintenance.OutstandingLease = lease.ID
		case DrainRelease:
			SettleLease(state, lease, LeaseReleased, at, "")
			maintenance.Drained = true
		case DrainExpire:
			SettleLease(state, lease, LeaseExpired, at, "")
			maintenance.Drained = true
		}
	} else {
		maintenance.Drained = true
	}

	node = state.Nodes[nodeID]
	node.State = NodeMaintenance
	node.Version++
	node.UpdatedAt = at
	state.Nodes[nodeID] = node
	state.Maintenances[nodeID] = maintenance
	return maintenance, nil
}

// CancelMaintenance aborts an active maintenance window. When a waited lease is
// still outstanding the node returns to leased (the lease keeps occupying it);
// otherwise it returns to ready. Either way the node can be placed under
// maintenance again afterwards.
func CancelMaintenance(state *State, nodeID string, expectedVersion uint64, now time.Time) (NodeMaintenanceRecord, error) {
	node, ok := state.Nodes[nodeID]
	if !ok {
		return NodeMaintenanceRecord{}, NotFound("node", nodeID)
	}
	maintenance, active := state.Maintenances[nodeID]
	if !active || maintenance.Status != MaintenanceActive {
		return NodeMaintenanceRecord{}, NewError(CodeNotInMaintenance, "node is not under maintenance", map[string]any{
			"node_id": nodeID,
		})
	}
	if expectedVersion != 0 && node.Version != expectedVersion {
		return NodeMaintenanceRecord{}, NewError(CodeVersionConflict, "node version does not match", map[string]any{
			"node_id":  nodeID,
			"expected": expectedVersion,
			"actual":   node.Version,
		})
	}

	at := now.UTC()
	maintenance.CanceledAt = &at
	maintenance.Status = MaintenanceCanceled
	maintenance.OutstandingLease = ""
	maintenance.Drained = true
	state.Maintenances[nodeID] = maintenance

	if _, claimed := ActiveLeaseForNode(state, nodeID); claimed {
		node.State = NodeLeased
	} else {
		node.State = NodeReady
	}
	node.Version++
	node.UpdatedAt = at
	state.Nodes[nodeID] = node
	return maintenance, nil
}

// ResumeMaintenance completes maintenance and returns a fully drained node to
// service. A node that is still draining a waited lease must wait for that
// lease to report, release or expire before it can resume.
func ResumeMaintenance(state *State, nodeID string, expectedVersion uint64, now time.Time) (NodeMaintenanceRecord, error) {
	node, ok := state.Nodes[nodeID]
	if !ok {
		return NodeMaintenanceRecord{}, NotFound("node", nodeID)
	}
	maintenance, active := state.Maintenances[nodeID]
	if !active || maintenance.Status != MaintenanceActive {
		return NodeMaintenanceRecord{}, NewError(CodeNotInMaintenance, "node is not under maintenance", map[string]any{
			"node_id": nodeID,
		})
	}
	if expectedVersion != 0 && node.Version != expectedVersion {
		return NodeMaintenanceRecord{}, NewError(CodeVersionConflict, "node version does not match", map[string]any{
			"node_id":  nodeID,
			"expected": expectedVersion,
			"actual":   node.Version,
		})
	}
	if lease, claimed := ActiveLeaseForNode(state, nodeID); claimed {
		return NodeMaintenanceRecord{}, NewError(CodeMaintenanceDraining, "node is still draining an active lease", map[string]any{
			"node_id":  nodeID,
			"lease_id": lease.ID,
		})
	}
	if !maintenance.Drained {
		return NodeMaintenanceRecord{}, NewError(CodeMaintenanceDraining, "node maintenance has not drained yet", map[string]any{
			"node_id": nodeID,
		})
	}

	at := now.UTC()
	maintenance.ResumedAt = &at
	maintenance.Status = MaintenanceResumed
	maintenance.OutstandingLease = ""
	state.Maintenances[nodeID] = maintenance

	node.State = NodeReady
	node.Version++
	node.UpdatedAt = at
	state.Nodes[nodeID] = node
	return maintenance, nil
}

// BuildMaintenanceView returns the recovery read for a node.
func BuildMaintenanceView(state *State, nodeID string) (MaintenanceView, error) {
	node, ok := state.Nodes[nodeID]
	if !ok {
		return MaintenanceView{}, NotFound("node", nodeID)
	}
	view := MaintenanceView{
		Node:          node,
		Participation: NodeParticipation(state, nodeID),
	}
	if maintenance, ok := state.Maintenances[nodeID]; ok {
		record := maintenance
		view.Maintenance = &record
	}
	return view, nil
}
