package platform_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/deploy/platform"
	podmanRuntime "github.com/containers/podman-tui/deploy/podman"
)

type serviceFixture struct {
	t          *testing.T
	ctx        context.Context
	connection string
	manager    *deploy.Manager
	adapter    *podmanRuntime.Runtime
	token      string
	apps       []string
	secrets    []string
	networks   []string
	retained   []deploy.Application
	containers []string
	images     []string
	recovered  []struct {
		application deploy.Application
		container   string
	}
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	connection := os.Getenv("PODMAN_TUI_TEST_CONNECTION")
	if connection == "" {
		t.Skip("set PODMAN_TUI_TEST_CONNECTION to a dedicated disposable rootless Podman v6 host")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatal("real service integration requires the Podman CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	conn, err := platform.Resolve(connection)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	m, adapter, closeConnection, err := platform.Open(ctx, filepath.Join(t.TempDir(), "state"), conn, false, true)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	token, err := deploy.NewID()
	if err != nil {
		closeConnection()
		cancel()
		t.Fatal(err)
	}
	f := &serviceFixture{t: t, ctx: ctx, connection: connection, manager: m, adapter: adapter, token: token}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		defer cancel()
		defer closeConnection()
		for _, recovered := range f.recovered {
			if err := adapter.Remove(cleanup, recovered.application, recovered.container); err != nil {
				t.Errorf("remove recovered service: %v", err)
			}
		}
		for i := len(f.apps) - 1; i >= 0; i-- {
			a, err := m.Get(f.apps[i])
			if errors.Is(err, deploy.ErrNotFound) {
				continue
			}
			if err != nil {
				t.Errorf("read cleanup application: %v", err)
				continue
			}
			if err = m.Delete(cleanup, a.Name, true); err != nil {
				t.Errorf("delete test application: %v", err)
				continue
			}
			f.removeServiceStorage(cleanup, a)
		}
		for _, id := range f.containers {
			f.cleanupCommand(cleanup, "container", "rm", "--force", id)
		}
		for _, a := range f.retained {
			f.removeServiceStorage(cleanup, a)
		}
		for _, name := range f.networks {
			f.cleanupCommand(cleanup, "network", "rm", name)
		}
		for _, name := range f.secrets {
			f.cleanupCommand(cleanup, "secret", "rm", name)
		}
		for _, image := range f.images {
			// Never force removal: another consumer must make cleanup fail safely.
			f.cleanupCommand(cleanup, "image", "rm", image)
		}
	})
	if strings.TrimSpace(string(f.must("", "info", "--format", "{{.Host.Security.Rootless}}"))) != "true" {
		t.Fatal("service integration is restricted to a disposable rootless Podman server")
	}
	return f
}

func (f *serviceFixture) command(ctx context.Context, input string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "podman", append([]string{"--connection", f.connection}, args...)...)
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	// Protocol assertions inspect stdout, not client diagnostics on stderr.
	// Database diagnostics remain private because they may contain credentials.
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	err := command.Run()
	return stdout.Bytes(), err
}

func (f *serviceFixture) must(input string, args ...string) []byte {
	f.t.Helper()
	out, err := f.command(f.ctx, input, args...)
	if err != nil {
		// Do not print Podman output: database diagnostics can contain supplied credentials.
		f.t.Fatalf("Podman %s failed: %v", args[0], err)
	}
	return out
}

func (f *serviceFixture) cleanupCommand(ctx context.Context, args ...string) {
	f.t.Helper()
	if _, err := f.command(ctx, "", args...); err != nil {
		f.t.Errorf("cleanup Podman %s failed: %v", args[0], err)
	}
}

func (f *serviceFixture) owns(ctx context.Context, resource, name, applicationID string) bool {
	out, err := f.command(ctx, "", resource, "inspect", name, "--format", "{{json .Labels}}")
	if err != nil {
		return false // A failed create need not have provisioned storage.
	}
	var labels map[string]string
	if err = json.Unmarshal(out, &labels); err != nil {
		f.t.Errorf("decode cleanup ownership: %v", err)
		return false
	}
	return labels[deploy.OwnerLabel] == deploy.OwnerValue && labels[deploy.AppLabel] == applicationID
}

