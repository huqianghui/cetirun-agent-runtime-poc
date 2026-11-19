// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-agent-broker/internal/broker"
	"github.com/Azure/CetiRun/app/internal/tlsreload"
)

func main() {
	cfg, err := broker.LoadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	service, err := broker.NewServer(cfg)
	if err != nil {
		slog.Error("initialize service", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	servers := []*http.Server{{Addr: cfg.ListenAddress, Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}}
	if cfg.TLSCertFile != "" {
		material, err := tlsreload.New(tlsreload.Files{Certificate: cfg.TLSCertFile, Key: cfg.TLSKeyFile, CA: cfg.TLSClientCAFile})
		if err != nil {
			slog.Error("load TLS material", "error", err)
			os.Exit(1)
		}
		go material.Run(ctx, nil)
		servers = append(servers, &http.Server{Addr: cfg.TLSListenAddress, Handler: service.TLSHandler(), TLSConfig: material.ServerConfig(true), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second})
	}
	go service.RunMaintenance(ctx)
	failures := make(chan error, len(servers))
	for _, server := range servers {
		go func() {
			slog.Info("Agent Identity Broker listener started", "address", server.Addr)
			if server.TLSConfig != nil {
				failures <- server.ListenAndServeTLS("", "")
			} else {
				failures <- server.ListenAndServe()
			}
		}()
	}
	failed := false
	select {
	case <-ctx.Done():
	case err := <-failures:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("serve", "error", err)
			failed = true
		}
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			slog.Error("shutdown", "error", err)
			_ = server.Close()
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
