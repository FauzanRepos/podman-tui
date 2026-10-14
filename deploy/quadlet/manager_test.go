package quadlet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	if err := m.prepareEnvironment(a.ID, env); err != nil {
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
	m.run = func(ctx context.Context, name string, args, _ []string) ([]byte, error) {
		if name == m.generators[0] {
			return []byte("ptui-" + a.ID + ".service"), nil
		}
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
	if err := m.Disable(ctx, a); err == nil {
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

func TestInterruptedQuadletTransactionsRecoverBeforeGeneration(t *testing.T) {
	for _, operation := range []string{"install", "update", "remove"} {
		for boundary := range 4 {
			t.Run(fmt.Sprintf("%s/%d", operation, boundary), func(t *testing.T) {
				m := testManager(t)
				a, r := fixture()
				if operation != "install" {
					a, r = seed(t, m)
				}
				before, _, err := m.snapshot(a.ID)
				if err != nil {
					t.Fatal(err)
				}
				active := true
				reloads := 0
				m.run = func(_ context.Context, name string, args, _ []string) ([]byte, error) {
					if name == m.generators[0] {
						return []byte("ptui-" + a.ID + ".service"), nil
					}
					switch args[1] {
					case "daemon-reload":
						reloads++
						return nil, nil
					case "show":
						if !active {
							t.Fatal("healthy runtime was stopped")
						}
						return []byte("ActiveState=active\nSubState=running\nResult=success"), nil
					default:
						active = false
						t.Fatalf("recovery invoked runtime action %v", args)
						return nil, nil
					}
				}
				m.transactionStep = func(stage string, index int) {
					if stage == "commit" && index == boundary {
						panic("simulated process exit")
					}
				}
				func() {
					defer func() {
						if recover() == nil {
							t.Fatal("transaction did not reach interrupted boundary")
						}
					}()
					if operation == "remove" {
						if err := m.Remove(context.Background(), a); err != nil {
							t.Fatal(err)
						}
					} else {
						r.ID = strings.Repeat("d", 32)
						r.ImageID = "sha256:" + strings.Repeat("e", 64)
						cfg := a.Config()
						cfg.Env["MESSAGE"] = "updated"
						a.Profiles[a.Profile] = cfg
						if err := m.Install(context.Background(), a, r, false); err != nil {
							t.Fatal(err)
						}
					}
				}()
				m.transactionStep = nil
				status, err := m.Status(context.Background(), a)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "install" && status != "not-installed" {
					t.Fatalf("initial install was adopted: %s", status)
				}
				if operation != "install" && !strings.Contains(status, "ActiveState=active") {
					t.Fatalf("healthy runtime lost: %s", status)
				}
				for _, file := range before {
					got, exists, err := readPrivate(file.path)
					if err != nil || exists != file.exists || !bytes.Equal(got, file.data) {
						t.Fatalf("mixed recovered generation: %s: %v", file.path, err)
					}
				}
				if reloads != 1 {
					t.Fatalf("recovery reloads = %d", reloads)
				}
				if _, err := os.Lstat(m.journalPath()); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("completed journal retained: %v", err)
				}
			})
		}
	}
}

func interruptQuadlet(t *testing.T, m *Manager, stage string, boundary int, operation func() error) {
	t.Helper()
	m.transactionStep = func(current string, index int) {
		if current == stage && index == boundary {
			panic("simulated process exit")
		}
	}
	defer func() {
		m.transactionStep = nil
		if recovered := recover(); recovered != "simulated process exit" {
			t.Fatalf("expected interrupted transaction, got %v", recovered)
		}
	}()
	if err := operation(); err != nil {
		t.Fatal(err)
	}
}

func quadletCommands(t *testing.T, m *Manager, id string) *int {
	t.Helper()
	reloads := 0
	m.run = func(_ context.Context, name string, args, _ []string) ([]byte, error) {
		if name == m.generators[0] {
			return []byte("ptui-" + id + ".service"), nil
		}
		switch args[1] {
		case "daemon-reload":
			reloads++
			return nil, nil
		case "show":
			return []byte("ActiveState=active\nSubState=running\nResult=success"), nil
		default:
			t.Fatalf("unexpected runtime action %v", args)
			return nil, nil
		}
	}
	return &reloads
}

func TestQuadletRecoveryInterruptedAgainRemainsRetryable(t *testing.T) {
	for _, operation := range []string{"install", "update", "remove"} {
		for boundary := range 4 {
			t.Run(fmt.Sprintf("%s/%d", operation, boundary), func(t *testing.T) {
				m := testManager(t)
				a, r := fixture()
				if operation != "install" {
					a, r = seed(t, m)
				}
				before, _, err := m.snapshot(a.ID)
				if err != nil {
					t.Fatal(err)
				}
				quadletCommands(t, m, a.ID)
				interruptQuadlet(t, m, "commit", 3, func() error {
					if operation == "remove" {
						return m.Remove(context.Background(), a)
					}
					r.ID = strings.Repeat("d", 32)
					return m.Install(context.Background(), a, r, false)
				})
				stage, index := "recovery", boundary+1
				if boundary == 3 {
					stage, index = "recovery-reload", 0
				}
				interruptQuadlet(t, m, stage, index, func() error {
					_, err := m.Status(context.Background(), a)
					return err
				})
				installed, err := m.Installed(a)
				if err != nil || installed != (operation != "install") {
					t.Fatalf("retry recovery installed=%v: %v", installed, err)
				}
				for _, file := range before {
					data, exists, err := readPrivate(file.path)
					if err != nil || exists != file.exists || !bytes.Equal(data, file.data) {
						t.Fatalf("retry left mixed generation: %s: %v", file.path, err)
					}
				}
				if _, err := os.Lstat(m.journalPath()); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("recovered journal remains: %v", err)
				}
			})
		}
	}
}

func TestQuadletRecoveryReloadFailureRetriesBeforeObservation(t *testing.T) {
	m := testManager(t)
	a, r := seed(t, m)
	quadletCommands(t, m, a.ID)
	interruptQuadlet(t, m, "commit", 2, func() error {
		return m.Install(context.Background(), a, r, false)
	})
	m.run = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("systemd temporarily unavailable")
	}
	if _, err := m.Status(context.Background(), a); err == nil {
		t.Fatal("failed recovery reload reported successful observation")
	}
	if _, err := os.Lstat(m.journalPath()); err != nil {
		t.Fatalf("retry journal lost: %v", err)
	}
	reloads := quadletCommands(t, m, a.ID)
	active, err := m.Active(context.Background(), a)
	if err != nil || !active || *reloads != 1 {
		t.Fatalf("retry active=%v reloads=%d: %v", active, *reloads, err)
	}
}