func (f *serviceFixture) removeServiceStorage(ctx context.Context, a deploy.Application) {
	if a.Kind != "service" {
		return
	}
	for _, volume := range a.Config().Volumes {
		if f.owns(ctx, "volume", volume.Source, a.ID) {
			f.cleanupCommand(ctx, "volume", "rm", volume.Source)
		}
	}
	for _, network := range a.Config().Networks {
		if f.owns(ctx, "network", network, a.ID) {
			f.cleanupCommand(ctx, "network", "rm", network)
		}
	}
}

func (f *serviceFixture) secret(suffix, contents string) string {
	f.t.Helper()
	name := "ptui-test-" + suffix + "-" + f.token
	if err := f.adapter.CreateSecret(f.ctx, name, strings.NewReader(contents)); err != nil {
		f.t.Fatal(err)
	}
	f.secrets = append(f.secrets, name)
	return name
}

func (f *serviceFixture) service(kind, secret string) (deploy.Application, deploy.Release) {
	f.t.Helper()
	image, _, err := podmanRuntime.ServiceConfig(kind, secret, "ptui-test-data-"+f.token, "ptui-test-net-"+f.token)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.command(f.ctx, "", "image", "exists", image); err != nil {
		f.images = append(f.images, image)
	}
	name := "ptui-test-" + kind + "-" + f.token[:12]
	f.apps = append(f.apps, name) // Include metadata retained by a failed deploy.
	release, err := platform.CreateService(f.ctx, f.manager, name, kind, secret, "", io.Discard)
	if err != nil {
		f.t.Fatal(err)
	}
	a, err := f.manager.Get(name)
	if err != nil {
		f.t.Fatal(err)
	}
	f.assertPrivate(release.ContainerID)
	if !f.owns(f.ctx, "volume", a.Config().Volumes[0].Source, a.ID) || !f.owns(f.ctx, "network", a.Config().Networks[0], a.ID) {
		f.t.Fatal("service storage/network lacks application-scoped ownership")
	}
	return a, release
}

