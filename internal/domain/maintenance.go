package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxMaintenanceReasonRunes = 300
	MaxRecoveryConditionRunes = 500
)

type MaintenanceBeginInput struct {
	RequestKey        string `json:"request_key"`
	Reason            string `json:"reason"`
	RecoveryCondition string `json:"recovery_condition"`
	DrainMode         string `json:"drain_mode"`
}

type MaintenanceCancelInput struct {
	ExpectedVersion uint64 `json:"expected_version"`
}

type MaintenanceRecoverInput struct {
	ExpectedVersion uint64 `json:"expected_version"`
}

// ValidateMaintenanceBeginInput validates the maintenance registration. The
// reason and the condition under which the node may come back are both
// mandatory: a maintenance window without a recorded exit condition never
// converges.
func ValidateMaintenanceBeginInput(input MaintenanceBeginInput) error {
	input = NormalizeMaintenanceBeginInput(input)
	details := make(map[string]any)
	if err := validateRequestKey(input.RequestKey); err != nil {
		details["request_key"] = err.Error()
	}
	if count := utf8.RuneCountInString(input.Reason); count < 3 || count > MaxMaintenanceReasonRunes {
		details["reason"] = "must contain between 3 and 300 characters"
	}
	if count := utf8.RuneCountInString(input.RecoveryCondition); count < 3 || count > MaxRecoveryConditionRunes {
		details["recovery_condition"] = "must contain between 3 and 500 characters"
	}
	if DrainMode(input.DrainMode) != DrainWait &&
		DrainMode(input.DrainMode) != DrainRelease &&
		DrainMode(input.DrainMode) != DrainExpire {
		details["drain_mode"] = "must be one of wait, release, or expire"
	}
	if len(details) > 0 {
		return Validation("maintenance input is invalid", details)
	}
	return nil
}

func NormalizeMaintenanceBeginInput(input MaintenanceBeginInput) MaintenanceBeginInput {
	input.RequestKey = strings.TrimSpace(input.RequestKey)
	input.Reason = strings.TrimSpace(input.Reason)
	input.RecoveryCondition = strings.TrimSpace(input.RecoveryCondition)
	input.DrainMode = strings.ToLower(strings.TrimSpace(input.DrainMode))
	if input.DrainMode == "" {
		input.DrainMode = string(DrainWait)
	}
	return input
}

