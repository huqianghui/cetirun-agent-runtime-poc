// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrRegistryConflict = errors.New("instance or registry credentials changed; refresh and try again")
	ErrRegistrySave     = errors.New("registry credentials could not be saved; retry for the existing instance")
)

const (
	AnnotationCreateRequest        = "cetirun.microsoft.com/create-request-id"
	AnnotationCreateCaller         = "cetirun.microsoft.com/create-caller"
	AnnotationCreateConfig         = "cetirun.microsoft.com/create-config"
	AnnotationCreateRegistrySource = "cetirun.microsoft.com/create-registry-source"
)

func RegistryID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", ErrRegistrySave
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (c *Client) SetRegistryCredentialNamespace(namespace string) {
	c.registryCredentialNamespace = namespace
}

func (c *Client) registryNamespace() string {
	if c.registryCredentialNamespace != "" {
		return c.registryCredentialNamespace
	}
	return registryauth.DefaultNamespace
}

func RegistryETag(agent *api.Agent) string {
	return `"` + string(agent.UID) + "/" + agent.ResourceVersion + `"`
}

func (c *Client) BeginRegistryUpdate(ctx context.Context, expected *api.Agent, remove bool) (*api.Agent, error) {
	if expected.UID == "" || !expected.DeletionTimestamp.IsZero() {
		return nil, ErrRegistryConflict
	}
	current := expected.DeepCopy()
	id, err := RegistryID()
	if err != nil {
		return nil, err
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[registryauth.Operation] = id
	if remove {
		current.Spec.Agent.RegistrySecretRef = nil
	} else if current.Spec.Agent.RegistrySecretRef == nil {
		current.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "registry-source-" + id}
	}
	if c.client.Patch(ctx, current, client.MergeFromWithOptions(expected, client.MergeFromWithOptimisticLock{})) != nil {
		return nil, ErrRegistryConflict
	}
	return current, nil
}

func (c *Client) registryIntentCurrent(ctx context.Context, expected *api.Agent) bool {
	current := &api.Agent{}
	return c.client.Get(ctx, client.ObjectKeyFromObject(expected), current) == nil &&
		current.UID == expected.UID && current.DeletionTimestamp.IsZero() &&
		reflect.DeepEqual(current.Spec.Agent.Image, expected.Spec.Agent.Image) &&
		reflect.DeepEqual(current.Spec.Agent.RegistrySecretRef, expected.Spec.Agent.RegistrySecretRef) &&
		current.Annotations[registryauth.Operation] == expected.Annotations[registryauth.Operation]
}

// SaveRegistryCredentials never logs or returns upstream Secret errors. Reads
// precede the intent check so a newer successful rotation cannot be overwritten
// by an older operation that resumed after it.
func (c *Client) SaveRegistryCredentials(ctx context.Context, expected *api.Agent, credentials registryauth.Credentials, createOnly bool) error {
	if expected.Spec.Agent.RegistrySecretRef == nil || expected.Spec.Agent.Image == nil || expected.UID == "" {
		return ErrRegistryConflict
	}
	raw, err := registryauth.Encode(*expected.Spec.Agent.Image, credentials)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{}
	err = c.client.Get(ctx, client.ObjectKey{Namespace: c.registryNamespace(), Name: expected.Spec.Agent.RegistrySecretRef.Name}, secret)
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return ErrRegistrySave
	}
	if !c.registryIntentCurrent(ctx, expected) {
		return ErrRegistryConflict
	}
	if !missing {
		if !registryauth.Owned(secret, expected) || !secret.DeletionTimestamp.IsZero() || secret.Type != corev1.SecretTypeDockerConfigJson {
			return ErrRegistryConflict
		}
		if createOnly {
			if registryauth.Validate(secret, expected) != nil || secret.Annotations[registryauth.Operation] != expected.Annotations[registryauth.Operation] {
				return ErrRegistryConflict
			}
			return nil
		}
	} else {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: expected.Spec.Agent.RegistrySecretRef.Name,
			Namespace: c.registryNamespace(), OwnerReferences: []metav1.OwnerReference{registryauth.Owner(expected)},
			Labels: registryauth.Labels(expected.UID)}, Type: corev1.SecretTypeDockerConfigJson}
	}
	if err := registryauth.CheckIsolation(ctx, c.client, c.registryNamespace(), "cetirun-"+expected.Name, "default", expected.Spec.Agent.RegistrySecretRef.Name); err != nil {
		return err
	}
	if !c.registryIntentCurrent(ctx, expected) {
		return ErrRegistryConflict
	}
	secret.Data = map[string][]byte{corev1.DockerConfigJsonKey: raw}
	secret.Annotations = map[string]string{registryauth.Operation: expected.Annotations[registryauth.Operation]}
	if missing {
		err = c.client.Create(ctx, secret)
	} else {
		err = c.client.Update(ctx, secret)
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || !c.registryIntentCurrent(ctx, expected) {
		return ErrRegistryConflict
	}
	if err != nil {
		return ErrRegistrySave
	}
	return nil
}

func (c *Client) RegistryAuthConfigured(ctx context.Context, instance *api.Agent) bool {
	configured, _ := c.RegistryAuthState(ctx, instance)
	return configured
}

func (c *Client) RegistryAuthState(ctx context.Context, instance *api.Agent) (configured, synchronized bool) {
	if instance.Spec.Agent.RegistrySecretRef == nil {
		return false, false
	}
	secret := &corev1.Secret{}
	if c.client.Get(ctx, client.ObjectKey{Namespace: c.registryNamespace(), Name: instance.Spec.Agent.RegistrySecretRef.Name}, secret) != nil {
		return false, false
	}
	configured = registryauth.Validate(secret, instance) == nil && secret.Annotations[registryauth.Operation] == instance.Annotations[registryauth.Operation]
	receipt := instance.Status.RegistryCredentials
	return configured, configured && receipt != nil && receipt.SourceUID == string(secret.UID) && receipt.SourceResourceVersion == secret.ResourceVersion
}
