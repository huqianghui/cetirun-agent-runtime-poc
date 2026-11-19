// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/terminal"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"golang.org/x/term"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func run() error {
	if len(os.Args) != 2 || (os.Args[1] != "serve" && os.Args[1] != "exec") {
		fmt.Println("Usage: terminal serve | exec\nThe Chart supplies a fixed Agent identity and target. exec is internal to ttyd; it never opens a local shell.")
		if len(os.Args) == 2 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
			return nil
		}
		return fmt.Errorf("invalid terminal command")
	}
	cfg, err := terminal.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	configuration, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("terminal requires its own in-cluster service account")
	}
	backend, err := terminal.NewKubernetes(cfg, configuration)
	if err != nil {
		return fmt.Errorf("terminal Kubernetes client could not be created")
	}
	if os.Args[1] == "serve" {
		return terminal.Serve(ctx, cfg, backend)
	}
	session, err := terminal.ClaimSession(ctx, os.Getenv("CETIRUN_TERMINAL_CONTROL_SOCKET"), os.Getenv("TTYD_USER"))
	if err != nil || session.AgentUID != cfg.AgentUID {
		return terminal.ErrDenied
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("terminal exec requires a PTY")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, state)
	sizes := &terminal.SizeQueue{Changes: make(chan remotecommand.TerminalSize, 1), Done: ctx.Done()}
	sizes.Current = func() *remotecommand.TerminalSize {
		width, height, err := term.GetSize(fd)
		if err != nil || width < 1 || height < 1 {
			return nil
		}
		return &remotecommand.TerminalSize{Width: uint16(width), Height: uint16(height)}
	}
	resize := func() {
		if size := sizes.Current(); size != nil {
			select {
			case sizes.Changes <- *size:
			default:
			}
		}
	}
	changes := make(chan os.Signal, 1)
	notifyResize(changes)
	defer signal.Stop(changes)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-changes:
				resize()
			}
		}
	}()
	runner := terminal.NewRunner(backend, terminal.Streams{In: os.Stdin, Out: os.Stdout, Sizes: sizes})
	return runner.Run(ctx, session)
}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, terminalpolicy.ErrNoShell) {
			fmt.Fprintln(os.Stderr, terminalpolicy.ErrNoShell)
		} else {
			// API errors can contain request details. Do not print them to the browser.
			fmt.Fprintln(os.Stderr, "\r\nTerminal ended. Reopen it from the Portal after checking Agent status.")
		}
		if len(os.Args) == 2 && os.Args[1] == "exec" {
			// ttyd 1.7.7 maps nonzero child exits to WebSocket code 1006.
			// Runner has already decided to stop; report the error above, then
			// close normally so expiry, denial or shell exit cannot auto-reconnect.
			return
		}
		os.Exit(1)
	}
}
