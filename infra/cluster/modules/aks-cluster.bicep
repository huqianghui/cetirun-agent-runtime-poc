// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// AKS cluster with system + agent + agent2 node pools, Azure CNI + Cilium overlay networking,
// Istio service mesh addon, Workload Identity, Azure RBAC, and Container Insights.

// --- Cluster parameters ---

param clusterName string
param location string
param kubernetesVersion string
param dnsPrefix string

@allowed(['Free', 'Standard', 'Premium'])
param skuTier string

param tags object
param resourceGroupName string
param nodeResourceGroup string = '${resourceGroupName}-nodes'

// --- System node pool parameters ---

@allowed([
  'Standard_D4s_v5'
  'Standard_D4as_v5'
  'Standard_D4ds_v6'
  'Standard_D8s_v5'
  'Standard_D8as_v5'
  'Standard_D8ds_v6'
  'Standard_D8s_v6'
])
@description('VM size for system node pool')
param systemNodePoolVmSize string = 'Standard_D8s_v6'

param systemNodePoolMinCount int
param systemNodePoolMaxCount int
param systemNodePoolOsDiskSizeGB int

@allowed(['Managed', 'Ephemeral'])
param systemNodePoolOsDiskType string

param systemNodePoolOsSku string
param systemNodePoolAvailabilityZones array

// --- Agent node pool parameters ---

@allowed([
  'Standard_D4s_v5'
  'Standard_D4as_v5'
  'Standard_D4ds_v6'
  'Standard_D8s_v5'
  'Standard_D8as_v5'
  'Standard_D8ds_v6'
  'Standard_D16s_v5'
  'Standard_D16as_v5'
  'Standard_D16s_v6'
])
@description('VM size for agent node pool')
param agentNodePoolVmSize string = 'Standard_D16s_v6'

param agentNodePoolMinCount int
param agentNodePoolMaxCount int
param agentNodePoolOsDiskSizeGB int
param agentNodePoolAvailabilityZones array

// --- Agent2 node pool parameters ---

@allowed([
  'Standard_D4s_v5'
  'Standard_D4as_v5'
  'Standard_D4ds_v6'
  'Standard_D8s_v5'
  'Standard_D8as_v5'
  'Standard_D8ds_v6'
  'Standard_D32s_v5'
  'Standard_D32as_v5'
  'Standard_D32s_v6'
])
@description('VM size for agent2 node pool')
param agent2NodePoolVmSize string = 'Standard_D32s_v6'

param agent2NodePoolMinCount int = 3
param agent2NodePoolMaxCount int = 10
param agent2NodePoolOsDiskSizeGB int = 128
param agent2NodePoolAvailabilityZones array = ['1', '2', '3']

// --- Networking parameters ---

param podCidr string
param serviceCidr string
param dnsServiceIP string

// --- Istio parameters ---

param istioIngressGatewayEnabled bool

@allowed(['External', 'Internal'])
param istioIngressGatewayType string

@description('Optional managed Istio revision. Leave empty to let AKS select its default revision.')
param istioRevision string = ''

// --- Monitoring parameters ---

param enableContainerInsights bool
param enablePrometheusMetrics bool
param logAnalyticsWorkspaceId string

// --- Log Analytics workspace (created for Container Insights if no existing ID provided) ---

resource logAnalytics 'Microsoft.OperationalInsights/workspaces@2023-09-01' = if (enableContainerInsights && empty(logAnalyticsWorkspaceId)) {
  name: '${clusterName}-logs'
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
  }
}

var logAnalyticsWorkspaceResourceId = empty(logAnalyticsWorkspaceId)
  ? logAnalytics.id
  : logAnalyticsWorkspaceId

// --- Azure Monitor Workspace for managed Prometheus ---

resource azureMonitorWorkspace 'Microsoft.Monitor/accounts@2023-04-03' = if (enablePrometheusMetrics) {
  name: '${clusterName}-prometheus'
  location: location
  tags: tags
}

// --- AKS Cluster ---

