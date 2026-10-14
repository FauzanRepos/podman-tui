package deploy

import (
	"context"
	"errors"
	"testing"
)

// Model systemd's live definition: reload changes the container named by
// ExecStop, even while the old process is still active.
type unitSupervisor struct {
	runtime         *fakeRuntime
	definition      Release
	active          bool
	failNextRefresh bool
}

func (s *unitSupervisor) Installed(Application) (bool, error)               { return true, nil }
func (s *unitSupervisor) Active(context.Context, Application) (bool, error) { return s.active, nil }
func (s *unitSupervisor) Control(ctx context.Context, a Application, action string) error {
	if action == "stop" {
		s.active = false
		return s.runtime.Remove(ctx, a, s.definition.ContainerID)
	}
	if action == "start" {
		s.active = true
		return s.runtime.Start(ctx, a, s.definition.ContainerID)
	}
	return errors.New("unsupported test unit control")
}
func (s *unitSupervisor) Refresh(_ context.Context, _ Application, r Release) error {
	if s.failNextRefresh {
		s.failNextRefresh = false
		return errors.New("injected unit installation failure")
	}
	s.definition = r
	return nil
}
func (s *unitSupervisor) Install(ctx context.Context, a Application, r Release, _ bool) error {
	return s.Refresh(ctx, a, r)
}
func (*unitSupervisor) Disable(context.Context, Application) error { return nil }
func (*unitSupervisor) Remove(context.Context, Application) error  { return nil }

func TestActiveUnitCutoverDoesNotStopHealthyReplacement(t *testing.T) {
	m := testManager(t)
	a, err := m.Create(testApp("web"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Deploy(context.Background(), a.Name, DeployOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	unit := &unitSupervisor{runtime: m.Runtime.(*fakeRuntime), definition: first, active: true}
	m.Startup = unit
	second, err := m.Deploy(context.Background(), a.Name, DeployOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := m.Get(a.Name)
	if err != nil || current.CurrentRelease != second.ID {
		t.Fatal("replacement was not committed", err)
	}
	status, err := m.Status(context.Background(), a.Name)
	if err != nil || !status.Running {
		t.Fatal("systemd retirement stopped the healthy replacement", err)
	}
	if unit.runtime.running[first.ContainerID] {
		t.Fatal("old supervised deployment remains running")
	}
}

func TestUnitInstallationFailureRecreatesKnownGoodRelease(t *testing.T) {
	m := testManager(t)
	a, err := m.Create(testApp("web"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Deploy(context.Background(), a.Name, DeployOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	unit := &unitSupervisor{runtime: m.Runtime.(*fakeRuntime), definition: first, active: true, failNextRefresh: true}
	m.Startup = unit
	failed, err := m.Deploy(context.Background(), a.Name, DeployOptions{}, nil, nil)
	if err == nil || failed.Status != "failed" {
		t.Fatal("unit installation failure was not recorded")
	}
	current, err := m.Get(a.Name)
	if err != nil || current.CurrentRelease != first.ID {
		t.Fatal("failed unit installation replaced committed release", err)
	}
	status, err := m.Status(context.Background(), a.Name)
	if err != nil || !status.Running || !unit.active {
		t.Fatal("known-good supervised release was not restored", err)
	}
	if unit.runtime.running[failed.ContainerID] {
		t.Fatal("failed candidate retained after successful restoration")
	}
}
