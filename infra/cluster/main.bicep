// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Resource-group-scoped orchestrator: deploys AKS cluster into an existing resource group.
// The resource group is created by deploy.sh before this template runs.

// --- Resource group and location ---

param resourceGroupName string
param location string

// --- AKS cluster parameters ---

param clusterName string
@description('Stable Kubernetes version, at least 1.37 for Sandbox and CetiAdmin admission. The selected version must be available in the target AKS region.')
param kubernetesVersion string = '1.37'
param dnsPrefix string
param nodeResourceGroup string

@allowed(['Free', 'Standard', 'Premium'])
param skuTier string = 'Premium'

param tags object = {
  project: 'cetirun'
  milestone: 'm0'
}

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

param systemNodePoolMinCount int = 2
param systemNodePoolMaxCount int = 10
param systemNodePoolOsDiskSizeGB int = 128

@allowed(['Managed', 'Ephemeral'])
param systemNodePoolOsDiskType string = 'Managed'

param systemNodePoolOsSku string = 'Ubuntu'
param systemNodePoolAvailabilityZones array = ['1', '2', '3']

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

param agentNodePoolMinCount int = 15
param agentNodePoolMaxCount int = 150
param agentNodePoolOsDiskSizeGB int = 128
param agentNodePoolAvailabilityZones array = ['1', '2', '3']

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

param podCidr string = '10.244.0.0/16'
param serviceCidr string = '172.16.0.0/16'
param dnsServiceIP string = '172.16.0.10'

// --- Istio parameters ---

param istioIngressGatewayEnabled bool = true

@allowed(['External', 'Internal'])
param istioIngressGatewayType string = 'External'

@description('Optional managed Istio revision. Leave empty to let AKS select its default revision.')
param istioRevision string = ''

// --- Monitoring parameters ---

// --- Azure Data Explorer for ADX-Mon (cetirun-observability chart) ---

@allowed(['none', 'create', 'existing'])
@description('none: no ADX resources. create: a new Kusto cluster in adxResourceGroupName. existing: the databases and roles in the existing cluster adxClusterName.')
param adxMode string = 'none'

@description('Container Insights to Log Analytics. Off by default when ADX-Mon collects into Azure Data Explorer (adxMode create or existing). On Kata node pools with recent node images, it collects nothing from those nodes.')
param enableContainerInsights bool = adxMode == 'none'

@description('Managed Prometheus to an Azure Monitor workspace. Off by default when ADX-Mon collects the metrics.')
param enablePrometheusMetrics bool = adxMode == 'none'

@description('Existing Log Analytics workspace for Container Insights. Empty creates one when Container Insights is on.')
param logAnalyticsWorkspaceId string = ''

@description('Kusto cluster name: 4 to 22 lowercase letters and digits, starting with a letter. Kusto cluster names are unique in Azure.')
param adxClusterName string = 'adx${uniqueString(subscription().subscriptionId, resourceGroupName, clusterName)}'

@description('Resource group of the Kusto cluster. It must exist; deploy.sh creates only the cluster resource group.')
param adxResourceGroupName string = resourceGroupName

param adxSubscriptionId string = subscription().subscriptionId

@description('Region of a new Kusto cluster. Not every region offers every Kusto SKU, for example the Dev SKUs.')
param adxLocation string = location

param adxSkuName string = 'Dev(No SLA)_Standard_E2a_v4'

@allowed(['Basic', 'Standard'])
param adxSkuTier string = 'Basic'

param adxSkuCapacity int = 1
param adxLogsRetentionDays int = 90
param adxMetricsRetentionDays int = 30
param adxAgentLogsRetentionDays int = 14

@description('Namespace of the cetirun-observability release, for the ingestor federated credential.')
param observabilityNamespace string = 'cetirun-system'

@description('Readers of the logs and metrics databases: objects with principalId and principalType (Group, User or App).')
param adxViewers array = []

@description('Readers of the Agent logs database, which can hold conversation content.')
param adxAgentLogsViewers array = []

// --- Modules ---

