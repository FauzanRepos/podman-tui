package deploy

import (
	"context"
	"errors"
	"time"
)

func (m *Manager) GetByID(id string) (Application, error) {
	state, err := m.Store.Snapshot()
	if err != nil {
		return Application{}, err
	}
	for _, a := range state.Applications {
		if a.ID == id {
			return a, nil
		}
	}
	return Application{}, ErrNotFound
}

// Active returns the committed runtime snapshot, not unapplied desired configuration.
func (m *Manager) Active(name string) (Application, Release, error) {
	state, err := m.Store.Snapshot()
	if err != nil {
		return Application{}, Release{}, err
	}
	ai := appIndex(state, name)
	if ai < 0 {
		return Application{}, Release{}, ErrNotFound
	}
	ri := releaseIndex(state, state.Applications[ai].CurrentRelease)
	if ri < 0 {
		return Application{}, Release{}, ErrNotFound
	}
	release := state.Releases[ri]
	a := release.Snapshot
	a.Name = state.Applications[ai].Name
	a.CurrentRelease = release.ID
	return a, release, nil
}

// ApplyProxy serializes TLS/routing changes with deployments and rolls routing back on metadata failure.
// Certificate paths are host infrastructure, not a rollback of persistent application data.
func (m *Manager) ApplyProxy(ctx context.Context, name string, action func(Application, Candidate) (Application, error)) error {
	session, err := m.Store.Begin()
	if err != nil {
		return err
	}
	defer session.Close()
	ai := appIndex(session.State, name)
	if ai < 0 {
		return ErrNotFound
	}
	desired := session.State.Applications[ai]
	ri := releaseIndex(session.State, desired.CurrentRelease)
	if ri < 0 {
		return ErrNotFound
	}
	release := session.State.Releases[ri]
	active := release.Snapshot
	active.Name = desired.Name
	active.CurrentRelease = release.ID
	if active.Proxy == nil || m.Router == nil {
		return errors.New("deploy an HTTP domain route before configuring TLS")
	}
	candidate, err := m.Runtime.Find(ctx, active, release.ID)
	if err != nil {
		return err
	}
	status, err := m.Runtime.Inspect(ctx, active, candidate.ID)
	if err != nil {
		return err
	}
	if !status.Running {
		return errors.New("TLS activation requires a running committed application")
	}
	candidate.Endpoint, err = m.Runtime.Endpoint(ctx, active, candidate.ID)
	if err != nil {
		return err
	}
	metadataChanged := false
	updated, actionErr := action(active, candidate)
	if actionErr == nil {
		if updated.ID != active.ID || updated.Proxy == nil {
			actionErr = errors.New("host integration returned an invalid application identity")
		}
		if actionErr == nil {
			actionErr = updated.Validate()
		}
		if actionErr == nil {
			metadataChanged = true
			session.State.Applications[ai].Proxy = updated.Proxy
			session.State.Releases[ri].Snapshot.Proxy = updated.Proxy
			session.State.Releases[ri].ContainerID = candidate.ID
			session.State.Releases[ri].Endpoint = candidate.Endpoint
			actionErr = session.Save()
		}
	}
	if actionErr != nil {
		recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if metadataChanged {
			session.State.Applications[ai] = desired
			session.State.Releases[ri] = release
			actionErr = errors.Join(actionErr, session.Save())
		}
		_, restoreErr := m.Router.Switch(recovery, active, candidate)
		if restoreErr != nil {
			return errors.Join(actionErr, restoreErr, ErrRecoveryRequired)
		}
		return actionErr
	}
	return nil
}
