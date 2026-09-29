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
	NodeDraining    NodeState = "draining"
	NodeDrained     NodeState = "drained"
)

// MaintenanceState is the lifecycle state of a node maintenance record.
type MaintenanceState string

const (
	MaintenanceActive    MaintenanceState = "active"
	MaintenanceCanceled  MaintenanceState = "canceled"
	MaintenanceRecovered MaintenanceState = "recovered"
)

// DrainMode selects the deterministic convergence path for leases that are
// still claimed when maintenance begins.
type DrainMode string

const (
	DrainWait    DrainMode = "wait"
	DrainRelease DrainMode = "release"
	DrainExpire  DrainMode = "expire"
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
	ID            string    `json:"id"`
	NodeName      string    `json:"node_name"`
	Region        string    `json:"region"`
	State         NodeState `json:"state"`
	Version       uint64    `json:"version"`
	MaintenanceID string    `json:"maintenance_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// NodeMaintenance records why a node was removed from rollout participation,
// how its outstanding leases must converge, and the conditions recorded for
// bringing the node back online.
type NodeMaintenance struct {
	ID                 string           `json:"id"`
	NodeID             string           `json:"node_id"`
	State              MaintenanceState `json:"state"`
	Reason             string           `json:"reason"`
	RecoveryCondition  string           `json:"recovery_condition"`
	DrainMode          DrainMode        `json:"drain_mode"`
	RequestKey         string           `json:"request_key"`
	RequestFingerprint string           `json:"request_fingerprint"`
	LeaseIDs           []string         `json:"lease_ids"`
	AffectedLeaseCount int              `json:"affected_lease_count"`
	CreatedAt          time.Time        `json:"created_at"`
	CanceledAt         *time.Time       `json:"canceled_at,omitempty"`
	RecoveredAt        *time.Time       `json:"recovered_at,omitempty"`
}

// Participation is the single source of truth for whether a node may take
// part in a rollout. Recovery reads, allocation reads and lease list reads
// all derive their conclusion from it.
type Participation struct {
	Eligible bool      `json:"eligible"`
	State    NodeState `json:"state"`
	Reason   string    `json:"reason"`
}

const (
	ParticipationReasonReady            = "ready"
	ParticipationReasonLeased           = "leased"
	ParticipationReasonQuarantined      = "quarantined"
	ParticipationReasonMaintenanceWait  = "maintenance_draining_wait"
	ParticipationReasonMaintenanceForce = "maintenance_draining"
	ParticipationReasonDrained          = "maintenance_drained"
	ParticipationReasonUnknown          = "unknown_state"
)

// NodeView combines the node record, its participation verdict and its active
// maintenance record (when present).
type NodeView struct {
	ResolverNode
	Participation Participation    `json:"participation"`
	Maintenance   *NodeMaintenance `json:"maintenance,omitempty"`
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
	SchemaVersion          int                          `json:"schema_version"`
	Revision               uint64                       `json:"revision"`
	Plans                  map[string]RolloutPlan       `json:"plans"`
	Revisions              map[string]ZoneRevision      `json:"revisions"`
	Nodes                  map[string]ResolverNode      `json:"nodes"`
	Maintenances           map[string]NodeMaintenance   `json:"maintenances"`
	Leases                 map[string]NodeLease         `json:"leases"`
	LeaseGroups            map[string]LeaseGroup        `json:"lease_groups"`
	Probes                 map[string]Probe             `json:"probes"`
	Assessments            map[string]RolloutAssessment `json:"assessments"`
	LeaseRequestKeys       map[string]string            `json:"lease_request_keys"`
	AssessmentRequestKeys  map[string]string            `json:"assessment_request_keys"`
	MaintenanceRequestKeys map[string]string            `json:"maintenance_request_keys"`
	AuditEvents            []AuditEvent                 `json:"audit_events"`
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
	Items []NodeView `json:"items"`
	Meta  PageMeta   `json:"meta"`
}

// LeaseView augments a lease with the current state and participation verdict
// of its node, so lease list reads reach the same conclusion as node reads
// and allocation reads.
type LeaseView struct {
	NodeLease
	NodeState     NodeState     `json:"node_state"`
	Participation Participation `json:"participation"`
}

type LeasePage struct {
	Items []LeaseView `json:"items"`
	Meta  PageMeta    `json:"meta"`
}
