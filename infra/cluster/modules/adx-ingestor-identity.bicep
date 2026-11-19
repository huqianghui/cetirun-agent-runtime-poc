// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// The ADX-Mon ingestor's Azure identity: a user-assigned managed identity that
// the ingestor's Kubernetes service account uses through workload identity.
// Its only grants are the database roles that the ADX module assigns.

param name string
param location string = resourceGroup().location
param tags object = {
  project: 'cetirun'
}

@description('OIDC issuer URL of the AKS cluster, exactly as AKS returns it.')
param oidcIssuerUrl string

@description('Namespace of the cetirun-observability release.')
param namespace string = 'cetirun-system'

@description('Service account of the ingestor; the chart names it cetirun-adx-mon-ingestor.')
param serviceAccountName string = 'cetirun-adx-mon-ingestor'

resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: name
  location: location
  tags: tags
}

resource federation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: identity
  name: 'cetirun-adx-mon-ingestor'
  properties: {
    issuer: oidcIssuerUrl
    subject: 'system:serviceaccount:${namespace}:${serviceAccountName}'
    audiences: [
      'api://AzureADTokenExchange'
    ]
  }
}

output clientId string = identity.properties.clientId
output principalId string = identity.properties.principalId
