package dialogs

import (
	"fmt"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestLargeCommandMenuNavigatesWithoutActivatingUnboundCommands(t *testing.T) {
	options := make([][]string, 40)
	for i := range options {
		options[i] = []string{fmt.Sprintf("action-%d", i), "Select explicitly"}
	}
	dialog := NewCommandDialog(options)
	selected := ""
	dialog.SetCancelFunc(func() {})
	dialog.SetSelectedFunc(func() { selected = dialog.GetSelectedItem() })
	application := tview.NewApplication()
	focus := func(p tview.Primitive) { application.SetFocus(p) }
	dialog.Display()
	focus(dialog)
	handler := dialog.InputHandler()
	handler(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone), focus)
	if selected != "" {
		t.Fatalf("Down activated %q instead of navigating", selected)
	}
	if got := dialog.GetSelectedItem(); got != "action-1" {
		t.Fatalf("Down selected %q", got)
	}
	handler(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone), focus)
	if selected != "" {
		t.Fatal("End activated a command")
	}
	if got := dialog.GetSelectedItem(); got != "action-39" {
		t.Fatalf("End selected %q", got)
	}
	handler(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), focus)
	if selected != "action-39" {
		t.Fatalf("Enter did not activate selected command: %q", selected)
	}
}
