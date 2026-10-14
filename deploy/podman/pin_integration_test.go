package podman_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/deploy/platform"
)

func TestRootlessUnjournaledImagePinsRemainOwnershipScoped(t *testing.T) {
	connection := os.Getenv("PODMAN_TUI_TEST_CONNECTION")
	if connection == "" {
		t.Skip("requires dedicated disposable Podman v6 connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := platform.Resolve(connection)
	if err != nil {
		t.Fatal(err)
	}
	m, adapter, close, err := platform.Open(ctx, filepath.Join(t.TempDir(), "state"), conn, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	a, err := m.Create(deploy.Application{Name: "pin-recovery", Image: "docker.io/library/busybox:1.37", Profiles: map[string]deploy.RuntimeConfig{"production": {Command: []string{"sleep", "60"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Delete(context.Background(), a.Name, true)
	good, err := m.Deploy(ctx, a.Name, deploy.DeployOptions{}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	goodPin, err := adapter.PinImage(ctx, a, good)
	if err != nil {
		t.Fatal(err)
	}
	wrong := a
	wrong.ID = strings.Repeat("a", 32)
	if _, err = adapter.FindImagePin(ctx, wrong, good); !errors.Is(err, deploy.ErrNotFound) {
		t.Fatal("foreign application pin adopted", err)
	}
	mismatch := good
	mismatch.ImageID = "sha256:" + strings.Repeat("0", 64)
	if _, err = adapter.FindImagePin(ctx, a, mismatch); err == nil {
		t.Fatal("wrong image identity accepted")
	}
	pending := good
	pending.ID, err = deploy.NewID()
	if err != nil {
		t.Fatal(err)
	}
	pending.ContainerID = ""
	pending.ImagePinID = ""
	pending.Status = "failed"
	session, err := m.Store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	session.State.Releases = append(session.State.Releases, pending)
	err = session.Save()
	session.Close()
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := adapter.PinImage(ctx, a, pending)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Remove(context.Background(), a, orphan)
	if err = m.Cleanup(ctx, a.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.FindImagePin(ctx, a, pending); !errors.Is(err, deploy.ErrNotFound) {
		t.Fatal("unjournaled eligible pin leaked", err)
	}
	retained, err := adapter.FindImagePin(ctx, a, good)
	if err != nil || retained != goodPin {
		t.Fatal("successful image holder removed", err)
	}
	status, err := m.Status(ctx, a.Name)
	if err != nil || !status.Running {
		t.Fatal("pin cleanup stopped current deployment", err)
	}
}