resource aksCluster 'Microsoft.ContainerService/managedClusters@2026-01-01' = {
  name: clusterName
  location: location
  tags: tags
  identity: {
    type: 'SystemAssigned'
  }
  sku: {
    name: 'Base'
    tier: skuTier
  }
  properties: {
    kubernetesVersion: kubernetesVersion
    dnsPrefix: dnsPrefix
    nodeResourceGroup: nodeResourceGroup
    disableLocalAccounts: false

    agentPoolProfiles: [
      {
        name: 'system'
        mode: 'System'
        vmSize: systemNodePoolVmSize
        enableAutoScaling: true
        minCount: systemNodePoolMinCount
        maxCount: systemNodePoolMaxCount
        count: systemNodePoolMinCount
        osDiskSizeGB: systemNodePoolOsDiskSizeGB
        osDiskType: systemNodePoolOsDiskType
        osType: 'Linux'
        osSKU: systemNodePoolOsSku
        type: 'VirtualMachineScaleSets'
        availabilityZones: systemNodePoolAvailabilityZones
        nodeTaints: ['CriticalAddonsOnly=true:NoSchedule']
        nodeLabels: {
          'cetirun.microsoft.com/pool': 'system'
        }
        maxPods: 110
        upgradeSettings: {
          maxSurge: '33%'
        }
      }
      {
        name: 'agent'
        mode: 'User'
        vmSize: agentNodePoolVmSize
        enableAutoScaling: true
        minCount: agentNodePoolMinCount
        maxCount: agentNodePoolMaxCount
        count: agentNodePoolMinCount
        osDiskSizeGB: agentNodePoolOsDiskSizeGB
        osDiskType: 'Managed'
        osType: 'Linux'
        osSKU: 'AzureLinux'
        workloadRuntime: 'KataVmIsolation'
        type: 'VirtualMachineScaleSets'
        availabilityZones: agentNodePoolAvailabilityZones
        nodeTaints: []
        nodeLabels: {
          'cetirun.microsoft.com/pool': 'agent'
        }
        maxPods: 110
        upgradeSettings: {
          maxSurge: '33%'
        }
      }
      {
        name: 'agent2'
        mode: 'User'
        vmSize: agent2NodePoolVmSize
        enableAutoScaling: true
        minCount: agent2NodePoolMinCount
        maxCount: agent2NodePoolMaxCount
        count: agent2NodePoolMinCount
        osDiskSizeGB: agent2NodePoolOsDiskSizeGB
        osDiskType: 'Managed'
        osType: 'Linux'
        osSKU: 'AzureLinux'
        workloadRuntime: 'KataVmIsolation'
        type: 'VirtualMachineScaleSets'
        availabilityZones: agent2NodePoolAvailabilityZones
        nodeTaints: []
        nodeLabels: {
          'cetirun.microsoft.com/pool': 'agent2'
        }
        maxPods: 110
        upgradeSettings: {
          maxSurge: '33%'
        }
      }
    ]

    networkProfile: {
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      networkDataplane: 'cilium'
      networkPolicy: 'cilium'
      podCidr: podCidr
      serviceCidr: serviceCidr
      dnsServiceIP: dnsServiceIP
      loadBalancerSku: 'standard'
      outboundType: 'loadBalancer'
    }

    aadProfile: {
      managed: true
      enableAzureRBAC: true
    }

    oidcIssuerProfile: {
      enabled: true
    }

    securityProfile: {
      workloadIdentity: {
        enabled: true
      }
    }

    serviceMeshProfile: {
      istio: union({
        components: {
          ingressGateways: [
            {
              enabled: istioIngressGatewayEnabled
              mode: istioIngressGatewayType
            }
          ]
          // Agents run in Kata VM-isolated pods. With CNIChaining, the
          // istio-validation init container in those pods keeps failing and
          // the agent never starts; InitContainers redirects traffic from
          // inside the pod instead. Set explicitly so a new cluster does not
          // depend on the service default.
          proxyRedirectionMechanism: 'InitContainers'
        }
      }, empty(istioRevision) ? {} : {
        revisions: [istioRevision]
      })
      mode: 'Istio'
    }

    storageProfile: {
      diskCSIDriver: { enabled: true }
      blobCSIDriver: { enabled: true }
      fileCSIDriver: { enabled: true }
      snapshotController: { enabled: true }
    }

    // The monitoring add-on runs the Container Insights agent (ama-logs), with
    // managed identity authentication. What it collects, ContainerLogV2
    // included, and where it sends it are set by containerInsightsDcr and its
    // association below. The rule and association alone collect nothing: on a
    // cluster without the add-on no agent ran and ContainerLogV2 stayed empty.
    // (containerInsights was removed from azureMonitorProfile in API 2026-01-01)
    addonProfiles: union({
      azureKeyvaultSecretsProvider: {
        enabled: true
        config: {
          enableSecretRotation: 'true'
          rotationPollInterval: '2m'
        }
      }
    }, enableContainerInsights ? {
      omsagent: {
        enabled: true
        config: {
          logAnalyticsWorkspaceResourceID: logAnalyticsWorkspaceResourceId
          useAADAuth: 'true'
        }
      }
    } : {})

    azureMonitorProfile: {
      metrics: { enabled: enablePrometheusMetrics }
    }

    autoUpgradeProfile: {
      upgradeChannel: 'patch'
      nodeOSUpgradeChannel: 'NodeImage'
    }

    apiServerAccessProfile: {
      authorizedIPRanges: [] // Open during M0 dev; lock down in M2
    }
  }
}

