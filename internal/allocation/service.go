package allocation

import (
	"context"
	"sort"
	"strings"
	"time"

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

func (s *Service) Allocate(
	ctx context.Context,
	planID string,
	input domain.LeaseInput,
) (domain.LeaseGroup, error) {
	if err := domain.ValidateLeaseInput(input); err != nil {
		return domain.LeaseGroup{}, err
	}
	input.Region = strings.TrimSpace(strings.ToLower(input.Region))
	requestIndex := planID + "\x00" + input.RequestKey
	fingerprint := domain.LeaseFingerprint(planID, input)
	now := s.clock.Now().UTC()
	var group domain.LeaseGroup

	err := s.store.Write(ctx, func(state *domain.State) error {
		plan, ok := state.Plans[planID]
		if !ok {
			return domain.NotFound("plan", planID)
		}
		if plan.State != domain.PlanActive {
			return domain.InvalidState("plan must be active for node leasing", map[string]any{
				"plan_id": planID,
				"state":   plan.State,
			})
		}
		if existingID, exists := state.LeaseRequestKeys[requestIndex]; exists {
			existing, found := state.LeaseGroups[existingID]
			if !found {
				return domain.NewError(domain.CodeStorageFailure, "lease request index is inconsistent", map[string]any{
					"lease_group_id": existingID,
				})
			}
			if existing.RequestFingerprint != fingerprint {
				return domain.NewError(domain.CodeIdempotencyConflict, "request key was reused with different lease parameters", map[string]any{
					"request_key": input.RequestKey,
				})
			}
			group = existing
			return nil
		}

		expireLeases(state, now)
		candidates := make([]domain.ResolverNode, 0)
		for nodeID, node := range state.Nodes {
			if input.Region != "" && node.Region != input.Region {
				continue
			}
			participation := domain.NodeParticipation(state, nodeID)
			if !participation.Eligible {
				continue
			}
			candidates = append(candidates, node)
		}
		if len(candidates) == 0 {
			return domain.NewError(domain.CodeNodeUnavailable, "no eligible node is ready", map[string]any{
				"plan_id": planID,
				"region":  input.Region,
			})
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Region != candidates[j].Region {
				return candidates[i].Region < candidates[j].Region
			}
			return candidates[i].ID < candidates[j].ID
		})
		if len(candidates) > input.MaxNodes {
			candidates = candidates[:input.MaxNodes]
		}

		groupID := s.ids.New("grp")
		leaseIDs := make([]string, 0, len(candidates))
		expiresAt := now.Add(time.Duration(input.LeaseSeconds) * time.Second)
		for _, candidate := range candidates {
			// Re-check inside the loop against the state we are mutating: a node
			// must never end up with two claimed leases even under contention.
			if participation := domain.NodeParticipation(state, candidate.ID); !participation.Eligible {
				return domain.NewError(domain.CodeNodeUnavailable, "node became unavailable during allocation", map[string]any{
					"node_id": candidate.ID,
					"reason":  participation.Reason,
				})
			}
			leaseID := s.ids.New("lse")
			candidate.State = domain.NodeLeased
			candidate.Version++
			candidate.UpdatedAt = now
			state.Nodes[candidate.ID] = candidate
			state.Leases[leaseID] = domain.NodeLease{
				ID:           leaseID,
				PlanID:       planID,
				NodeID:       candidate.ID,
				AllocationID: groupID,
				State:        domain.LeaseClaimed,
				ClaimedAt:    now,
				ExpiresAt:    expiresAt,
			}
			leaseIDs = append(leaseIDs, leaseID)
		}
		group = domain.LeaseGroup{
			ID:                 groupID,
			PlanID:             planID,
			RequestKey:         input.RequestKey,
			RequestFingerprint: fingerprint,
			Region:             input.Region,
			LeaseIDs:           leaseIDs,
			CreatedAt:          now,
		}
		state.LeaseGroups[group.ID] = group
		state.LeaseRequestKeys[requestIndex] = group.ID
		store.AddAudit(state, s.ids, "lease_group", group.ID, "created", now, map[string]any{
			"plan_id":     planID,
			"lease_count": len(leaseIDs),
		})
		return nil
	})
	return group, err
}

func (s *Service) Release(ctx context.Context, leaseID string) (domain.NodeLease, error) {
	now := s.clock.Now().UTC()
	var result domain.NodeLease
	err := s.store.Write(ctx, func(state *domain.State) error {
		lease, ok := state.Leases[leaseID]
		if !ok {
			return domain.NotFound("lease", leaseID)
		}
		if lease.State == domain.LeaseReleased || lease.State == domain.LeaseExpired {
			result = lease
			return nil
		}
		if lease.State != domain.LeaseClaimed {
			return domain.InvalidState("only a claimed lease can be released", map[string]any{
				"lease_id": leaseID,
				"state":    lease.State,
			})
		}
		domain.SettleLease(state, lease, domain.LeaseReleased, now, "")
		store.AddAudit(state, s.ids, "lease", leaseID, "released", now, nil)
		result = state.Leases[leaseID]
		return nil
	})
	return result, err
}

func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.clock.Now().UTC()
	expired := 0
	err := s.store.Write(ctx, func(state *domain.State) error {
		expired = expireLeases(state, now)
		return nil
	})
	return expired, err
}

func expireLeases(state *domain.State, now time.Time) int {
	count := 0
	for _, lease := range state.Leases {
		if lease.State != domain.LeaseClaimed || lease.ExpiresAt.After(now) {
			continue
		}
		domain.SettleLease(state, lease, domain.LeaseExpired, now, "")
		count++
	}
	return count
}

// List returns leases enriched with the participation conclusion of each node.
// It is a read path, so eligibility is computed with the same NodeParticipation
// evaluator used by allocation and recovery reads.
func (s *Service) List(
	ctx context.Context,
	nodeID string,
	planID string,
	stateFilter domain.LeaseState,
	limit int,
	cursor string,
) (domain.LeasePage, error) {
	if limit < 1 || limit > 100 {
		return domain.LeasePage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	if stateFilter != "" &&
		stateFilter != domain.LeaseClaimed &&
		stateFilter != domain.LeaseReported &&
		stateFilter != domain.LeaseReleased &&
		stateFilter != domain.LeaseExpired {
		return domain.LeasePage{}, domain.Validation("lease state is not supported", map[string]any{
			"state": stateFilter,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.LeasePage{}, domain.Validation("cursor is invalid", nil)
	}
	nodeID = strings.TrimSpace(nodeID)
	planID = strings.TrimSpace(planID)
	page := domain.LeasePage{Items: []domain.LeaseView{}}
	err = s.store.Read(ctx, func(state *domain.State) error {
		if nodeID != "" {
			if _, ok := state.Nodes[nodeID]; !ok {
				return domain.NotFound("node", nodeID)
			}
		}
		if planID != "" {
			if _, ok := state.Plans[planID]; !ok {
				return domain.NotFound("plan", planID)
			}
		}
		items := make([]domain.LeaseView, 0, len(state.Leases))
		for _, lease := range state.Leases {
			if nodeID != "" && lease.NodeID != nodeID {
				continue
			}
			if planID != "" && lease.PlanID != planID {
				continue
			}
			if stateFilter != "" && lease.State != stateFilter {
				continue
			}
			if after != "" && lease.ID <= after {
				continue
			}
			items = append(items, domain.LeaseView{
				NodeLease:     lease,
				Participation: domain.NodeParticipation(state, lease.NodeID),
			})
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}
