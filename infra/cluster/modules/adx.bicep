// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Azure Data Explorer for ADX-Mon: an optional Kusto cluster, the three
// CetiRun databases, their retention, caching and ingestion batching, and the
// database roles of the ADX-Mon ingestor and of the people who read the data.
// Deploy it to the resource group of the Kusto cluster. Several CetiRun
// clusters can share one Kusto cluster: each one's rows carry its name in the
// Cluster column.
//
// The ingestor creates the tables and its query functions on first use, so it
// is a database User (create tables and functions) and Ingestor (write data),
// not an Admin. ADX-Mon also alters three functions at every start; this
// module creates them and makes the ingestor their admin, so that several
// clusters' ingestors can share the metrics database. Retention, caching and
// batching are database properties that this module owns.

@description('Name of the Kusto cluster: 4 to 22 lowercase letters and digits, starting with a letter.')
@minLength(4)
@maxLength(22)
param clusterName string

@description('Create the Kusto cluster. False uses an existing cluster of this name in this resource group.')
param createCluster bool = true

param location string = resourceGroup().location

@description('Kusto cluster SKU. The default Dev SKU has one engine node and no SLA; use a production SKU for production data.')
param skuName string = 'Dev(No SLA)_Standard_E2a_v4'

@allowed(['Basic', 'Standard'])
param skuTier string = 'Basic'

@minValue(1)
param skuCapacity int = 1

param tags object = {
  project: 'cetirun'
}

@description('Database of platform logs, AI Gateway request logs, Kubernetes events and the Agent inventory.')
param logsDatabaseName string = 'CetiRunLogs'

@description('Database of metrics.')
param metricsDatabaseName string = 'CetiRunMetrics'

@description('Database of Agent workload logs, which can hold conversation content. Grant it separately.')
param agentLogsDatabaseName string = 'CetiRunAgents'

@minValue(1)
param logsRetentionDays int = 90

@minValue(1)
param metricsRetentionDays int = 30

@minValue(1)
param agentLogsRetentionDays int = 14

@minValue(1)
@description('Days of each database kept in the hot cache, at most its retention.')
param hotCacheDays int = 7

@minValue(10)
@maxValue(300)
@description('Longest time ADX waits before it ingests a batch, in seconds. Shorter makes data visible sooner and creates more extents.')
param ingestionBatchingSeconds int = 30

@description('Client (application) ID of the ADX-Mon ingestor identity. Empty skips its role assignments.')
param ingestorClientId string = ''

param tenantId string = tenant().tenantId

@description('Principals that read the logs and metrics databases: objects with principalId (Entra group or user object ID, or application ID) and principalType (Group, User or App).')
param viewers array = []

@description('Principals that read the Agent logs database, in the same form as viewers.')
param agentLogsViewers array = []

// The query functions of the databases. Their files are scripts of management
// commands that can also run on a Kusto cluster made without this module;
// their comment lines are left out here.
var logsFunctions = join(filter(split(loadTextContent('adx/cetirun-logs.kql'), '\n'), line => !startsWith(line, '//')), '\n')
var metricsFunctions = join(filter(split('${loadTextContent('adx/cetirun-metrics.kql')}\n${replace(loadTextContent('adx/cetirun-health.kql'), '__LOGS_DATABASE__', logsDatabaseName)}\n${loadTextContent('adx/adxmon-functions.kql')}', '\n'), line => !startsWith(line, '//')), '\n')
// Each CetiRun cluster's ingestor alters ADX-Mon's functions at start, which
// needs it to be their admin; a second cluster's deployment adds its own.
var adxmonFunctionAdmins = empty(ingestorClientId) ? '' : join(map(['prom_increase', 'prom_rate', 'prom_delta'], name => format('.add function {0} admins (\'aadapp={1};{2}\') \'ADX-Mon ingestor\'', name, ingestorClientId, tenantId)), '\n\n')
var agentLogsFunctions = replace(join(filter(split(loadTextContent('adx/cetirun-agents.kql'), '\n'), line => !startsWith(line, '//')), '\n'), '__LOGS_DATABASE__', logsDatabaseName)

