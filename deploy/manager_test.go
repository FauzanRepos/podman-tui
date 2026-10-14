package deploy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRuntime struct {
	fail     bool
	failedID string
	removed  []string
	running  map[string]bool
}

func (f *fakeRuntime) ResolveImage(context.Context, string, io.Writer) (string, error) {
	return "sha256:immutable", nil
}
func (f *fakeRuntime) Build(context.Context, SourceTree, string, io.Writer) (string, error) {
	return "sha256:built", nil
}
func (f *fakeRuntime) Create(_ context.Context, _ Application, r Release) (Candidate, error) {
	if f.fail {
		f.failedID = r.ID
	}
	return Candidate{ID: r.ID}, nil
}
func (f *fakeRuntime) PinImage(_ context.Context, _ Application, r Release) (string, error) {
	return "hold-" + r.ID, nil
}
func (f *fakeRuntime) FindImagePin(_ context.Context, _ Application, r Release) (string, error) {
	id := "hold-" + r.ID
	if _, exists := f.running[id]; exists {
		return id, nil
	}
	return "", ErrNotFound
}
func (f *fakeRuntime) Endpoint(context.Context, Application, string) (string, error) {
	return "", nil
}
func (f *fakeRuntime) Find(_ context.Context, _ Application, id string) (Candidate, error) {
	if _, exists := f.running[id]; exists {
		return Candidate{ID: id}, nil
	}
	return Candidate{}, ErrNotFound
}
func (f *fakeRuntime) Inspect(_ context.Context, _ Application, id string) (RuntimeStatus, error) {
	if f.fail && id == f.failedID {
		return RuntimeStatus{}, errors.New("candidate failure")
	}
	return RuntimeStatus{Running: f.running[id], Health: "healthy"}, nil
}
func (f *fakeRuntime) Start(_ context.Context, _ Application, id string) error {
	f.running[id] = true
	return nil
}
func (f *fakeRuntime) Stop(_ context.Context, _ Application, id string) error {
	f.running[id] = false
	return nil
}
func (f *fakeRuntime) Remove(_ context.Context, _ Application, id string) error {
	f.removed = append(f.removed, id)
	delete(f.running, id)
	return nil
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "state"), "unix:///test")
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(store, &fakeRuntime{running: map[string]bool{}}, nil, nil)
}
func testApp(name string) Application {
	return Application{Name: name, Image: "example/image:latest", Profile: "production", Profiles: map[string]RuntimeConfig{"production": {}}, Readiness: Readiness{Mode: "running", TimeoutSeconds: 2}}
}

func TestApplicationPersistenceIsolationAndRename(t *testing.T) {
	m := testManager(t)
	a, err := m.Create(testApp("web"))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Rename("web", "api"); err != nil {
		t.Fatal(err)
	}
	second := NewManager(m.Store, m.Runtime, nil, nil)
	got, err := second.Get("api")
	if err != nil || got.ID != a.ID {
		t.Fatalf("identity did not survive restart/rename: %v", err)
	}
	other, err := NewStore(filepath.Dir(m.Store.Root), "unix:///other")
	if err != nil {
		t.Fatal(err)
	}
	state, err := other.Snapshot()
	if err != nil || len(state.Applications) != 0 {
		t.Fatal("connection state leaked", err)
	}
	if _, err = m.Create(testApp("api")); err == nil {
		t.Fatal("duplicate application allowed")
	}
	if _, err = m.Create(testApp("../outside")); err == nil {
		t.Fatal("path traversal allowed")
	}
	if err = m.Delete(context.Background(), "api", false); err == nil {
		t.Fatal("unconfirmed delete allowed")
	}
	if _, err = m.Get("api"); err != nil {
		t.Fatal("unconfirmed delete changed metadata")
	}
}

func TestFailedDeploymentKeepsKnownGoodAndRollback(t *testing.T) {
	m := testManager(t)
	_, err := m.Create(testApp("web"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Deploy(context.Background(), "web", DeployOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := m.Runtime.(*fakeRuntime)
	runtime.fail = true
	failed, err := m.Deploy(context.Background(), "web", DeployOptions{}, nil, nil)
	if err == nil {
		t.Fatal("failed candidate accepted")
	}
	a, _ := m.Get("web")
	if a.CurrentRelease != first.ID || !runtime.running[first.ContainerID] {
		t.Fatal("known-good deployment changed")
	}
	history, _ := m.History("web")
	if history[1].Status != "failed" || history[1].Health != "failed" {
		t.Fatal("failure not persisted")
	}
	if !strings.Contains(strings.Join(runtime.removed, ","), failed.ContainerID) {
		t.Fatal("failed candidate not removed")
	}
	runtime.fail = false
	rolled, err := m.Deploy(context.Background(), "web", DeployOptions{RollbackRelease: first.ID}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.ImageID != first.ImageID || rolled.Previous != first.ID || runtime.running[first.ContainerID] {
		t.Fatal("rollback image/history/retirement invariant failed")
	}
}

func TestStateLockAndUnsafeFiles(t *testing.T) {
	m := testManager(t)
	session, err := m.Store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Store.Begin(); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent state writer accepted", err)
	}
	session.Close()
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte(`{"version":1}`), 0600)
	if err = os.Symlink(outside, filepath.Join(m.Store.Root, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Store.Snapshot(); err == nil {
		t.Fatal("symlink state accepted")
	}
}

func TestUnsafeGitDomainsAndSecrets(t *testing.T) {
	for _, url := range []string{"https://user:password@git.example.invalid/repo", "file:///etc", "ext::sh -c cmd", "--upload-pack=evil"} {
		if ValidateGit(GitConfig{URL: url, Revision: "main"}) == nil {
			t.Fatal("unsafe Git URL accepted", url)
		}
	}
	for _, d := range []string{"evil;include.conf", "*.example.com", "bad\nexample.com", "a..com", "127.0.0.1"} {
		if ValidateDomain(d) == nil {
			t.Fatal("unsafe domain accepted", d)
		}
	}
	a := testApp("web")
	c := a.Config()
	c.Secrets = []SecretRef{{Name: "db", Type: "env", Target: "PASSWORD\n"}}
	a.Profiles[a.Profile] = c
	if a.Validate() == nil {
		t.Fatal("invalid secret reference accepted")
	}
}

func TestBoundedControlSafeOutput(t *testing.T) {
	var b LogBuffer
	b.Write([]byte(strings.Repeat("x", LogLimit*2) + "\x1b[2J\u009bsecret\x07"))
	s := b.String()
	if len(s) > LogLimit || strings.ContainsAny(s, "\x1b\x07\u009b") {
		t.Fatal("output limit/control boundary failed")
	}
	if !strings.HasSuffix(s, "[2Jsecret") {
		t.Fatal("printable output lost")
	}
}
