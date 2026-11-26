package placement

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

const (
	Auto   = "auto"
	Pinned = "pinned"

	SourceWorkerBudget = "resourced.QueryWorkerBudget"
	MaxQueryDuration   = 5 * time.Second
	DefaultSampleAge   = 15 * time.Second
)

type Resources struct {
	VCPUs        int64 `json:"vcpus"`
	MemoryMiB    int64 `json:"memory_mib"`
	StorageBytes int64 `json:"storage_bytes"`
	HeavySlots   int64 `json:"heavy_slots"`
}

func (r Resources) Valid() bool {
	return r.VCPUs >= 0 && r.MemoryMiB >= 0 && r.StorageBytes >= 0 && r.HeavySlots >= 0
}

// Subtract floors the result at zero. A shrinking Worker budget must block
// further placement even when outstanding reservations exceed local capacity.
func (r Resources) Subtract(s Resources) Resources {
	return Resources{
		VCPUs:        max(0, r.VCPUs-s.VCPUs),
		MemoryMiB:    max(0, r.MemoryMiB-s.MemoryMiB),
		StorageBytes: max(0, r.StorageBytes-s.StorageBytes),
		HeavySlots:   max(0, r.HeavySlots-s.HeavySlots),
	}
}

func (r Resources) Fits(available Resources) bool {
	return r.VCPUs <= available.VCPUs && r.MemoryMiB <= available.MemoryMiB &&
		r.StorageBytes <= available.StorageBytes && r.HeavySlots <= available.HeavySlots
}

type Budget struct {
	Capacity  Resources `json:"capacity"`
	Allocated Resources `json:"allocated"`
}

type Pressure struct {
	AvailableMemoryMiB int64 `json:"available_memory_mib"`
	DiskFreeBytes      int64 `json:"disk_free_bytes"`
	MemoryStalled      bool  `json:"memory_stalled"`
	DiskStalled        bool  `json:"disk_stalled"`
}

type Session struct {
	HostBootID      string `json:"host_boot_id"`
	BrokerSessionID string `json:"broker_session_id"`
	NodeSessionID   string `json:"node_session_id"`
}

func (s Session) Valid() bool {
	return s.HostBootID != "" && s.BrokerSessionID != "" && s.NodeSessionID != ""
}

type RuntimeProfile struct {
	RuntimeInstanceID string   `json:"runtime_instance_id"`
	LocalInstanceID   string   `json:"local_instance_id"`
	BundleID          string   `json:"bundle_id"`
	Reader            string   `json:"reader,omitempty"`
	Architecture      string   `json:"architecture"`
	Hypervisor        string   `json:"hypervisor"`
	RuntimeProcessID  string   `json:"runtime_process_id"`
	Ready             bool     `json:"ready"`
	Capabilities      []string `json:"capabilities,omitempty"`
	Artifacts         []string `json:"artifacts,omitempty"`
	CachedArtifacts   []string `json:"cached_artifacts,omitempty"`
}

type Sample struct {
	WorkerID             string           `json:"worker_id"`
	ControlEpoch         string           `json:"control_epoch"`
	EnrollmentGeneration uint64           `json:"enrollment_generation"`
	Session              Session          `json:"session"`
	Sequence             int64            `json:"sequence"`
	QueryNonce           string           `json:"query_nonce"`
	Source               string           `json:"source"`
	SampleAge            time.Duration    `json:"sample_age_ns"`
	Reconciled           bool             `json:"reconciled"`
	Budget               Budget           `json:"budget"`
	Pressure             *Pressure        `json:"pressure,omitempty"`
	Profiles             []RuntimeProfile `json:"profiles"`
}

// Query holds the control plane's own send and receive times for its nonce.
type Query struct {
	Nonce       string
	StartedAt   time.Time
	CompletedAt time.Time
}

func (s Sample) Validate(q Query, now time.Time) error {
	roundTrip := q.CompletedAt.Sub(q.StartedAt)
	if q.Nonce == "" || s.QueryNonce != q.Nonce || q.StartedAt.IsZero() || q.CompletedAt.IsZero() ||
		roundTrip < 0 || roundTrip > MaxQueryDuration || q.CompletedAt.After(now) ||
		s.SampleAge < 0 || s.SampleAge > DefaultSampleAge ||
		now.Sub(q.CompletedAt)+roundTrip+s.SampleAge > DefaultSampleAge {
		return errors.New("missing or stale Worker budget query evidence")
	}
	if s.WorkerID == "" || s.ControlEpoch == "" || !s.Session.Valid() || s.Sequence <= 0 ||
		s.Source != SourceWorkerBudget || !s.Budget.Capacity.Valid() || !s.Budget.Allocated.Valid() ||
		s.Budget.Capacity.VCPUs == 0 || s.Budget.Capacity.MemoryMiB == 0 {
		return errors.New("invalid Worker budget sample")
	}
	if s.Pressure != nil && (s.Pressure.AvailableMemoryMiB < 0 || s.Pressure.DiskFreeBytes < 0) {
		return errors.New("invalid Worker pressure sample")
	}
	profiles := make(map[string]bool, len(s.Profiles))
	for _, p := range s.Profiles {
		if p.RuntimeInstanceID == "" || p.LocalInstanceID == "" || p.BundleID == "" ||
			(p.Ready && (p.RuntimeProcessID == "" || p.Architecture == "" || p.Hypervisor == "")) ||
			profiles[p.RuntimeInstanceID] {
			return errors.New("invalid or duplicate Worker Runtime profile")
		}
		profiles[p.RuntimeInstanceID] = true
	}
	return nil
}

