package dialogs

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestInputDialogKeyboardSubmissionAndCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		keys   []tcell.Key
		submit bool
	}{
		{"submit from input", []tcell.Key{tcell.KeyEnter}, true},
		{"cancel with escape", []tcell.Key{tcell.KeyEsc}, false},
		{"cancel with button", []tcell.Key{tcell.KeyTab, tcell.KeyEnter}, false},
		{"submit with button", []tcell.Key{tcell.KeyTab, tcell.KeyTab, tcell.KeyEnter}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dialog := NewSimpleInputDialog("")
			application := tview.NewApplication()
			focus := func(primitive tview.Primitive) { application.SetFocus(primitive) }
			submitted, canceled := false, false
			value := ""
			dialog.SetSelectedFunc(func() {
				submitted = true
				value = dialog.GetInputText()
			})
			dialog.SetCancelFunc(func() { canceled = true })
			dialog.Display()
			focus(dialog)
			handler := dialog.InputHandler()
			for _, character := range "my-container" {
				handler(tcell.NewEventKey(tcell.KeyRune, character, tcell.ModNone), focus)
			}
			for _, key := range scenario.keys {
				handler(tcell.NewEventKey(key, 0, tcell.ModNone), focus)
			}
			if submitted != scenario.submit || canceled == scenario.submit {
				t.Fatalf("submitted=%t canceled=%t, want submit=%t", submitted, canceled, scenario.submit)
			}
			if scenario.submit && value != "my-container" {
				t.Fatalf("submitted %q, want typed container name", value)
			}
		})
	}
}
