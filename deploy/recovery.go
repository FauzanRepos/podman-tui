package deploy

import (
	"context"
	"errors"
	"io"
)

// RecoverAbort explicitly restores the committed image/configuration before retiring an interrupted release.
// Exclusive writers or a missing previous container can require maintenance downtime; volumes are never removed.
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
		pending := &session.State.Releases[i]
		if pending.ApplicationID != a.ID || pending.Status != "deploying" {
			continue
		}
		candidate, findErr := m.Runtime.Find(ctx, pending.Snapshot, pending.ID)
		if findErr != nil && !errors.Is(findErr, ErrNotFound) {
			return findErr
		}
		previousIndex := releaseIndex(session.State, pending.Previous)
		if previousIndex >= 0 {
			old := session.State.Releases[previousIndex]
			previous, status, lookupErr := m.resolveCurrent(ctx, old.Snapshot, old)
			if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
				return lookupErr
			}
			// Stop/remove the interrupted writer before starting a stopped or recreated predecessor.
			if (lookupErr != nil || !status.Running) && findErr == nil {
				if m.Startup != nil {
					active, e := m.Startup.Active(ctx, a)
					if e != nil {
						return e
					}
					if active {
						if e = m.Startup.Control(ctx, a, "stop"); e != nil {
							return e
						}
					}
				}
				if err = m.Runtime.Remove(ctx, pending.Snapshot, candidate.ID); err != nil {
					return err
				}
				findErr = ErrNotFound
			}
			installed := false
			if m.Startup != nil {
				installed, err = m.Startup.Installed(a)
				if err != nil {
					return err
				}
				if installed {
					active, e := m.Startup.Active(ctx, a)
					if e != nil {
						return e
					}
					if active {
						// Stop the unchanged live definition before Refresh can alter ExecStop.
						if e = m.Startup.Control(ctx, a, "stop"); e != nil {
							return e
						}
						previous, status, lookupErr = m.resolveCurrent(ctx, old.Snapshot, old)
						if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
							return lookupErr
						}
					}
					if err = m.Startup.Refresh(ctx, old.Snapshot, old); err != nil {
						return err
					}
				}
			}
			if errors.Is(lookupErr, ErrNotFound) {
				if installed {
					if err = m.Startup.Control(ctx, a, "start"); err != nil {
						return err
					}
					previous, err = m.Runtime.Find(ctx, old.Snapshot, old.ID)
				} else {
					if _, err = m.Runtime.ResolveImage(ctx, old.ImageID, io.Discard); err != nil {
						return err
					}
					previous, err = m.Runtime.Create(ctx, old.Snapshot, old)
				}
				if err != nil {
					return err
				}
			}
			if err = m.Runtime.Start(ctx, old.Snapshot, previous.ID); err != nil {
				return err
			}
			previous.Endpoint, err = m.Runtime.Endpoint(ctx, old.Snapshot, previous.ID)
			if err != nil {
				return err
			}
			if err = m.waitReady(ctx, old.Snapshot, previous); err != nil {
				return err
			}
			if old.Snapshot.Proxy != nil {
				if m.Router == nil {
					return errors.New("proxy adapter required for recovery")
				}
				if _, err = m.Router.Switch(ctx, old.Snapshot, previous); err != nil {
					return err
				}
			}
			if old.Snapshot.Proxy == nil && pending.Snapshot.Proxy != nil {
				if m.Router == nil {
					return errors.New("proxy adapter required for recovery")
				}
				if err = m.Router.Remove(ctx, pending.Snapshot); err != nil {
					return err
				}
			}
			if err = m.discoverPin(ctx, old.Snapshot, &old); err != nil {
				return err
			}
			session.State.Releases[previousIndex].ImagePinID = old.ImagePinID
			session.State.Releases[previousIndex].ContainerID = previous.ID
			session.State.Releases[previousIndex].Endpoint = previous.Endpoint
		} else if pending.Snapshot.Proxy != nil {
			if m.Router == nil {
				return errors.New("proxy adapter required for recovery")
			}
			if err = m.Router.Remove(ctx, pending.Snapshot); err != nil {
				return err
			}
		}
		if findErr == nil {
			if err = m.Runtime.Remove(ctx, pending.Snapshot, candidate.ID); err != nil {
				return err
			}
			pending.ContainerID = candidate.ID
		}
		if err = m.discoverPin(ctx, pending.Snapshot, pending); err != nil {
			return err
		}
		if pending.ImagePinID != "" {
			if err = m.Runtime.Remove(ctx, pending.Snapshot, pending.ImagePinID); err != nil {
				return err
			}
			pending.ImagePinID = ""
		}
		pending.Status = "failed"
		pending.Stage = "recovered"
		pending.Health = "interrupted"
		pending.Error = "interrupted candidate aborted; previous committed release restored"
	}
	return session.Save()
}
