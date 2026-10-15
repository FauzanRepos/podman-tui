package applications

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/pdcs/registry"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestStaleOperationAndDialogCannotCrossEndpoints(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("PODMAN_TUI_STATE_DIR", root)
	a := registry.Connection{Name: "a", URI: "unix:///isolated-a"}
	b := registry.Connection{Name: "b", URI: "unix:///isolated-b"}
	makeManager := func(conn registry.Connection) (*deploy.Manager, deploy.Application) {
		store, err := deploy.NewStore(root, conn.URI)
		if err != nil {
			t.Fatal(err)
		}
		m := deploy.NewManager(store, nil, nil, nil)
		app, err := m.Create(deploy.Application{Name: "web", Image: "docker.io/library/busybox:1.37"})
		if err != nil {
			t.Fatal(err)
		}
		return m, app
	}
	ma, _ := makeManager(a)
	mb, ab := makeManager(b)
	conn := a
	completion := make(chan struct{}, 1)
	application := tview.NewApplication()
	focus := func(p tview.Primitive) { application.SetFocus(p) }
	page := New(func(f func()) { f(); completion <- struct{}{} }, focus, func() registry.Connection { return conn })
	await := func() {
		t.Helper()
		select {
		case <-completion:
		case <-time.After(time.Second):
			t.Fatal("UI update did not complete")
		}
	}
	page.UpdateData()
	await()
	page.action("rename")
	_, primitive := page.GetFrontPage()
	renameDialog := primitive
	application.SetFocus(renameDialog)
	renameDialog.InputHandler()(tcell.NewEventKey(tcell.KeyCtrlU, 0, tcell.ModNone), focus)
	for _, char := range "renamed" {
		renameDialog.InputHandler()(tcell.NewEventKey(tcell.KeyRune, char, tcell.ModNone), focus)
	}
	renameDialog.InputHandler()(tcell.NewEventKey(tcell.KeyBacktab, 0, tcell.ModNone), focus)
	started, resume := make(chan struct{}), make(chan struct{})
	page.run(false, func(context.Context, *deploy.Manager, io.Writer) error { close(started); <-resume; return nil })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("operation did not start")
	}
	conn = b
	page.ClearData()
	await()
	close(resume)
	await()
	if _, err := page.selected(); err == nil {
		t.Fatal("stale worker republished old-host rows")
	}
	page.UpdateData()
	await()
	selected, err := page.selected()
	if err != nil || selected.ID != ab.ID {
		t.Fatal("old worker replaced current-host rows", err)
	}
	application.SetFocus(renameDialog)
	renameDialog.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), focus)
	for _, m := range []*deploy.Manager{ma, mb} {
		if _, err = m.Get("web"); err != nil {
			t.Fatal("stale dialog renamed an application", err)
		}
		if _, err = m.Get("renamed"); err == nil {
			t.Fatal("stale dialog crossed endpoint")
		}
	}
}
