package workflowprobe

import (
	"fmt"
	"net/http"
	"sync"

	"solo-0014-zone-rollout-control/internal/domain"
)

func runNodeMaintenanceDrain() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	east := make([]domain.ResolverNode, 0, 4)
	for _, name := range []string{"resolver-maint-01", "resolver-maint-02", "resolver-maint-03"} {
		node, err := enrollNode(harness, name, "east")
		if err != nil {
			return err
		}
		east = append(east, node)
	}
	plan, _, err := createActivePlan(harness, "maint.example", 5001, 3, 1, 100)
	if err != nil {
		return err
	}

	// --- Section 1: wait mode stops new leases and converges on release ---
	first, err := allocate(harness, plan.ID, "maint-lease-0001", "east", 2)
	if err != nil {
		return err
	}
	if len(first.LeaseIDs) != 2 {
		return ensure(false, "first group must lease two east nodes")
	}
	ownerA := reportOwner(harness, first.LeaseIDs[0])
	ownerB := reportOwner(harness, first.LeaseIDs[1])
	if ownerA == "" || ownerB == "" || ownerA == ownerB {
		return ensure(false, "first group must occupy two distinct nodes")
	}
	// The enrolled east node not covered by the group is the ready leftover.
	ownerReady := ""
	leasedSet := map[string]bool{ownerA: true, ownerB: true}
	for _, node := range east {
		if !leasedSet[node.ID] {
			ownerReady = node.ID
		}
	}
	if ownerReady == "" {
		return ensure(false, "one east node must remain ready")
	}
	n1Lease := ""
	for _, leaseID := range first.LeaseIDs {
		if reportOwner(harness, leaseID) == ownerA {
			n1Lease = leaseID
		}
	}
	n1, n2, n3 := ownerA, ownerB, ownerReady

	beginView, err := beginMaintenance(harness, n1, "maint-begin-0001", "hardware replacement", "replacement verified", "wait")
	if err != nil {
		return err
	}
	if beginView.State != domain.NodeDraining {
		return ensure(false, "wait maintenance on a leased node must be draining")
	}
	if beginView.Participation.Eligible || beginView.Participation.Reason != domain.ParticipationReasonMaintenanceWait {
		return ensure(false, "draining wait node must be ineligible with the wait reason")
	}
	if beginView.Maintenance == nil || beginView.Maintenance.State != domain.MaintenanceActive {
		return ensure(false, "begin response must embed the active maintenance record")
	}
	if beginView.Maintenance.AffectedLeaseCount != 1 {
		return ensure(false, "maintenance must register the outstanding lease")
	}

	// While n1 drains, only the still-ready n3 may be selected.
	during, err := allocate(harness, plan.ID, "maint-lease-draining-01", "east", 2)
	if err != nil {
		return err
	}
	if len(during.LeaseIDs) != 1 || leaseForNode(harness, during, n1) != "" {
		return ensure(false, "a draining node must not receive new leases")
	}

	// Idempotent begin: same key/content replays; same key/different content conflicts.
	replay, err := beginMaintenance(harness, n1, "maint-begin-0001", "hardware replacement", "replacement verified", "wait")
	if err != nil {
		return err
	}
	if replay.Maintenance == nil || replay.Maintenance.ID != beginView.Maintenance.ID {
		return ensure(false, "maintenance begin must replay the same record")
	}
	_, err = beginMaintenance(harness, n1, "maint-begin-0001", "different reason text", "replacement verified", "wait")
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeIdempotencyConflict)); err != nil {
		return err
	}

	// Recovery is impossible before the node has fully drained.
	_, err = recoverMaintenance(harness, n1, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Natural release converges the wait drain to drained (not ready).
	var released domain.NodeLease
	if err := harness.call(http.MethodPatch, "/v1/leases/"+n1Lease+"/release", nil, &released); err != nil {
		return err
	}
	if released.State != domain.LeaseReleased {
		return ensure(false, "lease must be released")
	}
	drainedView, err := getNode(harness, n1)
	if err != nil {
		return err
	}
	if drainedView.State != domain.NodeDrained || drainedView.Participation.Eligible ||
		drainedView.Participation.Reason != domain.ParticipationReasonDrained {
		return ensure(false, "wait node must converge to drained after release, not ready")
	}
	// The lease list read must reach the same conclusion as the node read.
	leaseViews, err := listLeases(harness, "/v1/leases?node_id="+n1)
	if err != nil {
		return err
	}
	if len(leaseViews.Items) != 1 || leaseViews.Items[0].Participation.Eligible ||
		leaseViews.Items[0].NodeState != domain.NodeDrained {
		return ensure(false, "lease list read disagrees with node recovery read")
	}

	recovered, err := recoverMaintenance(harness, n1, &drainedView.Version)
	if err != nil {
		return err
	}
	if recovered.State != domain.NodeReady || !recovered.Participation.Eligible {
		return ensure(false, "recovered node must be ready and eligible")
	}
	after, err := allocate(harness, plan.ID, "maint-lease-after-recover", "east", 2)
	if err != nil {
		return err
	}
	if len(after.LeaseIDs) != 1 || leaseForNode(harness, after, n1) == "" {
		return ensure(false, "a recovered node must be selectable again")
	}

	// --- Section 2: release mode terminates the lease immediately ---
	n2Lease := leaseForNode(harness, first, n2)
	if n2Lease == "" {
		return ensure(false, "n2 must hold a lease from the first group")
	}
	releaseView, err := beginMaintenance(harness, n2, "maint-begin-0002", "firmware rollback", "firmware re-flashed", "release")
	if err != nil {
		return err
	}
	if releaseView.State != domain.NodeDrained {
		return ensure(false, "release mode on a leased node must be drained immediately")
	}
	if releaseView.Maintenance == nil || releaseView.Maintenance.DrainMode != domain.DrainRelease {
		return ensure(false, "release maintenance must record its drain mode")
	}
	var releasedLease domain.LeaseView
	if err := harness.call(http.MethodGet, "/v1/leases/"+n2Lease, nil, &releasedLease); err != nil {
		return err
	}
	if releasedLease.State != domain.LeaseReleased || releasedLease.FinishedAt == nil {
		return ensure(false, "release mode must terminate the lease as released")
	}
	if _, err := recoverMaintenance(harness, n2, nil); err != nil {
		return err
	}

	// A ready node with no leases entering maintenance drains at once; cancel
	// returns it to ready immediately.
	emptyView, err := beginMaintenance(harness, n2, "maint-begin-ready-02", "rack re-cabling", "uplinks tested", "wait")
	if err != nil {
		return err
	}
	if emptyView.State != domain.NodeDrained {
		return ensure(false, "maintenance with no outstanding leases must be drained at once")
	}
	canceledMaintenanceID := ""
	if emptyView.Maintenance != nil {
		canceledMaintenanceID = emptyView.Maintenance.ID
	}
	canceled, err := cancelMaintenance(harness, n2, nil)
	if err != nil {
		return err
	}
	if canceled.State != domain.NodeReady || canceled.Maintenance != nil {
		return ensure(false, "canceling a drained maintenance must restore a ready node")
	}
	record, err := getMaintenanceByID(harness, canceledMaintenanceID)
	if err != nil {
		return err
	}
	if record.State != domain.MaintenanceCanceled {
		return ensure(false, "maintenance read must expose the canceled record")
	}

	// --- Section 3: expire mode terminates the lease as expired ---
	n3Lease := leaseForNode(harness, during, n3)
	if n3Lease == "" {
		return ensure(false, "n3 must hold a lease from the draining-period group")
	}
	expireView, err := beginMaintenance(harness, n3, "maint-begin-0003", "power maintenance", "power restored", "expire")
	if err != nil {
		return err
	}
	if expireView.State != domain.NodeDrained {
		return ensure(false, "expire mode on a leased node must be drained immediately")
	}
	var expiredLease domain.LeaseView
	if err := harness.call(http.MethodGet, "/v1/leases/"+n3Lease, nil, &expiredLease); err != nil {
		return err
	}
	if expiredLease.State != domain.LeaseExpired {
		return ensure(false, "expire mode must terminate the lease as expired")
	}
	if _, err := recoverMaintenance(harness, n3, nil); err != nil {
		return err
	}

	// --- Section 4: quarantined nodes are distinct from maintenance nodes ---
	var quarantined domain.ResolverNode
	if err := harness.call(http.MethodPut, "/v1/nodes", domain.NodeCreateInput{
		NodeName:   "resolver-maint-quar",
		Region:     "east",
		Quarantine: true,
	}, &quarantined); err != nil {
		return err
	}
	_, err = beginMaintenance(harness, quarantined.ID, "maint-on-quar-0001", "misdiagnosed isolation", "health check green", "wait")
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}
	quarView, err := getNode(harness, quarantined.ID)
	if err != nil {
		return err
	}
	if quarView.State != domain.NodeQuarantined || quarView.Participation.Eligible ||
		quarView.Participation.Reason != domain.ParticipationReasonQuarantined {
		return ensure(false, "quarantine must remain distinguishable from maintenance")
	}
	// Maintenance lifecycle operations do not apply to an isolated node.
	if _, err := cancelMaintenance(harness, quarantined.ID, nil); err == nil {
		return ensure(false, "cancel must be rejected for a node without active maintenance")
	}
	if _, err := recoverMaintenance(harness, quarantined.ID, nil); err == nil {
		return ensure(false, "recover must be rejected for a node without active maintenance")
	}

	// --- Section 5: wait mode converges on natural expiry ---
	waitGroup, err := allocate(harness, plan.ID, "maint-lease-wait-exp", "east", 1)
	if err != nil {
		return err
	}
	waitLease := waitGroup.LeaseIDs[0]
	waitNode := reportOwner(harness, waitLease)
	if waitNode == "" {
		return ensure(false, "wait-expiry lease must have an owner")
	}
	waitBegin, err := beginMaintenance(harness, waitNode, "maint-wait-expire-01", "second hardware pass", "post-check green", "wait")
	if err != nil {
		return err
	}
	if waitBegin.State != domain.NodeDraining {
		return ensure(false, "wait maintenance must be draining")
	}
	harness.clock.Advance(700 * 1e9)
	// A subsequent allocation runs the expiry sweep: the outstanding lease
	// expires and the draining node must converge to drained deterministically.
	if _, err := allocate(harness, plan.ID, "maint-sweep-expire-1", "east", 1); err != nil {
		return err
	}
	expiredDrained, err := getNode(harness, waitNode)
	if err != nil {
		return err
	}
	if expiredDrained.State != domain.NodeDrained {
		return ensure(false, "wait node must be drained once its lease expires")
	}
	if _, err := recoverMaintenance(harness, waitNode, nil); err != nil {
		return err
	}

	// --- Section 6: wait mode converges on a successful probe ---
	reportGroup, err := allocate(harness, plan.ID, "maint-lease-wait-rpt", "east", 1)
	if err != nil {
		return err
	}
	reportLease := reportGroup.LeaseIDs[0]
	reportNode := reportOwner(harness, reportLease)
	reportBegin, err := beginMaintenance(harness, reportNode, "maint-wait-report-1", "probe convergence check", "probe accepted", "wait")
	if err != nil {
		return err
	}
	if reportBegin.State != domain.NodeDraining {
		return ensure(false, "report case must start draining")
	}
	if _, err := submitProbe(harness, reportLease, "maint-probe-report-1", true, 5001, 12); err != nil {
		return err
	}
	reportedView, err := getNode(harness, reportNode)
	if err != nil {
		return err
	}
	if reportedView.State != domain.NodeDrained {
		return ensure(false, "a reported lease must drain the node, not return it to ready")
	}
	if _, err := recoverMaintenance(harness, reportNode, nil); err != nil {
		return err
	}

	// --- Section 7: optimistic version mismatch rejects recovery ---
	versionView, err := beginMaintenance(harness, n3, "maint-version-0001", "version guard check", "guard verified", "wait")
	if err != nil {
		return err
	}
	if versionView.State != domain.NodeDrained {
		return ensure(false, "ready n3 must be drained immediately")
	}
	wrong := versionView.Version + 5
	if _, err := recoverMaintenance(harness, n3, &wrong); err == nil {
		return ensure(false, "stale expected_version must be rejected")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeVersionConflict)); err != nil {
		return err
	}
	if _, err := recoverMaintenance(harness, n3, &versionView.Version); err != nil {
		return err
	}

	// --- Section 8: canceling wait hands an outstanding lease back to rotation ---
	concNodes := make([]domain.ResolverNode, 0, 4)
	for i := 0; i < 4; i++ {
		node, err := enrollNode(harness, fmt.Sprintf("resolver-conc-%02d", i+1), "conc")
		if err != nil {
			return err
		}
		concNodes = append(concNodes, node)
	}
	// Concurrent allocations against an exactly-sized pool must never
	// double-occupy a node: one writer wins, the rest see an exhausted pool.
	groupResults := make([]domain.LeaseGroup, 4)
	errResults := make([]error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			groupResults[index], errResults[index] = allocate(
				harness,
				plan.ID,
				fmt.Sprintf("conc-lease-%04d", index+1),
				"conc",
				4,
			)
		}(i)
	}
	wg.Wait()
	successes := 0
	leasedConc := make(map[string]int)
	for i, callErr := range errResults {
		if callErr == nil {
			successes++
			for _, leaseID := range groupResults[i].LeaseIDs {
				var view domain.LeaseView
				if err := harness.call(http.MethodGet, "/v1/leases/"+leaseID, nil, &view); err != nil {
					return err
				}
				leasedConc[view.NodeID]++
			}
			continue
		}
		if err := expectFailure(callErr, http.StatusConflict, string(domain.CodeNodeUnavailable)); err != nil {
			return err
		}
	}
	if successes != 1 {
		return ensure(false, "exactly one concurrent allocation must win an exhausted pool")
	}
	if len(leasedConc) != 4 {
		return ensure(false, "the winning group must occupy four distinct nodes")
	}
	for nodeID, count := range leasedConc {
		if count != 1 {
			return ensure(false, "node "+nodeID+" was double-occupied")
		}
		page, err := listLeases(harness, "/v1/leases?node_id="+nodeID+"&state=claimed")
		if err != nil {
			return err
		}
		if len(page.Items) != 1 {
			return ensure(false, "every concurrent node must hold exactly one claimed lease")
		}
	}

	c1 := concNodes[0]
	c1View, err := beginMaintenance(harness, c1.ID, "maint-cancel-wait-1", "cancel while leased", "not needed anymore", "wait")
	if err != nil {
		return err
	}
	if c1View.State != domain.NodeDraining {
		return ensure(false, "c1 must be draining under wait")
	}
	cancelLeased, err := cancelMaintenance(harness, c1.ID, &c1View.Version)
	if err != nil {
		return err
	}
	if cancelLeased.State != domain.NodeLeased || cancelLeased.Participation.Reason != domain.ParticipationReasonLeased {
		return ensure(false, "cancel while a lease is outstanding must restore leased, not ready")
	}
	c1Leases, err := listLeases(harness, "/v1/leases?node_id="+c1.ID+"&state=claimed")
	if err != nil {
		return err
	}
	if len(c1Leases.Items) != 1 {
		return ensure(false, "the outstanding lease must survive cancellation intact")
	}

	return nil
}

