// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	strictjson "sigs.k8s.io/json"
)

type armClient struct {
	endpoint string
	pipeline runtime.Pipeline
}

type attemptKey struct{}

type singleAttemptTransport struct{ client *http.Client }

func (t singleAttemptTransport) Do(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet && request.Method != http.MethodDelete {
		return nil, errors.New("only scoped ARM GET and conditional DELETE requests are permitted")
	}
	if request.Method == http.MethodDelete {
		used, ok := request.Context().Value(attemptKey{}).(*atomic.Bool)
		if !ok || used.Swap(true) {
			return nil, errors.New("automatic DELETE replay was blocked")
		}
	}
	result, err := t.client.Do(request)
	if err != nil {
		return result, err
	}
	// azcore buffers bodies, including error responses, before returning to request.
	result.Body = http.MaxBytesReader(nil, result.Body, maxResponseBytes)
	return result, nil
}

func newARMClient(reservation Reservation) (*armClient, error) {
	target, err := parsePoolID(reservation.PoolID)
	if err != nil {
		return nil, err
	}
	credential := &cliCredential{tenant: reservation.TenantID, subscription: target.subscription}
	return makeARMClient(credential, "https://management.azure.com", &http.Client{})
}

func makeARMClient(credential azcore.TokenCredential, endpoint string, transport *http.Client) (*armClient, error) {
	transportCopy := *transport
	transportCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("ARM redirects are not permitted")
	}
	client, err := arm.NewClient("github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxfence", "", credential, &arm.ClientOptions{
		DisableRPRegistration: true,
		ClientOptions: policy.ClientOptions{
			Cloud: cloud.Configuration{
				Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
					cloud.ResourceManager: {Endpoint: endpoint, Audience: "https://management.azure.com"},
				},
			},
			Retry:     policy.RetryOptions{MaxRetries: -1},
			Telemetry: policy.TelemetryOptions{Disabled: true},
			Transport: singleAttemptTransport{client: &transportCopy},
		},
	})
	if err != nil {
		return nil, err
	}
	return &armClient{endpoint: strings.TrimRight(endpoint, "/"), pipeline: client.Pipeline()}, nil
}

type response struct {
	status  int
	headers http.Header
	body    []byte
}

