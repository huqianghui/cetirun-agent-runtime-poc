// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync/atomic"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
)

// MaxConfigurationBytes bounds each complete typed configuration image,
// including status and managed fields, not just its writable content.
const MaxConfigurationBytes = 256 * 1024

type configurationImage struct {
	object    client.Object
	wire      *unstructured.Unstructured
	reference w.SourceReference
	digest    string
	spec      any
	status    any
	metadata  metav1.ObjectMeta
}

type configurationChange struct {
	before, after   configurationImage
	team            *TeamChange
	unchangedDigest string
}

// AcquireConfigurationChange reserves an exact update of a persisted Team,
// Sandbox or AgentPolicy. Existing tickets are recovered before reading the
// source: a committed write may already have advanced its resource version.
// Recovery never changes the recorded operation or its affected Team classes.
func (s Store) AcquireConfigurationChange(ctx context.Context, before, after client.Object) (ChangeTicket, error) {
	change, err := newConfigurationChange(before, after, false)
	if err != nil {
		return ChangeTicket{}, err
	}
	ticket, found, err := s.configurationTicket(ctx, change)
	if err != nil || found {
		return ticket, err
	}
	if err := change.checkBefore(ctx, s.APIReader); err != nil {
		return ChangeTicket{}, err
	}
	if team, ok := change.before.object.(*api.Team); ok && change.team != nil {
		return s.AcquireTeamChange(ctx, team, change.after.object.(*api.Team))
	}
	return s.acquireChange(ctx, change.before.reference, change.after.digest, change.before.digest, nil, change.unchangedDigest, "")
}

// ReadConfigurationChange only looks up the exact journaled update. It neither
// acquires nor releases a reservation and works with a read-only Store.
func (s Store) ReadConfigurationChange(ctx context.Context, before, after client.Object) (ChangeTicket, error) {
	change, err := newConfigurationChange(before, after, false)
	if err != nil {
		return ChangeTicket{}, err
	}
	ticket, found, err := s.configurationTicket(ctx, change)
	if err != nil {
		return ChangeTicket{}, err
	}
	if !found {
		return ChangeTicket{}, fmt.Errorf("%w: configuration change is not recorded", ErrUnavailable)
	}
	return ticket, nil
}

// AuthorizeConfigurationUpdate is an additional read-only admission check, not
// an RBAC or ordinary configuration-admission bypass. Only the exact Changing
// ticket authorizes the proposed whole write; operator identity grants nothing.
//
// Unlike file/adapter inputs, admission objects have passed API machinery.
// A spec change may already have incremented generation by exactly one, and
// managedFields has already been rebuilt by the API field manager. Those fields
// are not writable authorization inputs. All other immutable metadata and status
// must be preserved, and the old object must still match an authoritative read.
// This cannot attest to original request fields that the API server has already
// reset (including status); it validates admission objects, not raw request bytes.
func (s Store) AuthorizeConfigurationUpdate(ctx context.Context, before, after client.Object) error {
	change, err := newConfigurationChange(before, after, true)
	if err != nil {
		return err
	}
	ticket, found, err := s.configurationTicket(ctx, change)
	if err != nil {
		return err
	}
	if !found || ticket.Change.Phase != Changing {
		return fmt.Errorf("%w: the exact changing configuration ticket is required", ErrConflict)
	}
	if err := change.checkBefore(ctx, s.APIReader); err != nil {
		return err
	}
	return s.CheckChange(ctx, ticket)
}

