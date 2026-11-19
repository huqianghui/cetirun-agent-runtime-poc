// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// maxSkillMarketplaceCatalogBytes bounds the catalog document the Portal
// decodes. A ConfigMap cannot hold more anyway.
const maxSkillMarketplaceCatalogBytes = 1 << 20

// skillMarketplaceCatalogCache keeps the last decoded catalog with the
// ConfigMap version it came from. Every Agent page polls the catalog: a read
// of the ConfigMap's metadata tells whether the cached catalog is current, so
// the Portal transfers and decodes the catalog only after it changed.
type skillMarketplaceCatalogCache struct {
	mu              sync.Mutex
	key             client.ObjectKey
	uid             types.UID
	resourceVersion string
	catalog         cetirunv1alpha1.SkillMarketplaceCatalog
	known           bool
}

// GetSkillMarketplaceCatalog reads the catalog the Skill Marketplace
// publishes in the operator namespace, from the API server. It reports false,
// without an error, while the marketplace has not published one. It reads and
// decodes the whole catalog only when the ConfigMap changed since the last
// read.
func (c *Client) GetSkillMarketplaceCatalog(ctx context.Context, namespace string) (cetirunv1alpha1.SkillMarketplaceCatalog, bool, error) {
	key := client.ObjectKey{Namespace: namespace, Name: cetirunv1alpha1.SkillMarketplaceCatalogConfigMap}
	metadata := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}}
	err := c.client.Get(ctx, key, metadata)
	if errors.IsNotFound(err) {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, false, nil
	}
	if err != nil {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, false, err
	}
	cache := &c.skillMarketplaceCatalog
	cache.mu.Lock()
	if cache.key == key && cache.uid == metadata.UID && cache.resourceVersion == metadata.ResourceVersion && metadata.ResourceVersion != "" {
		catalog, known := cache.catalog, cache.known
		cache.mu.Unlock()
		return catalog, known, nil
	}
	cache.mu.Unlock()
	configMap := &corev1.ConfigMap{}
	err = c.client.Get(ctx, key, configMap)
	if errors.IsNotFound(err) {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, false, nil
	}
	if err != nil {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, false, err
	}
	catalog, known, err := catalogFromConfigMap(configMap)
	if err != nil {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, false, err
	}
	cache.mu.Lock()
	cache.key, cache.uid, cache.resourceVersion = key, configMap.UID, configMap.ResourceVersion
	cache.catalog, cache.known = catalog, known
	cache.mu.Unlock()
	return catalog, known, nil
}

// catalogFromConfigMap decodes the catalog of the ConfigMap. A ConfigMap
// without the catalog key holds no catalog yet.
func catalogFromConfigMap(configMap *corev1.ConfigMap) (cetirunv1alpha1.SkillMarketplaceCatalog, bool, error) {
	raw, ok := configMap.Data[cetirunv1alpha1.SkillMarketplaceCatalogKey]
	if !ok {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, false, nil
	}
	catalog, err := decodeSkillMarketplaceCatalog(raw)
	if err != nil {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, false, fmt.Errorf("decode the Skill Marketplace catalog: %w", err)
	}
	return catalog, true, nil
}

// decodeSkillMarketplaceCatalog accepts one JSON object of the supported
// catalog version, with case-sensitive field names, no duplicate fields and
// at most maxSkillMarketplaceCatalogBytes. Fields this Portal does not know
// are ignored, so a newer marketplace can add them within the same version.
func decodeSkillMarketplaceCatalog(raw string) (cetirunv1alpha1.SkillMarketplaceCatalog, error) {
	var catalog cetirunv1alpha1.SkillMarketplaceCatalog
	if len(raw) > maxSkillMarketplaceCatalogBytes {
		return catalog, fmt.Errorf("it is longer than %d bytes", maxSkillMarketplaceCatalogBytes)
	}
	strict, err := strictjson.UnmarshalStrict([]byte(raw), &catalog, strictjson.DisallowDuplicateFields)
	if err == nil {
		err = stderrors.Join(strict...)
	}
	if err != nil {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, err
	}
	if catalog.Version != cetirunv1alpha1.SkillMarketplaceCatalogVersion {
		return cetirunv1alpha1.SkillMarketplaceCatalog{}, fmt.Errorf("version %q is not the supported version %q",
			catalog.Version, cetirunv1alpha1.SkillMarketplaceCatalogVersion)
	}
	return catalog, nil
}
