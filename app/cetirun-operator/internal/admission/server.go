// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type ServerOptions struct {
	CertDirectory     string
	Host              string
	Port              int
	MetricsAddress    string
	ProbeAddress      string
	PodStateNamespace string
	PodRequestTimeout time.Duration
}

const PlacementValidationPath = "/validate-sandbox-placement"

// Run serves only admission. It never starts runtime reconcilers, participates
// in controller leader election, or creates/renews serving certificates.
func Run(ctx context.Context, restConfig *rest.Config, config Config, options ServerOptions) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if config.PlacementConfigMapName != "" && options.PodStateNamespace != strings.Split(config.ControllerUsername, ":")[2] {
		return fmt.Errorf("Sandbox placement requires Pod admission in the configured controller state namespace")
	}
	if config.CoordinationConfigMapName != "" && options.PodStateNamespace != strings.Split(config.ControllerUsername, ":")[2] {
		return fmt.Errorf("Sandbox coordination requires Pod admission in the configured controller state namespace")
	}
	if options.CertDirectory == "" || options.Port < 1 || options.Port > 65535 {
		return fmt.Errorf("Sandbox admission requires an explicit certificate directory and valid serving port")
	}
	certificates := &servingCertificateCache{directory: options.CertDirectory}
	if _, err := certificates.load(time.Now()); err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := api.AddToScheme(scheme); err != nil {
		return err
	}
	server := webhook.NewServer(webhook.Options{
		Host: options.Host, Port: options.Port, CertDir: options.CertDirectory,
		TLSOpts: []func(*tls.Config){func(config *tls.Config) {
			config.MinVersion = tls.VersionTLS12
			config.NextProtos = []string{"http/1.1"}
			// Check current bytes on every handshake, but parse only changed
			// material. Neither watches nor mtimes authorize stale certificates.
			config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return certificates.load(time.Now())
			}
		}},
	})
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme, LeaderElection: false, WebhookServer: server,
		Metrics:                 metricsserver.Options{BindAddress: options.MetricsAddress},
		HealthProbeBindAddress:  options.ProbeAddress,
		GracefulShutdownTimeout: ptr.To(35 * time.Second),
	})
	if err != nil {
		return fmt.Errorf("configure Sandbox admission server: %w", err)
	}
	// GetWebhookServer registers the manager runnable; supplying the option alone
	// does not start the server.
	server = manager.GetWebhookServer()
	handler, err := NewHandler(manager.GetAPIReader(), config)
	if err != nil {
		return err
	}
	if options.PodRequestTimeout != 0 {
		if options.PodRequestTimeout <= 0 || options.PodRequestTimeout >= 30*time.Second {
			return fmt.Errorf("Sandbox admission request timeout must be positive and less than 30 seconds")
		}
		handler.requestTimeout = options.PodRequestTimeout
	}
	server.Register(ValidationPath, &admission.Webhook{Handler: instrumentAdmission("validate", handler.Validate)})
	server.Register(DefaultingPath, &admission.Webhook{Handler: instrumentAdmission("default", handler.DefaultAgent)})
	if config.CetiAdmin != nil {
		cetiAdmin, err := NewCetiAdminHandler(manager.GetAPIReader(), config)
		if err != nil {
			return err
		}
		server.Register(CetiAdminValidationPath, &admission.Webhook{Handler: instrumentAdmission("cetiadmin", cetiAdmin.Validate)})
	}
	if options.PodStateNamespace != "" {
		podHandler, err := NewPodHandler(manager.GetAPIReader(), PodConfig{
			StateNamespace: options.PodStateNamespace, RequestTimeout: options.PodRequestTimeout,
			PlacementConfigMapName:    config.PlacementConfigMapName,
			CoordinationConfigMapName: config.CoordinationConfigMapName, CoordinationConfigMapUID: config.CoordinationConfigMapUID,
			CetiAdminSource: config.cetiAdminSource(),
		})
		if err != nil {
			return err
		}
		server.Register(PodValidationPath, &admission.Webhook{Handler: instrumentAdmission("pod", podHandler.Validate)})
	}
	if config.PlacementConfigMapName != "" {
		placement, err := sandboxplacement.NewHandler(manager.GetAPIReader(), sandboxplacement.Config{
			Namespace: options.PodStateNamespace, ConfigMapName: config.PlacementConfigMapName,
			RequestTimeout: options.PodRequestTimeout,
		})
		if err != nil {
			return err
		}
		server.Register(PlacementValidationPath, &admission.Webhook{Handler: instrumentAdmission("placement", placement.Validate)})
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := manager.AddReadyzCheck("webhook", server.StartedChecker()); err != nil {
		return err
	}
	if err := manager.AddReadyzCheck("serving-certificate", func(*http.Request) error {
		_, err := certificates.load(time.Now())
		return err
	}); err != nil {
		return err
	}
	return manager.Start(ctx)
}

type servingCertificateCache struct {
	mu                sync.Mutex
	directory         string
	certificate       *tls.Certificate
	certificateDigest [32]byte
	keyDigest         [32]byte
}

func (c *servingCertificateCache) load(now time.Time) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	certificatePEM, err := os.ReadFile(filepath.Join(c.directory, "tls.crt"))
	if err != nil {
		c.certificate = nil
		servingCertificateExpiry.Set(0)
		return nil, fmt.Errorf("read Sandbox admission serving certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(c.directory, "tls.key"))
	if err != nil {
		c.certificate = nil
		servingCertificateExpiry.Set(0)
		return nil, fmt.Errorf("read Sandbox admission serving key: %w", err)
	}
	certificateDigest, keyDigest := sha256.Sum256(certificatePEM), sha256.Sum256(keyPEM)
	if c.certificate == nil || c.certificateDigest != certificateDigest || c.keyDigest != keyDigest {
		c.certificate = nil
		servingCertificateExpiry.Set(0)
		pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load platform-issued Sandbox admission certificate: %w", err)
		}
		if pair.Leaf == nil {
			pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				return nil, fmt.Errorf("parse Sandbox admission certificate: %w", err)
			}
		}
		c.certificate, c.certificateDigest, c.keyDigest = &pair, certificateDigest, keyDigest
	}
	certificate := c.certificate.Leaf
	servingCertificateExpiry.Set(float64(certificate.NotAfter.Unix()))
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return nil, fmt.Errorf("Sandbox admission serving certificate is outside its validity period")
	}
	return c.certificate, nil
}