func (c *armClient) request(ctx context.Context, method, uri, etag, requestID string) (response, error) {
	if method == http.MethodDelete {
		ctx = context.WithValue(ctx, attemptKey{}, &atomic.Bool{})
	}
	request, err := runtime.NewRequest(ctx, method, uri)
	if err != nil {
		return response{}, err
	}
	request.Raw().Header.Set("Accept", "application/json")
	if method == http.MethodDelete {
		if !validETag(etag) || !uuidRE.MatchString(requestID) {
			return response{}, errors.New("conditional DELETE requires a fixed ETag and durable request ID")
		}
		request.Raw().Header.Set("If-Match", etag)
		request.Raw().Header.Set("x-ms-client-request-id", requestID)
	}
	result, err := c.pipeline.Do(request)
	if err != nil {
		return response{}, fmt.Errorf("ARM request outcome unavailable: %w", err)
	}
	defer result.Body.Close()
	body, err := io.ReadAll(io.LimitReader(result.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return response{}, errors.New("ARM response was unreadable or exceeded the response limit")
	}
	return response{status: result.StatusCode, headers: result.Header.Clone(), body: body}, nil
}

func (c *armClient) resourceURL(id, version string) string {
	return c.endpoint + id + "?api-version=" + version
}

func authoritativeAbsent(result response) bool {
	if result.status != http.StatusNotFound {
		return false
	}
	var body map[string]any
	problems, err := strictjson.UnmarshalStrict(result.body, &body, strictjson.DisallowDuplicateFields)
	if err != nil || len(problems) != 0 {
		return false
	}
	if envelope, present := body["error"]; present {
		if _, ambiguous := body["code"]; ambiguous {
			return false
		}
		detail, ok := envelope.(map[string]any)
		if !ok {
			return false
		}
		body = detail
	}
	value, ok := body["code"].(string)
	return ok &&
		slices.Contains([]string{"ResourceNotFound", "NotFound", "ParentResourceNotFound"}, value)
}

type clusterDocument struct {
	ID         string `json:"id"`
	Properties struct {
		ResourceUID       string `json:"resourceUID"`
		NodeResourceGroup string `json:"nodeResourceGroup"`
		ProvisioningState string `json:"provisioningState"`
		AgentPoolProfiles []struct {
			Name string `json:"name"`
			Mode string `json:"mode"`
		} `json:"agentPoolProfiles"`
	} `json:"properties"`
}

func (c *armClient) cluster(ctx context.Context, r Reservation) (clusterDocument, error) {
	target, _ := parsePoolID(r.PoolID)
	result, err := c.request(ctx, http.MethodGet, c.resourceURL(target.clusterID, APIVersion), "", "")
	if err != nil {
		return clusterDocument{}, err
	}
	var cluster clusterDocument
	if result.status != http.StatusOK {
		return cluster, unexpectedHTTP(result.status)
	}
	if err := json.Unmarshal(result.body, &cluster); err != nil {
		return cluster, errors.New("cluster GET returned invalid JSON")
	}
	if !strings.EqualFold(cluster.ID, target.clusterID) ||
		cluster.Properties.ResourceUID != r.ClusterResourceUID ||
		!nameRE.MatchString(cluster.Properties.NodeResourceGroup) ||
		cluster.Properties.ProvisioningState != "Succeeded" {
		return cluster, errors.New("cluster identity, node resource group or completed provisioning does not match")
	}
	systemPools := 0
	for _, pool := range cluster.Properties.AgentPoolProfiles {
		if pool.Mode == "System" && pool.Name != target.poolName {
			systemPools++
		} else if pool.Mode != "User" || pool.Name != target.poolName {
			return cluster, errors.New("qualification requires one system pool and only the reserved User pool")
		}
	}
	if systemPools != 1 {
		return cluster, errors.New("qualification requires exactly one untouched system pool")
	}
	return cluster, nil
}

type poolDocument struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Properties struct {
		ETag              string            `json:"eTag"`
		Mode              string            `json:"mode"`
		Type              string            `json:"type"`
		ProvisioningState string            `json:"provisioningState"`
		Count             *int              `json:"count"`
		EnableAutoScaling *bool             `json:"enableAutoScaling"`
		Tags              map[string]string `json:"tags"`
	} `json:"properties"`
}

func (c *armClient) pool(ctx context.Context, j *Journal) (*poolDocument, error) {
	result, err := c.request(ctx, http.MethodGet, c.resourceURL(j.Reservation.PoolID, APIVersion), "", "")
	if err != nil {
		return nil, err
	}
	if authoritativeAbsent(result) {
		return nil, nil
	}
	if result.status != http.StatusOK {
		return nil, unexpectedHTTP(result.status)
	}
	var pool poolDocument
	if err := json.Unmarshal(result.body, &pool); err != nil {
		return nil, errors.New("pool GET returned invalid JSON")
	}
	target, _ := parsePoolID(j.Reservation.PoolID)
	if !strings.EqualFold(pool.ID, j.Reservation.PoolID) || pool.Name != target.poolName ||
		pool.Properties.Mode != "User" || !slices.Contains([]string{"VirtualMachineScaleSets", "VirtualMachines"}, pool.Properties.Type) ||
		pool.Properties.ProvisioningState != "Succeeded" || !validETag(pool.Properties.ETag) ||
		pool.Properties.Tags[RunTag] != j.Reservation.RunID || pool.Properties.Count == nil || *pool.Properties.Count != 1 ||
		pool.Properties.EnableAutoScaling == nil || *pool.Properties.EnableAutoScaling {
		return nil, errors.New("pool is not the registered run's stable, single-VM, non-autoscaling User pool with a usable ETag")
	}
	return &pool, nil
}

