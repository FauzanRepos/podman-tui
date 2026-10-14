package git

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/podman-tui/config"
	"github.com/containers/podman-tui/deploy"
	podmanRuntime "github.com/containers/podman-tui/deploy/podman"
	"github.com/containers/podman-tui/pdcs/registry"
	"go.podman.io/podman/v6/pkg/bindings"
	"go.podman.io/podman/v6/pkg/bindings/images"
)

func archiveFixture(t *testing.T, headers []*tar.Header, contents []string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for i, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(w, contents[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestArchiveRejectsTraversalAndSymlinkWrites(t *testing.T) {
	fixtures := [][]*tar.Header{
		{{Name: "../outside", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "directory"}, {Name: "link/file", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: ".git/config", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "nested/.git/config", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: ".Git/config", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "missing"}},
		{{Name: "one", Typeflag: tar.TypeSymlink, Linkname: "two"}, {Name: "two", Typeflag: tar.TypeSymlink, Linkname: "one"}},
	}
	for _, headers := range fixtures {
		contents := make([]string, len(headers))
		for i := range contents {
			contents[i] = "x"
		}
		if err := extract(context.Background(), bytes.NewReader(archiveFixture(t, headers, contents)), t.TempDir()); err == nil {
			t.Fatal("unsafe archive accepted", headers)
		}
	}
}

type repositoryFixture struct {
	t                                      *testing.T
	git, root, work, repo, remote, initial string
}

func (f *repositoryFixture) invoke(args ...string) string {
	f.t.Helper()
	cmd := exec.Command(f.git, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "user.name=Isolated fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("Git fixture failed: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *repositoryFixture) write(name, content string, mode os.FileMode) {
	f.t.Helper()
	file := filepath.Join(f.work, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *repositoryFixture) publish(message string) string {
	f.t.Helper()
	f.invoke("-C", f.work, "add", "--all")
	f.invoke("-C", f.work, "commit", "-m", message)
	commit := f.invoke("-C", f.work, "rev-parse", "HEAD")
	f.invoke("-C", f.work, "push", f.repo, "main")
	return commit
}

func fixtureRepository(t *testing.T, authenticated ...bool) *repositoryFixture {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git CLI unavailable")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	// Isolate fixture creation too: user templates, signing and hooks must not run.
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	f := &repositoryFixture{t: t, git: gitPath, root: root, work: filepath.Join(root, "work"), repo: filepath.Join(root, "repository.git")}
	f.invoke("init", "--initial-branch=main", f.work)
	f.write("Containerfile", "FROM docker.io/library/busybox:1.37\nCOPY . /app/\nUSER 65534:65534\nCMD [\"sh\",\"-c\",\"test -r /app/value.txt && /app/bin/check && sleep 300\"]\n", 0600)
	f.write("value.txt", "known source revision\n", 0600)
	f.write("bin/check", "#!/bin/sh\ntest -r /app/value.txt && test -x /app/bin/check\n", 0700)
	f.invoke("-C", f.work, "add", "--all")
	f.invoke("-C", f.work, "commit", "-m", "fixture")
	f.initial = f.invoke("-C", f.work, "rev-parse", "HEAD")
	f.invoke("-C", f.work, "tag", "-a", "v1", "-m", "annotated immutable fixture")
	f.invoke("clone", "--bare", f.work, f.repo)
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(authenticated) != 0 && authenticated[0] {
			user, password, ok := r.BasicAuth()
			if !ok || user != "fixture" || password != "isolated-credential" {
				w.Header().Set("WWW-Authenticate", `Basic realm="Git fixture"`)
				http.Error(w, "configured credential helper required", http.StatusUnauthorized)
				return
			}
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	cert := filepath.Join(root, "git-ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[http]\n\tsslCAInfo = "+cert+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.remote = server.URL + "/repository.git"
	return f
}

func privateWorkdir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func retrieveFixture(t *testing.T, f *repositoryFixture, revision string) deploy.SourceTree {
	t.Helper()
	root := privateWorkdir(t)
	tree, err := New().Retrieve(context.Background(), deploy.Application{Repository: &deploy.GitConfig{URL: f.remote, Revision: revision, Trusted: true}}, root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func assertSource(t *testing.T, tree deploy.SourceTree, commit, content string) {
	t.Helper()
	if tree.Commit != commit {
		t.Fatalf("wrong immutable commit: got %s want %s", tree.Commit, commit)
	}
	value, err := os.ReadFile(filepath.Join(tree.Directory, "value.txt"))
	if err != nil || string(value) != content {
		t.Fatalf("incorrect retrieved source: %q %v", value, err)
	}
	if tree.Containerfile != filepath.Join(tree.Directory, "Containerfile") {
		t.Fatal("root Containerfile was not discovered")
	}
	if _, err = os.Lstat(filepath.Join(tree.Directory, ".git")); !os.IsNotExist(err) {
		t.Fatal("Git metadata leaked into build context", err)
	}
	for name, mode := range map[string]os.FileMode{".": 0755, "bin": 0755, "bin/check": 0755, "value.txt": 0644, "Containerfile": 0644} {
		info, err := os.Stat(filepath.Join(tree.Directory, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unsafe COPY/nonroot permissions for %s: %v %v", name, info, err)
		}
	}
	parent, err := os.Stat(filepath.Dir(tree.Directory))
	if err != nil || parent.Mode().Perm()&0077 != 0 {
		t.Fatal("host source workdir is not private", err)
	}
}

func TestTrustedGitBranchesTagsCommitsAndUpdates(t *testing.T) {
	f := fixtureRepository(t)
	first := retrieveFixture(t, f, "main")
	assertSource(t, first, f.initial, "known source revision\n")
	f.write("value.txt", "updated source revision\n", 0600)
	updated := f.publish("update source")
	for _, revision := range []string{"main", "refs/heads/main", "HEAD", updated} {
		t.Run(revision, func(t *testing.T) {
			assertSource(t, retrieveFixture(t, f, revision), updated, "updated source revision\n")
		})
	}
	for _, revision := range []string{"v1", "refs/tags/v1", f.initial} {
		t.Run(revision, func(t *testing.T) {
			assertSource(t, retrieveFixture(t, f, revision), f.initial, "known source revision\n")
		})
	}
	assertSource(t, first, f.initial, "known source revision\n")
}

func TestGitErrorsDoNotAlterRetrievedSource(t *testing.T) {
	f := fixtureRepository(t)
	good := retrieveFixture(t, f, "main")
	for _, revision := range []string{"missing-branch", "main:refs/heads/injected", "*", "+main", "main~1", "main^", "main..other", "@{1}", "-main"} {
		app := deploy.Application{Repository: &deploy.GitConfig{URL: f.remote, Revision: revision, Trusted: true}}
		if _, err := New().Retrieve(context.Background(), app, privateWorkdir(t), io.Discard); err == nil {
			t.Fatalf("invalid/missing revision %q accepted", revision)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app := deploy.Application{Repository: &deploy.GitConfig{URL: f.remote, Revision: "main", Trusted: true}}
	if _, err := New().Retrieve(ctx, app, privateWorkdir(t), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retrieval error = %v", err)
	}
	app.Repository.Trusted = false
	if _, err := New().Retrieve(context.Background(), app, privateWorkdir(t), io.Discard); err == nil {
		t.Fatal("untrusted build instructions accepted")
	}
	assertSource(t, good, f.initial, "known source revision\n")
}

func TestGitRootContainerfileDiscovery(t *testing.T) {
	f := fixtureRepository(t)
	if err := os.Rename(filepath.Join(f.work, "Containerfile"), filepath.Join(f.work, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	f.publish("Dockerfile fallback")
	tree := retrieveFixture(t, f, "main")
	if tree.Containerfile != filepath.Join(tree.Directory, "Dockerfile") {
		t.Fatal("root Dockerfile fallback not discovered")
	}
	if err := os.Rename(filepath.Join(f.work, "Dockerfile"), filepath.Join(f.work, "bin", "Containerfile")); err != nil {
		t.Fatal(err)
	}
	f.publish("nested build instructions only")
	app := deploy.Application{Repository: &deploy.GitConfig{URL: f.remote, Revision: "main", Trusted: true}}
	if _, err := New().Retrieve(context.Background(), app, privateWorkdir(t), io.Discard); err == nil {
		t.Fatal("nested Containerfile incorrectly accepted", err)
	}
	if err := os.Symlink("bin/Containerfile", filepath.Join(f.work, "Containerfile")); err != nil {
		t.Fatal(err)
	}
	f.publish("symlink build instructions")
	if _, err := New().Retrieve(context.Background(), app, privateWorkdir(t), io.Discard); err == nil {
		t.Fatal("symlink Containerfile incorrectly accepted", err)
	}
}

func TestGitArchiveNeverRunsConfiguredHooksFiltersOrSubmodules(t *testing.T) {
	f := fixtureRepository(t)
	marker := filepath.Join(f.root, "executed")
	hooks := filepath.Join(f.root, "unsafe-hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.write(".gitattributes", "value.txt filter=unsafe\nContainerfile export-ignore\n", 0600)
	f.write("bin/.gitattributes", "check filter=unsafe\n", 0600)
	f.write(".gitmodules", "[submodule \"dependency\"]\n\tpath = dependency\n\turl = "+f.remote+"\n", 0600)
	f.invoke("-C", f.work, "add", "--all")
	f.invoke("-C", f.work, "update-index", "--add", "--cacheinfo", "160000,"+f.initial+",dependency")
	f.invoke("-C", f.work, "commit", "-m", "filters and submodule")
	commit := f.invoke("-C", f.work, "rev-parse", "HEAD")
	f.invoke("-C", f.work, "push", f.repo, "main")
	f.invoke("config", "--global", "core.hooksPath", hooks)
	f.invoke("config", "--global", "filter.unsafe.smudge", "touch '"+marker+"'")
	f.invoke("config", "--global", "filter.unsafe.required", "true")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	tree := retrieveFixture(t, f, "main")
	assertSource(t, tree, commit, "known source revision\n")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("configured repository code executed", err)
	}
	if _, err := os.Stat(filepath.Join(tree.Directory, "dependency", "Containerfile")); !os.IsNotExist(err) {
		t.Fatal("submodule was downloaded", err)
	}
	// A long-running process filter takes precedence over smudge. It must
	// remain disabled too, including for files beneath nested attributes.
	f.invoke("config", "--global", "filter.unsafe.process", "touch '"+marker+"'")
	tree = retrieveFixture(t, f, "main")
	assertSource(t, tree, commit, "known source revision\n")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("configured process filter executed", err)
	}
}

func TestGitPreservesConfiguredCredentialHelper(t *testing.T) {
	f := fixtureRepository(t, true)
	app := deploy.Application{Repository: &deploy.GitConfig{URL: f.remote, Revision: "main", Trusted: true}}
	if _, err := New().Retrieve(context.Background(), app, privateWorkdir(t), io.Discard); err == nil {
		t.Fatal("authenticated Git retrieval succeeded without credentials")
	}
	helper := filepath.Join(f.root, "git-credential-fixture")
	// This is a real Git credential helper speaking its documented protocol,
	// not a replacement Git executable or simulated retrieval command.
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ncase \"$1\" in\nget) printf 'username=fixture\\npassword=isolated-credential\\n\\n' ;;\nesac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.invoke("config", "--global", "credential.helper", "!\""+helper+"\"")
	var progress bytes.Buffer
	tree, err := New().Retrieve(context.Background(), app, privateWorkdir(t), &progress)
	if err != nil {
		t.Fatal(err)
	}
	assertSource(t, tree, f.initial, "known source revision\n")
	if strings.Contains(progress.String(), "isolated-credential") {
		t.Fatal("Git credentials leaked into progress")
	}
}

func TestGitCancellationDuringTLSFetchPreservesSource(t *testing.T) {
	f := fixtureRepository(t)
	good := retrieveFixture(t, f, "main")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	cert := filepath.Join(f.root, "blocking-ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f.invoke("config", "--global", "http.sslCAInfo", cert)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := privateWorkdir(t)
	result := make(chan error, 1)
	go func() {
		_, err := New().Retrieve(ctx, deploy.Application{Repository: &deploy.GitConfig{URL: server.URL + "/blocked.git", Revision: "main", Trusted: true}}, work, io.Discard)
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("fetch failed before reaching TLS endpoint: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Git fetch did not reach TLS endpoint")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Git fetch did not stop after cancellation")
	}
	assertSource(t, good, f.initial, "known source revision\n")
}

// Opt-in on a dedicated disposable connection only. All deletions use this
// application's recorded container IDs and freshly built immutable image IDs.
func TestRealGitBuildDeploymentUpdateAndRollback(t *testing.T) {
	name := os.Getenv("PODMAN_TUI_TEST_CONNECTION")
	if name == "" {
		t.Skip("set PODMAN_TUI_TEST_CONNECTION to a dedicated disposable Podman v6 host")
	}
	// Resolve AND connect before the Git fixture changes HOME and XDG_CONFIG_HOME.
	cfg, err := config.NewPodmanRemoteConfig()
	if err != nil {
		t.Fatal(err)
	}
	var selected registry.Connection
	for _, connection := range cfg.RemoteConnections() {
		if connection.Name == name {
			selected = connection
			break
		}
	}
	if selected.URI == "" {
		t.Fatalf("disposable Podman connection %q is not configured", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	base, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	stopConnectCancellation := context.AfterFunc(ctx, disconnect)
	conn, err := bindings.NewConnectionWithIdentity(base, selected.URI, selected.Identity, false)
	stopConnectCancellation()
	if err != nil {
		t.Fatal(err)
	}
	if bindings.ServiceVersion(conn).Major != 6 {
		t.Fatal("integration test requires Podman v6")
	}
	f := fixtureRepository(t)
	// Make the built content unique, not just the tag: image-ID cleanup must
	// never untag a byte-identical image belonging to another test/application.
	token, err := deploy.NewID()
	if err != nil {
		t.Fatal(err)
	}
	f.write("ownership.txt", token+"\n", 0600)
	f.initial = f.publish("unique disposable build ownership")
	store, err := deploy.NewStore(filepath.Join(t.TempDir(), "state"), selected.URI)
	if err != nil {
		t.Fatal(err)
	}
	adapter := podmanRuntime.New(conn)
	m := deploy.NewManager(store, adapter, New(), nil)
	app, err := m.Create(deploy.Application{Name: "git-source-integration", Repository: &deploy.GitConfig{URL: f.remote, Revision: "main", Trusted: true}, Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": {}}, Readiness: deploy.Readiness{Mode: "running", TimeoutSeconds: 3}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		history, historyErr := m.History(app.Name)
		if historyErr != nil {
			t.Error(historyErr)
			return
		}
		if err := m.Delete(cleanup, app.Name, true); err != nil {
			t.Error("owned deployment cleanup", err)
			return
		}
		seen := make(map[string]bool)
		for _, release := range history {
			if release.ImageID == "" || seen[release.ImageID] {
				continue
			}
			seen[release.ImageID] = true
			imageCtx, imageCancel := context.WithTimeout(conn, 30*time.Second)
			_, errs := images.Remove(imageCtx, []string{release.ImageID}, new(images.RemoveOptions).WithNoPrune(true))
			imageCancel()
			for _, err := range errs {
				t.Error("owned image cleanup", err)
			}
		}
	}()
	first, err := m.Deploy(ctx, app.Name, deploy.DeployOptions{}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if first.Commit != f.initial || first.ImageID == "" {
		t.Fatal("initial Git commit/image identity was not retained")
	}
	f.write("value.txt", "updated source revision\n", 0600)
	updated := f.publish("new deployed revision")
	second, err := m.Deploy(ctx, app.Name, deploy.DeployOptions{Rebuild: true}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second.Commit != updated || second.ImageID == first.ImageID {
		t.Fatal("updated Git source did not produce a new immutable image")
	}
	assertCurrent := func(release deploy.Release) {
		t.Helper()
		current, err := m.Get(app.Name)
		if err != nil || current.CurrentRelease != release.ID {
			t.Fatal("known-good current release changed", err)
		}
		status, err := m.Status(ctx, app.Name)
		if err != nil || !status.Running || status.ImageID != release.ImageID {
			t.Fatal("known-good container was changed or stopped", err)
		}
	}
	assertCurrent(second)
	desired, err := m.Get(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	desired.Repository.Revision = "missing-branch"
	if err := m.Configure(app.Name, desired); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, app.Name, deploy.DeployOptions{Rebuild: true}, nil, io.Discard); err == nil {
		t.Fatal("nonexistent Git revision deployed")
	}
	assertCurrent(second)
	desired.Repository.Revision = "main"
	if err := m.Configure(app.Name, desired); err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := m.Deploy(canceled, app.Name, deploy.DeployOptions{Rebuild: true}, nil, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled deployment did not return context cancellation", err)
	}
	assertCurrent(second)
	rolled, err := m.Deploy(ctx, app.Name, deploy.DeployOptions{RollbackRelease: first.ID}, nil, io.Discard)
	if err != nil || rolled.Commit != first.Commit || rolled.ImageID != first.ImageID {
		t.Fatal("rollback did not retain immutable Git/image identity", err)
	}
	assertCurrent(rolled)
	history, err := m.History(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	var retainedFirst, retainedSecond, failedRevision bool
	for _, release := range history {
		retainedFirst = retainedFirst || release.ID == first.ID && release.Commit == f.initial && release.ImageID == first.ImageID
		retainedSecond = retainedSecond || release.ID == second.ID && release.Commit == updated && release.ImageID == second.ImageID
		failedRevision = failedRevision || release.Status == "failed" && release.Snapshot.Repository.Revision == "missing-branch"
	}
	if !retainedFirst || !retainedSecond || !failedRevision {
		t.Fatal("immutable Git/image history or failed revision was lost")
	}
}