var databases = [
  {
    name: logsDatabaseName
    retentionDays: logsRetentionDays
    agentLogs: false
    functions: logsFunctions
  }
  {
    name: metricsDatabaseName
    retentionDays: metricsRetentionDays
    agentLogs: false
    functions: join(filter([metricsFunctions, adxmonFunctionAdmins], part => !empty(part)), '\n\n')
  }
  {
    name: agentLogsDatabaseName
    retentionDays: agentLogsRetentionDays
    agentLogs: true
    functions: agentLogsFunctions
  }
]

var ingestorRoles = empty(ingestorClientId) ? [] : ['User', 'Ingestor']

var viewerAssignments = flatten(map(range(0, length(databases)), i => map(databases[i].agentLogs ? agentLogsViewers : viewers, viewer => {
  database: i
  principalId: viewer.principalId
  principalType: viewer.principalType
})))

var ingestorAssignments = flatten(map(range(0, length(databases)), i => map(ingestorRoles, role => {
  database: i
  role: role
})))

resource newCluster 'Microsoft.Kusto/clusters@2024-04-13' = if (createCluster) {
  name: clusterName
  location: location
  tags: tags
  sku: {
    name: skuName
    tier: skuTier
    capacity: skuCapacity
  }
  properties: {
    // A cluster without ingestion or queries for five days stops; a stopped
    // cluster bills storage only and must be started by hand.
    enableAutoStop: true
    enableStreamingIngest: false
    publicNetworkAccess: 'Enabled'
    // Only the cluster's own tenant.
    trustedExternalTenants: []
  }
}

resource cluster 'Microsoft.Kusto/clusters@2024-04-13' existing = {
  name: clusterName
}

resource database 'Microsoft.Kusto/clusters/databases@2024-04-13' = [for item in databases: {
  parent: cluster
  name: item.name
  location: location
  kind: 'ReadWrite'
  properties: {
    softDeletePeriod: 'P${item.retentionDays}D'
    hotCachePeriod: 'P${min(hotCacheDays, item.retentionDays)}D'
  }
  dependsOn: [
    newCluster
  ]
}]

// Ingestion batching, then the query functions. The deploying principal, an
// Admin of the databases it creates, runs the script.
resource schema 'Microsoft.Kusto/clusters/databases/scripts@2024-04-13' = [for (item, i) in databases: {
  parent: database[i]
  name: 'cetirun-schema'
  properties: {
    // The script holds no secret: database names, a policy and functions.
    #disable-next-line use-secure-value-for-secure-inputs
    scriptContent: join(filter([
      format('.alter database [\'{0}\'] policy ingestionbatching \'{{"MaximumBatchingTimeSpan":"{1}","MaximumNumberOfItems":500,"MaximumRawDataSizeMB":1024}}\'', item.name, format('00:{0:00}:{1:00}', ingestionBatchingSeconds / 60, ingestionBatchingSeconds % 60))
      item.functions
    ], part => !empty(part)), '\n\n')
    continueOnErrors: false
  }
}]

resource ingestorAssignment 'Microsoft.Kusto/clusters/databases/principalAssignments@2024-04-13' = [for item in ingestorAssignments: {
  parent: database[item.database]
  name: guid(cluster.id, databases[item.database].name, ingestorClientId, item.role)
  properties: {
    principalId: ingestorClientId
    principalType: 'App'
    role: item.role
    tenantId: tenantId
  }
}]

resource viewerAssignment 'Microsoft.Kusto/clusters/databases/principalAssignments@2024-04-13' = [for item in viewerAssignments: {
  parent: database[item.database]
  name: guid(cluster.id, databases[item.database].name, item.principalId, 'Viewer')
  properties: {
    principalId: item.principalId
    principalType: item.principalType
    role: 'Viewer'
    tenantId: tenantId
  }
}]

@description('Query URI of the Kusto cluster; the ingestor derives the ingestion URI from it.')
output clusterUri string = cluster.properties.uri
output dataIngestionUri string = cluster.properties.dataIngestionUri
output logsDatabaseName string = logsDatabaseName
output metricsDatabaseName string = metricsDatabaseName
output agentLogsDatabaseName string = agentLogsDatabaseName
