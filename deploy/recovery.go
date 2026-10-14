package deploy

import (
	"context"
	"errors"
)

// RecoverAbort restores the committed route/container before removing an interrupted candidate.
// It never assumes an in-progress journal entry reached readiness or routing activation.
func (m *Manager) RecoverAbort(ctx context.Context, name string, confirmed bool) error {
	if !confirmed {
		return errors.New("interrupted deployment recovery requires confirmation")
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
	for i := range session.State.Releases {
		r := &session.State.Releases[i]
		if r.ApplicationID != a.ID || r.Status != "deploying" {
			continue
		}
		previous := releaseIndex(session.State, r.Previous)
		if previous >= 0 {
			old := session.State.Releases[previous]
			if err = m.Runtime.Start(ctx, old.Snapshot, old.ContainerID); err != nil {
				return err
			}
			if old.Snapshot.Proxy != nil {
				if m.Router == nil {
					return errors.New("proxy adapter required for recovery")
				}
				endpoint, e := m.Runtime.Endpoint(ctx, old.Snapshot, old.ContainerID)
				if e != nil {
					return e
				}
				if _, e = m.Router.Switch(ctx, old.Snapshot, Candidate{ID: old.ContainerID, Endpoint: endpoint}); e != nil {
					return e
				}
			}
		} else if r.Snapshot.Proxy != nil {
			if m.Router == nil {
				return errors.New("proxy adapter required for recovery")
			}
			if err = m.Router.Remove(ctx, r.Snapshot); err != nil {
				return err
			}
		}
		candidate, e := m.Runtime.Find(ctx, r.Snapshot, r.ID)
		if e != nil && !errors.Is(e, ErrNotFound) {
			return e
		}
		if e == nil {
			if err = m.Runtime.Remove(ctx, r.Snapshot, candidate.ID); err != nil {
				return err
			}
			r.ContainerID = candidate.ID
		}
		r.Status = "failed"
		r.Stage = "recovered"
		r.Health = "interrupted"
		r.Error = "interrupted candidate aborted; previous committed release restored"
	}
	return session.Save()
}
