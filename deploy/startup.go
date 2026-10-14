package deploy

import (
	"context"
	"errors"
)

func (m *Manager) EnableStartup(ctx context.Context, name string, enabled bool) error {
	if m.Startup == nil {
		return errors.New("Quadlet requires a local Linux connection; remote/manual operations remain available")
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
	snapshot := r.Snapshot
	snapshot.Name = a.Name
	pin, err := m.Runtime.PinImage(ctx, snapshot, r)
	if err != nil {
		return err
	}
	r.ImagePinID = pin
	session.State.Releases[ri] = r
	if err = session.Save(); err != nil {
		return err
	}
	return m.Startup.Install(ctx, snapshot, r, enabled)
}

func (m *Manager) DisableStartup(ctx context.Context, name string, remove bool, confirmed bool) error {
	if m.Startup == nil {
		return errors.New("Quadlet requires a local Linux connection")
	}
	if remove && !confirmed {
		return errors.New("startup unit removal requires confirmation")
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
	if !remove {
		return m.Startup.Disable(ctx, a)
	}
	active, err := m.Startup.Active(ctx, a)
	if err != nil {
		return err
	}
	if active {
		return errors.New("stop the supervised application before removing its startup unit")
	}
	return m.Startup.Remove(ctx, a)
}

// resolveCurrent discovers a Quadlet-recreated container through immutable release labels.
func (m *Manager) resolveCurrent(ctx context.Context, a Application, r Release) (Candidate, RuntimeStatus, error) {
	if r.ContainerID != "" {
		status, err := m.Runtime.Inspect(ctx, a, r.ContainerID)
		if err == nil {
			return Candidate{ID: r.ContainerID, Endpoint: r.Endpoint}, status, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Candidate{}, RuntimeStatus{}, err
		}
	}
	candidate, err := m.Runtime.Find(ctx, a, r.ID)
	if err != nil {
		return Candidate{}, RuntimeStatus{}, err
	}
	status, err := m.Runtime.Inspect(ctx, a, candidate.ID)
	if err != nil {
		return Candidate{}, RuntimeStatus{}, err
	}
	candidate.Endpoint, err = m.Runtime.Endpoint(ctx, r.Snapshot, candidate.ID)
	return candidate, status, err
}
