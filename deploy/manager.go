package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

type Manager struct {
	Store       *Store
	Runtime     Runtime
	Source      Source
	Router      Router
	LocalPodman bool // true only for a Unix connection on the controller's Linux host
	Startup     Supervisor
}
type DeployOptions struct {
	RollbackRelease string
	Rebuild         bool
	Maintenance     bool
}

func NewManager(store *Store, runtime Runtime, source Source, router Router) *Manager {
	return &Manager{Store: store, Runtime: runtime, Source: source, Router: router}
}

func defaults(a *Application) {
	if a.Profile == "" {
		a.Profile = "production"
	}
	if a.Profiles == nil {
		a.Profiles = map[string]RuntimeConfig{a.Profile: {}}
	}
	if a.Readiness.Mode == "" {
		a.Readiness.Mode = "running"
	}
	if a.Readiness.TimeoutSeconds == 0 {
		a.Readiness.TimeoutSeconds = 30
	}
	if a.Retention == 0 {
		a.Retention = 3
	}
	if a.Kind == "" {
		a.Kind = "application"
	}
}

func appIndex(s State, name string) int {
	return slices.IndexFunc(s.Applications, func(a Application) bool { return a.Name == name })
}
func releaseIndex(s State, id string) int {
	return slices.IndexFunc(s.Releases, func(r Release) bool { return r.ID == id })
}

func checkConflicts(s State, a Application) error {
	for _, other := range s.Applications {
		if other.ID == a.ID {
			continue
		}
		if other.Name == a.Name {
			return errors.New("application name already exists")
		}
		for _, domain := range a.Domains {
			if slices.Contains(other.Domains, domain) {
				return fmt.Errorf("domain %s belongs to %s", domain, other.Name)
			}
		}
	}
	return nil
}

