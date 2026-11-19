// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	goyaml "go.yaml.in/yaml/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

const usage = `Usage: sandbox-config-change
  --kubeconfig ABSOLUTE_FILE --context EXPLICIT_CONTEXT
  --authority-namespace NAMESPACE --authority-name NAME --authority-uid UID
  --before FILE --after FILE [--action observe|update] [--timeout 30s]
  --before FILE --action observe-delete|delete [--timeout 30s]
  --operation-id ID --action observe-delete|delete [--timeout 30s]

observe is the default and has no side effects: it only looks up the exact
recorded operation and, if changing, observes its current configuration.
It never acquires, submits, or releases a reservation.

update is explicit opt-in. It reserves and submits one conditional Update, or
observes an already-changing operation without resubmitting it. Only a verified
matching committed version, or unchanged writable content at a superseding
version of the same UID, permits update to release the reservation. The latter
is reported as update-superseded: the old conditional Update cannot commit,
and the desired configuration has not been applied.
delete is a separate explicit opt-in and forbids --after. It reserves the whole
original source and submits one UID/resourceVersion-conditional background Delete.
observe-delete only observes that exact recorded deletion; it never writes.
Recovery uses the SAME original --before file, or the recorded --operation-id
when its journal entry has deletion recovery metadata. A terminating original
UID or its absence establishes acceptance, not completion of finalizers/dependent
cleanup. Explicit delete then releases the change reservation without resubmission.
A live original UID at a newer resourceVersion instead proves the conditional
Delete was superseded without applying. Its distinct deletion-superseded status
also permits explicit release, never replay of the old Delete.
No resources are created. No readiness, finalizer or admission overrides exist.

Each input is exactly one strict YAML/JSON cetirun.microsoft.com/v1alpha1 Team,
Sandbox or AgentPolicy, at most 256 KiB. Both images must pin the same original
key, UID and resourceVersion. Only spec, labels, annotations, owner references
and finalizers may change. Preserve server metadata and status, including the
original generation and managedFields. Retain the exact original input files.
Schema defaults are applied to private copies before reserving and submitting
the write; explicit values and list order are preserved. No-op updates,
including changes that only omit defaults, are rejected before reservation.

Unchanged UID/resourceVersion pairs, failed reads and otherwise unknown changing
outcomes remain reserved.
After any uncertain result, run --action observe (updates) or observe-delete
(deletions) with the SAME inputs. A deletion with recorded recovery metadata
may instead use its --operation-id. Do not refresh their UID/resourceVersion,
replay a write, or edit the journal.
An applied-reservation-retained observation is read-only; explicit update can
then reconcile that same changing operation to release it, without submission.
The same applies to update-superseded-reservation-retained. After explicit
release, read the current configuration before preparing a new update.

Only the named kubeconfig/context is used. HTTPS with certificate validation is
required; exec/auth-provider login plugins and redirects are refused. There is
no in-cluster/default-context fallback, cloud login or automatic mutation retry.
timeout must be positive and at most 5m.

Output is bounded operation/phase/identity status only; configuration bodies,
credentials and raw server/file errors are never printed. phase is the last
recorded phase, including when status=completed confirms its release.
Exit 0 = successful observation/completion, 1 = uncertain/unavailable/I/O failure,
2 = invalid arguments or configuration inputs.
`

const observationInstruction = "run --action observe with the same --before and --after inputs; do not replay an Update or edit the coordination journal"
const deletionObservationInstruction = "run --action observe-delete with the same --before input or recorded --operation-id; do not replay a Delete or edit the coordination journal"

type options struct {
	kubeconfig, contextName                    string
	authorityNamespace, authorityName, uid     string
	beforePath, afterPath, operationID, action string
	timeout                                    time.Duration
}

type clientFactory func(options) (client.Client, client.Reader, error)

type operationStatus struct {
	Action             string                    `json:"action"`
	Status             string                    `json:"status"`
	AuthorityNamespace string                    `json:"authorityNamespace"`
	Authority          w.ObjectIdentity          `json:"authority"`
	Source             w.SourceReference         `json:"source"`
	Operation          string                    `json:"operation,omitempty"`
	Phase              sandboxcoordination.Phase `json:"phase,omitempty"`
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWithClientFactory(ctx, args, stdout, stderr, newClients)
}

