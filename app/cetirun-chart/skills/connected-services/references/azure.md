# Azure Resource Manager

Service: `azure_arm`. Origin: `https://management.azure.com`. Base path: `/`.
Choose the documented `api-version` for the resource provider and operation.
Substitute resource IDs from the user's context or provider discovery.

| Operation | Method | Path | Query / JSON body |
| --- | --- | --- | --- |
| List subscriptions | GET | `/subscriptions` | query `{"api-version":"2022-12-01"}` |
| List resource groups | GET | `/subscriptions/{subscription}/resourcegroups` | query `{"api-version":"2021-04-01"}` |
| Query Resource Graph | POST | `/providers/Microsoft.ResourceGraph/resources` | query `{"api-version":"2022-10-01"}`; body `{"query":"Resources | project id, name, type | take 20"}` |
| Create or update a group | PUT | `/subscriptions/{subscription}/resourcegroups/{name}` | query `{"api-version":"2021-04-01"}`; body `{"location":"eastus","tags":{"purpose":"example"}}` |
| Merge tags on a group | PATCH | `/subscriptions/{subscription}/resourcegroups/{name}/providers/Microsoft.Resources/tags/default` | query `{"api-version":"2021-04-01"}`; body `{"operation":"Merge","properties":{"tags":{"purpose":"example"}}}` |

Resource Graph POST is a read query. A resource PUT can update an existing
resource and is not a create-only guarantee.

Follow ARM `nextLink` with a separately validated request. For asynchronous
writes, inspect `Azure-AsyncOperation`, `Operation-Location`, `Location` and
`Retry-After` in the returned headers and follow the operation's documented
polling contract. Check the final resource state before reporting completion.

Azure RBAC evaluates the displayed executor's permissions, including inherited
roles and their scopes. An independent Azure connection acts as its Agent
principal; a delegated connection acts with personal authority. CetiRun does
not require a second resource grant or preverification of RBAC.

For independent identity attribution, compare an applicable Azure Activity Log
write event's caller claims with the displayed Agent principal ID and provider
request IDs. Many list/GET operations do not appear in Activity Log.
