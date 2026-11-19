// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
)

const AssetsMountPath = "/opt/cetirun/cetiadmin"

type RuntimeProjection struct {
	Enabled               bool                  `json:"enabled"`
	AgentName             string                `json:"agentName"`
	AgentUID              string                `json:"agentUID"`
	NamespaceUID          string                `json:"namespaceUID"`
	SourceUID             string                `json:"sourceUID"`
	ConfigHash            string                `json:"configHash"`
	ServiceAccountName    string                `json:"serviceAccountName"`
	OperatorImage         string                `json:"operatorImage"`
	Kubernetes            KubernetesAccess      `json:"kubernetes"`
	AzureWorkloadIdentity AzureWorkloadIdentity `json:"azureWorkloadIdentity"`
	Monitor               Monitor               `json:"monitor"`
	RuntimeConfig         string                `json:"runtimeConfig"`
	Kubeconfig            string                `json:"kubeconfig"`
}

func (p RuntimeProjection) Validate() error {
	if !p.Enabled || p.AgentUID == "" || p.NamespaceUID == "" || p.SourceUID == "" || p.ConfigHash == "" ||
		len(validation.IsDNS1123Label(p.AgentName)) != 0 || len(validation.IsDNS1123Subdomain(p.ServiceAccountName)) != 0 ||
		p.OperatorImage == "" || strings.ContainsAny(p.OperatorImage, " \t\r\n") {
		return fmt.Errorf("CetiAdmin runtime projection requires its source, Agent, namespace, ServiceAccount and Operator image")
	}
	if err := p.Kubernetes.validate(); err != nil {
		return err
	}
	if err := p.Monitor.validate(); err != nil {
		return err
	}
	var document map[string]any
	input := json.NewDecoder(strings.NewReader(p.Kubeconfig))
	if input.Decode(&document) != nil || input.Decode(new(any)) != io.EOF || !reflect.DeepEqual(document, kubeconfigDocument(p.Kubernetes.APIServer, "cetirun-"+p.AgentName)) {
		return fmt.Errorf("CetiAdmin kubeconfig must contain only its fixed context, server, CA and tokenFile")
	}
	kubeconfig, err := clientcmd.Load([]byte(p.Kubeconfig))
	if err != nil || kubeconfig.CurrentContext != "cetiadmin" || len(kubeconfig.Contexts) != 1 || len(kubeconfig.Clusters) != 1 || len(kubeconfig.AuthInfos) != 1 {
		return fmt.Errorf("CetiAdmin kubeconfig must contain the explicit cetiadmin context")
	}
	context, cluster, auth := kubeconfig.Contexts["cetiadmin"], kubeconfig.Clusters["cetiadmin"], kubeconfig.AuthInfos["cetiadmin"]
	if context == nil || cluster == nil || auth == nil || context.Cluster != "cetiadmin" || context.AuthInfo != "cetiadmin" || context.Namespace != "cetirun-"+p.AgentName ||
		cluster.Server != p.Kubernetes.APIServer || cluster.CertificateAuthority != "/var/run/cetirun/kubernetes/ca.crt" || cluster.InsecureSkipTLSVerify || len(cluster.CertificateAuthorityData) != 0 ||
		auth.Token != "" || auth.TokenFile != "/var/run/cetirun/kubernetes/token" || auth.Exec != nil || auth.AuthProvider != nil || auth.Username != "" || auth.Password != "" || auth.ClientKey != "" || len(auth.ClientKeyData) != 0 {
		return fmt.Errorf("CetiAdmin kubeconfig does not match the bound tokenFile and CA")
	}
	var monitor struct {
		Instance ObjectIdentity `json:"instance"`
		Monitor  Monitor        `json:"monitor"`
	}
	decoder := json.NewDecoder(strings.NewReader(p.RuntimeConfig))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&monitor); err != nil || string(monitor.Instance.UID) != p.AgentUID || monitor.Instance.Name != p.AgentName || monitor.Instance.Namespace != "cetirun-"+p.AgentName || monitor.Monitor != p.Monitor {
		return fmt.Errorf("CetiAdmin monitor input does not match its bound instance")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("CetiAdmin monitor input must contain exactly one object")
	}
	return nil
}

func (b Binding) RuntimeProjection(operatorImage, mcpAudience string) (RuntimeProjection, error) {
	if !b.valid {
		return RuntimeProjection{}, ErrUnbound
	}
	if operatorImage == "" || strings.ContainsAny(operatorImage, " \t\n\r") {
		return RuntimeProjection{}, fmt.Errorf("CetiAdmin requires the resolved Operator image")
	}
	if err := b.config.ValidateMCPAudience(mcpAudience); err != nil {
		return RuntimeProjection{}, err
	}
	runtimeConfig, err := json.Marshal(struct {
		Instance ObjectIdentity `json:"instance"`
		Monitor  Monitor        `json:"monitor"`
	}{
		Instance: ObjectIdentity{Name: b.record.Agent.Name, Namespace: b.record.Namespace.Name, UID: b.record.Agent.UID},
		Monitor:  b.config.Monitor,
	})
	if err != nil {
		return RuntimeProjection{}, err
	}
	kubeconfig, err := json.Marshal(kubeconfigDocument(b.config.Kubernetes.APIServer, b.record.Namespace.Name))
	if err != nil {
		return RuntimeProjection{}, err
	}
	return RuntimeProjection{
		Enabled: true, AgentName: b.record.Agent.Name, AgentUID: string(b.record.Agent.UID),
		NamespaceUID: string(b.record.Namespace.UID), SourceUID: string(b.record.Source.UID),
		ConfigHash: b.record.ConfigHash, ServiceAccountName: b.record.ServiceAccount.Name,
		OperatorImage: operatorImage, Kubernetes: cloneConfig(b.config).Kubernetes,
		AzureWorkloadIdentity: b.config.AzureWorkloadIdentity, Monitor: b.config.Monitor,
		RuntimeConfig: string(runtimeConfig), Kubeconfig: string(kubeconfig),
	}, nil
}

func kubeconfigDocument(server, namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Config", "current-context": "cetiadmin",
		"clusters": []any{map[string]any{"name": "cetiadmin", "cluster": map[string]any{
			"server": server, "certificate-authority": "/var/run/cetirun/kubernetes/ca.crt",
		}}},
		"contexts": []any{map[string]any{"name": "cetiadmin", "context": map[string]any{
			"cluster": "cetiadmin", "user": "cetiadmin", "namespace": namespace,
		}}},
		"users": []any{map[string]any{"name": "cetiadmin", "user": map[string]any{
			"tokenFile": "/var/run/cetirun/kubernetes/token",
		}}},
	}
}
