// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

// ConnectionsMCP is an operator-resolved endpoint and public trust reference.
// The common executor retains its already UID-pinned namespace ServiceAccount;
// this purpose-bound proof grants no Kubernetes API audience or provider token.
type ConnectionsMCP struct {
	URL          string
	Audience     string
	CASecretName string
	Namespace    string
}

func (c *ConnectionsMCP) Validate() error {
	if c == nil {
		return nil
	}
	endpoint, err := url.Parse(c.URL)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.Fragment != "" || endpoint.Path != "/mcp" || c.Audience == "" ||
		c.Audience == "api://AzureADTokenExchange" ||
		len(validation.IsDNS1123Subdomain(c.CASecretName)) != 0 {
		return fmt.Errorf("native Connections requires an exact endpoint, purpose audience and public CA source")
	}
	parts := strings.Split(endpoint.Hostname(), ".")
	if len(parts) != 5 || parts[1] != c.Namespace || strings.Join(parts[2:], ".") != "svc.cluster.local" ||
		len(validation.IsDNS1123Label(parts[0])) != 0 || len(validation.IsDNS1123Label(c.Namespace)) != 0 {
		return fmt.Errorf("native Connections requires a fixed in-cluster endpoint")
	}
	return nil
}

func (c *ConnectionsMCP) TokenVolume() corev1.Volume {
	return corev1.Volume{Name: "connections-identity", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0440)), Sources: []corev1.VolumeProjection{{
			ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: c.Audience, Path: "token", ExpirationSeconds: ptr.To(int64(600))},
		}}},
	}}
}

func (c *ConnectionsMCP) ValidatePod(spec corev1.PodSpec) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	volumes, mounts := 0, 0
	for _, volume := range spec.Volumes {
		if volume.Name == "connections-identity" {
			if !equality.Semantic.DeepEqual(volume, c.TokenVolume()) {
				return fmt.Errorf("Connections proof differs from the exact purpose projection")
			}
			volumes++
		}
	}
	for index, container := range append(spec.Containers, spec.InitContainers...) {
		for _, mount := range container.VolumeMounts {
			if mount.Name == "connections-identity" {
				if index != 0 || container.Name != "cetirun" ||
					mount.MountPath != "/var/run/cetirun/connections" || !mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" {
					return fmt.Errorf("only the primary Agent may receive the Connections purpose proof")
				}
				mounts++
			}
		}
	}
	if volumes != 1 || mounts != 1 {
		return fmt.Errorf("Connections purpose projection is missing or duplicated")
	}
	return nil
}