func (c *armClient) vm(ctx context.Context, identity VMIdentity) (string, bool, error) {
	result, err := c.request(ctx, http.MethodGet, c.resourceURL(identity.ResourceID, ComputeAPIVersion), "", "")
	if err != nil {
		return "", false, err
	}
	if authoritativeAbsent(result) {
		return "", true, nil
	}
	if result.status != http.StatusOK {
		return "", false, unexpectedHTTP(result.status)
	}
	var document struct {
		ID         string `json:"id"`
		Properties struct {
			VMID string `json:"vmId"`
		} `json:"properties"`
	}
	if json.Unmarshal(result.body, &document) != nil ||
		!strings.EqualFold(document.ID, identity.ResourceID) || !uuidRE.MatchString(document.Properties.VMID) {
		return "", false, errors.New("Compute GET did not return the requested VM resource and unique vmId")
	}
	return document.Properties.VMID, false, nil
}

func (c *armClient) machines(ctx context.Context, j *Journal) ([]VMIdentity, error) {
	target, _ := parsePoolID(j.Reservation.PoolID)
	uri := c.resourceURL(j.Reservation.PoolID+"/machines", APIVersion)
	seenPages := map[string]bool{}
	seenVMs := map[string]bool{}
	var machines []VMIdentity
	for uri != "" {
		if seenPages[uri] || len(seenPages) >= 16 {
			return nil, errors.New("machine pagination repeated or exceeded its bound")
		}
		seenPages[uri] = true
		result, err := c.request(ctx, http.MethodGet, uri, "", "")
		if err != nil {
			return nil, err
		}
		if result.status != http.StatusOK {
			return nil, unexpectedHTTP(result.status)
		}
		var page struct {
			Value []struct {
				Name       string `json:"name"`
				Properties struct {
					ResourceID string `json:"resourceId"`
				} `json:"properties"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if json.Unmarshal(result.body, &page) != nil || page.Value == nil {
			return nil, errors.New("Machines List returned an invalid page")
		}
		for _, machine := range page.Value {
			identity := VMIdentity{MachineName: machine.Name, ResourceID: machine.Properties.ResourceID,
				VMID: "00000000-0000-0000-0000-000000000000"}
			if err := validateVM(identity, target, j.NodeResourceGroup); err != nil {
				return nil, err
			}
			vmID, absent, err := c.vm(ctx, identity)
			if err != nil {
				return nil, err
			}
			if absent || seenVMs[strings.ToLower(identity.ResourceID)] || seenVMs[strings.ToLower(vmID)] {
				return nil, errors.New("pool machine is missing or its VM identity is ambiguous")
			}
			identity.VMID = vmID
			seenVMs[strings.ToLower(identity.ResourceID)] = true
			seenVMs[strings.ToLower(vmID)] = true
			machines = append(machines, identity)
			if len(machines) > maxMachines {
				return nil, errors.New("pool machine inventory exceeds the qualification bound")
			}
		}
		uri = page.NextLink
		if uri != "" {
			parsed, err := c.sameEndpointURL(uri)
			if err != nil || !strings.EqualFold(parsed.Path, j.Reservation.PoolID+"/machines") ||
				parsed.Query().Get("api-version") != APIVersion {
				return nil, errors.New("machine pagination escaped the registered pool or API version")
			}
		}
	}
	slices.SortFunc(machines, func(a, b VMIdentity) int { return strings.Compare(a.ResourceID, b.ResourceID) })
	if len(machines) != 1 {
		return nil, errors.New("empty-pool qualification requires exactly one unambiguous VM")
	}
	return machines, nil
}

func (c *armClient) sameEndpointURL(raw string) (*url.URL, error) {
	uri, err := url.Parse(raw)
	base, _ := url.Parse(c.endpoint)
	if err != nil || len(raw) > 4096 || uri.Scheme != "https" || uri.Host != base.Host ||
		uri.User != nil || uri.Fragment != "" || uri.RawPath != "" {
		return nil, errors.New("ARM continuation URL is not on the fixed HTTPS ARM endpoint")
	}
	return uri, nil
}

func (c *armClient) operationURL(raw string, r Reservation) error {
	uri, err := c.sameEndpointURL(raw)
	if err != nil {
		return err
	}
	target, _ := parsePoolID(r.PoolID)
	query, err := url.ParseQuery(uri.RawQuery)
	versions := query["api-version"]
	if err != nil || len(versions) != 1 {
		return errors.New("operation handle must contain one unambiguous monitor API version")
	}
	for key := range query {
		if key != "api-version" && strings.EqualFold(key, "api-version") {
			return errors.New("operation handle has ambiguous monitor API version keys")
		}
	}
	version := strings.TrimSuffix(versions[0], "-preview")
	if _, err := time.Parse("2006-01-02", version); err != nil {
		return errors.New("operation handle has an invalid monitor API version")
	}
	p := strings.Split(uri.Path, "/")
	if len(p) != 9 || p[0] != "" || !strings.EqualFold(p[1], "subscriptions") ||
		!strings.EqualFold(p[2], target.subscription) || !strings.EqualFold(p[3], "providers") ||
		!strings.EqualFold(p[4], "Microsoft.ContainerService") || !strings.EqualFold(p[5], "locations") ||
		!nameRE.MatchString(p[6]) || !slices.Contains([]string{"operations", "operationresults"}, strings.ToLower(p[7])) ||
		!literal(p[8], 256) {
		return errors.New("operation handle is outside the registered subscription's AKS operation endpoint")
	}
	return nil
}

func (c *armClient) submit(ctx context.Context, j *Journal, attempt *Attempt) error {
	result, err := c.request(ctx, http.MethodDelete, c.resourceURL(j.Reservation.PoolID, APIVersion),
		attempt.IfMatch, attempt.RequestID)
	if err != nil {
		return err
	}
	attempt.HTTPStatus = result.status
	if result.status == http.StatusAccepted {
		attempt.OperationURL = result.headers.Get("Azure-AsyncOperation")
		attempt.PollKind = "azure-async-operation"
		if attempt.OperationURL == "" {
			attempt.OperationURL = result.headers.Get("Location")
			attempt.PollKind = "location"
		}
		return c.operationURL(attempt.OperationURL, j.Reservation)
	}
	return nil
}

func (c *armClient) poll(ctx context.Context, j *Journal) (string, error) {
	attempt := j.Deletion
	if err := c.operationURL(attempt.OperationURL, j.Reservation); err != nil {
		return "", err
	}
	result, err := c.request(ctx, http.MethodGet, attempt.OperationURL, "", "")
	if err != nil {
		return "", err
	}
	if result.status != http.StatusOK && result.status != http.StatusAccepted && result.status != http.StatusNoContent {
		return "", unexpectedHTTP(result.status)
	}
	var body struct {
		Status     string `json:"status"`
		Properties struct {
			ProvisioningState string `json:"provisioningState"`
		} `json:"properties"`
	}
	if len(result.body) != 0 && json.Unmarshal(result.body, &body) != nil {
		return "", errors.New("operation monitor returned invalid JSON")
	}
	if body.Status != "" && body.Properties.ProvisioningState != "" && body.Status != body.Properties.ProvisioningState {
		return "", errors.New("operation monitor returned conflicting states")
	}
	status := body.Status
	if status == "" {
		status = body.Properties.ProvisioningState
	}
	if status == "" && attempt.PollKind == "location" {
		if result.status == http.StatusAccepted {
			return "InProgress", nil
		}
		// The pinned SDK's Location poller treats non-202 successful HTTP as terminal.
		return "Succeeded", nil
	}
	if !slices.Contains([]string{"Succeeded", "Failed", "Canceled", "Cancelled", "InProgress", "Running", "Accepted", "Deleting"}, status) {
		return "", errors.New("operation monitor did not establish a recognized operation state")
	}
	return status, nil
}