// Request combines caller intent and placement requirements derived by the
// platform. EligibleRuntimeIDs is nil when there is no additional route filter.
type Request struct {
	Mode               string        `json:"mode"`
	WorkerID           string        `json:"worker_id,omitempty"`
	OwnerWorkerID      string        `json:"-"`
	RuntimeInstanceID  string        `json:"runtime_instance_id,omitempty"`
	BundleID           string        `json:"bundle_id,omitempty"`
	Reader             string        `json:"reader,omitempty"`
	Architecture       string        `json:"architecture,omitempty"`
	Hypervisor         string        `json:"hypervisor,omitempty"`
	Capabilities       []string      `json:"capabilities,omitempty"`
	ArtifactDigest     string        `json:"artifact_digest,omitempty"`
	Resources          Resources     `json:"resources"`
	Headroom           Resources     `json:"headroom"`
	MaxSampleAge       time.Duration `json:"-"`
	EligibleRuntimeIDs []string      `json:"-"`
}

func (r Request) Validate() error {
	if (r.Mode != Auto && r.Mode != Pinned) || (r.Mode == Pinned) != (r.WorkerID != "") ||
		!r.Resources.Valid() || !r.Headroom.Valid() || r.MaxSampleAge < 0 || r.MaxSampleAge > DefaultSampleAge {
		return errors.New("invalid placement requirements")
	}
	return nil
}

const (
	Reserved        = "reserved_not_sent"
	DispatchStarted = "dispatch_started"
	Unknown         = "unknown"
	Admitted        = "admitted"
	Covered         = "covered"
	Rejected        = "terminal_rejected"
	Released        = "released"
)

type Reservation struct {
	OperationID       string    `json:"operation_id"`
	WorkerID          string    `json:"worker_id"`
	RuntimeInstanceID string    `json:"runtime_instance_id"`
	BundleID          string    `json:"bundle_id"`
	ControlEpoch      string    `json:"control_epoch"`
	RequestDigest     string    `json:"request_digest"`
	NativeDigest      string    `json:"native_digest,omitempty"`
	Resources         Resources `json:"resources"`
	Phase             string    `json:"phase"`
	Attempt           int64     `json:"attempt"`
	NativeAttempt     int64     `json:"native_attempt"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Rejection struct {
	WorkerID          string `json:"worker_id"`
	RuntimeInstanceID string `json:"runtime_instance_id,omitempty"`
	Reason            string `json:"reason"`
}

var ErrNoCandidate = errors.New("no eligible Worker for placement")

type Rejections struct {
	Candidates []Rejection `json:"candidates"`
}

func (r *Rejections) Error() string {
	return fmt.Sprintf("%s (%d candidate exclusions)", ErrNoCandidate, len(r.Candidates))
}
func (r *Rejections) Unwrap() error { return ErrNoCandidate }

// Score is the sum of the fractions of local budget left after placement,
// including headroom and holds. Lower values consolidate where capacity permits.
func Score(capacity, after Resources) float64 {
	fraction := func(remainder, total int64) float64 {
		if total == 0 {
			return 0
		}
		return float64(remainder) / float64(total)
	}
	return fraction(after.VCPUs, capacity.VCPUs) + fraction(after.MemoryMiB, capacity.MemoryMiB) +
		fraction(after.StorageBytes, capacity.StorageBytes) + fraction(after.HeavySlots, capacity.HeavySlots)
}

func (r Request) ProfileRejection(p RuntimeProfile) string {
	switch {
	case !p.Ready:
		return "runtime_not_ready"
	case r.Architecture != "" && p.Architecture != r.Architecture,
		r.Hypervisor != "" && p.Hypervisor != r.Hypervisor:
		return "hardware_incompatible"
	case r.BundleID != "" && p.BundleID != r.BundleID:
		return "bundle_incompatible"
	case r.Reader != "" && p.Reader != r.Reader:
		return "reader_incompatible"
	}
	for _, required := range r.Capabilities {
		if !slices.Contains(p.Capabilities, required) {
			return "capability_unavailable"
		}
	}
	if r.ArtifactDigest != "" && !slices.Contains(p.Artifacts, r.ArtifactDigest) {
		return "artifact_unavailable"
	}
	return ""
}

const (
	AdmissionOperation = "local_operation_admission"
	AdmissionRestore   = "local_restore_admission"
)

type Admission struct {
	OperationID          string
	Attempt              int64
	WorkerID             string
	RuntimeInstanceID    string
	BundleID             string
	ControlEpoch         string
	EnrollmentGeneration uint64
	NativeDigest         string
	Kind                 string
	ReceiptID            string
	Session              Session
	// The gateway reads its current sequence after confirming the durable
	// admission and before starting the covering Worker budget query.
	ConfirmedThroughSequence int64
}