// --- Container Insights DCR (ContainerLogV2) ---

resource containerInsightsDcr 'Microsoft.Insights/dataCollectionRules@2023-03-11' = if (enableContainerInsights) {
  name: 'MSCI-${location}-${clusterName}'
  location: location
  tags: tags
  kind: 'Linux'
  properties: {
    dataSources: {
      extensions: [
        {
          name: 'ContainerInsightsExtension'
          extensionName: 'ContainerInsights'
          streams: ['Microsoft-ContainerInsights-Group-Default']
          extensionSettings: {
            dataCollectionSettings: {
              enableContainerLogV2: true
            }
          }
        }
      ]
    }
    destinations: {
      logAnalytics: [
        {
          workspaceResourceId: logAnalyticsWorkspaceResourceId
          name: 'la-workspace'
        }
      ]
    }
    dataFlows: [
      {
        streams: ['Microsoft-ContainerInsights-Group-Default']
        destinations: ['la-workspace']
      }
    ]
  }
}

resource containerInsightsDcrAssociation 'Microsoft.Insights/dataCollectionRuleAssociations@2023-03-11' = if (enableContainerInsights) {
  name: 'ContainerInsightsExtension'
  scope: aksCluster
  properties: {
    dataCollectionRuleId: containerInsightsDcr.id
  }
}

// --- Prometheus DCR ---

resource prometheusDcr 'Microsoft.Insights/dataCollectionRules@2023-03-11' = if (enablePrometheusMetrics) {
  name: '${clusterName}-prometheus'
  location: location
  tags: tags
  properties: {
    dataSources: {
      prometheusForwarder: [
        {
          name: 'PrometheusDataSource'
          streams: ['Microsoft-PrometheusMetrics']
        }
      ]
    }
    destinations: {
      monitoringAccounts: [
        {
          accountResourceId: azureMonitorWorkspace.id
          name: 'MonitoringAccountDestination'
        }
      ]
    }
    dataFlows: [
      {
        streams: ['Microsoft-PrometheusMetrics']
        destinations: ['MonitoringAccountDestination']
      }
    ]
  }
}

resource prometheusDcrAssociation 'Microsoft.Insights/dataCollectionRuleAssociations@2023-03-11' = if (enablePrometheusMetrics) {
  name: 'MSProm-${location}-${clusterName}'
  scope: aksCluster
  properties: {
    dataCollectionRuleId: prometheusDcr.id
  }
}

// --- Outputs ---

output clusterName string = aksCluster.name
output clusterId string = aksCluster.id
output oidcIssuerUrl string = aksCluster.properties.oidcIssuerProfile.issuerURL
output nodeResourceGroup string = aksCluster.properties.nodeResourceGroup
output kubeletIdentityObjectId string = any(aksCluster.properties.identityProfile).kubeletidentity.objectId
output keyvaultSecretsProviderClientId string = aksCluster.properties.addonProfiles.azureKeyvaultSecretsProvider.identity.clientId
output keyvaultSecretsProviderObjectId string = aksCluster.properties.addonProfiles.azureKeyvaultSecretsProvider.identity.objectId
