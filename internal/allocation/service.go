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
		for _, node := range state.Nodes {
			view := state.NodeView(node)
			// The same predicate that recovery reads and lease list reads use:
			// only a genuinely ready (eligible) node may be offered a lease.
			// Leased, quarantined, draining and drained nodes are all excluded.
			if !view.Participation.Eligible {
				continue
			}
			if input.Region != "" && node.Region != input.Region {
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

		// Double-occupancy guard: even if state somehow disagrees with the
		// eligibility predicate, never create a second claimed lease for a node
		// that already holds one.
		for _, candidate := range candidates {
			if state.ClaimedLeaseCount(candidate.ID) > 0 {
				return domain.NewError(domain.CodeNodeUnavailable, "node already holds a claimed lease", map[string]any{
					"node_id": candidate.ID,
				})
			}
		}

		groupID := s.ids.New("grp")
		leaseIDs := make([]string, 0, len(candidates))
		expiresAt := now.Add(time.Duration(input.LeaseSeconds) * time.Second)
		for _, candidate := range candidates {
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
		if _, ok := state.Nodes[lease.NodeID]; !ok {
			return domain.NewError(domain.CodeStorageFailure, "lease references a missing node", map[string]any{
				"node_id": lease.NodeID,
			})
		}
		finishedAt := now
		lease.State = domain.LeaseReleased
		lease.FinishedAt = &finishedAt
		state.Leases[leaseID] = lease
		// Derive the node posture from the leases that remain claimed and the
		// active maintenance record, so a release while draining converges to
		// drained instead of being silently returned to the ready pool.
		domain.ReconcileNodeAfterLease(state, lease, now)
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

// List returns leases annotated with their node's participation verdict.
// Filters by plan, node and lease state are supported. The participation
// annotation is derived from the same predicate as allocation and recovery
// reads, so every read agrees on whether a node may take part in a rollout.
func (s *Service) List(
	ctx context.Context,
	planID string,
	nodeID string,
	stateFilter domain.LeaseState,
	limit int,
	cursor string,
) (domain.LeasePage, error) {
	if limit < 1 || limit > 100 {
		return domain.LeasePage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	if stateFilter != "" {
		switch stateFilter {
		case domain.LeaseClaimed, domain.LeaseReported, domain.LeaseReleased, domain.LeaseExpired:
		default:
			return domain.LeasePage{}, domain.Validation("lease state is not supported", map[string]any{
				"state": stateFilter,
			})
		}
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.LeasePage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.LeasePage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.NodeLease, 0, len(state.Leases))
		for _, lease := range state.Leases {
			if planID != "" && lease.PlanID != planID {
				continue
			}
			if nodeID != "" && lease.NodeID != nodeID {
				continue
			}
			if stateFilter != "" && lease.State != stateFilter {
				continue
			}
			if after != "" && lease.ID <= after {
				continue
			}
			items = append(items, lease)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			for _, lease := range items[:limit] {
				page.Items = append(page.Items, state.LeaseView(lease))
			}
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		for _, lease := range items {
			page.Items = append(page.Items, state.LeaseView(lease))
		}
		return nil
	})
	return page, err
}

// Get returns a single lease annotated with its node's participation verdict.
func (s *Service) Get(ctx context.Context, leaseID string) (domain.LeaseView, error) {
	var view domain.LeaseView
	err := s.store.Read(ctx, func(state *domain.State) error {
		lease, ok := state.Leases[leaseID]
		if !ok {
			return domain.NotFound("lease", leaseID)
		}
		view = state.LeaseView(lease)
		return nil
	})
	return view, err
}

func expireLeases(state *domain.State, now time.Time) int {
	count := 0
	for leaseID, lease := range state.Leases {
		if lease.State != domain.LeaseClaimed || lease.ExpiresAt.After(now) {
			continue
		}
		finishedAt := now
		lease.State = domain.LeaseExpired
		lease.FinishedAt = &finishedAt
		state.Leases[leaseID] = lease
		// Expiry is the deterministic end of a wait-mode drain: a leased node
		// becomes ready, a draining node becomes drained. The conclusion is
		// computed from remaining claimed leases and the maintenance record.
		domain.ReconcileNodeAfterLease(state, lease, now)
		count++
	}
	return count
}
