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
	// Serialize initializers without blocking endpoint selection during a dial.
	pdcsRegistry.connectMu.Lock()
	defer pdcsRegistry.connectMu.Unlock()
	pdcsRegistry.mu.Lock()
	if !pdcsRegistry.connectionIsSet {
		pdcsRegistry.mu.Unlock()
		return nil, ErrConnectionNotSelected
	}
	if pdcsRegistry.connContext != nil {
		conn := *pdcsRegistry.connContext
		pdcsRegistry.mu.Unlock()
		return conn, nil
	}
	selected, generation := pdcsRegistry.connection, pdcsRegistry.generation
	ctx, cancel := context.WithCancel(context.Background())
	pdcsRegistry.connContextCancel = cancel // endpoint changes also cancel an unfinished handshake
	pdcsRegistry.mu.Unlock()
	published := false
	defer func() {
		if !published {
			cancel()
			pdcsRegistry.mu.Lock()
			if pdcsRegistry.generation == generation {
				pdcsRegistry.connContextCancel = nil
			}
			pdcsRegistry.mu.Unlock()
		}
	}()

	connURI, err := url.Parse(selected.URI)
	if err != nil {
		return nil, err
	}
	if connURI.Scheme == "ssh" {
		passPhrase := os.Getenv("CONTAINER_PASSPHRASE")
		username := ""
		if connURI.User != nil {
			username = connURI.User.Username()
		}
		connURI.User = url.UserPassword(username, passPhrase)
	}
	conn, err := bindings.NewConnectionWithIdentity(ctx, connURI.String(), selected.Identity, false)
	if err != nil {
		cancel()
		return nil, err
	}
	pdcsRegistry.mu.Lock()
	defer pdcsRegistry.mu.Unlock()
	if !pdcsRegistry.connectionIsSet || pdcsRegistry.generation != generation {
		cancel()
		return nil, errors.New("Podman connection changed while connecting; retry")
	}
	pdcsRegistry.connContext = &conn
	pdcsRegistry.connContextCancel = cancel
	published = true
	return conn, nil
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