func (f *serviceFixture) assertPrivate(id string) {
	f.t.Helper()
	out := f.must("", "container", "inspect", id)
	var inspected []struct {
		NetworkSettings struct {
			Ports map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(out, &inspected); err != nil || len(inspected) != 1 {
		f.t.Fatal("cannot inspect service network", err)
	}
	for _, mapping := range inspected[0].NetworkSettings.Ports {
		if string(mapping) != "null" && string(mapping) != "[]" {
			f.t.Fatal("private service published a host port")
		}
	}
}

func (f *serviceFixture) client(service deploy.Application, inject bool) (deploy.Application, deploy.Release) {
	f.t.Helper()
	name := "ptui-test-client-" + f.token[:12]
	a, err := f.manager.Create(deploy.Application{Name: name, Image: service.Image, Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": {Command: []string{"sleep", "600"}}}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.apps = append(f.apps, name)
	if err = platform.LinkService(f.manager, name, service.Name, inject); err != nil {
		f.t.Fatal(err)
	}
	a, err = f.manager.Get(name)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(a.Config().Secrets) != 0 || len(a.Config().Volumes) != 0 {
		f.t.Fatal("link implicitly granted credential or data access")
	}
	if !inject && len(a.Config().Env) != 0 {
		f.t.Fatal("link injected connection settings without authorization")
	}
	release, err := f.manager.Deploy(f.ctx, name, deploy.DeployOptions{}, nil, io.Discard)
	if err != nil {
		f.t.Fatal(err)
	}
	return a, release
}

func (f *serviceFixture) await(run func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if run() {
			return
		}
		select {
		case <-f.ctx.Done():
			f.t.Fatal("service readiness timed out")
		case <-time.After(250 * time.Millisecond):
		}
	}
	f.t.Fatal("real service protocol never became ready")
}

func TestRealRedisOptionalAuthentication(t *testing.T) {
	f := newServiceFixture(t)
	service, _ := f.service("redis", "")
	_, client := f.client(service, true)
	f.await(func() bool {
		out, err := f.command(f.ctx, "", "exec", client.ContainerID, "sh", "-c", `exec redis-cli --raw -h "$REDIS_HOST" -p "$REDIS_PORT" PING`)
		return err == nil && strings.TrimSpace(string(out)) == "PONG"
	})
}

func TestRealRedisPrivateAuthorizationPersistenceAndOwnership(t *testing.T) {
	f := newServiceFixture(t)
	password := "redis-test-" + f.token
	digest := sha256.Sum256([]byte(password))
	secret := f.secret("acl", "user default off\nuser app on #"+hex.EncodeToString(digest[:])+" ~* &* +@all\n")
	service, release := f.service("redis", secret)
	if got := strings.TrimSpace(string(f.must("", "exec", release.ContainerID, "stat", "-c", "%u:%g:%a", "/run/secrets/redis.acl"))); got != "999:999:400" {
		t.Fatalf("Redis ACL ownership/mode = %s", got)
	}
	unmanaged := strings.TrimSpace(string(f.must("", "run", "--detach", "--image-volume=ignore", "--name", "ptui-test-unmanaged-"+f.token, service.Image, "sleep", "600")))
	f.containers = append(f.containers, unmanaged)
	// An unrelated container does not join the dedicated service network implicitly.
	probe, stopProbe := context.WithTimeout(f.ctx, 10*time.Second)
	out, err := f.command(probe, "", "exec", unmanaged, "redis-cli", "-h", "ptui-"+service.ID, "PING")
	stopProbe()
	if err == nil || strings.Contains(string(out), "PONG") {
		t.Fatal("unlinked unmanaged container reached the service")
	}
	client, clientRelease := f.client(service, false)
	if err = platform.LinkService(f.manager, client.Name, service.Name, true); err != nil {
		t.Fatal(err)
	}
	clientRelease, err = f.manager.Deploy(f.ctx, client.Name, deploy.DeployOptions{}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	redis := func(input string) ([]byte, error) {
		return f.command(f.ctx, input, "exec", "--interactive", clientRelease.ContainerID, "sh", "-c", `exec redis-cli --raw -h "$REDIS_HOST" -p "$REDIS_PORT"`)
	}
	f.await(func() bool {
		out, err := redis("AUTH app " + password + "\nPING\n")
		return err == nil && strings.TrimSpace(string(out)) == "OK\nPONG"
	})
	out, _ = redis("PING\n")
	if !strings.Contains(string(out), "NOAUTH") {
		t.Fatal("authenticated Redis accepted a client without authentication")
	}
	out, _ = redis("AUTH app incorrect-credential\nPING\n")
	if !strings.Contains(string(out), "WRONGPASS") || strings.Contains(string(out), "PONG") {
		t.Fatal("Redis accepted invalid credentials")
	}
	out, err = redis("AUTH app " + password + "\nSET ptui-proof persisted\n")
	if err != nil || strings.TrimSpace(string(out)) != "OK\nOK" {
		t.Fatal("real Redis write failed")
	}
	for _, action := range []string{"stop", "start"} {
		if err = f.manager.Lifecycle(f.ctx, service.Name, action); err != nil {
			t.Fatal(err)
		}
	}
	f.await(func() bool {
		out, err := redis("AUTH app " + password + "\nGET ptui-proof\n")
		return err == nil && strings.TrimSpace(string(out)) == "OK\npersisted"
	})
	state, err := os.ReadFile(filepath.Join(f.manager.Store.Root, "state.json"))
	if err != nil || strings.Contains(string(state), password) || strings.Contains(string(state), hex.EncodeToString(digest[:])) {
		t.Fatal("credential material leaked into controller state", err)
	}
	inspection := f.must("", "container", "inspect", release.ContainerID)
	if strings.Contains(string(inspection), password) || strings.Contains(string(inspection), hex.EncodeToString(digest[:])) {
		t.Fatal("credential material leaked into container configuration")
	}
	if err = f.manager.Delete(f.ctx, service.Name, true); err != nil {
		t.Fatal(err)
	}
	f.retained = append(f.retained, service)
	if !f.owns(f.ctx, "volume", service.Config().Volumes[0].Source, service.ID) {
		t.Fatal("service removal destroyed its persistent data volume")
	}
	if strings.TrimSpace(string(f.must("", "container", "inspect", unmanaged, "--format", "{{.State.Running}}"))) != "true" {
		t.Fatal("service removal stopped an unmanaged container")
	}
	// Read the real stored value from a new server container, not merely a volume listing.
	release.ID, err = deploy.NewID()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := f.adapter.Create(f.ctx, service, release)
	if err != nil {
		t.Fatal(err)
	}
	f.recovered = append(f.recovered, struct {
		application deploy.Application
		container   string
	}{service, restored.ID})
	if err = f.adapter.Start(f.ctx, service, restored.ID); err != nil {
		t.Fatal(err)
	}
	f.await(func() bool {
		out, err := redis("AUTH app " + password + "\nGET ptui-proof\n")
		return err == nil && strings.TrimSpace(string(out)) == "OK\npersisted"
	})
	// Collision attempts must reject both unmanaged and other-service managed networks.
	collision := "ptui-test-collision-" + f.token
	f.must("", "network", "create", collision)
	f.networks = append(f.networks, collision)
	for i, network := range []string{collision, service.Config().Networks[0]} {
		name := "ptui-test-collision-" + f.token[:12] + "-" + string(rune('a'+i))
		f.apps = append(f.apps, name)
		if _, err = platform.CreateService(f.ctx, f.manager, name, "redis", "", network, io.Discard); err == nil {
			t.Fatal("service adopted an unmanaged or another service's network")
		}
	}
	f.must("", "network", "inspect", collision)
	missingName := "ptui-test-missing-secret-" + f.token[:12]
	f.apps = append(f.apps, missingName)
	if _, err = platform.CreateService(f.ctx, f.manager, missingName, "redis", "ptui-test-does-not-exist-"+f.token, "", io.Discard); err == nil {
		t.Fatal("service deployment accepted a nonexistent ACL secret")
	}
}

// The larger SQL images are independently opt-in. These are real SQL operations over the
// authorized application network; no database scenario is implied by the Redis smoke run.
func TestRealSQLServiceCredentialAndRestartPersistence(t *testing.T) {
	if os.Getenv("PODMAN_TUI_TEST_DATABASES") != "1" {
		t.Skip("set PODMAN_TUI_TEST_DATABASES=1 as well as PODMAN_TUI_TEST_CONNECTION for real SQL scenarios")
	}
	for _, kind := range []string{"postgres", "mariadb", "mysql"} {
		t.Run(kind, func(t *testing.T) {
			f := newServiceFixture(t)
			password := "sql-test-" + f.token
			service, _ := f.service(kind, f.secret("password", password))
			_, client := f.client(service, true)
			var script string
			if kind == "postgres" {
				script = `IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -X -w -v ON_ERROR_STOP=1 -At`
			} else {
				binary := "mysql"
				if kind == "mariadb" {
					binary = "mariadb"
				}
				// Password enters the client only through stdin -> process environment, never exec arguments.
				script = `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec ` + binary + ` --protocol=tcp --host="$MYSQL_HOST" --port="$MYSQL_PORT" --user="$MYSQL_USER" --database="$MYSQL_DATABASE" --batch --skip-column-names`
			}
			query := func(credential, sql string) ([]byte, error) {
				return f.command(f.ctx, credential+"\n"+sql+"\n", "exec", "--interactive", client.ContainerID, "sh", "-c", script)
			}
			f.await(func() bool {
				out, err := query(password, "SELECT 1;")
				return err == nil && strings.TrimSpace(string(out)) == "1"
			})
			if out, err := query("incorrect-credential", "SELECT 1;"); err == nil || strings.TrimSpace(string(out)) == "1" {
				t.Fatal("database accepted invalid credentials")
			}
			if _, err := query(password, "CREATE TABLE ptui_proof (value VARCHAR(32)); INSERT INTO ptui_proof VALUES ('persisted');"); err != nil {
				t.Fatal("real SQL write failed")
			}
			for _, action := range []string{"stop", "start"} {
				if err := f.manager.Lifecycle(f.ctx, service.Name, action); err != nil {
					t.Fatal(err)
				}
			}
			f.await(func() bool {
				out, err := query(password, "SELECT value FROM ptui_proof;")
				return err == nil && strings.TrimSpace(string(out)) == "persisted"
			})
		})
	}
}