func runWithClientFactory(ctx context.Context, args []string, stdout, stderr io.Writer, factory clientFactory) int {
	flags := flag.NewFlagSet("sandbox-config-change", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var o options
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "absolute kubeconfig file")
	flags.StringVar(&o.contextName, "context", "", "explicit context")
	flags.StringVar(&o.authorityNamespace, "authority-namespace", "", "coordination namespace")
	flags.StringVar(&o.authorityName, "authority-name", "", "coordination ConfigMap name")
	flags.StringVar(&o.uid, "authority-uid", "", "pinned coordination ConfigMap UID")
	flags.StringVar(&o.beforePath, "before", "", "persisted original image")
	flags.StringVar(&o.afterPath, "after", "", "desired image with original UID/resourceVersion")
	flags.StringVar(&o.operationID, "operation-id", "", "recorded changing deletion operation")
	flags.StringVar(&o.action, "action", "observe", "observe, update, observe-delete or delete")
	flags.DurationVar(&o.timeout, "timeout", 30*time.Second, "overall deadline, at most 5m")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, err := io.WriteString(stdout, usage); err != nil {
				fmt.Fprintln(stderr, "sandbox-config-change: help output failed")
				return 1
			}
			return 0
		}
		fmt.Fprintln(stderr, "sandbox-config-change: invalid arguments; see --help")
		return 2
	}
	deleting := o.action == "delete" || o.action == "observe-delete"
	observing := o.action == "observe" || o.action == "observe-delete"
	hasBefore, hasOperationID := o.beforePath != "", o.operationID != ""
	if flags.NArg() != 0 || !filepath.IsAbs(o.kubeconfig) || !boundedToken(o.contextName, 256) ||
		len(validation.IsDNS1123Label(o.authorityNamespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(o.authorityName)) != 0 || !boundedToken(o.uid, 128) ||
		(deleting && (hasBefore == hasOperationID || o.afterPath != "" || (hasOperationID && !boundedToken(o.operationID, 128)))) ||
		(!deleting && (!hasBefore || hasOperationID || o.afterPath == "")) ||
		(!deleting && o.action != "observe" && o.action != "update") ||
		o.timeout <= 0 || o.timeout > 5*time.Minute {
		fmt.Fprintln(stderr, "sandbox-config-change: explicit identities are required; updates require --before and --after, deletions require exactly one of --before or --operation-id and forbid --after; action must be observe, update, observe-delete or delete and timeout must be positive and at most 5m; see --help")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(stderr, "sandbox-config-change: request canceled before any client operation")
		return 1
	}
	var before client.Object
	var err error
	if hasBefore {
		before, err = readConfiguration(o.beforePath)
		if err != nil {
			fmt.Fprintln(stderr, "sandbox-config-change: --before must contain one bounded strict supported configuration object")
			return 2
		}
	}
	var after client.Object
	if !deleting {
		after, err = readConfiguration(o.afterPath)
		if err != nil {
			fmt.Fprintln(stderr, "sandbox-config-change: --after must contain one bounded strict supported configuration object")
			return 2
		}
	}
	writer, reader, err := factory(o)
	if err != nil {
		fmt.Fprintln(stderr, "sandbox-config-change: explicit kubeconfig/context client setup failed; no configuration request submitted")
		return 1
	}
	if observing {
		writer = nil
	}
	store := sandboxcoordination.Store{Client: writer, APIReader: reader,
		Namespace: o.authorityNamespace, Identity: w.ObjectIdentity{Name: o.authorityName, UID: types.UID(o.uid)}}
	result := operationStatus{
		Action: o.action, Status: "unavailable", AuthorityNamespace: store.Namespace, Authority: store.Identity,
	}
	if before != nil {
		result.Source = w.SourceReference{APIVersion: api.GroupVersion.String(), Kind: before.GetObjectKind().GroupVersionKind().Kind,
			Name: before.GetName(), UID: before.GetUID(), ResourceVersion: before.GetResourceVersion()}
	}
	var ticket sandboxcoordination.ChangeTicket
	var adapter sandboxcoordination.ChangeAdapter
	if deleting && hasOperationID {
		ticket, err = store.ReadConfigurationDeletionOperation(ctx, o.operationID)
		if err == nil {
			result.Source = ticket.Change.Before
			result.Operation, result.Phase = ticket.Change.ID, ticket.Change.Phase
			adapter, err = sandboxcoordination.NewKubernetesDeletionRecoveryAdapter(reader, ticket)
		}
	} else {
		if deleting {
			adapter, err = sandboxcoordination.NewKubernetesDeleteAdapter(writer, reader, before)
		} else {
			adapter, err = sandboxcoordination.NewKubernetesChangeAdapter(writer, reader, before, after)
		}
		if err == nil {
			switch o.action {
			case "observe-delete":
				ticket, err = store.ReadConfigurationDeletion(ctx, before)
			case "delete":
				ticket, err = store.AcquireConfigurationDeletion(ctx, before)
			case "observe":
				ticket, err = store.ReadConfigurationChange(ctx, before, after)
			case "update":
				ticket, err = store.AcquireConfigurationChange(ctx, before, after)
			}
		}
	}
	if err != nil {
		if adapter == nil && !(deleting && hasOperationID) {
			fmt.Fprintln(stderr, "sandbox-config-change: invalid original nonterminating configuration or proposed update; preserve exact persisted identities and server fields")
			return 2
		}
		fmt.Fprintln(stderr, "sandbox-config-change: exact operation unavailable;", observationHint(o.action))
		return printStatus(stdout, stderr, result, 1)
	}
	result.Operation, result.Phase = ticket.Change.ID, ticket.Change.Phase
	result.Source = ticket.Change.Before
	observed := &observedChangeAdapter{ChangeAdapter: adapter}
	if observing {
		if ticket.Change.Phase == sandboxcoordination.Reserved {
			result.Status = "reserved"
			return printStatus(stdout, stderr, result, 0)
		}
		_, err = observed.ObserveChange(ctx, ticket)
		if err == nil {
			err = store.CheckChange(ctx, ticket)
		}
		result.Status = "applied-reservation-retained"
		if deleting {
			result.Status = "deletion-accepted-reservation-retained"
		}
	} else {
		ticket, err = store.ReconcileChange(ctx, ticket, observed)
		result.Operation, result.Phase = ticket.Change.ID, ticket.Change.Phase
		result.Status = "completed"
		if deleting {
			result.Status = "deletion-accepted"
		}
	}
	if observed.outcome != nil && observed.outcome.Superseded {
		result.Status = "update-superseded"
		if deleting {
			result.Status = "deletion-superseded"
		}
		if observing {
			result.Status += "-reservation-retained"
		}
	}
	if err != nil {
		result.Status = "uncertain"
		fmt.Fprintln(stderr, "sandbox-config-change: outcome uncertain; reservation not confirmed released;", observationHint(o.action))
		return printStatus(stdout, stderr, result, 1)
	}
	return printStatus(stdout, stderr, result, 0)
}

