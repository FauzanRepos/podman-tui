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

func TestLongOperationKeepsCommandInputResponsiveAndCancels(t *testing.T) {
	t.Setenv("PODMAN_TUI_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	application := tview.NewApplication()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(40, 20)
	page := New(func(update func()) { application.QueueUpdateDraw(update) }, func(p tview.Primitive) { application.SetFocus(p) }, func() registry.Connection {
		return registry.Connection{Name: "isolated", URI: "unix:///isolated-not-contacted"}
	})
	done := make(chan error, 1)
	go func() { done <- application.SetScreen(screen).SetRoot(page, true).Run() }()
	t.Cleanup(func() {
		application.Stop()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("UI did not stop")
		}
	})
	started, canceled := make(chan struct{}), make(chan struct{})
	application.QueueUpdateDraw(func() {
		page.run(false, func(ctx context.Context, _ *deploy.Manager, _ io.Writer) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		})
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("operation did not start")
	}
	application.QueueEvent(tcell.NewEventKey(tcell.KeyRune, 'm', tcell.ModNone))
	responsive := make(chan bool, 1)
	application.QueueUpdate(func() { responsive <- page.SubDialogHasFocus() })
	select {
	case visible := <-responsive:
		if !visible {
			t.Fatal("long operation prevented contextual command input")
		}
	case <-time.After(time.Second):
		t.Fatal("TUI event loop blocked")
	}
	application.QueueEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	application.QueueUpdate(func() {
		if page.SubDialogHasFocus() {
			t.Error("Esc did not close dialog")
		}
	})
	application.QueueEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Esc did not cancel operation")
	}
	// Drain completion update before stopping the simulation event loop.
	application.QueueUpdate(func() {})
}
