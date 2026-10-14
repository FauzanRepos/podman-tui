package quadlet

import (
	"context"
	"errors"
	"strings"

	"github.com/containers/podman-tui/deploy"
)

func (m *Manager) Installed(a deploy.Application) (bool, error) {
	if !m.local || m.platform != "linux" {
		return false, errors.New("Quadlet requires a local Linux Podman connection")
	}
	if err := appID(a); err != nil {
		return false, err
	}
	unlock, err := m.lockRecovered(context.Background(), "")
	if err != nil {
		return false, err
	}
	defer unlock()
	_, metadata, err := m.snapshot(a.ID)
	return metadata != nil, err
}
func (m *Manager) Active(ctx context.Context, a deploy.Application) (bool, error) {
	installed, err := m.Installed(a)
	if err != nil || !installed {
		return false, err
	}
	status, err := m.Status(ctx, a)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "ActiveState=") {
			state := strings.TrimPrefix(line, "ActiveState=")
			return state != "inactive" && state != "failed", nil
		}
	}
	return false, errors.New("systemd returned no ActiveState")
}
func (m *Manager) Control(ctx context.Context, a deploy.Application, action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return errors.New("unsupported systemd application action")
	}
	ctl, _, err := m.probe()
	if err != nil {
		return err
	}
	if err = appID(a); err != nil {
		return err
	}
	unlock, err := m.lockRecovered(ctx, ctl)
	if err != nil {
		return err
	}
	defer unlock()
	_, metadata, err := m.snapshot(a.ID)
	if err != nil {
		return err
	}
	if metadata == nil {
		return errors.New("application has no managed Quadlet unit")
	}
	_, err = m.ctl(ctx, ctl, action, "ptui-"+a.ID+".service")
	return err
}

// Refresh preserves the operator's boot enablement while replacing the pinned release definition.
// The deployment manager stops the old unchanged unit before Refresh; this
// method only reloads definitions and never invokes ExecStop on the replacement.
func (m *Manager) Refresh(ctx context.Context, a deploy.Application, r deploy.Release) error {
	ctl, generator, err := m.probe()
	if err != nil {
		return err
	}
	if err := appID(a); err != nil {
		return err
	}
	unlock, err := m.lockRecovered(ctx, ctl)
	if err != nil {
		return err
	}
	defer unlock()
	_, metadata, err := m.snapshot(a.ID)
	if err != nil {
		return err
	}
	if metadata == nil {
		return nil
	}
	return m.installLocked(ctx, ctl, generator, a, r, metadata.Enabled)
}

var _ deploy.Supervisor = (*Manager)(nil)