// AuthorizeConfigurationFinalization permits only finalizer removal from an
// exact terminating configuration unused by any recorded operation. Ordinary
// authorization still applies; this read-only check never releases reservations.
func (s Store) AuthorizeConfigurationFinalization(ctx context.Context, before, after client.Object) error {
	previous, err := newConfigurationImage(before)
	if err != nil {
		return err
	}
	proposed, err := newConfigurationImage(after)
	if err != nil {
		return err
	}
	if previous.reference != proposed.reference || previous.object.GetDeletionTimestamp().IsZero() {
		return ErrConflict
	}
	oldFinalizers, newFinalizers := previous.object.GetFinalizers(), proposed.object.GetFinalizers()
	if len(newFinalizers) >= len(oldFinalizers) {
		return ErrConflict
	}
	for i, finalizer := range newFinalizers {
		if !slices.Contains(oldFinalizers, finalizer) || slices.Contains(newFinalizers[:i], finalizer) {
			return ErrConflict
		}
	}
	oldObject := previous.object.DeepCopyObject().(client.Object)
	newObject := proposed.object.DeepCopyObject().(client.Object)
	newObject.SetFinalizers(oldFinalizers)
	oldObject.SetManagedFields(nil)
	newObject.SetManagedFields(nil)
	if !equality.Semantic.DeepEqual(oldObject, newObject) {
		return ErrConflict
	}
	if err := (&configurationChange{before: previous}).checkBefore(ctx, s.APIReader); err != nil {
		return err
	}
	_, state, err := s.read(ctx)
	if err != nil {
		return err
	}
	for _, change := range state.Changes {
		if sourceKey(change.Before) == sourceKey(previous.reference) {
			return ErrConflict
		}
	}
	for _, execution := range state.Executions {
		if uses(execution.Intent.Sources, previous.reference) {
			return ErrConflict
		}
	}
	for _, native := range state.NativeReservations {
		if uses(native.Intent.Sources, previous.reference) {
			return ErrConflict
		}
	}
	return ctx.Err()
}

func (s Store) configurationTicket(ctx context.Context, change *configurationChange) (ChangeTicket, bool, error) {
	return s.matchConfigurationTicket(ctx, change.before.reference, change.matches)
}

func (s Store) matchConfigurationTicket(ctx context.Context, before w.SourceReference, matches func(Change) bool) (ChangeTicket, bool, error) {
	authority, state, err := s.read(ctx)
	if err != nil {
		return ChangeTicket{}, false, err
	}
	for _, existing := range state.Changes {
		if sourceKey(existing.Before) != sourceKey(before) {
			continue
		}
		if !matches(existing) {
			return ChangeTicket{}, false, ErrConflict
		}
		return ChangeTicket{AuthorityUID: authority.UID, Change: cloneChange(existing)}, true, nil
	}
	return ChangeTicket{}, false, nil
}

// KubernetesChangeAdapter submits at most one conditional Update per adapter.
// Readers must be authoritative/uncached. A nil writer permits observation
// only. Neither client may weaken the original UID/resourceVersion precondition
// or implement an application-level Update retry.
type KubernetesChangeAdapter struct {
	writer    client.Client
	reader    client.Reader
	change    *configurationChange
	attempted atomic.Bool
}

var _ ChangeAdapter = (*KubernetesChangeAdapter)(nil)

// NewKubernetesChangeAdapter retains private copies of both input images.
// Only spec, labels, annotations, owner references and finalizers may differ.
// Schema defaults and omitted empty metadata are applied before pinning the
// complete write, without sorting or rewriting explicitly supplied values.
// Spec no-ops retain the verified server representation so serialization alone
// cannot invalidate a current acceptance observation.
func NewKubernetesChangeAdapter(writer client.Client, reader client.Reader, before, after client.Object) (*KubernetesChangeAdapter, error) {
	change, err := newConfigurationChange(before, after, false)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("%w: an authoritative configuration reader is required", ErrUnavailable)
	}
	return &KubernetesChangeAdapter{writer: writer, reader: reader, change: change}, nil
}

