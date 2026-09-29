package domain

import "time"

type PlanState string

const (
	PlanDraft     PlanState = "draft"
	PlanActive    PlanState = "active"
	PlanCompleted PlanState = "completed"
)

type NodeState string

const (
	NodeReady       NodeState = "ready"
	NodeLeased      NodeState = "leased"
	NodeQuarantined NodeState = "quarantined"
	NodeMaintenance NodeState = "maintenance"
)

// MaintenanceStatus is the lifecycle state of a node maintenance record.
type MaintenanceStatus string

const (
	MaintenanceActive   MaintenanceStatus = "active"
	MaintenanceCanceled MaintenanceStatus = "canceled"
	MaintenanceResumed  MaintenanceStatus = "resumed"
)

// DrainPolicy decides how a claimed lease is converged when maintenance begins.
type DrainPolicy string

const (
	// DrainWait keeps the claimed lease until it is reported, released or expires.
	DrainWait DrainPolicy = "wait"
	// DrainRelease releases the claimed lease immediately.
	DrainRelease DrainPolicy = "release"
	// DrainExpire forces the claimed lease to expire immediately.
	DrainExpire DrainPolicy = "expire"
)

type LeaseState string

const (
	LeaseClaimed  LeaseState = "claimed"
	LeaseReported LeaseState = "reported"
	LeaseReleased LeaseState = "released"
	LeaseExpired  LeaseState = "expired"
)

type Verdict string

const (
	VerdictConsistent     Verdict = "consistent"
	VerdictReviewRequired Verdict = "review_required"
)

type EntryType string

const (
	EntryA     EntryType = "A"
	EntryAAAA  EntryType = "AAAA"
	EntryCNAME EntryType = "CNAME"
)

type ZoneEntry struct {
	Owner string    `json:"owner"`
	Type  EntryType `json:"type"`
	Value string    `json:"value"`
}

type ZoneRevision struct {
	ID         string      `json:"id"`
	PlanID     string      `json:"plan_id"`
	Serial     uint32      `json:"serial"`
	Authority  string      `json:"authority"`
	Entries    []ZoneEntry `json:"entries"`
	EntryCount int         `json:"entry_count"`
	CreatedAt  time.Time   `json:"created_at"`
}

type RolloutPlan struct {
	ID                  string     `json:"id"`
	Zone                string     `json:"zone"`
	State               PlanState  `json:"state"`
	MinimumSuccessRatio float64    `json:"minimum_success_ratio"`
	MaximumLatencyMS    int        `json:"maximum_latency_ms"`
	ExpectedNodes       int        `json:"expected_nodes"`
	Version             uint64     `json:"version"`
	LatestRevisionID    string     `json:"latest_revision_id,omitempty"`
	LatestAssessmentID  string     `json:"latest_assessment_id,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	ActivatedAt         *time.Time `json:"activated_at,omitempty"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
}