func TestQuadletRecoveryRefusesChangedOrUnmanagedTargets(t *testing.T) {
	for _, kind := range []string{"unit", "env", "metadata", "same-owner-changed-unit", "target-symlink"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(t)
			a, r := seed(t, m)
			reloads := quadletCommands(t, m, a.ID)
			interruptQuadlet(t, m, "commit", 3, func() error {
				return m.Install(context.Background(), a, r, false)
			})
			unit, env, meta := m.names(a.ID)
			path, data := unit, []byte("unmanaged")
			switch kind {
			case "env":
				path = env
			case "metadata":
				path = meta
			case "same-owner-changed-unit":
				data = []byte(marker + a.ID + "\n[Container]\nImage=unmanaged")
			}
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "target-symlink" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			saved := make(map[string][]byte)
			for _, target := range []string{unit, env, meta, victim, m.journalPath()} {
				saved[target], _ = os.ReadFile(target)
			}
			if _, err := m.Status(context.Background(), a); err == nil {
				t.Fatal("recovery adopted changed/unmanaged target")
			}
			for target, expected := range saved {
				got, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(got, expected) {
					t.Fatalf("unsafe recovery modified %s: %v", target, err)
				}
			}
			if *reloads != 0 {
				t.Fatal("unsafe recovery reloaded systemd")
			}
		})
	}
}

