package workflowprobe

import (
	"fmt"
	"net/http"
	"sync"

	"solo-0014-zone-rollout-control/internal/domain"
)

func beginMaintenance(
	harness *harness,
	nodeID string,
	reason string,
	recovery string,
	policy string,
) (domain.MaintenanceView, error) {
	var view domain.MaintenanceView
	err := harness.call(http.MethodPut, "/v1/nodes/"+nodeID+"/maintenance", domain.MaintenanceStartInput{
		Reason:            reason,
		RecoveryCondition: recovery,
		DrainPolicy:       domain.DrainPolicy(policy),
	}, &view)
	return view, err
}

func getMaintenance(harness *harness, nodeID string) (domain.MaintenanceView, error) {
	var view domain.MaintenanceView
	err := harness.call(http.MethodGet, "/v1/nodes/"+nodeID+"/maintenance", nil, &view)
	return view, err
}

func listLeases(harness *harness, query string) (domain.LeasePage, error) {
	var page domain.LeasePage
	err := harness.call(http.MethodGet, "/v1/leases"+query, nil, &page)
	return page, err
}

func leaseByID(page domain.LeasePage, leaseID string) (domain.LeaseView, bool) {
	for _, item := range page.Items {
		if item.ID == leaseID {
			return item, true
		}
	}
	return domain.LeaseView{}, false
}