func (a *KubernetesChangeAdapter) Submit(ctx context.Context, ticket ChangeTicket, guard func(context.Context) error) error {
	ticket = ticket.DeepCopy()
	if err := a.validate(ctx, ticket); err != nil {
		return err
	}
	if a.writer == nil || guard == nil {
		return fmt.Errorf("%w: configuration writer and coordination guard are required", ErrUnavailable)
	}
	if !a.attempted.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: configuration submission cannot be retried; observe with the same inputs", ErrConflict)
	}
	if err := guard(ctx); err != nil {
		return err
	}
	current, err := a.change.readBefore(ctx, a.reader)
	if err != nil {
		return err
	}
	request, err := a.change.updateRequest(current)
	if err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// No rebasing, patching, create-on-missing or retry. Update carries the
	// original UID and resourceVersion in the complete request object.
	if err := a.writer.Update(ctx, request, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		return fmt.Errorf("%w: configuration Update outcome is uncertain; observe with the same inputs: %w", ErrUnavailable, err)
	}
	return nil
}

func (change *configurationChange) updateRequest(current configurationImage) (client.Object, error) {
	request := change.after.object.DeepCopyObject().(client.Object)
	beforeSpec, err := json.Marshal(change.before.spec)
	if err != nil {
		return nil, fmt.Errorf("%w: encode original configuration spec: %w", ErrUnavailable, err)
	}
	afterSpec, err := json.Marshal(change.after.spec)
	if err != nil {
		return nil, fmt.Errorf("%w: encode desired configuration spec: %w", ErrUnavailable, err)
	}
	if !bytes.Equal(beforeSpec, afterSpec) {
		return request, nil
	}
	if current.wire == nil {
		return nil, fmt.Errorf("%w: authoritative configuration representation is required", ErrUnavailable)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: encode configuration Update: %w", ErrUnavailable, err)
	}
	proposed := &unstructured.Unstructured{}
	if err := proposed.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("%w: decode configuration Update: %w", ErrUnavailable, err)
	}
	if reflect.DeepEqual(current.wire.Object["spec"], proposed.Object["spec"]) {
		return request, nil
	}
	// A complete typed update can insert omitted zero-value objects. Retain
	// only the verified original spec; desired metadata and original CAS pins
	// still come from the reserved request, never from a newer source version.
	spec, found, err := unstructured.NestedFieldCopy(current.wire.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("%w: read original configuration spec: %w", ErrUnavailable, err)
	}
	if found {
		proposed.Object["spec"] = spec
	} else {
		delete(proposed.Object, "spec")
	}
	raw, err = proposed.MarshalJSON()
	if err != nil || len(raw) > MaxConfigurationBytes {
		return nil, fmt.Errorf("%w: metadata-only configuration Update exceeds its encoding bound", ErrUnavailable)
	}
	return proposed, nil
}

