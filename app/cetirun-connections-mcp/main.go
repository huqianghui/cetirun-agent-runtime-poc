// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/brokerclient"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/identity"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/mcp"
	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/tools"
	"github.com/Azure/CetiRun/app/internal/tlsreload"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("configuration error: %v", err)
		os.Exit(1)
	}

	kube, err := identity.NewKubernetes(cfg)
	if err != nil {
		log.Printf("Kubernetes client error: %v", err)
		os.Exit(1)
	}
	broker, err := brokerclient.NewMutualTLS(cfg.BrokerBaseURL, os.Getenv("BROKER_TLS_CA_FILE"), os.Getenv("BROKER_TLS_CERT_FILE"), os.Getenv("BROKER_TLS_KEY_FILE"))
	if err != nil {
		log.Printf("Broker client error: %v", err)
		os.Exit(1)
	}
	material, err := tlsreload.New(tlsreload.Files{Certificate: cfg.TLSCertFile, Key: cfg.TLSKeyFile})
	if err != nil {
		log.Printf("TLS configuration error: %v", err)
		os.Exit(1)
	}
	lifecycle, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go material.Run(lifecycle, nil)
	go broker.RunTLSReload(lifecycle)
	service := tools.New(kube, broker, cfg)
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           mcp.New(service),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig:         material.ServerConfig(false),
	}

	go func() {
		log.Printf("CetiRun Connections MCP listening on %s", cfg.ListenAddress)
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
			os.Exit(1)
		}
	}()

	<-lifecycle.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