func runNodeMaintenanceDrain() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	waitNode, err := enrollNode(harness, "resolver-maint-wait", "east")
	if err != nil {
		return err
	}
	releaseNode, err := enrollNode(harness, "resolver-maint-release", "east")
	if err != nil {
		return err
	}
	plan, _, err := createActivePlan(harness, "maint.example", 4242, 2, 0.5, 100)
	if err != nil {
		return err
	}

	// The two nodes claim leases.
	group, err := allocate(harness, plan.ID, "maint-lease-0001", "east", 2)
	if err != nil {
		return err
	}
	if len(group.LeaseIDs) != 2 {
		return ensure(false, "expected two initial leases")
	}
	// A spare node and a quarantined node join after the first allocation so the
	// initial leases deterministically belong to the two drained nodes.
	spareNode, err := enrollNode(harness, "resolver-maint-spare", "east")
	if err != nil {
		return err
	}
	quarantineNode, err := enrollNodeQuarantined(harness, "resolver-maint-quar", "east")
	if err != nil {
		return err
	}
	allLeases, err := listLeases(harness, "?limit=100")
	if err != nil {
		return err
	}
	leaseByNode := map[string]string{}
	for _, leaseID := range group.LeaseIDs {
		item, ok := leaseByID(allLeases, leaseID)
		if !ok {
			return ensure(false, "allocated lease missing from lease list")
		}
		leaseByNode[item.NodeID] = leaseID
	}
	waitLease := leaseByNode[waitNode.ID]
	releaseLease := leaseByNode[releaseNode.ID]
	if waitLease == "" || releaseLease == "" {
		return ensure(false, "could not map leases to nodes")
	}

	// Begin maintenance with the "release" policy: the claimed lease is released
	// deterministically in the same transaction, the node drains immediately.
	released, err := beginMaintenance(harness, releaseNode.ID, "disk replacement", "smart check passes", "release")
	if err != nil {
		return err
	}
	if released.Node.State != domain.NodeMaintenance {
		return ensure(false, "release-policy node must enter maintenance")
	}
	if !released.Maintenance.Drained || released.Participation.Eligible {
		return ensure(false, "release-policy node must be drained but not eligible")
	}
	if released.Participation.Reason != domain.ReasonNodeMaintenance {
		return ensure(false, "drained maintenance node must report node_maintenance")
	}
	releasedLeasePage, err := listLeases(harness, "?node_id="+releaseNode.ID)
	if err != nil {
		return err
	}
	if len(releasedLeasePage.Items) != 1 || releasedLeasePage.Items[0].State != domain.LeaseReleased {
		return ensure(false, "release policy must settle the lease to released")
	}
	if releasedLeasePage.Items[0].Participation.Reason != domain.ReasonNodeMaintenance ||
		releasedLeasePage.Items[0].Participation.InMaintenance != true {
		return ensure(false, "lease list must agree the maintained node cannot participate")
	}

	// Begin maintenance with the "wait" policy: the claimed lease stays claimed
	// and maintenance is recorded as still draining.
	waiting, err := beginMaintenance(harness, waitNode.ID, "power supply repair", "load test passes", "wait")
	if err != nil {
		return err
	}
	if waiting.Maintenance.Drained || waiting.Maintenance.OutstandingLease != waitLease {
		return ensure(false, "wait policy must keep the lease and flag draining")
	}
	if waiting.Participation.Reason != domain.ReasonNodeMaintenanceDraining || waiting.Participation.ActiveLeaseID != waitLease {
		return ensure(false, "draining participation must expose the outstanding lease")
	}

	// Resume is refused while the waited lease is still claimed.
	if err := harness.call(http.MethodPatch, "/v1/nodes/"+waitNode.ID+"/resume", nil, nil); err == nil {
		return ensure(false, "resume must fail while a lease is still draining")
	} else if f, ok := err.(*apiFailure); !ok || f.Code != string(domain.CodeMaintenanceDraining) {
		return fmt.Errorf("expected maintenance_draining conflict, got %v", err)
	}

	// Allocation while nodes are under maintenance must only pick the spare,
	// proving maintenance nodes stop getting new leases.
	second, err := allocate(harness, plan.ID, "maint-lease-0002", "east", 5)
	if err != nil {
		return err
	}
	if len(second.LeaseIDs) != 1 {
		return ensure(false, "only the single spare node may be allocated during maintenance")
	}
	claimedPage, err := listLeases(harness, "?plan_id="+plan.ID+"&state=claimed")
	if err != nil {
		return err
	}
	newLease, ok := leaseByID(claimedPage, second.LeaseIDs[0])
	if !ok || newLease.NodeID != spareNode.ID {
		return ensure(false, "the new lease during maintenance must go to the spare node")
	}

	// With every eligible node leased, another allocation must fail fast.
	_, err = allocate(harness, plan.ID, "maint-lease-0003", "east", 5)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeNodeUnavailable)); err != nil {
		return err
	}

	// Converge the waited lease by releasing it: the node stays under maintenance
	// but becomes drained, and never returns to the allocatable pool.
	var releasedWait domain.NodeLease
	if err := harness.call(http.MethodPatch, "/v1/leases/"+waitLease+"/release", nil, &releasedWait); err != nil {
		return err
	}
	if releasedWait.State != domain.LeaseReleased {
		return ensure(false, "waited lease must release")
	}
	waitView, err := getMaintenance(harness, waitNode.ID)
	if err != nil {
		return err
	}
	if !waitView.Maintenance.Drained || waitView.Node.State != domain.NodeMaintenance {
		return ensure(false, "after wait lease settles the node must remain maintained and drained")
	}
	if waitView.Participation.Eligible || waitView.Participation.Reason != domain.ReasonNodeMaintenance {
		return ensure(false, "drained maintained node must remain ineligible")
	}

	// Resume returns the node to service; allocation can select it again.
	var resumed domain.MaintenanceView
	if err := harness.call(http.MethodPatch, "/v1/nodes/"+waitNode.ID+"/resume", nil, &resumed); err != nil {
		return err
	}
	if resumed.Node.State != domain.NodeReady || !resumed.Participation.Eligible {
		return ensure(false, "resume must make the node eligible again")
	}
	if resumed.Maintenance.Status != domain.MaintenanceResumed {
		return ensure(false, "maintenance record must be marked resumed")
	}

	// The resumed node is the only free eligible node and must receive the next
	// lease, with exactly one claimed lease for it.
	third, err := allocate(harness, plan.ID, "maint-lease-0004", "east", 5)
	if err != nil {
		return err
	}
	if len(third.LeaseIDs) != 1 {
		return ensure(false, "the resumed node must receive exactly one lease")
	}
	latestPage, err := listLeases(harness, "?state=claimed")
	if err != nil {
		return err
	}
	item, ok := leaseByID(latestPage, third.LeaseIDs[0])
	if !ok || item.NodeID != waitNode.ID {
		return ensure(false, "the only free eligible node should be the resumed node")
	}

	// A quarantined node must be distinguishable from a maintained node on every
	// read path: allocation skips it and the recovery read reports quarantine.
	quarantineView, err := getMaintenance(harness, quarantineNode.ID)
	if err != nil {
		return err
	}
	if quarantineView.Maintenance != nil {
		return ensure(false, "quarantined node must not carry a maintenance record")
	}
	if quarantineView.Participation.Reason != domain.ReasonNodeQuarantined || quarantineView.Participation.InMaintenance {
		return ensure(false, "quarantine must be distinct from maintenance")
	}
	_, err = beginMaintenance(harness, quarantineNode.ID, "try on quarantined", "never", "wait")
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Maintenance cannot be started twice for the same node.
	_, err = beginMaintenance(harness, releaseNode.ID, "second window", "never", "wait")
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeMaintenanceActive)); err != nil {
		return err
	}

	// Cancel on a drained maintained node returns it to ready and keeps the
	// terminal maintenance record readable.
	var canceled domain.MaintenanceView
	if err := harness.call(http.MethodPatch, "/v1/nodes/"+releaseNode.ID+"/maintenance/cancel", nil, &canceled); err != nil {
		return err
	}
	if canceled.Node.State != domain.NodeReady || !canceled.Participation.Eligible {
		return ensure(false, "cancel of a drained node must return it to ready")
	}
	if canceled.Maintenance.Status != domain.MaintenanceCanceled {
		return ensure(false, "maintenance record must be marked canceled")
	}
	return nil
}