func beginMaintenance(
	harness *harness,
	nodeID string,
	requestKey string,
	reason string,
	recoveryCondition string,
	drainMode string,
) (domain.NodeView, error) {
	var view domain.NodeView
	err := harness.call(http.MethodPut, "/v1/nodes/"+nodeID+"/maintenance", domain.MaintenanceBeginInput{
		RequestKey:        requestKey,
		Reason:            reason,
		RecoveryCondition: recoveryCondition,
		DrainMode:         drainMode,
	}, &view)
	return view, err
}

func cancelMaintenance(harness *harness, nodeID string, expectedVersion *uint64) (domain.NodeView, error) {
	var view domain.NodeView
	body := map[string]any(nil)
	if expectedVersion != nil {
		body = map[string]any{"expected_version": *expectedVersion}
	}
	err := harness.call(http.MethodPatch, "/v1/nodes/"+nodeID+"/maintenance/cancel", body, &view)
	return view, err
}

func recoverMaintenance(harness *harness, nodeID string, expectedVersion *uint64) (domain.NodeView, error) {
	var view domain.NodeView
	body := map[string]any(nil)
	if expectedVersion != nil {
		body = map[string]any{"expected_version": *expectedVersion}
	}
	err := harness.call(http.MethodPatch, "/v1/nodes/"+nodeID+"/maintenance/recover", body, &view)
	return view, err
}

