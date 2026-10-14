package deploy

import (
	"context"
	"errors"
	"testing"
)

type recoveryRouter struct {
	active string
	fail   bool
}

func (r *recoveryRouter) Switch(_ context.Context, _ Application, c Candidate) (func(context.Context) error, error) {
	r.active = c.ID
	return func(context.Context) error { return nil }, nil
}
func (r *recoveryRouter) Remove(context.Context, Application) error {
	if r.fail {
		return errors.New("reload failed")
	}
	r.active = ""
	return nil
}

func TestImmutableSelectionDoesNotDeleteReusedName(t *testing.T) {
	m := testManager(t)
	old, err := m.Create(testApp("web"))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Rename("web", "original"); err != nil {
		t.Fatal(err)
	}
	replacement, err := m.Create(testApp("web"))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Delete(context.Background(), "@"+old.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get("web")
	if err != nil || got.ID != replacement.ID {
		t.Fatal("immutable selection deleted replacement", err)
	}
	if _, err = m.Get("original"); !errors.Is(err, ErrNotFound) {
		t.Fatal("selected identity not deleted", err)
	}
}

func TestRecoverAbortRemovesAddedRouteBeforeCandidate(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "restored", true: "route-failure-preserves-candidate"}[fail], func(t *testing.T) {
			m := testManager(t)
			a, err := m.Create(testApp("web"))
			if err != nil {
				t.Fatal(err)
			}
			good, err := m.Deploy(context.Background(), a.Name, DeployOptions{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			pending := good
			pending.ID = "11111111111111111111111111111111"
			pending.ContainerID = pending.ID
			pending.ImagePinID = ""
			pending.Previous = good.ID
			pending.Status = "deploying"
			pending.Stage = "routing"
			pending.Snapshot.Proxy = &ProxyConfig{}
			pending.Snapshot.Domains = []string{"web.example.invalid"}
			session, err := m.Store.Begin()
			if err != nil {
				t.Fatal(err)
			}
			session.State.Releases = append(session.State.Releases, pending)
			err = session.Save()
			session.Close()
			if err != nil {
				t.Fatal(err)
			}
			runtime := m.Runtime.(*fakeRuntime)
			runtime.running[pending.ID] = true
			runtime.running["hold-"+pending.ID] = false
			runtime.running["hold-"+good.ID] = false
			router := &recoveryRouter{active: pending.ID, fail: fail}
			m.Router = router
			err = m.RecoverAbort(context.Background(), a.Name, true)
			history, e := m.History(a.Name)
			if e != nil {
				t.Fatal(e)
			}
			if fail {
				if err == nil || router.active != pending.ID || !runtime.running[pending.ID] || history[1].Status != "deploying" {
					t.Fatal("failed routing recovery deleted candidate or lost retry state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if router.active != "" {
				t.Fatal("removed candidate still has a proxy route")
			}
			if _, exists := runtime.running[pending.ID]; exists {
				t.Fatal("interrupted candidate retained")
			}
			if _, exists := runtime.running["hold-"+pending.ID]; exists {
				t.Fatal("crash-created pending pin leaked")
			}
			if _, exists := runtime.running["hold-"+good.ID]; !exists {
				t.Fatal("committed image pin removed")
			}
			if !runtime.running[good.ID] || history[1].Status != "failed" || history[0].ImagePinID != "hold-"+good.ID {
				t.Fatal("committed release or recovered history incorrect")
			}
		})
	}
}

func TestCleanupDiscoversUnjournaledImagePin(t *testing.T) {
	m := testManager(t)
	a, err := m.Create(testApp("web"))
	if err != nil {
		t.Fatal(err)
	}
	good, err := m.Deploy(context.Background(), a.Name, DeployOptions{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed := good
	failed.ID = "22222222222222222222222222222222"
	failed.Status = "failed"
	failed.ContainerID = ""
	failed.ImagePinID = ""
	session, err := m.Store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	session.State.Releases = append(session.State.Releases, failed)
	err = session.Save()
	session.Close()
	if err != nil {
		t.Fatal(err)
	}
	runtime := m.Runtime.(*fakeRuntime)
	runtime.running["hold-"+failed.ID] = false
	runtime.running["hold-"+good.ID] = false
	if err = m.Cleanup(context.Background(), a.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, exists := runtime.running["hold-"+failed.ID]; exists {
		t.Fatal("eligible orphan pin leaked")
	}
	if _, exists := runtime.running["hold-"+good.ID]; !exists {
		t.Fatal("retained successful pin deleted")
	}
}