func enrollNodeQuarantined(harness *harness, name string, region string) (domain.ResolverNode, error) {
	var node domain.ResolverNode
	err := harness.call(http.MethodPut, "/v1/nodes", map[string]any{
		"node_name":  name,
		"region":     region,
		"quarantine": true,
	}, &node)
	return node, err
}

// runMaintenanceConcurrency hammers allocation against maintenance begin to
// prove concurrent allocation can never double-occupy a node.
func runMaintenanceConcurrency() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	const nodeCount = 6
	nodeIDs := make([]string, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		node, err := enrollNode(harness, fmt.Sprintf("resolver-cont-%02d", i+1), "west")
		if err != nil {
			return err
		}
		nodeIDs = append(nodeIDs, node.ID)
	}
	plan, _, err := createActivePlan(harness, "cont.example", 7777, nodeCount, 0.1, 1000)
	if err != nil {
		return err
	}

	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers+nodeCount)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			_, err := allocate(harness, plan.ID, fmt.Sprintf("cont-lease-%08d", worker), "west", nodeCount)
			if err != nil {
				if f, ok := err.(*apiFailure); ok && f.Code == string(domain.CodeNodeUnavailable) {
					return
				}
				errs <- err
			}
		}(w)
	}
	// Concurrently drain every node with the release policy.
	for _, nodeID := range nodeIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := beginMaintenance(harness, id, "concurrent drain", "all clear", "release"); err != nil {
				if f, ok := err.(*apiFailure); ok && f.Code == string(domain.CodeMaintenanceActive) {
					return
				}
				errs <- err
			}
		}(nodeID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}

	// Invariant: at most one claimed lease per node; maintained nodes hold none.
	leasePage, err := listLeases(harness, "?limit=100")
	if err != nil {
		return err
	}
	claimedByNode := map[string]int{}
	for _, lease := range leasePage.Items {
		if lease.State == domain.LeaseClaimed {
			claimedByNode[lease.NodeID]++
		}
		if lease.Participation.InMaintenance && lease.State == domain.LeaseClaimed {
			return ensure(false, "maintained node still holds a claimed lease after release drain")
		}
	}
	for nodeID, count := range claimedByNode {
		if count > 1 {
			return fmt.Errorf("node %s ended with %d claimed leases", nodeID, count)
		}
	}
	for _, nodeID := range nodeIDs {
		view, err := getMaintenance(harness, nodeID)
		if err != nil {
			return err
		}
		if view.Maintenance == nil || view.Maintenance.Status != domain.MaintenanceActive {
			return ensure(false, "every node must end under active maintenance")
		}
		if view.Participation.Eligible {
			return ensure(false, "maintained node must remain ineligible")
		}
	}
	return nil
}