// MaintenanceFingerprint binds the maintenance request key to its reason,
// recovery condition and drain policy so same-key replays are detectable.
func MaintenanceFingerprint(nodeID string, input MaintenanceBeginInput) string {
	payload := fmt.Sprintf(
		"%s|%s|%s|%s|%s",
		nodeID,
		input.RequestKey,
		input.Reason,
		input.RecoveryCondition,
		input.DrainMode,
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// NodeParticipation is the unique predicate deciding whether a node may take
// part in a rollout. Every read path that needs the answer — recovery reads,
// allocation candidate reads and lease list reads — must go through it.
func NodeParticipation(node ResolverNode, maintenance *NodeMaintenance) Participation {
	answer := Participation{State: node.State}
	switch node.State {
	case NodeReady:
		answer.Eligible = true
		answer.Reason = ParticipationReasonReady
	case NodeLeased:
		answer.Eligible = false
		answer.Reason = ParticipationReasonLeased
	case NodeQuarantined:
		answer.Eligible = false
		answer.Reason = ParticipationReasonQuarantined
	case NodeDraining:
		answer.Eligible = false
		if maintenance != nil && maintenance.State == MaintenanceActive && maintenance.DrainMode == DrainWait {
			answer.Reason = ParticipationReasonMaintenanceWait
		} else {
			answer.Reason = ParticipationReasonMaintenanceForce
		}
	case NodeDrained:
		answer.Eligible = false
		answer.Reason = ParticipationReasonDrained
	default:
		answer.Eligible = false
		answer.Reason = ParticipationReasonUnknown
	}
	return answer
}

// ClaimedLeaseCount returns how many claimed leases reference a node. The
// ownership invariant is "a node is leased or draining iff it has at least one
// claimed lease"; deriving state from this count rather than from a transition
// label is what prevents double occupancy under concurrent writers.
func (s *State) ClaimedLeaseCount(nodeID string) int {
	count := 0
	for _, lease := range s.Leases {
		if lease.NodeID == nodeID && lease.State == LeaseClaimed {
			count++
		}
	}
	return count
}

func (s *State) activeMaintenance(nodeID string) *NodeMaintenance {
	maintenanceID, ok := nodeMaintenanceID(s, nodeID)
	if !ok {
		return nil
	}
	maintenance, found := s.Maintenances[maintenanceID]
	if !found || maintenance.State != MaintenanceActive {
		return nil
	}
	return &maintenance
}

func nodeMaintenanceID(s *State, nodeID string) (string, bool) {
	node, ok := s.Nodes[nodeID]
	if !ok || node.MaintenanceID == "" {
		return "", false
	}
	return node.MaintenanceID, true
}

// NodeView builds the recovery-read view from an already loaded node.
func (s *State) NodeView(node ResolverNode) NodeView {
	var maintenance *NodeMaintenance
	if node.MaintenanceID != "" {
		if found, ok := s.Maintenances[node.MaintenanceID]; ok {
			maintenance = &found
		}
	}
	return NodeView{
		ResolverNode:  node,
		Participation: NodeParticipation(node, maintenance),
		Maintenance:   maintenance,
	}
}

// LeaseView annotates a lease with the participation verdict of its node.
func (s *State) LeaseView(lease NodeLease) LeaseView {
	node, ok := s.Nodes[lease.NodeID]
	if !ok {
		return LeaseView{
			NodeLease:     lease,
			Participation: Participation{State: "", Eligible: false, Reason: ParticipationReasonUnknown},
		}
	}
	var maintenance *NodeMaintenance
	if node.MaintenanceID != "" {
		if found, exists := s.Maintenances[node.MaintenanceID]; exists {
			maintenance = &found
		}
	}
	return LeaseView{
		NodeLease:     lease,
		NodeState:     node.State,
		Participation: NodeParticipation(node, maintenance),
	}
}

// EnterMaintenance registers a maintenance window and applies the deterministic
// convergence policy to leases that are still claimed. The node ends in the
// draining state under wait, and in the drained state under release/expire.
func EnterMaintenance(
	state *State,
	node ResolverNode,
	maintenance NodeMaintenance,
	now time.Time,
) (ResolverNode, NodeMaintenance, error) {
	if node.State == NodeQuarantined {
		return ResolverNode{}, NodeMaintenance{}, InvalidState("quarantined nodes must be un-quarantined before maintenance", map[string]any{
			"node_id": node.ID,
			"state":   node.State,
		})
	}
	if node.MaintenanceID != "" {
		if existing, ok := state.Maintenances[node.MaintenanceID]; ok && existing.State == MaintenanceActive {
			return ResolverNode{}, NodeMaintenance{}, InvalidState("node is already under active maintenance", map[string]any{
				"node_id":        node.ID,
				"maintenance_id": node.MaintenanceID,
			})
		}
	}
	if node.State != NodeReady && node.State != NodeLeased {
		return ResolverNode{}, NodeMaintenance{}, InvalidState("node can enter maintenance only from ready or leased", map[string]any{
			"node_id": node.ID,
			"state":   node.State,
		})
	}

	claimed := claimedLeaseIDs(state, node.ID)
	maintenance.LeaseIDs = claimed
	maintenance.AffectedLeaseCount = len(claimed)
	maintenance.CreatedAt = now.UTC()

	switch {
	case len(claimed) == 0:
		// Nothing outstanding: the node is empty and converges immediately.
		node.State = NodeDrained
	case maintenance.DrainMode == DrainWait:
		// Deterministic convergence is deferred to natural release/expiry.
		node.State = NodeDraining
	default:
		for _, leaseID := range claimed {
			lease := state.Leases[leaseID]
			finishedAt := now.UTC()
			switch maintenance.DrainMode {
			case DrainRelease:
				lease.State = LeaseReleased
			case DrainExpire:
				lease.State = LeaseExpired
			default:
				return ResolverNode{}, NodeMaintenance{}, InvalidState("unsupported drain mode", map[string]any{
					"drain_mode": maintenance.DrainMode,
				})
			}
			lease.FinishedAt = &finishedAt
			state.Leases[leaseID] = lease
		}
		node.State = NodeDrained
	}
	node.MaintenanceID = maintenance.ID
	node.Version++
	node.UpdatedAt = now.UTC()
	state.Nodes[node.ID] = node
	state.Maintenances[maintenance.ID] = maintenance
	return node, maintenance, nil
}

// CancelMaintenance abandons a maintenance window. Under wait the node keeps
// servicing any outstanding claimed leases and goes back to leased; once
// drained there is nothing to converge and it becomes ready immediately.
func CancelMaintenance(
	state *State,
	node ResolverNode,
	maintenance NodeMaintenance,
	now time.Time,
) (ResolverNode, NodeMaintenance, error) {
	if maintenance.State != MaintenanceActive || node.MaintenanceID != maintenance.ID {
		return ResolverNode{}, NodeMaintenance{}, InvalidState("maintenance is not active for this node", map[string]any{
			"node_id":        node.ID,
			"maintenance_id": maintenance.ID,
		})
	}
	if node.State != NodeDraining && node.State != NodeDrained {
		return ResolverNode{}, NodeMaintenance{}, InvalidState("node can cancel maintenance only while draining or drained", map[string]any{
			"node_id": node.ID,
			"state":   node.State,
		})
	}
	claimed := state.ClaimedLeaseCount(node.ID)
	switch {
	case claimed > 0:
		// The node is still occupied: cancellation hands the lease back to
		// normal rotation and the node remains leased.
		node.State = NodeLeased
	default:
		// Fully drained (or emptied before any lease was claimed): the node
		// returns to the pool immediately.
		node.State = NodeReady
	}
	canceledAt := now.UTC()
	maintenance.State = MaintenanceCanceled
	maintenance.CanceledAt = &canceledAt
	node.MaintenanceID = ""
	node.Version++
	node.UpdatedAt = canceledAt
	state.Nodes[node.ID] = node
	state.Maintenances[maintenance.ID] = maintenance
	return node, maintenance, nil
}

// RecoverNode brings a drained node back into rotation after its recorded
// recovery conditions have been met. A node with outstanding claimed leases
// has not finished draining and cannot recover yet.
func RecoverNode(
	state *State,
	node ResolverNode,
	maintenance NodeMaintenance,
	now time.Time,
) (ResolverNode, NodeMaintenance, error) {
	if maintenance.State != MaintenanceActive || node.MaintenanceID != maintenance.ID {
		return ResolverNode{}, NodeMaintenance{}, InvalidState("maintenance is not active for this node", map[string]any{
			"node_id":        node.ID,
			"maintenance_id": maintenance.ID,
		})
	}
	if node.State != NodeDrained {
		return ResolverNode{}, NodeMaintenance{}, InvalidState("node can recover only after it has fully drained", map[string]any{
			"node_id": node.ID,
			"state":   node.State,
		})
	}
	if claimed := state.ClaimedLeaseCount(node.ID); claimed > 0 {
		return ResolverNode{}, NodeMaintenance{}, InvalidState("node cannot recover while it still holds claimed leases", map[string]any{
			"node_id":        node.ID,
			"claimed_leases": claimed,
		})
	}
	recoveredAt := now.UTC()
	maintenance.State = MaintenanceRecovered
	maintenance.RecoveredAt = &recoveredAt
	node.State = NodeReady
	node.MaintenanceID = ""
	node.Version++
	node.UpdatedAt = recoveredAt
	state.Nodes[node.ID] = node
	state.Maintenances[maintenance.ID] = maintenance
	return node, maintenance, nil
}

// ReconcileNodeAfterLease derives node state after a lease reaches a terminal
// state (reported, released or expired). The result is computed from the
// remaining claimed-lease count and the active maintenance record instead of
// being assumed, so wait-mode draining converges deterministically and no
// lease can ever be double-occupied.
func ReconcileNodeAfterLease(
	state *State,
	lease NodeLease,
	now time.Time,
) {
	node, ok := state.Nodes[lease.NodeID]
	if !ok {
		return
	}
	target := node.State
	switch {
	case state.ClaimedLeaseCount(node.ID) > 0:
		// Still occupied by another claimed lease: keep the current posture.
	case node.State == NodeLeased:
		target = NodeReady
	case node.State == NodeDraining:
		// Last outstanding lease converged: the maintenance window is drained.
		// If the window was canceled in the meantime, the node simply returns
		// to the ready pool.
		if state.activeMaintenance(node.ID) != nil {
			target = NodeDrained
		} else {
			target = NodeReady
		}
	}
	if target != node.State {
		node.State = target
		node.Version++
		node.UpdatedAt = now.UTC()
		state.Nodes[node.ID] = node
	}
}

func claimedLeaseIDs(state *State, nodeID string) []string {
	ids := make([]string, 0)
	for id, lease := range state.Leases {
		if lease.NodeID == nodeID && lease.State == LeaseClaimed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
