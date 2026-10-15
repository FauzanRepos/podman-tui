package dialogs

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestCancelFirstConfirmationRequiresExplicitSelection(t *testing.T) {
	dialog := NewConfirmDialog().SetFocusOnCancel()
	canceled, confirmed := false, false
	dialog.SetText("Delete the selected application? Persistent volumes are retained.")
	dialog.SetCancelFunc(func() { canceled = true })
	dialog.SetSelectedFunc(func() { confirmed = true })
	application := tview.NewApplication()
	focus := func(p tview.Primitive) { application.SetFocus(p) }
	dialog.Display()
	focus(dialog)
	dialog.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), focus)
	if !canceled || confirmed {
		t.Fatal("default Enter did not cancel destructive confirmation")
	}
	canceled = false
	dialog.Display()
	focus(dialog)
	dialog.InputHandler()(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone), focus)
	dialog.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), focus)
	if canceled || !confirmed {
		t.Fatal("explicitly selecting confirmation did not activate")
	}
}