func (m *Manager) List() ([]Application, error) {
	s, err := m.Store.Snapshot()
	return s.Applications, err
}
func (m *Manager) Get(name string) (Application, error) {
	s, err := m.Store.Snapshot()
	if err != nil {
		return Application{}, err
	}
	i := appIndex(s, name)
	if i < 0 {
		return Application{}, ErrNotFound
	}
	return s.Applications[i], nil
}
func (m *Manager) History(name string) ([]Release, error) {
	s, err := m.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	i := appIndex(s, name)
	if i < 0 {
		return nil, ErrNotFound
	}
	out := make([]Release, 0)
	for _, r := range s.Releases {
		if r.ApplicationID == s.Applications[i].ID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *Manager) Create(a Application) (Application, error) {
	defaults(&a)
	if a.ID != "" || a.CurrentRelease != "" {
		return Application{}, errors.New("new application cannot supply managed identity or current release")
	}
	id, err := NewID()
	if err != nil {
		return Application{}, err
	}
	a.ID = id
	a.CreatedAt = time.Now().UTC()
	if err = a.Validate(); err != nil {
		return Application{}, err
	}
	session, err := m.Store.Begin()
	if err != nil {
		return Application{}, err
	}
	defer session.Close()
	if err = checkConflicts(session.State, a); err != nil {
		return Application{}, err
	}
	session.State.Applications = append(session.State.Applications, a)
	return a, session.Save()
}

// Configure writes desired configuration, not the running release snapshot. Apply with Deploy.
func (m *Manager) Configure(name string, a Application) error {
	defaults(&a)
	session, err := m.Store.Begin()
	if err != nil {
		return err
	}
	defer session.Close()
	i := appIndex(session.State, name)
	if i < 0 {
		return ErrNotFound
	}
	old := session.State.Applications[i]
	a.ID = old.ID
	a.CurrentRelease = old.CurrentRelease
	a.CreatedAt = old.CreatedAt
	if a.Name != old.Name {
		return errors.New("use Rename to change application name")
	}
	if err = a.Validate(); err != nil {
		return err
	}
	if err = checkConflicts(session.State, a); err != nil {
		return err
	}
	session.State.Applications[i] = a
	return session.Save()
}

func (m *Manager) Rename(name, newName string) error {
	if err := ValidateName(newName); err != nil {
		return err
	}
	session, err := m.Store.Begin()
	if err != nil {
		return err
	}
	defer session.Close()
	i := appIndex(session.State, name)
	if i < 0 {
		return ErrNotFound
	}
	a := session.State.Applications[i]
	a.Name = newName
	if err = checkConflicts(session.State, a); err != nil {
		return err
	}
	session.State.Applications[i] = a
	return session.Save()
}

func (m *Manager) Status(ctx context.Context, name string) (RuntimeStatus, error) {
	a, err := m.Get(name)
	if err != nil {
		return RuntimeStatus{}, err
	}
	s, err := m.Store.Snapshot()
	if err != nil {
		return RuntimeStatus{}, err
	}
	i := releaseIndex(s, a.CurrentRelease)
	if i < 0 {
		return RuntimeStatus{}, ErrNotFound
	}
	_, status, err := m.resolveCurrent(ctx, a, s.Releases[i])
	if errors.Is(err, ErrNotFound) && m.Startup != nil {
		installed, startupErr := m.Startup.Installed(a)
		if startupErr != nil {
			return RuntimeStatus{}, startupErr
		}
		if installed {
			return RuntimeStatus{Running: false, Health: "stopped; inspect the systemd unit for failures", ImageID: s.Releases[i].ImageID}, nil
		}
	}
	return status, err
}

func (m *Manager) Lifecycle(ctx context.Context, name, action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return errors.New("unsupported lifecycle action")
	}
	session, err := m.Store.Begin()
	if err != nil {
		return err
	}
	defer session.Close()
	ai := appIndex(session.State, name)
	if ai < 0 {
		return ErrNotFound
	}
	a := session.State.Applications[ai]
	ri := releaseIndex(session.State, a.CurrentRelease)
	if ri < 0 {
		return ErrNotFound
	}
	r := session.State.Releases[ri]
	candidate, _, lookupErr := m.resolveCurrent(ctx, a, r)
	if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
		return lookupErr
	}
	installed, supervised := false, false
	if m.Startup != nil {
		installed, err = m.Startup.Installed(a)
		if err != nil {
			return err
		}
		if installed {
			supervised, err = m.Startup.Active(ctx, a)
			if err != nil {
				return err
			}
		}
	}
	if supervised || (installed && errors.Is(lookupErr, ErrNotFound)) {
		if r.ImagePinID == "" {
			r.ImagePinID, err = m.Runtime.PinImage(ctx, r.Snapshot, r)
			if err != nil {
				return err
			}
			session.State.Releases[ri] = r
			if err = session.Save(); err != nil {
				return err
			}
		}
		if err = m.Startup.Control(ctx, a, action); err != nil {
			return err
		}
		if action == "stop" {
			return nil
		}
		candidate, err = m.Runtime.Find(ctx, a, r.ID)
		if err != nil {
			return err
		}
	} else {
		if errors.Is(lookupErr, ErrNotFound) {
			if action == "stop" {
				return nil
			}
			if _, err = m.Runtime.ResolveImage(ctx, r.ImageID, io.Discard); err != nil {
				return err
			}
			candidate, err = m.Runtime.Create(ctx, r.Snapshot, r)
			if err != nil {
				return err
			}
		}
		if action == "stop" {
			return m.Runtime.Stop(ctx, r.Snapshot, candidate.ID)
		}
		if action == "restart" {
			if err = m.Runtime.Stop(ctx, r.Snapshot, candidate.ID); err != nil {
				return err
			}
		}
		if err = m.Runtime.Start(ctx, r.Snapshot, candidate.ID); err != nil {
			return err
		}
	}
	candidate.Endpoint, err = m.Runtime.Endpoint(ctx, r.Snapshot, candidate.ID)
	if err != nil {
		return err
	}
	if err = m.waitReady(ctx, r.Snapshot, candidate); err != nil {
		return err
	}
	if r.Snapshot.Proxy != nil {
		if _, err = m.Router.Switch(ctx, r.Snapshot, candidate); err != nil {
			return err
		}
	}
	r.ContainerID, r.Endpoint = candidate.ID, candidate.Endpoint
	session.State.Releases[ri] = r
	return session.Save()
}

