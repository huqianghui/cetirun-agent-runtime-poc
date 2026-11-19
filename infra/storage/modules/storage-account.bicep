// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Premium Blob Storage account and container for AKS skill artifacts.

param storageAccountName string
param location string
param containerName string
param tags object = {}

@description('AKS node subnet IDs allowed to access the storage account via NFS.')
param allowedSubnetIds array

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

resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageAccountName
  location: location
  tags: tags
  kind: 'BlockBlobStorage'
  sku: {
    name: 'Premium_LRS'
  }
  properties: {
    allowBlobPublicAccess: false
    supportsHttpsTrafficOnly: false // NFS v3 requires non-encrypted TCP
    minimumTlsVersion: 'TLS1_2'
    isHnsEnabled: true // Required by NFS v3
    isNfsV3Enabled: true
    publicNetworkAccess: publicNetworkAccess
    networkAcls: {
      defaultAction: 'Deny' // NFS v3 requires Deny with explicit subnet allow-rules or private endpoints
      bypass: 'AzureServices'
      virtualNetworkRules: [for subnetId in allowedSubnetIds: {
        id: subnetId
        action: 'Allow'
      }]
    }
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  parent: storageAccount
  name: 'default'
}

resource container 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blobService
  name: containerName
  properties: {
    publicAccess: 'None'
  }
}

// NFS 3.0 authorizes clients only by network. A private endpoint gives the
// blob service a private address in the selected subnet.
resource privateEndpoint 'Microsoft.Network/privateEndpoints@2023-11-01' = if (!empty(privateEndpointSubnetId)) {
  name: '${storageAccountName}-blob'
  location: location
  tags: tags
  properties: {
    subnet: {
      id: privateEndpointSubnetId
    }
    privateLinkServiceConnections: [
      {
        name: '${storageAccountName}-blob'
        properties: {
          privateLinkServiceId: storageAccount.id
          groupIds: [
            'blob'
          ]
        }
      }
    ]
  }
}

// Clients resolve the account's blob host name to the private address through
// this zone; link the zone to the cluster's virtual network.
resource privateDnsZoneGroup 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2023-11-01' = if (!empty(privateEndpointSubnetId) && !empty(privateDnsZoneId)) {
  parent: privateEndpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [
      {
        name: 'blob'
        properties: {
          privateDnsZoneId: privateDnsZoneId
        }
      }
    ]
  }
}

output storageAccountName string = storageAccount.name
output storageAccountId string = storageAccount.id
output containerName string = container.name
output primaryBlobEndpoint string = storageAccount.properties.primaryEndpoints.blob
