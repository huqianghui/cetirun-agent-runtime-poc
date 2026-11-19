// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package registryauth defines the internal Portal/Operator credential contract.
// It does not contact registries or pass credentials to a runtime renderer.
package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"oras.land/oras-go/v2/registry"
	strictjson "sigs.k8s.io/json"
)

const (
	DefaultNamespace = "cetirun-registry-credentials"
	PurposeLabel     = "cetirun.microsoft.com/credential-purpose"
	Purpose          = "registry"
	AgentUIDLabel    = "cetirun.microsoft.com/registry-agent-uid"
	Operation        = "cetirun.microsoft.com/registry-operation"
	SourceUID        = "cetirun.microsoft.com/registry-source-uid"
	SourceVersion    = "cetirun.microsoft.com/registry-source-version"
	MaxBodyBytes     = 64 << 10
	MaxSecretBytes   = 256 << 10
)

var (
	ErrImage       = errors.New("a valid target image is required for registry credentials")
	ErrCredentials = errors.New("a complete registry username and password or token are required")
	ErrSecret      = errors.New("registry credential structure, owner or target registry is invalid")
)

type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (c Credentials) Validate() error {
	if strings.TrimSpace(c.Username) == "" || len(c.Username) > 512 || strings.ContainsAny(c.Username, ":\x00\r\n") ||
		strings.TrimSpace(c.Password) == "" || len(c.Password) > 32<<10 || strings.ContainsRune(c.Password, 0) ||
		strings.Trim(c.Password, "*•●") == "" {
		return ErrCredentials
	}
	return nil
}

// Host follows Docker's default-registry rules and keeps explicit ports.
func Host(image string) (string, error) {
	if image == "" || len(image) > 2048 || strings.ContainsAny(image, " \t\r\n\x00\\?#") || strings.Contains(image, "://") {
		return "", ErrImage
	}
	first, _, slash := strings.Cut(image, "/")
	if !slash || (!strings.ContainsAny(first, ".:") && first != "localhost" && !strings.HasPrefix(first, "[")) {
		image = "docker.io/" + image
	}
	ref, err := registry.ParseReference(image)
	if err != nil {
		return "", ErrImage
	}
	host := strings.ToLower(ref.Registry)
	if host == "index.docker.io" {
		host = "docker.io"
	}
	return host, nil
}

type entry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

type dockerConfig struct {
	Auths map[string]entry `json:"auths"`
}

func Encode(image string, c Credentials) ([]byte, error) {
	host, err := Host(image)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(dockerConfig{Auths: map[string]entry{host: {
		Username: c.Username, Password: c.Password,
		Auth: base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password)),
	}}})
}

func Owner(agent *api.Agent) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}
}

func Owned(secret *corev1.Secret, agent *api.Agent) bool {
	if agent.UID == "" || len(secret.OwnerReferences) != 1 {
		return false
	}
	owner := secret.OwnerReferences[0]
	return owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" && owner.Name == agent.Name && owner.UID == agent.UID
}

func Labels(uid types.UID) map[string]string {
	return map[string]string{PurposeLabel: Purpose, AgentUIDLabel: string(uid)}
}

func PullSecretName(uid types.UID) (string, error) {
	name := "registry-pull-" + strings.ReplaceAll(string(uid), "-", "")
	if uid == "" || len(validation.IsDNS1123Label(name)) != 0 {
		return "", ErrSecret
	}
	return name, nil
}

func Validate(secret *corev1.Secret, agent *api.Agent) error {
	if !Owned(secret, agent) || !secret.DeletionTimestamp.IsZero() || secret.Type != corev1.SecretTypeDockerConfigJson ||
		len(secret.Data) != 1 || agent.Spec.Agent.Image == nil {
		return ErrSecret
	}
	host, err := Host(*agent.Spec.Agent.Image)
	if err != nil {
		return ErrSecret
	}
	raw := secret.Data[corev1.DockerConfigJsonKey]
	if len(raw) == 0 || len(raw) > MaxSecretBytes {
		return ErrSecret
	}
	var cfg dockerConfig
	problems, err := strictjson.UnmarshalStrict(raw, &cfg)
	if err != nil || len(problems) != 0 || len(cfg.Auths) != 1 {
		return ErrSecret
	}
	e, ok := cfg.Auths[host]
	if !ok || (Credentials{Username: e.Username, Password: e.Password}).Validate() != nil ||
		e.Auth != base64.StdEncoding.EncodeToString([]byte(e.Username+":"+e.Password)) {
		return ErrSecret
	}
	return nil
}
