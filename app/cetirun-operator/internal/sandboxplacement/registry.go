// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	strictjson "sigs.k8s.io/json"
)

const (
	ConfigMapKey        = "placement.json"
	RegistryVersion     = "v1"
	MaxRegistryBytes    = 256 * 1024
	MaxNodes            = sandboxworkload.MaxPlacementNodes
	MaxDaemonSets       = 64
	maxRegistryDepth    = 16
	maxRegistryTokens   = 65536
	maxBootstrapPods    = MaxNodes
	maxExpectationBytes = 512 * 1024
)

type registry struct {
	Version            string                `json:"version"`
	Nodes              []registeredNode      `json:"nodes"`
	PlatformDaemonSets []registeredDaemonSet `json:"platformDaemonSets"`
}

type registeredNode struct {
	Name        string           `json:"name"`
	UID         types.UID        `json:"uid"`
	ProviderID  string           `json:"providerID"`
	Reservation *nodeReservation `json:"reservation,omitempty"`
}

type nodeReservation struct {
	Agent       sandboxworkload.ObjectIdentity `json:"agent"`
	Namespace   sandboxworkload.ObjectIdentity `json:"namespace"`
	StatefulSet sandboxworkload.ObjectIdentity `json:"statefulSet"`
}

type registeredDaemonSet struct {
	Name           string                           `json:"name"`
	UID            types.UID                        `json:"uid"`
	Namespace      sandboxworkload.ObjectIdentity   `json:"namespace"`
	TemplateDigest string                           `json:"templateDigest"`
	TrustedCreator string                           `json:"trustedCreator"`
	BootstrapPods  []sandboxworkload.ObjectIdentity `json:"bootstrapPods"`
}

func decodeRegistry(raw, stateNamespace string) (*registry, error) {
	if len(raw) == 0 || len(raw) > MaxRegistryBytes || !utf8.ValidString(raw) {
		return nil, fmt.Errorf("placement registry must contain at most 256 KiB of nonempty UTF-8 JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	depth, tokens := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil || token == nil {
			return nil, fmt.Errorf("placement registry contains malformed or null JSON")
		}
		tokens++
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth > maxRegistryDepth || tokens > maxRegistryTokens {
			return nil, fmt.Errorf("placement registry exceeds its JSON nesting or token budget")
		}
	}
	var result registry
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &result)
	if err != nil || len(problems) != 0 {
		return nil, fmt.Errorf("placement registry contains malformed, unknown or duplicate fields")
	}
	if err := result.validate(stateNamespace); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *registry) validate(stateNamespace string) error {
	if r.Version != RegistryVersion || r.Nodes == nil || r.PlatformDaemonSets == nil ||
		len(r.Nodes) > MaxNodes || len(r.PlatformDaemonSets) > MaxDaemonSets {
		return fmt.Errorf("placement registry requires version v1, at most 256 Nodes and at most 64 DaemonSets")
	}
	nodeNames, nodeUIDs, providers := map[string]bool{}, map[types.UID]bool{}, map[string]bool{}
	namespaces := map[string]types.UID{}
	checkNamespace := func(identity sandboxworkload.ObjectIdentity) bool {
		if !validIdentity(identity) || len(validation.IsDNS1123Label(identity.Name)) != 0 {
			return false
		}
		if uid, found := namespaces[identity.Name]; found && uid != identity.UID {
			return false
		}
		namespaces[identity.Name] = identity.UID
		return true
	}
	for _, node := range r.Nodes {
		if !validName(node.Name) || !exactValue(string(node.UID), 128) || !exactValue(node.ProviderID, 2048) ||
			nodeNames[node.Name] || nodeUIDs[node.UID] || providers[node.ProviderID] {
			return fmt.Errorf("registered Node names, UIDs and provider IDs must be complete and unique")
		}
		nodeNames[node.Name], nodeUIDs[node.UID], providers[node.ProviderID] = true, true, true
		if reservation := node.Reservation; reservation != nil {
			if !validIdentity(reservation.Agent) || !validIdentity(reservation.StatefulSet) ||
				!checkNamespace(reservation.Namespace) || reservation.Namespace.Name == stateNamespace {
				return fmt.Errorf("Node reservations require exact Agent, workload namespace and StatefulSet identities")
			}
		}
	}
	daemonNames, daemonUIDs := map[string]bool{}, map[types.UID]bool{}
	podNames, podUIDs := map[string]bool{}, map[types.UID]bool{}
	for _, daemon := range r.PlatformDaemonSets {
		key := daemon.Namespace.Name + "/" + daemon.Name
		digest, err := hex.DecodeString(daemon.TemplateDigest)
		if !validName(daemon.Name) || !exactValue(string(daemon.UID), 128) || !checkNamespace(daemon.Namespace) ||
			daemonNames[key] || daemonUIDs[daemon.UID] || err != nil || len(digest) != 32 ||
			hex.EncodeToString(digest) != daemon.TemplateDigest || !exactValue(daemon.TrustedCreator, 256) ||
			strings.Contains(daemon.TrustedCreator, "*") || daemon.TrustedCreator == "system:anonymous" ||
			daemon.BootstrapPods == nil || len(daemon.BootstrapPods) > maxBootstrapPods {
			return fmt.Errorf("registered DaemonSets require unique identities, a SHA-256 template digest, an exact creator and a bounded Pod inventory")
		}
		daemonNames[key], daemonUIDs[daemon.UID] = true, true
		for _, pod := range daemon.BootstrapPods {
			key := daemon.Namespace.Name + "/" + pod.Name
			if !validIdentity(pod) || podNames[key] || podUIDs[pod.UID] {
				return fmt.Errorf("registered platform Pod names and UIDs must be complete and unique")
			}
			podNames[key], podUIDs[pod.UID] = true, true
		}
	}
	return nil
}

func validIdentity(identity sandboxworkload.ObjectIdentity) bool {
	return validName(identity.Name) && exactValue(string(identity.UID), 128)
}

func validName(name string) bool {
	return name != "" && len(validation.IsDNS1123Subdomain(name)) == 0
}

func exactValue(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for _, c := range value {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

func (r *registry) node(name string) *registeredNode {
	for i := range r.Nodes {
		if r.Nodes[i].Name == name {
			return &r.Nodes[i]
		}
	}
	return nil
}