func (m *Manager) Delete(ctx context.Context, name string, confirmed bool) error {
	if !confirmed {
		return errors.New("application deletion requires explicit confirmation")
	}
	session, err := m.Store.Begin()
	if err != nil {
		return err
	}
	defer session.Close()
	i := appIndex(session.State, name)
	if i < 0 {
		return ErrNotFound
	}
	a := session.State.Applications[i]
	// Remove routing first. If any resource deletion fails, retain metadata for explicit retry.
	ri := releaseIndex(session.State, a.CurrentRelease)
	if ri >= 0 && session.State.Releases[ri].Snapshot.Proxy != nil {
		if m.Router == nil {
			return errors.New("proxy adapter required to remove active routing")
		}
		if err = m.Router.Remove(ctx, session.State.Releases[ri].Snapshot); err != nil {
			return err
		}
	}
	if m.Startup != nil {
		installed, startupErr := m.Startup.Installed(a)
		if startupErr != nil {
			return startupErr
		}
		if installed {
			if err = m.Startup.Disable(ctx, a); err != nil {
				return err
			}
			if err = m.Startup.Control(ctx, a, "stop"); err != nil {
				return err
			}
			if err = m.Startup.Remove(ctx, a); err != nil {
				return err
			}
		}
	}
	for _, release := range session.State.Releases {
		if release.ApplicationID != a.ID {
			continue
		}
		for _, id := range []string{release.ContainerID, release.ImagePinID} {
			if id != "" {
				if err = m.Runtime.Remove(ctx, a, id); err != nil {
					return err
				}
			}
		}
	}
	session.State.Applications = slices.Delete(session.State.Applications, i, i+1)
	session.State.Releases = slices.DeleteFunc(session.State.Releases, func(r Release) bool { return r.ApplicationID == a.ID })
	return session.Save()
}

