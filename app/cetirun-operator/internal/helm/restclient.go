// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// inClusterClientGetter implements genericclioptions.RESTClientGetter using an in-cluster REST config.
type inClusterClientGetter struct {
	restConfig *rest.Config
	namespace  string
}

func restClientGetter(cfg *rest.Config, namespace string) *inClusterClientGetter {
	return &inClusterClientGetter{restConfig: cfg, namespace: namespace}
}

func (g *inClusterClientGetter) ToRESTConfig() (*rest.Config, error) {
	return g.restConfig, nil
}

func (g *inClusterClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(g.restConfig)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g *inClusterClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(dc)
	return mapper, nil
}

func (g *inClusterClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(
		api.Config{},
		&clientcmd.ConfigOverrides{
			Context: api.Context{
				Namespace: g.namespace,
			},
		},
	)
}
