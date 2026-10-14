package platform

import (
	"context"
	"errors"
	"net/url"

	"github.com/containers/podman-tui/deploy"
	podmanRuntime "github.com/containers/podman-tui/deploy/podman"
	"github.com/containers/podman-tui/pdcs/registry"
	"go.podman.io/podman/v6/pkg/bindings"
)

func endpointWithoutPassphrase(value *url.URL) string {
	copy := *value
	if copy.User != nil {
		copy.User = url.User(copy.User.Username())
	}
	return copy.String()
}

// OpenFrozen reuses the TUI registry's connection. It never dials a second SSH transport per navigation.
// The registry retains lifetime ownership; per-operation cancellation is applied by the runtime adapter.
func OpenFrozen(ctx context.Context, root string, connection registry.Connection, allowLocalIntegrations bool, frozen context.Context) (*deploy.Manager, func(), error) {
	client, err := bindings.GetClient(frozen)
	if err != nil {
		return nil, nil, err
	}
	expected, err := url.Parse(connection.URI)
	if err != nil {
		return nil, nil, err
	}
	if client.URI == nil || endpointWithoutPassphrase(client.URI) != endpointWithoutPassphrase(expected) {
		return nil, nil, errors.New("selected Podman connection changed; retry the operation")
	}
	if bindings.ServiceVersion(frozen).Major != 6 {
		return nil, nil, errors.New("deployment platform requires the repository-supported Podman v6 server")
	}
	manager, _, close, err := Open(ctx, root, connection, allowLocalIntegrations, false)
	if err != nil {
		return nil, nil, err
	}
	manager.Runtime = podmanRuntime.New(frozen)
	return manager, close, nil
}