func TestQuadletRecoveryRejectsUnsafeJournal(t *testing.T) {
	for _, kind := range []string{"public", "symlink", "oversized", "truncated", "path-traversal", "unowned-before", "unknown-fields"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(t)
			a, r := seed(t, m)
			reloads := quadletCommands(t, m, a.ID)
			interruptQuadlet(t, m, "commit", 2, func() error {
				return m.Install(context.Background(), a, r, false)
			})
			journal, err := m.readJournal()
			if err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "public":
				err = os.Chmod(m.journalPath(), 0644)
			case "symlink":
				if err = os.Remove(m.journalPath()); err == nil {
					err = os.Symlink(victim, m.journalPath())
				}
			case "oversized":
				err = os.Truncate(m.journalPath(), maxQuadletJournal+1)
			case "truncated":
				err = os.WriteFile(m.journalPath(), []byte("{"), 0600)
			case "path-traversal", "unowned-before":
				if kind == "path-traversal" {
					journal.ID = "../victim"
				} else {
					journal.Before[0].Data = []byte("unmanaged environment")
				}
				var data []byte
				data, err = json.Marshal(journal)
				if err == nil {
					err = os.WriteFile(m.journalPath(), data, 0600)
				}
			case "unknown-fields":
				err = os.WriteFile(m.journalPath(), []byte(`{"Version":1,"ID":"`+a.ID+`","Path":"../victim"}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			paths := m.transactionPaths(a.ID)
			saved := make(map[string][]byte)
			for _, path := range append(paths[:], victim) {
				saved[path], _ = os.ReadFile(path)
			}
			if err := m.Remove(context.Background(), a); err == nil {
				t.Fatal("mutation accepted unsafe journal")
			}
			for path, expected := range saved {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, expected) {
					t.Fatalf("unsafe journal modified %s: %v", path, err)
				}
			}
			if *reloads != 0 {
				t.Fatal("unsafe journal reloaded systemd")
			}
		})
	}
}

func TestQuadletPublicOperationsRecoverBeforeReadingOrMutating(t *testing.T) {
	for _, operation := range []string{"installed", "active", "status", "control", "refresh", "disable", "remove", "install"} {
		t.Run(operation, func(t *testing.T) {
			m := testManager(t)
			a, r := seed(t, m)
			before, _, err := m.snapshot(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			quadletCommands(t, m, a.ID)
			interruptQuadlet(t, m, "commit", 2, func() error {
				return m.Install(context.Background(), a, r, false)
			})
			recovered := false
			m.transactionStep = func(stage string, _ int) {
				if stage != "recovery-reload" {
					return
				}
				recovered = true
				for _, file := range before {
					data, exists, err := readPrivate(file.path)
					if err != nil || exists != file.exists || !bytes.Equal(data, file.data) {
						t.Fatalf("operation observed mixed generation: %s: %v", file.path, err)
					}
				}
			}
			if operation == "control" {
				m.run = func(_ context.Context, _ string, args, _ []string) ([]byte, error) {
					if args[1] == "start" {
						if !recovered {
							t.Fatal("runtime action occurred before recovery")
						}
						if _, err := os.Lstat(m.journalPath()); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("runtime action occurred before journal completion: %v", err)
						}
					} else if args[1] != "daemon-reload" {
						t.Fatalf("unexpected runtime action %v", args)
					}
					return nil, nil
				}
			}
			ctx := context.Background()
			switch operation {
			case "installed":
				_, err = m.Installed(a)
			case "active":
				_, err = m.Active(ctx, a)
			case "status":
				_, err = m.Status(ctx, a)
			case "control":
				err = m.Control(ctx, a, "start")
			case "refresh":
				err = m.Refresh(ctx, a, r)
			case "disable":
				err = m.Disable(ctx, a)
			case "remove":
				err = m.Remove(ctx, a)
			case "install":
				err = m.Install(ctx, a, r, true)
			}
			if err != nil || !recovered {
				t.Fatalf("operation did not complete recovery: %v", err)
			}
			if _, err := os.Lstat(m.journalPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("operation left transaction journal: %v", err)
			}
			_, md, err := m.snapshot(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "remove" {
				if md != nil {
					t.Fatal("remove retained installation")
				}
			} else if md == nil || md.Enabled != (operation != "disable") {
				t.Fatal("operation used interrupted metadata instead of the before generation")
			}
		})
	}
}

func TestInstalledWithoutJournalDoesNotRequireSystemd(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint(installed), func(t *testing.T) {
			m := testManager(t)
			a, _ := fixture()
			if installed {
				a, _ = seed(t, m)
			}
			m.systemdDir += "-absent"
			m.cgroupFile += "-absent"
			m.generators = nil
			got, err := m.Installed(a)
			if err != nil || got != installed {
				t.Fatalf("installed=%v, expected=%v: %v", got, installed, err)
			}
		})
	}
}

func seedLegacyQuadlet(t *testing.T, m *Manager) (deploy.Application, deploy.Release) {
	t.Helper()
	a, r := seed(t, m)
	unitPath, stable, _ := m.names(a.ID)
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(stable)
	if err != nil {
		t.Fatal(err)
	}
	generation := environmentGeneration(stable, env)
	unit = bytes.Replace(unit, []byte("EnvironmentFile="+quote(generation)+"\n"),
		[]byte("EnvironmentFile="+quote(stable)+"\n"), 1)
	if err := atomicWrite(unitPath, unit); err != nil {
		t.Fatal(err)
	}
	if err := durableRemove(generation); err != nil {
		t.Fatal(err)
	}
	return a, r
}

// Inspect exactly what a Quadlet generator sees before any CLI recovery: the
// published image and the actual bytes at its referenced EnvironmentFile.
func assertQuadletBootPair(t *testing.T, m *Manager, id, image string, env []byte, absent, legacy bool) {
	t.Helper()
	unitPath, stable, _ := m.names(id)
	unit, err := os.ReadFile(unitPath)
	if absent {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("boot-visible unit should be absent: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(unit), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "Image" && key != "EnvironmentFile") {
			continue
		}
		decoded, err := strconv.Unquote(value)
		if err != nil {
			t.Fatal(err)
		}
		values[key] = strings.ReplaceAll(decoded, "%%", "%")
	}
	if values["Image"] != image {
		t.Fatalf("boot image=%q, expected=%q", values["Image"], image)
	}
	envPath := values["EnvironmentFile"]
	if (envPath == stable) != legacy || filepath.Dir(envPath) != filepath.Dir(stable) {
		t.Fatalf("incorrect boot environment reference %q, legacy=%v", envPath, legacy)
	}
	got, exists, err := readPrivate(envPath)
	if err != nil || !exists || !bytes.Equal(got, env) {
		t.Fatalf("boot image %s paired with wrong or unavailable environment: %v", image, err)
	}
}

func TestInterruptedQuadletBootUsesMatchingImageAndEnvironment(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		for _, operation := range []string{"install", "update", "remove"} {
			for _, boundary := range []int{-1, 0, 1, 2, 3} {
				t.Run(fmt.Sprintf("legacy=%v/%s/%d", legacy, operation, boundary), func(t *testing.T) {
					m := testManager(t)
					a, oldRelease := fixture()
					if operation != "install" {
						if legacy {
							a, oldRelease = seedLegacyQuadlet(t, m)
						} else {
							a, oldRelease = seed(t, m)
						}
					}
					before, _, err := m.snapshot(a.ID)
					if err != nil {
						t.Fatal(err)
					}
					r := oldRelease
					r.ID, r.ImageID = strings.Repeat("d", 32), "sha256:"+strings.Repeat("e", 64)
					cfg := a.Config()
					cfg.Env = map[string]string{"MESSAGE": "new release environment"}
					a.Profiles[a.Profile] = cfg
					_, stable, _ := m.names(a.ID)
					_, newEnv, err := render(a, r, false, true, stable)
					if err != nil {
						t.Fatal(err)
					}
					quadletCommands(t, m, a.ID)
					stage, checkpoint := "commit", boundary
					if boundary == -1 {
						stage, checkpoint = "journal", 0
					}
					interruptQuadlet(t, m, stage, checkpoint, func() error {
						if operation == "remove" {
							return m.Remove(context.Background(), a)
						}
						return m.Install(context.Background(), a, r, false)
					})
					absent := (operation == "install" && boundary < 2) || (operation == "remove" && boundary >= 1)
					if operation != "remove" && boundary >= 2 {
						assertQuadletBootPair(t, m, a.ID, r.ImageID, newEnv, false, false)
					} else {
						assertQuadletBootPair(t, m, a.ID, oldRelease.ImageID, before[0].data, absent, legacy)
					}
					if _, err := m.Status(context.Background(), a); err != nil {
						t.Fatal(err)
					}
					assertQuadletBootPair(t, m, a.ID, oldRelease.ImageID, before[0].data, operation == "install", legacy)
					if operation != "remove" {
						if _, exists, err := readPrivate(environmentGeneration(stable, newEnv)); err != nil || exists {
							t.Fatalf("unreferenced rollback environment retained: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestInterruptedQuadletRecoveryKeepsBootPairConsistent(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		for _, operation := range []string{"install", "update", "remove"} {
			for boundary := range 4 {
				t.Run(fmt.Sprintf("legacy=%v/%s/%d", legacy, operation, boundary), func(t *testing.T) {
					m := testManager(t)
					a, oldRelease := fixture()
					if operation != "install" {
						if legacy {
							a, oldRelease = seedLegacyQuadlet(t, m)
						} else {
							a, oldRelease = seed(t, m)
						}
					}
					before, _, err := m.snapshot(a.ID)
					if err != nil {
						t.Fatal(err)
					}
					r := oldRelease
					r.ID, r.ImageID = strings.Repeat("d", 32), "sha256:"+strings.Repeat("e", 64)
					cfg := a.Config()
					cfg.Env = map[string]string{"MESSAGE": "new release environment"}
					a.Profiles[a.Profile] = cfg
					_, stable, _ := m.names(a.ID)
					_, newEnv, err := render(a, r, false, true, stable)
					if err != nil {
						t.Fatal(err)
					}
					quadletCommands(t, m, a.ID)
					interruptQuadlet(t, m, "commit", 3, func() error {
						if operation == "remove" {
							return m.Remove(context.Background(), a)
						}
						return m.Install(context.Background(), a, r, false)
					})
					stage, checkpoint := "recovery", boundary+1
					if boundary == 3 {
						stage, checkpoint = "recovery-reload", 0
					}
					interruptQuadlet(t, m, stage, checkpoint, func() error {
						_, err := m.Status(context.Background(), a)
						return err
					})
					if boundary < 2 && operation != "remove" {
						assertQuadletBootPair(t, m, a.ID, r.ImageID, newEnv, false, false)
					} else {
						absent := (operation == "remove" && boundary < 2) || operation == "install"
						assertQuadletBootPair(t, m, a.ID, oldRelease.ImageID, before[0].data, absent, legacy)
					}
					if _, err := m.Status(context.Background(), a); err != nil {
						t.Fatal(err)
					}
					assertQuadletBootPair(t, m, a.ID, oldRelease.ImageID, before[0].data, operation == "install", legacy)
				})
			}
		}
	}
}

func TestQuadletImmutableEnvironmentOwnershipAndCleanup(t *testing.T) {
	m := testManager(t)
	a, oldRelease := seed(t, m)
	_, stable, _ := m.names(a.ID)
	oldEnv, err := os.ReadFile(stable)
	if err != nil {
		t.Fatal(err)
	}
	r := oldRelease
	r.ID, r.ImageID = strings.Repeat("d", 32), "sha256:"+strings.Repeat("e", 64)
	cfg := a.Config()
	cfg.Env = map[string]string{"MESSAGE": "new release environment"}
	a.Profiles[a.Profile] = cfg
	_, newEnv, err := render(a, r, true, true, stable)
	if err != nil {
		t.Fatal(err)
	}
	newPath := environmentGeneration(stable, newEnv)
	if err := os.WriteFile(newPath, []byte("unmanaged collision"), 0600); err != nil {
		t.Fatal(err)
	}
	quadletCommands(t, m, a.ID)
	if err := m.Install(context.Background(), a, r, true); err == nil {
		t.Fatal("immutable environment adopted unmanaged collision")
	}
	if got, err := os.ReadFile(newPath); err != nil || string(got) != "unmanaged collision" {
		t.Fatal("unmanaged collision overwritten or deleted")
	}
	assertQuadletBootPair(t, m, a.ID, oldRelease.ImageID, oldEnv, false, false)
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(filepath.Dir(stable), a.ID+"."+strings.Repeat("f", 64)+".env")
	if err := os.WriteFile(unrelated, []byte("unmanaged neighbor"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Install(context.Background(), a, r, true); err != nil {
		t.Fatal(err)
	}
	assertQuadletBootPair(t, m, a.ID, r.ImageID, newEnv, false, false)
	if _, exists, err := readPrivate(environmentGeneration(stable, oldEnv)); err != nil || exists {
		t.Fatalf("obsolete environment generation remains: %v", err)
	}
	if got, err := os.ReadFile(stable); err != nil || !bytes.Equal(got, newEnv) {
		t.Fatal("stable compatibility environment not updated")
	}
	if err := m.Remove(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readPrivate(newPath); err != nil || exists {
		t.Fatalf("removed unit environment remains: %v", err)
	}
	if got, err := os.ReadFile(unrelated); err != nil || string(got) != "unmanaged neighbor" {
		t.Fatal("unrelated generation-shaped file changed")
	}
}

func TestQuadletImmutableEnvironmentRejectsUnsafePublicationTargets(t *testing.T) {
	for _, kind := range []string{"symlink", "public-file"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(t)
			a, oldRelease := seed(t, m)
			_, stable, _ := m.names(a.ID)
			oldEnv, err := os.ReadFile(stable)
			if err != nil {
				t.Fatal(err)
			}
			r := oldRelease
			r.ID, r.ImageID = strings.Repeat("d", 32), "sha256:"+strings.Repeat("e", 64)
			cfg := a.Config()
			cfg.Env = map[string]string{"MESSAGE": "new release environment"}
			a.Profiles[a.Profile] = cfg
			_, env, err := render(a, r, true, true, stable)
			if err != nil {
				t.Fatal(err)
			}
			path := environmentGeneration(stable, env)
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				err = os.Symlink(victim, path)
			} else {
				err = os.WriteFile(path, env, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			quadletCommands(t, m, a.ID)
			if err := m.Install(context.Background(), a, r, true); err == nil {
				t.Fatal("unsafe immutable environment target adopted")
			}
			assertQuadletBootPair(t, m, a.ID, oldRelease.ImageID, oldEnv, false, false)
			if got, err := os.ReadFile(victim); err != nil || string(got) != "untouched" {
				t.Fatal("immutable environment publication modified symlink victim")
			}
			fi, err := os.Lstat(path)
			if err != nil || (kind == "symlink" && fi.Mode()&os.ModeSymlink == 0) ||
				(kind == "public-file" && fi.Mode().Perm() != 0644) {
				t.Fatal("unsafe publication target replaced or removed")
			}
			if installed, err := m.Installed(a); err != nil || !installed {
				t.Fatalf("unsafe publication lost prior healthy installation: %v", err)
			}
		})
	}
}
