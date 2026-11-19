// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"errors"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConfigurationVersion identifies the exact Team or Sandbox an administrator read.
type ConfigurationVersion struct {
	UID             types.UID
	ResourceVersion string
}

var ErrConfigurationChanged = errors.New("Configuration changed; reload it before saving or deleting")

func checkConfigurationVersion(object client.Object, version ConfigurationVersion) error {
	if version.UID == "" || version.ResourceVersion == "" || object.GetUID() != version.UID || object.GetResourceVersion() != version.ResourceVersion {
		return ErrConfigurationChanged
	}
	return nil
}

func (c *Client) recoverConfigurationUpdate(ctx context.Context, before, after client.Object) error {
	if c.configurationCoordination != nil {
		if err := c.configurationCoordination.RecoverUpdate(ctx, before, after); err != nil {
			return fmt.Errorf("observe recorded configuration update: %w", err)
		}
	}
	return nil
}

func (c *Client) updateConfiguration(ctx context.Context, before, after client.Object) error {
	if c.configurationCoordination != nil {
		if err := c.configurationCoordination.Update(ctx, before, after); err != nil {
			return fmt.Errorf("coordinate configuration update: %w", err)
		}
		return c.client.Get(ctx, client.ObjectKeyFromObject(after), after)
	}
	return c.client.Patch(ctx, after, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.FieldValidation(metav1.FieldValidationStrict))
}

func (c *Client) deleteConfiguration(ctx context.Context, object client.Object) error {
	if c.configurationCoordination != nil {
		return c.configurationCoordination.Delete(ctx, object)
	}
	uid, version := object.GetUID(), object.GetResourceVersion()
	return c.client.Delete(ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &version}, client.PropagationPolicy(metav1.DeletePropagationBackground))
}

// UpdateTeamSpec replaces the complete editable Team configuration using the
// caller's version. A conflict is returned to the administrator, never retried.
func (c *Client) UpdateTeamSpec(ctx context.Context, name string, spec api.TeamSpec, version ConfigurationVersion) (*api.Team, error) {
	team, err := c.GetTeam(ctx, name)
	if err != nil {
		return nil, err
	}
	before := team.DeepCopy()
	team.Spec = *spec.DeepCopy()
	if err := c.recoverConfigurationUpdate(ctx, before, team); err != nil {
		return nil, err
	}
	if err := checkConfigurationVersion(team, version); err != nil {
		return nil, err
	}
	if equality.Semantic.DeepEqual(before.Spec, team.Spec) {
		return team, nil
	}
	if err := c.updateConfiguration(ctx, before, team); err != nil {
		return nil, err
	}
	return team, nil
}

func (c *Client) DeleteTeam(ctx context.Context, name string, version ConfigurationVersion) error {
	team, err := c.GetTeam(ctx, name)
	if err != nil {
		return err
	}
	if err := checkConfigurationVersion(team, version); err != nil {
		return err
	}
	return c.deleteConfiguration(ctx, team)
}

func (c *Client) ListSandboxes(ctx context.Context) ([]api.Sandbox, error) {
	list := &api.SandboxList{}
	if err := c.client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *Client) GetSandbox(ctx context.Context, name string) (*api.Sandbox, error) {
	box := &api.Sandbox{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: name}, box); err != nil {
		return nil, err
	}
	return box, nil
}

func (c *Client) CreateSandbox(ctx context.Context, box *api.Sandbox) (*api.Sandbox, error) {
	if err := c.client.Create(ctx, box, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		return nil, err
	}
	return box, nil
}

func (c *Client) UpdateSandboxSpec(ctx context.Context, name string, spec api.SandboxSpec, version ConfigurationVersion) (*api.Sandbox, error) {
	box, err := c.GetSandbox(ctx, name)
	if err != nil {
		return nil, err
	}
	before := box.DeepCopy()
	box.Spec = *spec.DeepCopy()
	if err := c.recoverConfigurationUpdate(ctx, before, box); err != nil {
		return nil, err
	}
	if err := checkConfigurationVersion(box, version); err != nil {
		return nil, err
	}
	if equality.Semantic.DeepEqual(before.Spec, box.Spec) {
		return box, nil
	}
	if err := c.updateConfiguration(ctx, before, box); err != nil {
		return nil, err
	}
	return box, nil
}

func (c *Client) DeleteSandbox(ctx context.Context, name string, version ConfigurationVersion) error {
	box, err := c.GetSandbox(ctx, name)
	if err != nil {
		return err
	}
	if err := checkConfigurationVersion(box, version); err != nil {
		return err
	}
	return c.deleteConfiguration(ctx, box)
}