type ResolverNode struct {
	ID        string    `json:"id"`
	NodeName  string    `json:"node_name"`
	Region    string    `json:"region"`
	State     NodeState `json:"state"`
	Version   uint64    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NodeMaintenanceRecord registers why a node stopped taking new leases and when
// it may participate again. Status is active while the node is under
// maintenance; it becomes canceled or resumed when the node returns to service.
type NodeMaintenanceRecord struct {
	ID                string            `json:"id"`
	NodeID            string            `json:"node_id"`
	Reason            string            `json:"reason"`
	RecoveryCondition string            `json:"recovery_condition"`
	DrainPolicy       DrainPolicy       `json:"drain_policy"`
	Status            MaintenanceStatus `json:"status"`
	Drained           bool              `json:"drained"`
	OutstandingLease  string            `json:"outstanding_lease,omitempty"`
	BegunAt           time.Time         `json:"begun_at"`
	CanceledAt        *time.Time        `json:"canceled_at,omitempty"`
	ResumedAt         *time.Time        `json:"resumed_at,omitempty"`
}

// Participation is the single answer every read path uses to decide whether a
// node can take new leases. Eligible is true only when the node is ready and
// free of any claimed lease or maintenance.
type Participation struct {
	NodeID        string    `json:"node_id"`
	Eligible      bool      `json:"eligible"`
	Reason        string    `json:"reason,omitempty"`
	NodeState     NodeState `json:"node_state"`
	InMaintenance bool      `json:"in_maintenance"`
	Drained       bool      `json:"drained"`
	ActiveLeaseID string    `json:"active_lease_id,omitempty"`
}

// MaintenanceView combines the node, its latest maintenance record and the
// participation conclusion used by recovery reads.
type MaintenanceView struct {
	Node          ResolverNode           `json:"node"`
	Maintenance   *NodeMaintenanceRecord `json:"maintenance,omitempty"`
	Participation Participation          `json:"participation"`
}

type NodeLease struct {
	ID           string     `json:"id"`
	PlanID       string     `json:"plan_id"`
	NodeID       string     `json:"node_id"`
	AllocationID string     `json:"allocation_id"`
	State        LeaseState `json:"state"`
	ClaimedAt    time.Time  `json:"claimed_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ProbeID      string     `json:"probe_id,omitempty"`
}

type LeaseGroup struct {
	ID                 string    `json:"id"`
	PlanID             string    `json:"plan_id"`
	RequestKey         string    `json:"request_key"`
	RequestFingerprint string    `json:"request_fingerprint"`
	Region             string    `json:"region,omitempty"`
	LeaseIDs           []string  `json:"lease_ids"`
	CreatedAt          time.Time `json:"created_at"`
}

type Probe struct {
	ID                 string    `json:"id"`
	LeaseID            string    `json:"lease_id"`
	PlanID             string    `json:"plan_id"`
	NodeID             string    `json:"node_id"`
	RequestKey         string    `json:"request_key"`
	Success            bool      `json:"success"`
	ObservedSerial     uint32    `json:"observed_serial"`
	LatencyMS          int       `json:"latency_ms"`
	ObservedAt         time.Time `json:"observed_at"`
	StoredAt           time.Time `json:"stored_at"`
	PayloadFingerprint string    `json:"payload_fingerprint"`
}

type RolloutAssessment struct {
	ID               string    `json:"id"`
	PlanID           string    `json:"plan_id"`
	RevisionID       string    `json:"revision_id"`
	RequestKey       string    `json:"request_key"`
	Verdict          Verdict   `json:"verdict"`
	Reasons          []string  `json:"reasons"`
	ProbeCount       int       `json:"probe_count"`
	SuccessCount     int       `json:"success_count"`
	SuccessRatio     float64   `json:"success_ratio"`
	MaximumLatencyMS int       `json:"maximum_latency_ms"`
	CreatedAt        time.Time `json:"created_at"`
}

type AuditEvent struct {
	ID         string         `json:"id"`
	EntityType string         `json:"entity_type"`
	EntityID   string         `json:"entity_id"`
	Action     string         `json:"action"`
	At         time.Time      `json:"at"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type State struct {
	SchemaVersion         int                              `json:"schema_version"`
	Revision              uint64                           `json:"revision"`
	Plans                 map[string]RolloutPlan           `json:"plans"`
	Revisions             map[string]ZoneRevision          `json:"revisions"`
	Nodes                 map[string]ResolverNode          `json:"nodes"`
	Maintenances          map[string]NodeMaintenanceRecord `json:"node_maintenances"`
	Leases                map[string]NodeLease             `json:"leases"`
	LeaseGroups           map[string]LeaseGroup            `json:"lease_groups"`
	Probes                map[string]Probe                 `json:"probes"`
	Assessments           map[string]RolloutAssessment     `json:"assessments"`
	LeaseRequestKeys      map[string]string                `json:"lease_request_keys"`
	AssessmentRequestKeys map[string]string                `json:"assessment_request_keys"`
	AuditEvents           []AuditEvent                     `json:"audit_events"`
}

type PageMeta struct {
	Limit      int    `json:"limit"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type PlanPage struct {
	Items []RolloutPlan `json:"items"`
	Meta  PageMeta      `json:"meta"`
}

type NodePage struct {
	Items []ResolverNode `json:"items"`
	Meta  PageMeta       `json:"meta"`
}

// LeaseView is a lease enriched with the participation conclusion of its node,
// so lease list reads answer the same "can this node serve online" question.
type LeaseView struct {
	NodeLease
	Participation Participation `json:"participation"`
}

type LeasePage struct {
	Items []LeaseView `json:"items"`
	Meta  PageMeta    `json:"meta"`
}