func getNode(harness *harness, nodeID string) (domain.NodeView, error) {
	var view domain.NodeView
	err := harness.call(http.MethodGet, "/v1/nodes/"+nodeID, nil, &view)
	return view, err
}

func getMaintenance(harness *harness, nodeID string) (domain.NodeMaintenance, error) {
	var record domain.NodeMaintenance
	err := harness.call(http.MethodGet, "/v1/nodes/"+nodeID+"/maintenance", nil, &record)
	return record, err
}

func getMaintenanceByID(harness *harness, maintenanceID string) (domain.NodeMaintenance, error) {
	var record domain.NodeMaintenance
	err := harness.call(http.MethodGet, "/v1/maintenances/"+maintenanceID, nil, &record)
	return record, err
}

func listLeases(harness *harness, path string) (domain.LeasePage, error) {
	var page domain.LeasePage
	err := harness.call(http.MethodGet, path, nil, &page)
	return page, err
}

func leaseForNode(harness *harness, group domain.LeaseGroup, nodeID string) string {
	for _, leaseID := range group.LeaseIDs {
		var view domain.LeaseView
		if err := harness.call(http.MethodGet, "/v1/leases/"+leaseID, nil, &view); err != nil {
			return ""
		}
		if view.NodeID == nodeID {
			return leaseID
		}
	}
	return ""
}

func reportOwner(harness *harness, leaseID string) string {
	var view domain.LeaseView
	if err := harness.call(http.MethodGet, "/v1/leases/"+leaseID, nil, &view); err != nil {
		return ""
	}
	return view.NodeID
}
