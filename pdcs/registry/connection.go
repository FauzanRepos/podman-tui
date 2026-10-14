package registry

import (
	"context"
	"errors"
	"net/url"
	"os"

	"go.podman.io/podman/v6/pkg/bindings"
)

// ErrConnectionNotSelected implements connection is not selected error.
var ErrConnectionNotSelected = errors.New("system connection not selected")

// GetConnection returns connection to podman socket.
func GetConnection() (context.Context, error) {
	if !ConnectionIsSet() {
		return nil, ErrConnectionNotSelected
	}

	if pdcsRegistry.connContext == nil {
		var (
			conn       context.Context
			err        error
			passPhrase string
		)

		dest := ConnectionURI()

		connURI, err := url.Parse(dest)
		if err != nil {
			return nil, err
		}

		if v, found := os.LookupEnv("CONTAINER_PASSPHRASE"); found {
			passPhrase = v
		}

		connURI.User = url.UserPassword(connURI.User.String(), passPhrase)

		ctx := context.Background()
		ctx, cancel := context.WithCancel(ctx)

		conn, err = bindings.NewConnectionWithIdentity(ctx, connURI.String(), ConnectionIdentity(), false)
		if err != nil {
			cancel()

			return nil, err
		}

		pdcsRegistry.connContext = &conn
		pdcsRegistry.connContextCancel = cancel

		return conn, nil
	}

	return *pdcsRegistry.connContext, nil
}

// GetCachedConnection captures a ready binding context without initializing another transport.
// Metadata and context are checked under one lock so TUI operations cannot drift to another host.
func GetCachedConnection(expected Connection) (context.Context, error) {
	pdcsRegistry.mu.Lock()
	defer pdcsRegistry.mu.Unlock()
	if !pdcsRegistry.connectionIsSet || pdcsRegistry.connection.Status != ConnectionStatusConnected {
		return nil, errors.New("connect a Podman server before running application operations")
	}
	if pdcsRegistry.connection.URI != expected.URI || pdcsRegistry.connection.Identity != expected.Identity || pdcsRegistry.connContext == nil {
		return nil, errors.New("Podman connection changed; retry the operation")
	}
	return *pdcsRegistry.connContext, nil
}
