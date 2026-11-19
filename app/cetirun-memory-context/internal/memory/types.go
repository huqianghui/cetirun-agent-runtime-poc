// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

const (
	ContractVersion  = "memory-context/v1"
	SchemaVersion    = "memory/v1"
	HelperABI        = 1
	RetrievalProfile = "memory-retrieval-v1"
	ArtifactProfile  = "artifact-contract-v1"
	MaxInput         = 128 << 10
	MaxManifest      = 256 << 10
	MaxScan          = 256
	MaxNoteScan      = 2048
	MaxScanBytes     = 32 << 20
	MaxRootBytes     = 64 << 20
	HighWaterBytes   = 60 << 20
)

type Error struct {
	Code   int
	Reason string
}

func (e *Error) Error() string              { return e.Reason }
func problem(code int, reason string) error { return &Error{code, reason} }

type Ref struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

func (r Ref) Key() string { return fmt.Sprintf("%s/%d", r.ID, r.Revision) }

var objectID = regexp.MustCompile(`^(note|obs|proc|eval|use|change)-[a-f0-9]{32}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func (r Ref) Valid() bool {
	return objectID.MatchString(r.ID) && r.Revision > 0 && digestPattern.MatchString(r.Digest)
}
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func newID(prefix string) string { return prefix + "-" + hex.EncodeToString(randomBytes()) }
func randomBytes() []byte {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return b[:]
}
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

type Scope map[string]string
type Learning struct {
	Mode                 string   `json:"mode"`
	Profiles             []string `json:"profiles"`
	MaxExtraTurnsPer24h  int      `json:"maxExtraTurnsPer24h"`
	AllowModelEvaluation bool     `json:"allowModelEvaluation"`
}
type Destination struct {
	Kind string  `json:"kind"`
	Ref  *string `json:"ref"`
}
type Delegation struct {
	ParentRequestID string `json:"parentRequestId"`
	Purpose         string `json:"purpose"`
	AllowedRefs     []Ref  `json:"allowedRefs"`
	NoCapture       bool   `json:"noCapture"`
	NoMemory        bool   `json:"noMemory"`
}
type RequestContext struct {
	NoCapture           bool                 `json:"noCapture"`
	NoMemory            bool                 `json:"noMemory"`
	OpportunityID       string               `json:"opportunityId"`
	Audience            string               `json:"audience"`
	Destination         Destination          `json:"destination"`
	Scope               Scope                `json:"scope"`
	Delegation          *Delegation          `json:"delegation,omitempty"`
	CurrentSourceChecks []CurrentSourceCheck `json:"currentSourceChecks,omitempty"`
}
type Request struct {
	ContractVersion    string          `json:"contractVersion"`
	RequestID          string          `json:"requestId"`
	ExpectedGeneration *int64          `json:"expectedGeneration,omitempty"`
	RequestContext     RequestContext  `json:"requestContext"`
	Payload            json.RawMessage `json:"payload"`
	native             bool
	nativeContextKey   string
	nativeBinding      *NativeBinding
}

type CurrentSourceCheck struct {
	Check      SourceCheck `json:"check"`
	ObservedAt string      `json:"observedAt"`
	Assurance  string      `json:"assurance"`
	ContextKey string      `json:"contextKey"`
}
type Partial struct {
	LimitHit  bool     `json:"limitHit"`
	Unscanned int      `json:"unscanned"`
	Omitted   int      `json:"omitted,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
}
type Response struct {
	OK         bool     `json:"ok"`
	Reason     string   `json:"reason"`
	Generation int64    `json:"generation"`
	Data       any      `json:"data"`
	Partial    *Partial `json:"partial"`
	Retryable  bool     `json:"retryable"`
}
type Source struct {
	Kind       string  `json:"kind"`
	Ref        string  `json:"ref"`
	Version    *string `json:"version"`
	CurrentRef *string `json:"currentRef"`
	Origin     string  `json:"origin"`
	ObservedAt *string `json:"observedAt"`
	Assurance  string  `json:"assurance"`
}
type Derived struct {
	Ref  Ref    `json:"ref"`
	Role string `json:"role"`
}
type Relation struct {
	Type      string `json:"type"`
	TargetRef Ref    `json:"targetRef"`
	Reason    string `json:"reason"`
}
type Revalidation struct {
	Mode        string  `json:"mode"`
	ReviewAfter *string `json:"reviewAfter"`
}
type Note struct {
	SchemaVersion string       `json:"schemaVersion"`
	ID            string       `json:"id"`
	Revision      int64        `json:"revision"`
	Kind          string       `json:"kind"`
	Title         string       `json:"title"`
	Scope         Scope        `json:"scope"`
	Status        string       `json:"status"`
	UpdatedAt     string       `json:"updatedAt"`
	TemporalClass string       `json:"temporalClass"`
	LoadHint      string       `json:"loadHint"`
	Revalidation  Revalidation `json:"revalidation"`
	Sources       []Source     `json:"sources"`
	DerivedFrom   []Derived    `json:"derivedFrom"`
	Relations     []Relation   `json:"relations"`
	DecisionState string       `json:"decisionState,omitempty"`
	Aliases       []string     `json:"aliases,omitempty"`
	Keywords      []string     `json:"keywords,omitempty"`
	Tags          []string     `json:"tags,omitempty"`
	Body          string       `json:"body,omitempty"`
}
type SourceCheck struct {
	TargetRef       Ref     `json:"targetRef"`
	SourceRef       string  `json:"sourceRef"`
	CurrentVersion  *string `json:"currentVersion"`
	Result          string  `json:"result"`
	EvidenceRef     *string `json:"evidenceRef"`
	NextReviewAfter *string `json:"nextReviewAfter"`
}
type Observation struct {
	ID              string       `json:"id"`
	Revision        int64        `json:"revision"`
	OpportunityID   string       `json:"opportunityId"`
	ObservedAt      string       `json:"observedAt"`
	SourceKind      string       `json:"sourceKind"`
	Assurance       string       `json:"assurance"`
	Outcome         string       `json:"outcome"`
	ProfileRef      *Ref         `json:"profileRef"`
	SourceRef       string       `json:"sourceRef"`
	RedactedSummary string       `json:"redactedSummary"`
	Rationale       string       `json:"rationale,omitempty"`
	SourceCheck     *SourceCheck `json:"sourceCheck,omitempty"`
}
type RetrievalPlan struct {
	Stages     []string `json:"stages"`
	RankBy     []string `json:"rankBy"`
	MaxResults int      `json:"maxResults"`
}

