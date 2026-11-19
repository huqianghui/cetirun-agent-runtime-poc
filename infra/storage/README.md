# CetiRun Blob storage

Read [Storage](../../site/content/en/docs/operations/storage.md)
for prerequisites, necessary parameters, helper side effects, NFS network
restrictions, shared skills mounts, and recovery.

`main.bicep` calls `modules/storage-account.bicep`. `deploy.sh` finds or
accepts an AKS subnet, updates the storage service endpoint of the subnet, and
deploys the account and the container. With `PRIVATE_ENDPOINT_SUBNET_ID`, it
creates a private endpoint for the blob service instead of the subnet steps.
`PRIVATE_DNS_ZONE_ID` and `PUBLIC_NETWORK_ACCESS` are optional. Supply a
reviewed `PARAM_FILE`. A default `main.bicepparam` is not included.
