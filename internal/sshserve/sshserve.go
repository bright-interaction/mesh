// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package sshserve serves the Mesh TUI over SSH, so a teammate can browse the
// knowledge graph by ssh-ing the host with no local install: charmbracelet/wish
// runs the same bubbletea TUI the `mesh tui` command does, once per session, over
// the same read-only index the agent uses. Auth is fail-closed.
package sshserve

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	bm "github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/logging"

	"github.com/bright-interaction/mesh/internal/netaddr"
	"github.com/bright-interaction/mesh/internal/tui"
)

// Options configures the SSH viewer server.
type Options struct {
	Addr         string // listen address, e.g. "127.0.0.1:2222"
	HostKeyPath  string // persistent host key (generated if missing)
	AuthKeysPath string // OpenSSH authorized_keys; only these public keys may connect
	AllowAnon    bool   // explicit opt-in to NO auth; only honoured on a loopback Addr
	Logf         func(string, ...any)
}

// Serve runs the SSH TUI server until ctx is cancelled, then drains in-flight
// sessions. It fails closed twice over: without an authorized_keys file it refuses
// to start unless AllowAnon is explicitly set, and it refuses AllowAnon on any bind
// that is not loopback.
func Serve(ctx context.Context, vaultRoot string, opts Options) error {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	// Validate auth FIRST, before touching the vault: fail closed on a
	// misconfiguration rather than opening the index and binding a port.
	if opts.AuthKeysPath == "" && !opts.AllowAnon {
		return errors.New("refusing to start without auth: pass --authorized-keys <file> (or --allow-anonymous on a loopback address for an open demo)")
	}
	// AllowAnon means every connection is handed the entire vault with no
	// credential, so it is only defensible where nothing off this machine can
	// reach the port. The help called it "localhost demos only" and nothing
	// enforced it: the default ":2222" bound every interface, so `ssh -p 2222
	// <host>` from anywhere on the LAN (or the internet, on a VPS) browsed the
	// whole graph. This is the same fail-closed rule `mesh mcp --http` already
	// applies to a tokenless bind.
	if opts.AuthKeysPath == "" && !netaddr.IsLoopback(opts.Addr) {
		return fmt.Errorf("refusing to bind %s with --allow-anonymous: an unauthenticated bind beyond loopback hands the whole vault to anyone who can reach this host. Use --addr 127.0.0.1:2222 for a local demo, or pass --authorized-keys <file> to serve it to teammates (fail-closed)", opts.Addr)
	}

	// One shared, read-only backend over the vault index; each SSH session gets its
	// own TUI model. The index store + retriever are concurrent-read safe.
	be, cleanup, err := tui.NewLocalBackend(vaultRoot)
	if err != nil {
		return fmt.Errorf("open vault %q: %w", vaultRoot, err)
	}
	defer cleanup()

	handler := func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
		return tui.NewApp(be), []tea.ProgramOption{tea.WithAltScreen()}
	}

	sopts := []ssh.Option{
		wish.WithAddress(opts.Addr),
		wish.WithHostKeyPath(opts.HostKeyPath),
		wish.WithIdleTimeout(30 * time.Minute),
		wish.WithMiddleware(
			bm.Middleware(handler),
			activeterm.Middleware(), // require a real terminal (a PTY)
			logging.Middleware(),
		),
	}
	if opts.AuthKeysPath != "" {
		sopts = append(sopts, wish.WithAuthorizedKeys(opts.AuthKeysPath))
	} else {
		opts.Logf("WARNING: serving with NO auth (--allow-anonymous) on %s; every local user of this machine can read the whole vault", opts.Addr)
	}

	srv, err := wish.NewServer(sopts...)
	if err != nil {
		return fmt.Errorf("ssh server: %w", err)
	}

	errc := make(chan error, 1)
	go func() {
		opts.Logf("mesh ssh viewer on %s (vault %s)", opts.Addr, vaultRoot)
		if e := srv.ListenAndServe(); e != nil && !errors.Is(e, ssh.ErrServerClosed) {
			errc <- e
		}
	}()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case e := <-errc:
		return e
	}
}