// ObserveChange never submits, adopts, aborts or clears an operation. An
// unchanged resourceVersion, missing, replaced or mismatched object leaves its
// outcome unknown; unchanged writable content at a new resourceVersion is a
// definitive supersession.
func (a *KubernetesChangeAdapter) ObserveChange(ctx context.Context, ticket ChangeTicket) (*ChangeOutcome, error) {
	ticket = ticket.DeepCopy()
	if err := a.validate(ctx, ticket); err != nil {
		return nil, err
	}
	current, err := a.change.readCurrent(ctx, a.reader)
	if err != nil {
		return nil, err
	}
	before := a.change.before
	oldMetadata, newMetadata := before.metadata, current.metadata
	newMetadata.ResourceVersion = oldMetadata.ResourceVersion
	newMetadata.Generation = oldMetadata.Generation
	newMetadata.ManagedFields = oldMetadata.ManagedFields
	if current.reference.UID != before.reference.UID ||
		current.reference.ResourceVersion == before.reference.ResourceVersion ||
		!current.object.GetDeletionTimestamp().IsZero() || current.object.GetDeletionGracePeriodSeconds() != nil ||
		!equality.Semantic.DeepEqual(oldMetadata, newMetadata) {
		return nil, fmt.Errorf("%w: configuration outcome is still uncertain; observe with the same inputs", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outcome := &ChangeOutcome{
		AuthorityUID: string(ticket.AuthorityUID), OperationID: ticket.Change.ID,
		Before: before.reference, After: current.reference, DesiredDigest: a.change.after.digest,
		NoPendingSubmissions: true,
	}
	switch current.digest {
	case a.change.after.digest:
		outcome.Applied = true
	case a.change.before.digest:
		// The current resourceVersion excludes every delayed conditional
		// submission, but the complete writable content is still the exact
		// before image. This is a safe aborted, superseded update rather than
		// evidence that the desired configuration committed.
		outcome.Aborted, outcome.Superseded = true, true
	default:
		return nil, fmt.Errorf("%w: configuration outcome is still uncertain; observe with the same inputs", ErrUnavailable)
	}
	return outcome, nil
}

func (a *KubernetesChangeAdapter) validate(ctx context.Context, ticket ChangeTicket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || a.reader == nil || a.change == nil {
		return fmt.Errorf("%w: a configured Kubernetes change adapter is required", ErrUnavailable)
	}
	if !literal(string(ticket.AuthorityUID), 128) || !literal(ticket.Change.ID, 128) ||
		ticket.Change.Phase != Changing || !a.change.matches(ticket.Change) {
		return ErrConflict
	}
	return nil
}

func (change *configurationChange) matches(recorded Change) bool {
	return recorded.Before == change.before.reference && recorded.DesiredDigest == change.after.digest &&
		recorded.BeforeContentDigest == change.before.digest && recorded.UnchangedDigest == change.unchangedDigest &&
		equality.Semantic.DeepEqual(recorded.Team, change.team)
}

func (change *configurationChange) readCurrent(ctx context.Context, reader client.Reader) (configurationImage, error) {
	if err := ctx.Err(); err != nil {
		return configurationImage{}, err
	}
	if reader == nil {
		return configurationImage{}, fmt.Errorf("%w: an authoritative configuration reader is required", ErrUnavailable)
	}
	// A typed GET would silently discard fields introduced by a newer server.
	// Read the whole approved kind and decode strictly before comparing images.
	observed := &unstructured.Unstructured{}
	observed.SetGroupVersionKind(change.before.object.GetObjectKind().GroupVersionKind())
	if err := reader.Get(ctx, client.ObjectKeyFromObject(change.before.object), observed); err != nil {
		return configurationImage{}, fmt.Errorf("%w: configuration observation failed: %w", ErrUnavailable, err)
	}
	raw, err := observed.MarshalJSON()
	if err != nil || len(raw) > MaxConfigurationBytes {
		return configurationImage{}, fmt.Errorf("%w: observed configuration exceeds its encoding bound", ErrUnavailable)
	}
	var object client.Object
	switch change.before.object.(type) {
	case *api.Team:
		object = &api.Team{}
	case *api.Sandbox:
		object = &api.Sandbox{}
	case *api.AgentPolicy:
		object = &api.AgentPolicy{}
	}
	problems, err := strictjson.UnmarshalStrict(raw, object)
	if err != nil || len(problems) != 0 {
		return configurationImage{}, fmt.Errorf("%w: unknown or malformed observed configuration fields", ErrUnavailable)
	}
	current, err := newConfigurationImage(object)
	if err != nil {
		return configurationImage{}, err
	}
	if sourceKey(current.reference) != sourceKey(change.before.reference) {
		return configurationImage{}, ErrConflict
	}
	current.wire = observed
	return current, ctx.Err()
}

func (change *configurationChange) checkBefore(ctx context.Context, reader client.Reader) error {
	_, err := change.readBefore(ctx, reader)
	return err
}

func (change *configurationChange) readBefore(ctx context.Context, reader client.Reader) (configurationImage, error) {
	current, err := change.readCurrent(ctx, reader)
	if err != nil {
		return configurationImage{}, err
	}
	before := change.before
	if before.reference != current.reference || before.digest != current.digest ||
		!equality.Semantic.DeepEqual(before.metadata, current.metadata) ||
		!equality.Semantic.DeepEqual(before.status, current.status) {
		return configurationImage{}, fmt.Errorf("%w: configuration before image is not authoritative", ErrConflict)
	}
	return current, nil
}

func newConfigurationChange(before, after client.Object, admission bool) (*configurationChange, error) {
	oldImage, err := newConfigurationImage(before)
	if err != nil {
		return nil, err
	}
	newImage, err := newConfigurationImage(after)
	if err != nil {
		return nil, err
	}
	for _, image := range []configurationImage{oldImage, newImage} {
		if !image.object.GetDeletionTimestamp().IsZero() || image.object.GetDeletionGracePeriodSeconds() != nil {
			return nil, fmt.Errorf("%w: configuration changes require nonterminating objects", ErrUnavailable)
		}
	}
	if oldImage.reference != newImage.reference {
		return nil, fmt.Errorf("%w: configuration updates require the same kind, key, UID and original resourceVersion", ErrConflict)
	}
	if oldImage.digest == newImage.digest {
		return nil, fmt.Errorf("%w: configuration updates require a writable-content change", ErrConflict)
	}
	oldMetadata, newMetadata := oldImage.metadata, newImage.metadata
	if admission {
		// The API server compares wire spec, not normalized effective inputs.
		// Materializing defaulted objects can advance generation without changing
		// the effective spec; the exact reserved mutation digest still must match.
		if newMetadata.Generation != oldMetadata.Generation &&
			(oldMetadata.Generation == math.MaxInt64 || newMetadata.Generation != oldMetadata.Generation+1) {
			return nil, fmt.Errorf("%w: unexpected admission generation", ErrConflict)
		}
		newMetadata.Generation = oldMetadata.Generation
		newMetadata.ManagedFields = oldMetadata.ManagedFields
	}
	if !equality.Semantic.DeepEqual(oldMetadata, newMetadata) ||
		!equality.Semantic.DeepEqual(oldImage.status, newImage.status) {
		return nil, fmt.Errorf("%w: configuration updates must preserve server metadata and status", ErrConflict)
	}
	change := &configurationChange{before: oldImage, after: newImage}
	if team, ok := oldImage.object.(*api.Team); ok {
		afterTeam := newImage.object.(*api.Team)
		if !sandbox.TeamPolicyConfigured(team) || !sandbox.TeamPolicyConfigured(afterTeam) {
			// Legacy transitions have no complete selection on both sides.
			// Reserve the whole Team rather than inventing a selection scope.
			return change, nil
		}
		oldInputs, err := w.ExecutionTeamInputs(team)
		if err != nil {
			return nil, err
		}
		newInputs, err := w.ExecutionTeamInputs(afterTeam)
		if err != nil {
			return nil, err
		}
		if err := oldInputs.Validate(); err != nil {
			return nil, err
		}
		if err := newInputs.Validate(); err != nil {
			return nil, err
		}
		change.team = &TeamChange{Before: oldInputs, After: newInputs}
	} else {
		oldDigest, err := w.ExecutionObjectDigest(oldImage.object)
		if err != nil {
			return nil, err
		}
		newDigest, err := w.ExecutionObjectDigest(newImage.object)
		if err != nil {
			return nil, err
		}
		if oldDigest == newDigest {
			change.unchangedDigest = oldDigest
		}
	}
	return change, nil
}

func newConfigurationImage(input client.Object) (configurationImage, error) {
	var image configurationImage
	var kind string
	switch object := input.(type) {
	case *api.Team:
		if object != nil {
			image.object, kind = object.DeepCopy(), "Team"
		}
	case *api.Sandbox:
		if object != nil {
			image.object, kind = object.DeepCopy(), "Sandbox"
		}
	case *api.AgentPolicy:
		if object != nil {
			image.object, kind = object.DeepCopy(), "AgentPolicy"
		}
	}
	if image.object == nil {
		return image, fmt.Errorf("%w: only typed Team, Sandbox and AgentPolicy configurations are supported", ErrUnavailable)
	}
	object := image.object
	gvk := object.GetObjectKind().GroupVersionKind()
	expectedGVK := api.GroupVersion.WithKind(kind)
	if !gvk.Empty() && gvk != expectedGVK {
		return image, fmt.Errorf("%w: configuration API version or kind does not match its type", ErrUnavailable)
	}
	object.GetObjectKind().SetGroupVersionKind(expectedGVK)
	image.reference = w.SourceReference{APIVersion: api.GroupVersion.String(), Kind: kind,
		Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()}
	// Persistence is established by UID/RV and the authoritative before image,
	// not a synthesized generation floor (fake persisted clients may use zero).
	if !validSource(image.reference) || object.GetNamespace() != "" ||
		len(validation.IsDNS1123Subdomain(object.GetName())) != 0 || object.GetGeneration() < 0 {
		return image, fmt.Errorf("%w: a persisted cluster-scoped configuration identity is required", ErrUnavailable)
	}
	// ObjectMeta omits empty collections on the wire. Canonicalizing only that
	// representation avoids mistaking an empty-vs-nil round trip for drift.
	if len(object.GetLabels()) == 0 {
		object.SetLabels(nil)
	}
	if len(object.GetAnnotations()) == 0 {
		object.SetAnnotations(nil)
	}
	if len(object.GetFinalizers()) == 0 {
		object.SetFinalizers(nil)
	}
	if len(object.GetOwnerReferences()) == 0 {
		object.SetOwnerReferences(nil)
	}
	switch object := object.(type) {
	case *api.Sandbox:
		object.Spec = sandbox.DefaultSandbox(object.Spec)
	case *api.AgentPolicy:
		object.Spec = sandbox.DefaultAgentPolicy(object.Spec)
	}
	raw, err := json.Marshal(object)
	if err != nil || len(raw) > MaxConfigurationBytes {
		return image, fmt.Errorf("%w: configuration image exceeds its encoding bound", ErrUnavailable)
	}
	switch object := object.(type) {
	case *api.Team:
		image.spec, image.status, image.metadata = object.Spec, object.Status, object.ObjectMeta
		image.digest, err = w.TeamMutationDigest(object)
	case *api.Sandbox:
		image.spec, image.status, image.metadata = object.Spec, object.Status, object.ObjectMeta
	case *api.AgentPolicy:
		image.spec, image.status, image.metadata = object.Spec, object.Status, object.ObjectMeta
	}
	if err != nil {
		return image, err
	}
	// Normalize comparison metadata only; retain the original submission image.
	image.metadata = *image.metadata.DeepCopy()
	for _, entry := range image.metadata.ManagedFields {
		if entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		problems, err := strictjson.UnmarshalStrict(entry.FieldsV1.Raw, &fields)
		if err != nil || len(problems) != 0 || fields == nil {
			return image, fmt.Errorf("%w: configuration managed fields must contain an unambiguous JSON object", ErrUnavailable)
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			return image, fmt.Errorf("encode configuration managed fields: %w", err)
		}
		entry.FieldsV1.Raw = raw
	}
	if image.digest == "" {
		raw, err := json.Marshal(struct {
			Spec                any
			Owners              []metav1.OwnerReference
			Finalizers          []string
			Labels, Annotations map[string]string
		}{image.spec, object.GetOwnerReferences(), object.GetFinalizers(), object.GetLabels(), object.GetAnnotations()})
		if err != nil {
			return image, fmt.Errorf("%w: configuration mutation encoding failed", ErrUnavailable)
		}
		sum := sha256.Sum256(raw)
		image.digest = hex.EncodeToString(sum[:])
	}
	image.metadata.Labels, image.metadata.Annotations = nil, nil
	image.metadata.OwnerReferences, image.metadata.Finalizers = nil, nil
	return image, nil
}
