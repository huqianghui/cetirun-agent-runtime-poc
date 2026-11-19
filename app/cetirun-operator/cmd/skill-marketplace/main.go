// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command skill-marketplace is the Skill Marketplace worker of the operator
// image. It synchronizes one Git repository of skills, publishes the selection
// of each Team of the operator's Teams document as an immutable release on
// shared Blob NFS storage, and writes the Skill Marketplace catalog.
//
// The worker runs in its own Pod, apart from the operator, because it parses
// repository content, holds the repository credentials and mounts the storage
// read-write. Its Kubernetes access is two ConfigMaps of the operator
// namespace: it reads the Teams document and updates the catalog. The
// operator reconciles Teams and Agents, and creates the Agents' claims.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/buildinfo"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/gitsync"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/publish"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/worker"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

type options struct {
	source            gitsync.Source
	path              string
	syncInterval      time.Duration
	storageRoot       string
	namespace         string
	reserved          string
	teamGCGrace       time.Duration
	maxRepositorySize string
	maxRepository     int64
	maxObjects        int64
	maxSkillSize      string
	maxSkillMDSize    string
	limits            skills.Limits
	healthAddress     string
	metricsAddress    string
	authMode          string
	credentialsDir    string
	allowInsecureHTTP bool
}

// size is the default flag text of a size limit, such as 5Mi.
func size(bytes int64) string {
	return resource.NewQuantity(bytes, resource.BinarySI).String()
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("skill-marketplace", flag.ContinueOnError)
	fs.StringVar(&o.source.URL, "repository-url", "", "Git repository URL (https://, or ssh with --auth=ssh)")
	fs.StringVar(&o.source.Ref, "repository-ref", "", "branch, tag or full ref; empty uses the remote HEAD")
	fs.StringVar(&o.path, "repository-path", "", "directory of the repository that holds the skills")
	fs.StringVar(&o.authMode, "auth", "none", "none, token or ssh")
	fs.StringVar(&o.source.TokenUsername, "token-username", "x-access-token", "user name sent with the token")
	fs.StringVar(&o.credentialsDir, "credentials-dir", "/var/run/cetirun/git", "directory of the mounted credentials Secret")
	fs.BoolVar(&o.allowInsecureHTTP, "allow-insecure-http", false, "allow an http:// repository URL (tests only)")
	fs.DurationVar(&o.syncInterval, "sync-interval", 5*time.Minute, "how often the repository is checked")
	fs.StringVar(&o.storageRoot, "storage-root", "/var/lib/cetirun-skills", "read-write mount of the marketplace container")
	fs.StringVar(&o.namespace, "namespace", "", "the operator namespace, which holds the Teams document and the catalog")
	fs.StringVar(&o.reserved, "reserved-names", strings.Join(skills.DefaultReserved, ","), "comma-separated reserved skill names")
	fs.DurationVar(&o.teamGCGrace, "team-gc-grace", time.Hour, "how long the releases of a Team that left the Teams document stay after its last publication")
	defaults := skills.DefaultLimits
	fs.StringVar(&o.maxRepositorySize, "max-repository-size", "256Mi", "largest repository commit to load: the pack that the Git server sends, its objects together, and the paths below the repository path together")
	fs.Int64Var(&o.maxObjects, "max-repository-objects", 100000, "most objects (files, directories and the commit) in the repository commit, and most files and directories below the repository path, counted per path")
	fs.IntVar(&o.limits.MaxFiles, "max-skill-files", defaults.MaxFiles, "most files in one skill")
	fs.StringVar(&o.maxSkillSize, "max-skill-size", size(defaults.MaxBytes), "largest total size of the files of one skill")
	fs.StringVar(&o.maxSkillMDSize, "max-skill-md-size", size(defaults.MaxSkillMD), "largest SKILL.md")
	fs.IntVar(&o.limits.MaxDepth, "max-skill-depth", defaults.MaxDepth, "deepest directory level of a file in a skill")
	fs.IntVar(&o.limits.MaxDescription, "max-description-length", defaults.MaxDescription, "longest SKILL.md description, in characters")
	fs.StringVar(&o.healthAddress, "health-address", ":8081", "health probe address")
	fs.StringVar(&o.metricsAddress, "metrics-address", ":8080", "metrics address; 0 disables")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.source.Auth = gitsync.Auth(o.authMode)
	o.source.CredentialsDir = o.credentialsDir
	o.source.AllowInsecureHTTP = o.allowInsecureHTTP
	if err := o.source.Validate(); err != nil {
		return o, err
	}
	if o.path != "" && (strings.HasPrefix(o.path, "/") || strings.Contains(o.path, "..")) {
		return o, fmt.Errorf("--repository-path must be relative to the repository root")
	}
	if o.syncInterval < 30*time.Second {
		return o, fmt.Errorf("--sync-interval must be at least 30s")
	}
	if len(validation.IsDNS1123Label(o.namespace)) != 0 {
		return o, fmt.Errorf("--namespace must name the operator namespace")
	}
	for _, limit := range []struct {
		flag, value string
		bytes       *int64
	}{
		{"--max-repository-size", o.maxRepositorySize, &o.maxRepository},
		{"--max-skill-size", o.maxSkillSize, &o.limits.MaxBytes},
		{"--max-skill-md-size", o.maxSkillMDSize, &o.limits.MaxSkillMD},
	} {
		quantity, err := resource.ParseQuantity(limit.value)
		if err != nil || quantity.Sign() <= 0 {
			return o, fmt.Errorf("%s must be a positive size, such as 5Mi", limit.flag)
		}
		*limit.bytes = quantity.Value()
	}
	for _, limit := range []struct {
		flag  string
		value int
	}{
		{"--max-skill-files", o.limits.MaxFiles}, {"--max-skill-depth", o.limits.MaxDepth},
		{"--max-description-length", o.limits.MaxDescription},
	} {
		if limit.value < 1 {
			return o, fmt.Errorf("%s must be at least 1", limit.flag)
		}
	}
	if o.maxObjects < 1 {
		return o, fmt.Errorf("--max-repository-objects must be at least 1")
	}
	// The repository limits bound the tree below the repository path too,
	// where each path of a file or directory counts.
	o.limits.MaxTreeEntries, o.limits.MaxTreePathBytes = int(o.maxObjects), o.maxRepository
	if o.limits.MaxSkillMD > o.limits.MaxBytes {
		return o, fmt.Errorf("--max-skill-md-size cannot be larger than --max-skill-size")
	}
	return o, nil
}