module cluster 'modules/aks-cluster.bicep' = {
  name: 'deploy-aks-cluster'
  params: {
    clusterName: clusterName
    location: location
    kubernetesVersion: kubernetesVersion
    dnsPrefix: dnsPrefix
    skuTier: skuTier
    tags: tags
    resourceGroupName: resourceGroupName
    nodeResourceGroup: nodeResourceGroup
    systemNodePoolVmSize: systemNodePoolVmSize
    systemNodePoolMinCount: systemNodePoolMinCount
    systemNodePoolMaxCount: systemNodePoolMaxCount
    systemNodePoolOsDiskSizeGB: systemNodePoolOsDiskSizeGB
    systemNodePoolOsDiskType: systemNodePoolOsDiskType
    systemNodePoolOsSku: systemNodePoolOsSku
    systemNodePoolAvailabilityZones: systemNodePoolAvailabilityZones
    agentNodePoolVmSize: agentNodePoolVmSize
    agentNodePoolMinCount: agentNodePoolMinCount
    agentNodePoolMaxCount: agentNodePoolMaxCount
    agentNodePoolOsDiskSizeGB: agentNodePoolOsDiskSizeGB
    agentNodePoolAvailabilityZones: agentNodePoolAvailabilityZones
    agent2NodePoolVmSize: agent2NodePoolVmSize
    agent2NodePoolMinCount: agent2NodePoolMinCount
    agent2NodePoolMaxCount: agent2NodePoolMaxCount
    agent2NodePoolOsDiskSizeGB: agent2NodePoolOsDiskSizeGB
    agent2NodePoolAvailabilityZones: agent2NodePoolAvailabilityZones
    podCidr: podCidr
    serviceCidr: serviceCidr
    dnsServiceIP: dnsServiceIP
    istioIngressGatewayEnabled: istioIngressGatewayEnabled
    istioIngressGatewayType: istioIngressGatewayType
    istioRevision: istioRevision
    enableContainerInsights: enableContainerInsights
    enablePrometheusMetrics: enablePrometheusMetrics
    logAnalyticsWorkspaceId: logAnalyticsWorkspaceId
  }
}

module adxIngestorIdentity 'modules/adx-ingestor-identity.bicep' = if (adxMode != 'none') {
  name: 'deploy-adx-ingestor-identity'
  params: {
    name: '${clusterName}-adx-ingestor'
    location: location
    tags: tags
    oidcIssuerUrl: cluster.outputs.oidcIssuerUrl
    namespace: observabilityNamespace
  }
}

module adx 'modules/adx.bicep' = if (adxMode != 'none') {
  name: 'deploy-adx'
  scope: resourceGroup(adxSubscriptionId, adxResourceGroupName)
  params: {
    clusterName: adxClusterName
    createCluster: adxMode == 'create'
    location: adxLocation
    skuName: adxSkuName
    skuTier: adxSkuTier
    skuCapacity: adxSkuCapacity
    tags: tags
    logsRetentionDays: adxLogsRetentionDays
    metricsRetentionDays: adxMetricsRetentionDays
    agentLogsRetentionDays: adxAgentLogsRetentionDays
    ingestorClientId: adxIngestorIdentity.?outputs.clientId ?? ''
    viewers: adxViewers
    agentLogsViewers: adxAgentLogsViewers
  }
}

// --- Outputs ---

output clusterName string = cluster.outputs.clusterName
output clusterId string = cluster.outputs.clusterId
output oidcIssuerUrl string = cluster.outputs.oidcIssuerUrl
output nodeResourceGroup string = cluster.outputs.nodeResourceGroup
output kubeletIdentityObjectId string = cluster.outputs.kubeletIdentityObjectId
output keyvaultSecretsProviderClientId string = cluster.outputs.keyvaultSecretsProviderClientId
output keyvaultSecretsProviderObjectId string = cluster.outputs.keyvaultSecretsProviderObjectId

// Inputs of the observability block of the make deploy environment file.
output adxClusterUri string = adx.?outputs.clusterUri ?? ''
output adxIngestorClientId string = adxIngestorIdentity.?outputs.clientId ?? ''