type observedChangeAdapter struct {
	sandboxcoordination.ChangeAdapter
	outcome *sandboxcoordination.ChangeOutcome
}

func (adapter *observedChangeAdapter) ObserveChange(ctx context.Context, ticket sandboxcoordination.ChangeTicket) (*sandboxcoordination.ChangeOutcome, error) {
	outcome, err := adapter.ChangeAdapter.ObserveChange(ctx, ticket)
	if err == nil {
		adapter.outcome = outcome
	}
	return outcome, err
}

func observationHint(action string) string {
	if action == "delete" || action == "observe-delete" {
		return deletionObservationInstruction
	}
	return observationInstruction
}

func printStatus(stdout, stderr io.Writer, status operationStatus, exitCode int) int {
	raw, err := json.Marshal(status)
	if err != nil || len(raw)+1 > 4096 {
		fmt.Fprintln(stderr, "sandbox-config-change: status encoding failed;", observationHint(status.Action))
		return 1
	}
	raw = append(raw, '\n')
	if n, err := stdout.Write(raw); err != nil || n != len(raw) {
		fmt.Fprintln(stderr, "sandbox-config-change: status output failed;", observationHint(status.Action))
		return 1
	}
	return exitCode
}

func boundedToken(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for _, c := range value {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

func readConfiguration(path string) (client.Object, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > sandboxcoordination.MaxConfigurationBytes {
		return nil, errors.New("configuration must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("configuration file could not be opened")
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, sandboxcoordination.MaxConfigurationBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > sandboxcoordination.MaxConfigurationBytes {
		return nil, errors.New("configuration file could not be read within its bound")
	}
	return decodeConfiguration(raw)
}

func decodeConfiguration(raw []byte) (client.Object, error) {
	if len(raw) > sandboxcoordination.MaxConfigurationBytes {
		return nil, errors.New("oversized configuration")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("empty configuration")
	}
	if raw[0] != '{' && raw[0] != '[' {
		decoder := goyaml.NewDecoder(bytes.NewReader(raw))
		decoder.SetStrict(true)
		var document any
		if err := decoder.Decode(&document); err != nil {
			return nil, errors.New("invalid YAML configuration")
		}
		if err := decoder.Decode(&document); !errors.Is(err, io.EOF) {
			return nil, errors.New("exactly one YAML document is required")
		}
		var err error
		raw, err = yaml.YAMLToJSONStrict(raw)
		if err != nil || len(raw) > sandboxcoordination.MaxConfigurationBytes {
			return nil, errors.New("invalid or oversized YAML configuration")
		}
	}
	var typeMeta metav1.TypeMeta
	problems, err := strictjson.UnmarshalStrict(raw, &typeMeta, strictjson.DisallowDuplicateFields)
	if err != nil || len(problems) != 0 || typeMeta.APIVersion != api.GroupVersion.String() {
		return nil, errors.New("explicit supported API version is required")
	}
	var object client.Object
	switch typeMeta.Kind {
	case "Team":
		object = &api.Team{}
	case "Sandbox":
		object = &api.Sandbox{}
	case "AgentPolicy":
		object = &api.AgentPolicy{}
	default:
		return nil, errors.New("unsupported configuration kind")
	}
	problems, err = strictjson.UnmarshalStrict(raw, object)
	if err != nil || len(problems) != 0 {
		return nil, errors.New("unknown, duplicate or malformed configuration fields")
	}
	if !boundedToken(string(object.GetUID()), 128) || !boundedToken(object.GetResourceVersion(), 256) {
		return nil, errors.New("original persisted UID and resourceVersion are required")
	}
	return object, nil
}

func loadRESTConfig(o options) (*rest.Config, error) {
	if !filepath.IsAbs(o.kubeconfig) || !boundedToken(o.contextName, 256) {
		return nil, errors.New("explicit absolute kubeconfig and context are required")
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: o.kubeconfig}
	loaded, err := rules.Load()
	if err != nil {
		return nil, errors.New("explicit kubeconfig could not be loaded")
	}
	selected := loaded.Contexts[o.contextName]
	if selected == nil || selected.Cluster == "" || selected.AuthInfo == "" {
		return nil, errors.New("explicit context must name its cluster and user")
	}
	cluster, user := loaded.Clusters[selected.Cluster], loaded.AuthInfos[selected.AuthInfo]
	if cluster == nil || cluster.Server == "" || cluster.InsecureSkipTLSVerify ||
		user == nil || user.Exec != nil || user.AuthProvider != nil {
		return nil, errors.New("explicit TLS cluster and non-plugin authentication are required")
	}
	endpoint, err := url.Parse(cluster.Server)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("an explicit HTTPS API endpoint is required")
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*loaded, o.contextName,
		&clientcmd.ConfigOverrides{CurrentContext: o.contextName}, rules).ClientConfig()
	if err != nil {
		return nil, errors.New("explicit client configuration is invalid")
	}
	config.Timeout = o.timeout
	config.UserAgent = "cetirun-sandbox-config-change"
	// Warning text can contain arbitrary input. Errors are reported separately
	// as bounded status, never raw API responses or credential-bearing text.
	config.WarningHandlerWithContext = rest.NoWarnings{}
	return config, nil
}

func newClients(o options) (client.Client, client.Reader, error) {
	config, err := loadRESTConfig(o)
	if err != nil {
		return nil, nil, err
	}
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, nil, errors.New("explicit TLS transport could not be constructed")
	}
	isolated := *httpClient
	httpClient = &isolated
	httpClient.Transport = &singleMutationTransport{next: httpClient.Transport}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	if err := api.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{api.GroupVersion, corev1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	for _, resource := range []struct{ kind, plural, singular string }{
		{"Team", "teams", "team"}, {"Sandbox", "sandboxes", "sandbox"}, {"AgentPolicy", "agentpolicies", "agentpolicy"},
	} {
		mapper.AddSpecific(api.GroupVersion.WithKind(resource.kind), api.GroupVersion.WithResource(resource.plural),
			api.GroupVersion.WithResource(resource.singular), meta.RESTScopeRoot)
	}
	base, err := client.New(config, client.Options{Scheme: scheme, Mapper: mapper, HTTPClient: httpClient})
	if err != nil {
		return nil, nil, errors.New("Kubernetes client could not be constructed")
	}
	return &singleMutationClient{Client: base}, base, nil
}

type mutationAttemptKey struct{}

type mutationAttempt struct {
	method    string
	attempted atomic.Bool
}

type singleMutationClient struct{ client.Client }

func (c *singleMutationClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	return c.Client.Update(context.WithValue(ctx, mutationAttemptKey{}, &mutationAttempt{method: http.MethodPut}), object, opts...)
}

func (c *singleMutationClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	return c.Client.Delete(context.WithValue(ctx, mutationAttemptKey{}, &mutationAttempt{method: http.MethodDelete}), object, opts...)
}

type singleMutationTransport struct{ next http.RoundTripper }

func (t *singleMutationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet {
		attempt, ok := request.Context().Value(mutationAttemptKey{}).(*mutationAttempt)
		if (request.Method != http.MethodPut && request.Method != http.MethodDelete) ||
			!ok || attempt == nil || attempt.method != request.Method || !attempt.attempted.CompareAndSwap(false, true) {
			return nil, errors.New("unapproved mutation or automatic retry refused")
		}
	}
	return t.next.RoundTrip(request)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cancel()
	os.Exit(code)
}