func main() {
	ctrl.SetLogger(zap.New())
	logger := ctrl.Log.WithName("skill-marketplace")
	o, err := parse(os.Args[1:])
	if err != nil {
		logger.Error(err, "invalid configuration")
		os.Exit(2)
	}
	if limit := setMemoryLimit("/sys/fs/cgroup/memory.max"); limit > 0 {
		logger.Info("set the Go memory limit from the container's memory limit", "bytes", limit)
	}
	if err := run(ctrl.SetupSignalHandler(), o); err != nil {
		logger.Error(err, "the Skill Marketplace worker stopped")
		os.Exit(1)
	}
}

// setMemoryLimit gives the Go runtime a soft memory limit of 90% of the
// container's memory limit, unless GOMEMLIMIT sets one: the garbage collector
// then runs before a fetch near the repository limit reaches the container
// limit. It returns the limit that it set, or 0.
func setMemoryLimit(cgroupFile string) int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	raw, err := os.ReadFile(cgroupFile)
	if err != nil {
		return 0
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || limit <= 0 {
		return 0
	}
	limit = limit / 10 * 9
	debug.SetMemoryLimit(limit)
	return limit
}

func run(ctx context.Context, o options) error {
	logger := ctrl.Log.WithName("skill-marketplace")
	logger.Info("starting", "version", buildinfo.Version, "commit", buildinfo.Commit, "repository", gitsync.Redacted(o.source.URL),
		"ref", o.source.Ref, "path", o.path, "maxRepositoryBytes", o.maxRepository, "maxRepositoryObjects", o.maxObjects,
		"maxSkillFiles", o.limits.MaxFiles, "maxSkillBytes", o.limits.MaxBytes, "maxSkillMDBytes", o.limits.MaxSkillMD,
		"maxSkillDepth", o.limits.MaxDepth, "maxDescriptionLength", o.limits.MaxDescription)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	publisher := &publish.Publisher{Root: o.storageRoot}
	if err := publisher.Init(); err != nil {
		return err
	}
	if err := gitsync.RemoveStalePacks(""); err != nil {
		logger.Error(err, "remove the pack files of earlier fetches")
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: o.healthAddress,
		Metrics:                metricsserver.Options{BindAddress: o.metricsAddress},
	})
	if err != nil {
		return err
	}
	// The worker reads only the two named ConfigMaps, directly, and never
	// starts an informer: its permissions do not allow listing.
	reader := mgr.GetAPIReader()
	catalog := worker.NewCatalog(mgr.GetClient(), reader, o.namespace, api.SkillMarketplaceRepository{
		URL: gitsync.Redacted(o.source.URL), Ref: o.source.Ref, Path: o.path,
	})
	if err := catalog.Seed(ctx); err != nil {
		logger.Error(err, "read the previous catalog")
	}
	var reserved []string
	for _, name := range strings.Split(o.reserved, ",") {
		if name = strings.TrimSpace(name); name != "" {
			reserved = append(reserved, name)
		}
	}
	changed := make(chan struct{}, 1)
	store := &worker.Store{}
	teams := &worker.TeamsSource{Reader: reader, Namespace: o.namespace, Changed: changed}
	syncer := newSyncer(o, reserved, store, catalog, changed)
	teamPublisher := &worker.TeamPublisher{Publisher: publisher, Store: store, Catalog: catalog, Teams: teams,
		GCGrace: o.teamGCGrace, Changed: changed}
	for _, runnable := range []interface {
		Start(context.Context) error
	}{syncer, teams, teamPublisher, catalog} {
		if err := mgr.Add(runnableFunc(runnable.Start)); err != nil {
			return err
		}
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

type runnableFunc func(context.Context) error

func (f runnableFunc) Start(ctx context.Context) error { return f(ctx) }

// newSyncer returns the repository synchronizer with the configured limits.
func newSyncer(o options, reserved []string, store *worker.Store, catalog *worker.Catalog, changed chan<- struct{}) *worker.Syncer {
	return &worker.Syncer{
		Source: o.source, Path: o.path, Interval: o.syncInterval, Limits: o.limits, Reserved: reserved,
		Fetch: gitsync.Limits{MaxBytes: o.maxRepository, MaxObjects: o.maxObjects},
		Store: store, Catalog: catalog, Changed: changed,
	}
}
