// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Resource-group-scoped orchestrator: deploys Premium Blob Storage for AKS skill artifacts.
// The resource group is created by deploy.sh before this template runs.

param location string
param storageAccountName string
param containerName string = 'aks-skills'

param tags object = {
  project: 'cetirun'
  milestone: 'm0'
}

@description('AKS node subnet IDs allowed to access the storage account via NFS.')
param allowedSubnetIds array = []

@description('Subnet of an optional private endpoint for the blob service. Empty creates no private endpoint.')
param privateEndpointSubnetId string = ''

@description('Private DNS zone (privatelink.blob) where the private endpoint registers its address. Empty registers none.')
param privateDnsZoneId string = ''

@description('Public network access of the account. Disabled leaves only private endpoints.')
@allowed([
  'Enabled'
  'Disabled'
])
param publicNetworkAccess string = 'Enabled'

// --- Modules ---

module storage 'modules/storage-account.bicep' = {
  name: 'deploy-storage-account'
  params: {
    storageAccountName: storageAccountName
    location: location
    containerName: containerName
    tags: tags
    allowedSubnetIds: allowedSubnetIds
    privateEndpointSubnetId: privateEndpointSubnetId
    privateDnsZoneId: privateDnsZoneId
    publicNetworkAccess: publicNetworkAccess
  }
}

// --- Outputs ---

output storageAccountName string = storage.outputs.storageAccountName
output storageAccountId string = storage.outputs.storageAccountId
output containerName string = storage.outputs.containerName
output primaryBlobEndpoint string = storage.outputs.primaryBlobEndpoint