func (m *Manager) Deploy(ctx context.Context, name string, opts DeployOptions, progress Progress, output io.Writer) (result Release, retErr error) {
	if output == nil {
		output = io.Discard
	}
	session, err := m.Store.Begin()
	if err != nil {
		return Release{}, err
	}
	defer session.Close()
	ai := appIndex(session.State, name)
	if ai < 0 {
		return Release{}, ErrNotFound
	}
	desired := session.State.Applications[ai]
	a := desired
	for _, r := range session.State.Releases {
		if r.ApplicationID == a.ID && r.Status == "deploying" {
			return Release{}, errors.New("interrupted deployment exists; run recover-abort before deploying")
		}
	}
	var old *Release
	if i := releaseIndex(session.State, a.CurrentRelease); i >= 0 {
		copy := session.State.Releases[i]
		old = &copy
	}
	startupInstalled, oldSupervised := false, false
	oldRunning := false
	if m.Startup != nil {
		startupInstalled, err = m.Startup.Installed(a)
		if err != nil {
			return Release{}, err
		}
		if startupInstalled {
			oldSupervised, err = m.Startup.Active(ctx, a)
			if err != nil {
				return Release{}, err
			}
		}
	}
	if oldSupervised && opts.Maintenance {
		return Release{}, errors.New("live Quadlet maintenance cannot retain its container: explicitly stop the application and remove its startup unit before maintenance deployment")
	}
	if old != nil {
		candidate, status, lookupErr := m.resolveCurrent(ctx, old.Snapshot, *old)
		oldRunning = lookupErr == nil && status.Running
		if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
			return Release{}, lookupErr
		}
		if errors.Is(lookupErr, ErrNotFound) {
			old.ContainerID = ""
		} else {
			old.ContainerID, old.Endpoint = candidate.ID, candidate.Endpoint
		}
	}
	var rollback *Release
	if opts.RollbackRelease != "" {
		i := releaseIndex(session.State, opts.RollbackRelease)
		if i < 0 || session.State.Releases[i].ApplicationID != a.ID || session.State.Releases[i].Status != "successful" {
			return Release{}, errors.New("rollback requires a retained successful release")
		}
		copy := session.State.Releases[i]
		rollback = &copy
		a = copy.Snapshot
		a.Name = desired.Name
		a.CurrentRelease = desired.CurrentRelease
		// Certificate renewal is operational state, not application code. Keep a
		// current certificate when it covers the rollback's domains and route.
		if a.Proxy != nil && old != nil && old.Snapshot.Proxy != nil {
			current := old.Snapshot.Proxy
			covered := current.Certificate != "" && current.PrivateKey != "" &&
				current.ManagedDir == a.Proxy.ManagedDir && current.ConfigFile == a.Proxy.ConfigFile
			for _, domain := range a.Domains {
				covered = covered && slices.Contains(old.Snapshot.Domains, domain)
			}
			if covered {
				proxy := *a.Proxy
				proxy.Certificate, proxy.PrivateKey = current.Certificate, current.PrivateKey
				a.Proxy = &proxy
			}
		}
	}
	if err = a.Validate(); err != nil {
		return Release{}, err
	}
	if err = checkConflicts(session.State, a); err != nil {
		return Release{}, err
	}
	if a.Proxy != nil && m.Router == nil {
		return Release{}, errors.New("proxy integration is not configured")
	}
	if old != nil && old.Snapshot.Proxy != nil {
		if m.Router == nil {
			return Release{}, errors.New("proxy adapter required to change active routing")
		}
		if a.Proxy != nil && (a.Proxy.ManagedDir != old.Snapshot.Proxy.ManagedDir || a.Proxy.ConfigFile != old.Snapshot.Proxy.ConfigFile || a.Proxy.Binary != old.Snapshot.Proxy.Binary) {
			return Release{}, errors.New("detach the active proxy before moving its managed directory, main configuration or binary")
		}
	}
	if old != nil && oldRunning && !opts.Maintenance {
		for _, p := range a.Config().Ports {
			if p.Host != 0 {
				return Release{}, errors.New("fixed host ports require explicitly authorized maintenance deployment")
			}
		}
		if !a.Config().ConcurrentSafe {
			for _, v := range a.Config().Volumes {
				if !v.ReadOnly {
					return Release{}, errors.New("overlapping persistent writers require concurrent_safe or maintenance deployment")
				}
			}
		}
	}
	if opts.Rebuild && a.Repository == nil {
		return Release{}, errors.New("rebuild requires a configured Git repository")
	}
	id, err := NewID()
	if err != nil {
		return Release{}, err
	}
	r := Release{ID: id, ApplicationID: a.ID, Timestamp: time.Now().UTC(), Status: "deploying", Stage: "validate", Previous: desired.CurrentRelease, Snapshot: a}
	session.State.Releases = append(session.State.Releases, r)
	ri := len(session.State.Releases) - 1
	saveStage := func(stage string) error {
		r.Stage = stage
		session.State.Releases[ri] = r
		if progress != nil {
			progress(Event{Stage: stage, Message: stage})
		}
		return session.Save()
	}
	if err = saveStage("retrieve"); err != nil {
		return r, err
	}
	var undo func(context.Context) error
	oldStopped := false
	committed := false
	startupTouched := false
	// Cancellation must not prevent recovery of a known-good release. Recovery has its own bound.
	defer func() {
		if retErr == nil || committed {
			return
		}
		recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		recoveryFailed := errors.Is(retErr, ErrRecoveryRequired)
		if startupTouched && old != nil && !recoveryFailed {
			if restoreErr := m.Startup.Refresh(recovery, old.Snapshot, *old); restoreErr != nil {
				retErr = errors.Join(retErr, restoreErr, ErrRecoveryRequired)
				recoveryFailed = true
			}
		}
		if oldStopped && oldSupervised && old != nil && !recoveryFailed {
			if startErr := m.Startup.Control(recovery, old.Snapshot, "start"); startErr != nil {
				retErr = errors.Join(retErr, startErr, ErrRecoveryRequired)
				recoveryFailed = true
			} else {
				restored, _, restoreErr := m.resolveCurrent(recovery, old.Snapshot, *old)
				if restoreErr == nil {
					restoreErr = m.waitReady(recovery, old.Snapshot, restored)
				}
				if restoreErr != nil {
					retErr = errors.Join(retErr, restoreErr, ErrRecoveryRequired)
					recoveryFailed = true
				} else {
					old.ContainerID, old.Endpoint = restored.ID, restored.Endpoint
					if index := releaseIndex(session.State, old.ID); index >= 0 {
						session.State.Releases[index] = *old
					}
					oldStopped = false
				}
			}
		}
		if undo != nil && !recoveryFailed {
			if undoErr := undo(recovery); undoErr != nil {
				retErr = errors.Join(retErr, undoErr, ErrRecoveryRequired)
				recoveryFailed = true
			}
		}
		if r.ContainerID != "" && !recoveryFailed {
			if removeErr := m.Runtime.Remove(recovery, a, r.ContainerID); removeErr != nil {
				retErr = errors.Join(retErr, removeErr, ErrRecoveryRequired)
				recoveryFailed = true
			}
		}
		if oldStopped && old != nil && !recoveryFailed {
			if startErr := m.Runtime.Start(recovery, old.Snapshot, old.ContainerID); startErr != nil {
				retErr = errors.Join(retErr, startErr, ErrRecoveryRequired)
				recoveryFailed = true
			}
		}
		r.Status = "failed"
		r.Health = "failed"
		if recoveryFailed {
			r.Status = "deploying"
			r.Stage = "recovery-required"
		}
		r.Error = Sanitize(retErr.Error())
		session.State.Releases[ri] = r
		retErr = errors.Join(retErr, session.Save())
		result = r
	}()
	if rollback != nil {
		r.ImageID = rollback.ImageID
		r.Commit = rollback.Commit
		r.Repository = rollback.Repository
		// A retained image ID is resolved locally by the runtime; never substitute a moving tag.
		r.ImageID, err = m.Runtime.ResolveImage(ctx, r.ImageID, output)
	} else if a.Repository != nil {
		if m.Source == nil {
			return r, errors.New("Git source integration is not configured")
		}
		dir, e := m.Store.SourceDir(r.ID)
		if e != nil {
			return r, e
		}
		defer os.RemoveAll(dir)
		tree, e := m.Source.Retrieve(ctx, a, dir, output)
		if e != nil {
			return r, e
		}
		r.Commit = tree.Commit
		r.Repository = a.Repository.URL
		if err = saveStage("build"); err != nil {
			return r, err
		}
		r.ImageID, err = m.Runtime.Build(ctx, tree, "localhost/ptui-"+a.ID+":"+r.ID, output)
	} else {
		r.ImageID, err = m.Runtime.ResolveImage(ctx, a.Image, output)
	}
	if err != nil {
		return r, err
	}
	if r.ImageID == "" {
		return r, errors.New("runtime returned no immutable image identity")
	}
	if err = saveStage("prepare"); err != nil {
		return r, err
	}
	if old != nil && old.ContainerID != "" && oldRunning && opts.Maintenance {
		if err = m.Runtime.Stop(ctx, old.Snapshot, old.ContainerID); err != nil {
			return r, err
		}
		oldStopped = true
	}
	if err = saveStage("candidate"); err != nil {
		return r, err
	}
	candidate, err := m.Runtime.Create(ctx, a, r)
	r.ContainerID = candidate.ID
	r.Endpoint = candidate.Endpoint
	if err != nil {
		return r, err
	}
	if r.ContainerID == "" {
		return r, errors.New("runtime returned no candidate identity")
	}
	if err = saveStage("start"); err != nil {
		return r, err
	}
	if err = m.Runtime.Start(ctx, a, r.ContainerID); err != nil {
		return r, err
	}
	candidate.Endpoint, err = m.Runtime.Endpoint(ctx, a, r.ContainerID)
	r.Endpoint = candidate.Endpoint
	if err != nil {
		return r, err
	}
	if err = saveStage("readiness"); err != nil {
		return r, err
	}
	if err = m.waitReady(ctx, a, candidate); err != nil {
		return r, err
	}
	r.Health = "passed"
	if err = saveStage("routing"); err != nil {
		return r, err
	}
	if a.Proxy != nil {
		undo, err = m.Router.Switch(ctx, a, candidate)
		if err != nil {
			return r, err
		}
	} else if old != nil && old.Snapshot.Proxy != nil {
		undo = func(recovery context.Context) error {
			_, restoreErr := m.Router.Switch(recovery, old.Snapshot, Candidate{ID: old.ContainerID, Endpoint: old.Endpoint})
			return restoreErr
		}
		if err = m.Router.Remove(ctx, old.Snapshot); err != nil {
			return r, err
		}
	}
	if startupInstalled {
		if old != nil && old.ImagePinID == "" {
			old.ImagePinID, err = m.Runtime.PinImage(ctx, old.Snapshot, *old)
			if err != nil {
				return r, err
			}
			if index := releaseIndex(session.State, old.ID); index >= 0 {
				session.State.Releases[index].ImagePinID = old.ImagePinID
			}
		}
		r.ImagePinID, err = m.Runtime.PinImage(ctx, a, r)
		if err != nil {
			return r, err
		}
		// daemon-reload changes ExecStop as well as the next ExecStart. Retire
		// the active old unit using its original definition before replacing it.
		// The candidate is already ready and routed; image pins permit recovery.
		if oldSupervised {
			if err = saveStage("startup-retire"); err != nil {
				return r, err
			}
			oldStopped = true
			if err = m.Startup.Control(ctx, old.Snapshot, "stop"); err != nil {
				return r, err
			}
		}
		startupTouched = true
		if err = m.Startup.Refresh(ctx, a, r); err != nil {
			return r, err
		}
	}
	if err = saveStage("commit"); err != nil {
		return r, err
	}
	r.Status = "successful"
	r.Stage = "successful"
	session.State.Releases[ri] = r
	a.CurrentRelease = r.ID
	session.State.Applications[ai] = a
	if err = session.Save(); err != nil {
		session.State.Applications[ai] = desired
		return r, err
	}
	committed = true
	if progress != nil {
		progress(Event{Stage: "successful", Message: "release committed; retiring previous container"})
	}
	// Retirement is post-commit; failure must not roll back a healthy committed release.
	if old != nil && !oldStopped && old.ContainerID != "" {
		if oldSupervised {
			err = m.Startup.Control(ctx, a, "stop")
		} else {
			err = m.Runtime.Stop(ctx, old.Snapshot, old.ContainerID)
		}
		if err != nil {
			return r, fmt.Errorf("release deployed; previous container retirement failed: %w", err)
		}
	}
	return r, nil
}

