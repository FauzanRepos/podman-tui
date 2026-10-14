// Package platform composes deployment modules at the CLI/TUI seam.
package platform

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containers/podman-tui/config"
	"github.com/containers/podman-tui/deploy"
	gitSource "github.com/containers/podman-tui/deploy/git"
	podmanRuntime "github.com/containers/podman-tui/deploy/podman"
	"github.com/containers/podman-tui/deploy/proxy"
	"github.com/containers/podman-tui/deploy/quadlet"
	"github.com/containers/podman-tui/pdcs/registry"
	"go.podman.io/podman/v6/pkg/bindings"
)

func DefaultStateDir() (string, error) {
	if configured := os.Getenv("PODMAN_TUI_STATE_DIR"); configured != "" {
		return filepath.Abs(configured)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "podman-tui", "deploy"), nil
}

func Resolve(name string) (registry.Connection, error) {
	cfg, err := config.NewPodmanRemoteConfig()
	if err != nil {
		return registry.Connection{}, err
	}
	for _, c := range cfg.RemoteConnections() {
		if c.Name == name || (name == "" && c.Default) {
			return c, nil
		}
	}
	return registry.Connection{}, errors.New("select a configured Podman connection with --connection")
}

// Open freezes an endpoint. Metadata-only operations do not contact Podman.
func Open(ctx context.Context, root string, connection registry.Connection, allowLocalIntegrations, connect bool) (*deploy.Manager, *podmanRuntime.Runtime, func(), error) {
	if root == "" {
		var err error
		root, err = DefaultStateDir()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	uri, err := url.Parse(connection.URI)
	if err != nil || uri.Scheme == "" {
		return nil, nil, nil, errors.New("invalid Podman connection endpoint")
	}
	store, err := deploy.NewStore(root, connection.URI)
	if err != nil {
		return nil, nil, nil, err
	}
	localPodman := uri.Scheme == "unix" && runtime.GOOS == "linux"
	local := allowLocalIntegrations || os.Getenv("PODMAN_TUI_LOCAL_INTEGRATIONS") == "1" || localPodman
	router := proxy.New(local)
	if !connect {
		manager := deploy.NewManager(store, nil, gitSource.New(), router)
		manager.LocalPodman = localPodman
		if localPodman {
			manager.Startup = quadlet.New(true)
		}
		return manager, nil, func() {}, nil
	}
	base, cancel := context.WithCancel(context.Background())
	stopConnectCancellation := context.AfterFunc(ctx, cancel)
	if uri.Scheme == "ssh" {
		if pass, ok := os.LookupEnv("CONTAINER_PASSPHRASE"); ok {
			user := ""
			if uri.User != nil {
				user = uri.User.Username()
			}
			uri.User = url.UserPassword(user, pass)
		}
	}
	conn, err := bindings.NewConnectionWithIdentity(base, uri.String(), connection.Identity, false)
	stopConnectCancellation()
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	if ctx.Err() != nil {
		cancel()
		return nil, nil, nil, ctx.Err()
	}
	if bindings.ServiceVersion(conn).Major != 6 {
		cancel()
		return nil, nil, nil, errors.New("deployment platform requires the repository-supported Podman v6 server")
	}
	adapter := podmanRuntime.New(conn)
	manager := deploy.NewManager(store, adapter, gitSource.New(), router)
	manager.LocalPodman = localPodman
	if localPodman {
		manager.Startup = quadlet.New(true)
	}
	close := func() {
		if client, err := bindings.GetClient(conn); err == nil {
			client.Client.CloseIdleConnections()
		}
		cancel()
	}
	return manager, adapter, close, nil
}
