// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	APIVersion        = "2026-05-01"
	ComputeAPIVersion = "2025-04-01"
	Scope             = "empty-disposable-user-pool-api-qualification/v1"
	RunTag            = "cetirun-fence-run"
	MaxJournalBytes   = 128 * 1024
	maxResponseBytes  = 1024 * 1024
	maxMachines       = 32
)

var (
	ErrPending = errors.New("outcome pending or unknown; observation is allowed, deletion must not be resubmitted")
	uuidRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	nameRE     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.()-]{0,89}$`)
	poolRE     = regexp.MustCompile(`^[a-z][a-z0-9]{0,11}$`)
)

type Action string

const (
	Inspect       Action = "inspect"
	Reserve       Action = "reserve"
	Register      Action = "register"
	TestCondition Action = "test-condition"
	Delete        Action = "delete"
	Observe       Action = "observe"
)

type Phase string

const (
	Reserved        Phase = "reserved"
	Registered      Phase = "registered"
	ProbePending    Phase = "condition-probe-pending"
	ProbeRejected   Phase = "condition-probe-rejected"
	Qualified       Phase = "conditional-api-qualified"
	DeletePending   Phase = "delete-submission-unknown"
	Deleting        Phase = "deleting"
	OperationDone   Phase = "operation-succeeded"
	Complete        Phase = "completed"
	Failed          Phase = "failed"
	ProbeUnexpected Phase = "condition-probe-not-enforced"
)

type Reservation struct {
	TenantID           string `json:"tenantID"`
	PoolID             string `json:"poolID"`
	ClusterResourceUID string `json:"clusterResourceUID"`
	RunID              string `json:"runID"`
}

type Options struct {
	Action            Action
	JournalDirectory  string
	Reservation       Reservation
	ConfirmDeletePool string
}

type VMIdentity struct {
	MachineName string `json:"machineName"`
	ResourceID  string `json:"resourceID"`
	VMID        string `json:"vmID"`
}

type Attempt struct {
	RequestID    string    `json:"requestID"`
	IfMatch      string    `json:"ifMatch"`
	IntentAt     time.Time `json:"intentAt"`
	HTTPStatus   int       `json:"httpStatus,omitempty"`
	OperationURL string    `json:"operationURL,omitempty"`
	PollKind     string    `json:"pollKind,omitempty"`
	Status       string    `json:"status,omitempty"`
}

type Completion struct {
	OperationURL string       `json:"operationURL"`
	ObservedAt   time.Time    `json:"observedAt"`
	AbsentVMs    []VMIdentity `json:"absentVMs"`
}

// Journal records an empty-pool ARM experiment, never a runtime execution grant.
type Journal struct {
	Version           int          `json:"version"`
	Scope             string       `json:"scope"`
	API               string       `json:"apiVersion"`
	Reservation       Reservation  `json:"reservation"`
	ReservedAt        time.Time    `json:"reservedAt"`
	NodeResourceGroup string       `json:"nodeResourceGroup"`
	Phase             Phase        `json:"phase"`
	ETag              string       `json:"eTag,omitempty"`
	Machines          []VMIdentity `json:"machines,omitempty"`
	Probe             *Attempt     `json:"conditionProbe,omitempty"`
	Deletion          *Attempt     `json:"deletion,omitempty"`
	Completion        *Completion  `json:"completion,omitempty"`
}

type poolTarget struct {
	subscription, clusterID, poolName string
}

func parsePoolID(id string) (poolTarget, error) {
	p := strings.Split(id, "/")
	if len(p) != 11 || p[0] != "" || !strings.EqualFold(p[1], "subscriptions") ||
		!uuidRE.MatchString(p[2]) || !strings.EqualFold(p[3], "resourceGroups") ||
		!nameRE.MatchString(p[4]) || !strings.EqualFold(p[5], "providers") ||
		!strings.EqualFold(p[6], "Microsoft.ContainerService") ||
		!strings.EqualFold(p[7], "managedClusters") || !nameRE.MatchString(p[8]) || len(p[8]) > 63 ||
		!strings.EqualFold(p[9], "agentPools") || !poolRE.MatchString(p[10]) {
		return poolTarget{}, errors.New("a literal, fully qualified AKS pool ARM ID with a lowercase pool name is required")
	}
	return poolTarget{subscription: p[2], clusterID: strings.Join(p[:9], "/"), poolName: p[10]}, nil
}

func (r Reservation) validate() error {
	if _, err := parsePoolID(r.PoolID); err != nil {
		return err
	}
	if !uuidRE.MatchString(r.TenantID) || !uuidRE.MatchString(r.RunID) ||
		!literal(r.ClusterResourceUID, 256) {
		return errors.New("explicit tenant UUID, run UUID and cluster resourceUID are required")
	}
	return nil
}

func literal(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func validETag(value string) bool {
	return literal(value, 256) && value != "*" && !strings.HasPrefix(value, "W/") &&
		!strings.ContainsAny(value, ",\\")
}

func differentETag(value string) (string, error) {
	if !validETag(value) {
		return "", errors.New("pool did not return a usable non-wildcard, non-weak ETag")
	}
	data := []byte(value)
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] >= '0' && data[i] <= '9' || data[i] >= 'a' && data[i] <= 'z' || data[i] >= 'A' && data[i] <= 'Z' {
			if data[i] == '0' {
				data[i] = '1'
			} else {
				data[i] = '0'
			}
			return string(data), nil
		}
	}
	return "", errors.New("cannot construct a same-shape nonmatching ETag")
}

func newRequestID() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	text := hex.EncodeToString(value[:])
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

func validateVM(vm VMIdentity, target poolTarget, nodeGroup string) error {
	p := strings.Split(vm.ResourceID, "/")
	if len(p) != 9 && len(p) != 11 {
		return errors.New("machine has an unsupported Compute resource ID")
	}
	if p[0] != "" || !strings.EqualFold(p[1], "subscriptions") || !strings.EqualFold(p[2], target.subscription) ||
		!strings.EqualFold(p[3], "resourceGroups") || !strings.EqualFold(p[4], nodeGroup) ||
		!strings.EqualFold(p[5], "providers") || !strings.EqualFold(p[6], "Microsoft.Compute") ||
		!nameRE.MatchString(p[8]) || !nameRE.MatchString(vm.MachineName) || !uuidRE.MatchString(vm.VMID) {
		return errors.New("machine identity is incomplete or outside the registered cluster node resource group")
	}
	if len(p) == 9 && !strings.EqualFold(p[7], "virtualMachines") ||
		len(p) == 11 && (!strings.EqualFold(p[7], "virtualMachineScaleSets") ||
			!strings.EqualFold(p[9], "virtualMachines") || !nameRE.MatchString(p[10])) {
		return errors.New("machine is not a supported Compute VM resource")
	}
	return nil
}

func (j *Journal) validate() error {
	if j == nil || j.Version != 1 || j.Scope != Scope || j.API != APIVersion || j.ReservedAt.IsZero() ||
		!nameRE.MatchString(j.NodeResourceGroup) {
		return errors.New("unsupported or incomplete fencing journal")
	}
	if err := j.Reservation.validate(); err != nil {
		return err
	}
	if !slices.Contains([]Phase{Reserved, Registered, ProbePending, ProbeRejected, Qualified, DeletePending,
		Deleting, OperationDone, Complete, Failed, ProbeUnexpected}, j.Phase) {
		return errors.New("unrecognized fencing journal phase")
	}
	if j.Phase == Reserved {
		if j.ETag != "" || len(j.Machines) != 0 || j.Probe != nil || j.Deletion != nil || j.Completion != nil {
			return errors.New("reserved journal contains unexpected execution state")
		}
		return nil
	}
	if !validETag(j.ETag) || len(j.Machines) != 1 {
		return errors.New("qualification requires one registered VM and a fixed usable ETag")
	}
	target, _ := parsePoolID(j.Reservation.PoolID)
	for _, vm := range j.Machines {
		if err := validateVM(vm, target, j.NodeResourceGroup); err != nil {
			return err
		}
	}
	for _, attempt := range []*Attempt{j.Probe, j.Deletion} {
		if attempt != nil && (!uuidRE.MatchString(attempt.RequestID) || !validETag(attempt.IfMatch) || attempt.IntentAt.IsZero()) {
			return errors.New("invalid durable deletion attempt")
		}
		if j.Phase == Registered && (j.Probe != nil || j.Deletion != nil) ||
			slices.Contains([]Phase{ProbePending, ProbeRejected, Qualified, ProbeUnexpected}, j.Phase) && j.Deletion != nil {
			return errors.New("journal phase would permit replay of an existing attempt")
		}
	}
	if j.Probe != nil && j.Probe.IfMatch == j.ETag {
		return errors.New("condition probe must not use the matching ETag")
	}
	if j.Deletion != nil && (j.Probe == nil || j.Probe.HTTPStatus != 412 || j.Deletion.IfMatch != j.ETag) {
		return errors.New("delete intent is not bound to the qualified ETag")
	}
	if slices.Contains([]Phase{ProbePending, ProbeRejected, Qualified, DeletePending, Deleting, OperationDone, Complete, ProbeUnexpected}, j.Phase) && j.Probe == nil {
		return errors.New("journal is missing its condition-probe intent")
	}
	if slices.Contains([]Phase{ProbeRejected, Qualified, DeletePending, Deleting, OperationDone, Complete}, j.Phase) && j.Probe.HTTPStatus != 412 {
		return errors.New("journal does not contain a rejected condition probe")
	}
	if slices.Contains([]Phase{DeletePending, Deleting, OperationDone, Complete}, j.Phase) && j.Deletion == nil {
		return errors.New("journal is missing its deletion intent")
	}
	if slices.Contains([]Phase{Deleting, OperationDone, Complete}, j.Phase) &&
		(j.Deletion.HTTPStatus != 202 || j.Deletion.OperationURL == "" ||
			!slices.Contains([]string{"azure-async-operation", "location"}, j.Deletion.PollKind)) {
		return errors.New("journal is missing its accepted operation handle")
	}
	if slices.Contains([]Phase{OperationDone, Complete}, j.Phase) && j.Deletion.Status != "Succeeded" {
		return errors.New("operation completion is not established")
	}
	if j.Phase == Complete {
		if j.Completion == nil || j.Completion.ObservedAt.IsZero() ||
			j.Completion.OperationURL != j.Deletion.OperationURL || !slices.Equal(j.Completion.AbsentVMs, j.Machines) {
			return errors.New("old VM destruction evidence is incomplete")
		}
	} else if j.Completion != nil {
		return errors.New("incomplete journal contains a completion receipt")
	}
	return nil
}

func unexpectedHTTP(status int) error {
	return fmt.Errorf("ARM returned HTTP %d; no success observation accepted", status)
}
