package quadlet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/containers/podman-tui/deploy"
)

func fixture() (deploy.Application, deploy.Release) {
	return deploy.Application{
		ID: strings.Repeat("a", 32), Name: "web", Image: "example/web:latest", Profile: "production",
		Profiles: map[string]deploy.RuntimeConfig{"production": {
			Env:           map[string]string{"MESSAGE": "space % $ quote\" slash\\"},
			Ports:         []deploy.Port{{Host: 8080, Container: 80}},
			Command:       []string{"echo", "two words", "%n", "$HOME"},
			RestartPolicy: "on-failure", MemoryBytes: 1024, CPUQuota: 150000,
			Secrets:  []deploy.SecretRef{{Name: "token", Type: "env", Target: "TOKEN"}},
			Networks: []string{"backend"},
			Volumes:  []deploy.Volume{{Source: "data", Target: "/data", ReadOnly: true}},
		}},
	}, deploy.Release{ID: strings.Repeat("b", 32), ImageID: "sha256:" + strings.Repeat("c", 64)}
}

func TestRenderBootRecovery(t *testing.T) {
	a, r := fixture()
	for _, rootless := range []bool{true, false} {
		unit, env, err := render(a, r, true, rootless, "/private/app.env")
		if err != nil {
			t.Fatal(err)
		}
		target := "multi-user.target"
		if rootless {
			target = "default.target"
		}
		if !strings.Contains(string(unit), "WantedBy="+target) {
			t.Fatal("startup enabled for the wrong systemd target")
		}
		if strings.Contains(string(unit), "MESSAGE=") {
			t.Fatal("environment leaked into unit")
		}
		if !strings.Contains(string(env), `MESSAGE="space % $ quote\" slash\\"`) {
			t.Fatalf("environment escaping: %s", env)
		}
		disabled, _, err := render(a, r, false, rootless, "/private/app.env")
		if err != nil || strings.Contains(string(disabled), "[Install]") {
			t.Fatalf("disabled unit: %s, %v", disabled, err)
		}
	}
}

func TestRejectConsumerVisibleInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		change func(*deploy.Application, *deploy.Release, *deploy.RuntimeConfig)
	}{
		{"path identity", func(a *deploy.Application, _ *deploy.Release, _ *deploy.RuntimeConfig) { a.ID = "../escape" }},
		{"release identity", func(_ *deploy.Application, r *deploy.Release, _ *deploy.RuntimeConfig) { r.ID = "latest" }},
		{"mutable image", func(_ *deploy.Application, r *deploy.Release, _ *deploy.RuntimeConfig) { r.ImageID = "web:latest" }},
		{"env directive", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) {
			c.Env["MESSAGE"] = "ok\n[Service]\nExecStart=bad"
		}},
		{"env name", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) { c.Env["X=bad"] = "value" }},
		{"argv control", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) {
			c.Command = []string{"x\x00y"}
		}},
		{"description injection", func(a *deploy.Application, _ *deploy.Release, _ *deploy.RuntimeConfig) { a.Name = "x\n[Install]" }},
		{"port options", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) {
			c.Ports[0].Protocol = "tcp\nBad=yes"
		}},
		{"hostname", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) { c.Ports[0].IP = "localhost" }},
		{"zero port", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) { c.Ports[0].Host = 0 }},
		{"volume options", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) {
			c.Volumes[0].Target = "/data:rw"
		}},
		{"secret options", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) {
			c.Secrets[0].Name = "token,type=mount"
		}},
		{"secret type", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) { c.Secrets[0].Type = "other" }},
		{"network options", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) {
			c.Networks = []string{"net:ip=1.2.3.4"}
		}},
		{"restart directive", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) { c.RestartPolicy = "something" }},
		{"negative limit", func(_ *deploy.Application, _ *deploy.Release, c *deploy.RuntimeConfig) { c.MemoryBytes = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, r := fixture()
			cfg := a.Config()
			tc.change(&a, &r, &cfg)
			a.Profiles[a.Profile] = cfg
			if _, _, err := render(a, r, true, true, "/tmp/env"); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	base := t.TempDir()
	m := New(true)
	m.platform, m.uid, m.configErr = "linux", 1000, nil
	m.dir = filepath.Join(base, "units")
	m.systemdDir = filepath.Join(base, "systemd")
	if err := os.Mkdir(m.systemdDir, 0700); err != nil {
		t.Fatal(err)
	}
	m.cgroupFile = filepath.Join(base, "cgroup.controllers")
	if err := os.WriteFile(m.cgroupFile, []byte("cpu memory"), 0600); err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(base, "generator")
	if err := os.WriteFile(gen, []byte("not executed"), 0700); err != nil {
		t.Fatal(err)
	}
	m.generators, m.systemctl = []string{gen}, "test-systemctl"
	m.run = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("unexpected command")
	}
	return m
}