func Baseline() RetrievalPlan {
	return RetrievalPlan{[]string{"exact-title", "alias", "keywords"}, []string{"scope-match", "evidence-recency"}, 5}
}

type Playbook struct {
	Steps            []string `json:"steps"`
	ExpectedEvidence []string `json:"expectedEvidence"`
	StopConditions   []string `json:"stopConditions"`
}
type Procedure struct {
	ID                    string            `json:"id"`
	Revision              int64             `json:"revision"`
	Kind                  string            `json:"kind"`
	Parent                *Ref              `json:"parent"`
	Scope                 Scope             `json:"scope"`
	Applicability         []string          `json:"applicability"`
	Exclusions            []string          `json:"exclusions"`
	Hypothesis            string            `json:"hypothesis"`
	SourceObservationIDs  []string          `json:"sourceObservationIds"`
	DerivedFrom           []Derived         `json:"derivedFrom"`
	RequiredKnowledgeRefs []Ref             `json:"requiredKnowledgeRefs"`
	Compatibility         map[string]string `json:"compatibility"`
	Payload               json.RawMessage   `json:"payload"`
}
type OutputRef struct {
	Kind   string  `json:"kind"`
	Ref    string  `json:"ref"`
	Digest *string `json:"digest"`
}
type TraceCoverage struct {
	Status    string   `json:"status"`
	Reasons   []string `json:"reasons"`
	Assurance string   `json:"assurance"`
}
type Use struct {
	ID                 string            `json:"id"`
	Revision           int64             `json:"revision"`
	OpportunityID      string            `json:"opportunityId"`
	SelectedRef        *Ref              `json:"selectedRef"`
	ManifestGeneration int64             `json:"manifestGeneration"`
	ProfileRef         *Ref              `json:"profileRef"`
	Compatibility      map[string]string `json:"compatibility"`
	Assignment         string            `json:"assignment"`
	OutcomeRef         *Ref              `json:"outcomeRef"`
	InjectedRefs       []Ref             `json:"injectedRefs"`
	CitedRefs          []Ref             `json:"citedRefs"`
	OutputRefs         []OutputRef       `json:"outputRefs"`
	TraceCoverage      TraceCoverage     `json:"traceCoverage"`
}
type Change struct {
	ID               string `json:"id"`
	Revision         int64  `json:"revision"`
	GenerationBefore int64  `json:"generationBefore"`
	GenerationAfter  int64  `json:"generationAfter"`
	Operation        string `json:"operation"`
	AffectedRefs     []Ref  `json:"affectedRefs"`
}