func (m *Manager) waitReady(ctx context.Context, a Application, c Candidate) error {
	timeout := a.Readiness.TimeoutSeconds
	if timeout == 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	successes := 0
	for {
		status, err := m.Runtime.Inspect(ctx, a, c.ID)
		if err != nil {
			return err
		}
		ready := status.Running
		switch a.Readiness.Mode {
		case "health":
			if status.Health == "" {
				return errors.New("health readiness requires an image HEALTHCHECK")
			}
			ready = ready && status.Health == "healthy"
		case "http":
			ready = false
			if status.Running && c.Endpoint != "" {
				req, e := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.Endpoint, "/")+a.Readiness.Path, nil)
				if e != nil {
					return e
				}
				resp, e := client.Do(req)
				if e == nil {
					ready = resp.StatusCode >= 200 && resp.StatusCode < 300
					resp.Body.Close()
				}
			}
		}
		if ready {
			successes++
		} else {
			successes = 0
		}
		if successes >= 3 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("candidate readiness failed: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Cleanup explicitly removes only stopped retained managed containers, never volumes or unrelated images.
func (m *Manager) Cleanup(ctx context.Context, name string, confirmed bool) error {
	if !confirmed {
		return errors.New("artifact cleanup requires confirmation")
	}
	session, err := m.Store.Begin()
	if err != nil {
		return err
	}
	defer session.Close()
	ai := appIndex(session.State, name)
	if ai < 0 {
		return ErrNotFound
	}
	a := session.State.Applications[ai]
	keep := a.Retention
	if keep == 0 {
		keep = 3
	}
	retained := 0
	for i := len(session.State.Releases) - 1; i >= 0; i-- {
		r := &session.State.Releases[i]
		if r.ApplicationID != a.ID {
			continue
		}
		if r.Status == "successful" {
			retained++
		}
		if r.ID == a.CurrentRelease || r.Status == "deploying" || (r.Status == "successful" && retained <= keep) {
			continue
		}
		if r.ContainerID != "" {
			status, inspectErr := m.Runtime.Inspect(ctx, a, r.ContainerID)
			if inspectErr != nil && !errors.Is(inspectErr, ErrNotFound) {
				return inspectErr
			}
			if inspectErr == nil && status.Running {
				continue
			}
			if err = m.Runtime.Remove(ctx, a, r.ContainerID); err != nil {
				return err
			}
		}
		if r.ImagePinID != "" {
			if err = m.Runtime.Remove(ctx, a, r.ImagePinID); err != nil {
				return err
			}
			r.ImagePinID = ""
		}
		r.ContainerID = ""
		r.Status = "retired"
	}
	return session.Save()
}
