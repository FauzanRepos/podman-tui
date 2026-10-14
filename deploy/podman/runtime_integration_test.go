package podman_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/deploy/platform"
	legacycontainers "github.com/containers/podman-tui/pdcs/containers"
	"github.com/containers/podman-tui/pdcs/registry"
	legacysecrets "github.com/containers/podman-tui/pdcs/secrets"
)

// Explicit opt-in only. Every resource is newly created with unique identity; no global prune/reset.
func TestRootlessBuildFailureRollbackOwnershipAndSecrets(t *testing.T) {
	connection := os.Getenv("PODMAN_TUI_TEST_CONNECTION")
	if connection == "" {
		t.Skip("set PODMAN_TUI_TEST_CONNECTION to a dedicated disposable Podman v6 host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	tree := t.TempDir()
	os.WriteFile(filepath.Join(tree, "Containerfile"), []byte("FROM docker.io/library/busybox:1.37\nCMD [\"sleep\",\"300\"]\n"), 0600)
	token, err := deploy.NewID()
	if err != nil {
		t.Fatal(err)
	}
	image, err := adapter.Build(ctx, deploy.SourceTree{Directory: tree, Containerfile: filepath.Join(tree, "Containerfile")}, "localhost/ptui-integration:"+token, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.Create(deploy.Application{Name: "integration-web", Image: image, Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": {}}, Readiness: deploy.Readiness{Mode: "running", TimeoutSeconds: 3}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Delete(context.Background(), a.Name, true)
	first, err := m.Deploy(ctx, a.Name, deploy.DeployOptions{}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := m.Get(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	c := desired.Config()
	c.Command = []string{"false"}
	desired.Profiles[desired.Profile] = c
	if err = m.Configure(a.Name, desired); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Deploy(ctx, a.Name, deploy.DeployOptions{}, nil, io.Discard); err == nil {
		t.Fatal("failed candidate passed readiness")
	}
	current, err := m.Get(a.Name)
	if err != nil || current.CurrentRelease != first.ID {
		t.Fatal("known-good release was changed", err)
	}
	status, err := m.Status(ctx, a.Name)
	if err != nil || !status.Running {
		t.Fatal("failed deploy stopped working container", err)
	}
	rolled, err := m.Deploy(ctx, a.Name, deploy.DeployOptions{RollbackRelease: first.ID}, nil, io.Discard)
	if err != nil || rolled.ImageID != first.ImageID {
		t.Fatal("retained immutable image rollback failed", err)
	}
	history, err := m.History(a.Name)
	if err != nil || len(history) != 3 || history[1].Status != "failed" {
		t.Fatal("release history did not persist failure", err)
	}
	// A forged application identity cannot mutate a real managed container.
	wrong := a
	wrong.ID = strings.Repeat("a", 32)
	if err = adapter.Remove(ctx, wrong, rolled.ContainerID); err == nil {
		t.Fatal("cross-application container removal allowed")
	}
	still, err := m.Status(ctx, a.Name)
	if err != nil || !still.Running {
		t.Fatal("ownership rejection changed container", err)
	}
	// Secret contents only enter the Podman reader, never metadata or runtime status.
	secretName := "ptui-secret-" + token
	if err = adapter.CreateSecret(ctx, secretName, strings.NewReader("synthetic-isolated-credential")); err != nil {
		t.Fatal(err)
	}
	latest, _ := m.Get(a.Name)
	cfg := latest.Config()
	cfg.Env = map[string]string{"VISIBLE_MODE": "non-sensitive-fixture"}
	cfg.Secrets = []deploy.SecretRef{{Name: secretName, Target: "TEST_CREDENTIAL", Type: "env"}}
	latest.Profiles[latest.Profile] = cfg
	if err = m.Configure(a.Name, latest); err != nil {
		t.Fatal(err)
	}
	secretRelease, err := m.Deploy(ctx, a.Name, deploy.DeployOptions{}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// The original Containers/Secrets screens must not reveal platform secrets.
	registry.SetConnection(conn)
	defer registry.UnsetConnection()
	inspection, err := legacycontainers.Inspect(secretRelease.ContainerID)
	if err != nil || strings.Contains(inspection, "synthetic-isolated-credential") {
		t.Fatal("original container inspection exposed secret contents", err)
	}
	var preview struct{ Config struct{ Env []string } }
	if err = json.Unmarshal([]byte(inspection), &preview); err != nil {
		t.Fatal(err)
	}
	modeFound := false
	for _, value := range preview.Config.Env {
		if value == "VISIBLE_MODE=<redacted>" {
			modeFound = true
		}
		if !strings.HasSuffix(value, "=<redacted>") {
			t.Fatal("managed runtime environment value exposed")
		}
	}
	if !modeFound {
		t.Fatal("managed configuration key lost from inspection")
	}
	secretInspection, err := legacysecrets.Inspect(secretName)
	if err != nil || strings.Contains(secretInspection, "synthetic-isolated-credential") {
		t.Fatal("original secret inspection exposed secret contents", err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(m.Store.Root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), "synthetic-isolated-credential") {
		t.Fatal("secret leaked into state")
	}
}