func seed(t *testing.T, m *Manager) (deploy.Application, deploy.Release) {
	t.Helper()
	a, r := fixture()
	unlock, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	u, e, j := m.names(a.ID)
	unit, env, err := render(a, r, true, true, e)
	if err != nil {
		t.Fatal(err)
	}
	md, err := json.Marshal(metadata{Version: 1, Application: a, Release: r, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{u: unit, e: env, j: md} {
		if err := atomicWrite(path, data); err != nil {
			t.Fatal(err)
		}
	}
	return a, r
}

func TestUnsupportedHosts(t *testing.T) {
	for _, kind := range []string{"remote", "non-linux", "no-systemd", "cgroup-v1", "no-generator"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(t)
			switch kind {
			case "remote":
				m.local = false
			case "non-linux":
				m.platform = "darwin"
			case "no-systemd":
				m.systemdDir += "-absent"
			case "cgroup-v1":
				m.cgroupFile += "-absent"
			case "no-generator":
				m.generators = nil
			}
			a, r := fixture()
			if err := m.Install(context.Background(), a, r, true); err == nil {
				t.Fatal("unsupported host accepted")
			}
			if _, err := os.Stat(m.dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsupported host modified files")
			}
		})
	}
}

func TestUnsafeOwnership(t *testing.T) {
	for _, kind := range []string{"unmanaged-unit", "unit-symlink", "env-symlink", "metadata-owner", "public-env"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(t)
			a, _ := seed(t, m)
			u, e, j := m.names(a.ID)
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unmanaged-unit":
				if err := os.WriteFile(u, []byte("[Container]\nImage=other"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unit-symlink", "env-symlink":
				target := u
				if kind == "env-symlink" {
					target = e
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, target); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "metadata-owner":
				if err := os.WriteFile(j, []byte(`{"Version":1,"Application":{"ID":"other"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "public-env":
				if err := os.Chmod(e, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Remove(context.Background(), a); err == nil {
				t.Fatal("unsafe files accepted")
			}
			got, err := os.ReadFile(victim)
			if err != nil || string(got) != "untouched" {
				t.Fatal("victim modified")
			}
		})
	}
}

func TestDisableAndRemoveOnlyOwnedFiles(t *testing.T) {
	m := testManager(t)
	a, _ := seed(t, m)
	calls := 0
	m.run = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		calls++
		if name == m.generators[0] {
			if !reflect.DeepEqual(args, []string{"--dryrun", "--user"}) || len(env) != 1 {
				t.Fatalf("preflight args %v env %v", args, env)
			}
			dir := strings.TrimPrefix(env[0], "QUADLET_UNIT_DIRS=")
			unit, err := os.ReadFile(filepath.Join(dir, "ptui-"+a.ID+".container"))
			if err != nil || strings.Contains(string(unit), "[Install]") {
				t.Fatalf("preflight unit: %s %v", unit, err)
			}
			return []byte("---ptui-" + a.ID + ".service---\n[Service]"), nil
		}
		if !reflect.DeepEqual(args, []string{"--user", "daemon-reload"}) {
			t.Fatalf("unexpected systemctl action %v", args)
		}
		return nil, nil
	}
	if err := m.Disable(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	u, e, j := m.names(a.ID)
	unit, err := os.ReadFile(u)
	if err != nil || strings.Contains(string(unit), "[Install]") {
		t.Fatalf("disabled unit: %s %v", unit, err)
	}
	other := filepath.Join(m.dir, "other.container")
	if err := os.WriteFile(other, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{u, e, j} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned file remains: %s", path)
		}
	}
	if got, err := os.ReadFile(other); err != nil || string(got) != "other" {
		t.Fatal("unrelated unit changed")
	}
	if calls != 3 {
		t.Fatalf("got %d commands", calls)
	}
}

func TestReloadFailureRestoresFilesWithIndependentContext(t *testing.T) {
	m := testManager(t)
	a, _ := seed(t, m)
	before, _, err := m.snapshot(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	m.run = func(ctx context.Context, _ string, args, _ []string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"--user", "daemon-reload"}) {
			t.Fatalf("unexpected command %v", args)
		}
		calls++
		if calls == 1 {
			cancel()
			return nil, errors.New("reload failed")
		}
		if ctx.Err() != nil {
			t.Fatal("rollback inherited cancellation")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("rollback must be bounded")
		}
		return nil, nil
	}
	after := [][]byte{[]byte("new env"), []byte("new metadata"), []byte("new unit")}
	if err := m.commit(ctx, m.systemctl, before, after, false); err == nil {
		t.Fatal("reload failure lost")
	}
	for _, file := range before {
		data, err := os.ReadFile(file.path)
		if err != nil || string(data) != string(file.data) {
			t.Fatalf("not restored: %s: %v", file.path, err)
		}
		fi, err := os.Stat(file.path)
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatal("private permissions lost")
		}
	}
	if calls != 2 {
		t.Fatalf("reload count %d", calls)
	}
}

func TestGeneratorMustActuallyProduceUnit(t *testing.T) {
	m := testManager(t)
	a, r := fixture()
	_, e, _ := m.names(a.ID)
	unit, _, err := render(a, r, true, true, e)
	if err != nil {
		t.Fatal(err)
	}
	m.run = func(context.Context, string, []string, []string) ([]byte, error) {
		return []byte("warning: unsupported key; skipping unit"), nil
	}
	if err := m.preflight(context.Background(), m.generators[0], a.ID, unit); err == nil {
		t.Fatal("silent generator failure accepted")
	}
}
