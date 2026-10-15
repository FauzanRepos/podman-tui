package applications

import (
	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/ui/dialogs"
	"github.com/containers/podman-tui/ui/style"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// dialogPanel keeps the existing page visible and clamps its child to the
// available application area, including when the terminal is resized.
type dialogPanel struct {
	tview.Primitive
	width, height int
}

func (d *dialogPanel) SetRect(x, y, width, height int) {
	w, h := min(d.width, max(0, width-2)), min(d.height, max(0, height-2))
	if width < 40 {
		w = width
	}
	if height < 14 {
		h = height
	}
	d.Primitive.SetRect(x+(width-w)/2, y+(height-h)/2, w, h)
}

func newForm() *tview.Form {
	form := tview.NewForm().SetItemPadding(0).SetButtonsAlign(tview.AlignRight)
	form.SetBackgroundColor(style.DialogBgColor)
	form.SetLabelColor(style.DialogFgColor)
	form.SetFieldBackgroundColor(style.FieldBackgroundColor)
	form.SetFieldTextColor(style.DialogFgColor)
	form.SetButtonBackgroundColor(style.ButtonBgColor)
	form.SetButtonTextColor(style.ButtonFgColor)
	form.SetButtonActivatedStyle(style.ButtonActivatedStyle)
	form.SetBorder(true).SetBorderColor(style.DialogBorderColor).SetTitleColor(style.DialogFgColor).SetBorderPadding(1, 1, 1, 1)
	return form
}

func (p *Applications) showForm(form *tview.Form) {
	for index := range form.GetFormItemCount() {
		switch item := form.GetFormItem(index).(type) {
		case *tview.InputField:
			item.SetLabel(item.GetLabel() + ":")
		case *tview.DropDown:
			item.SetLabel(item.GetLabel() + ":")
			item.SetListStyles(style.DropDownUnselected, style.DropDownSelected)
			item.SetFocusedStyle(style.DropDownFocused)
		case *tview.Checkbox:
			item.SetActivatedStyle(style.CheckboxActivatedStyle)
		}
	}
	p.showDialog(&dialogPanel{Primitive: form, width: 84, height: form.GetFormItemCount() + 7})
}

func (p *Applications) showEditor(editor *tview.TextArea, save func() error) {
	editor.SetBackgroundColor(style.BgColor)
	editor.SetTextStyle(tcell.StyleDefault.Foreground(style.FgColor).Background(style.BgColor))
	editor.SetSelectedStyle(style.DialogSelectedStyle)
	editor.SetBorder(true).SetBorderColor(style.DialogSubBoxBorderColor).SetTitle("JSON")
	hint := tview.NewTextView().SetText("Non-sensitive values / secret refs\nCtrl+S save | Tab | Esc close").SetWrap(true).SetTextColor(style.DialogFgColor)
	hint.SetBackgroundColor(style.DialogBgColor)
	form := newForm().AddButton("Cancel", p.closeDialog)
	form.SetBorder(false).SetBorderPadding(0, 0, 0, 0)
	submit := func() {
		if err := save(); err != nil {
			hint.SetText("Error: " + deploy.Sanitize(err.Error()))
			return
		}
	}
	form.AddButton("Save", submit)
	form.SetCancelFunc(p.closeDialog)
	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(hint, 2, 0, false).AddItem(editor, 0, 1, true).AddItem(form, dialogs.DialogFormHeight, 0, false)
	// Flex leaves padding transparent by default; dialogs must cover the page below.
	layout.Box = tview.NewBox()
	layout.SetBackgroundColor(style.DialogBgColor)
	layout.SetBorder(true).SetBorderColor(style.DialogBorderColor).SetTitleColor(style.DialogFgColor).SetTitle("CONFIGURATION").SetBorderPadding(0, 0, 1, 1)
	layout.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			p.closeDialog()
			return nil
		case tcell.KeyCtrlS:
			submit()
			return nil
		case tcell.KeyTab:
			if editor.HasFocus() {
				form.SetFocus(0)
				p.focus(form)
				return nil
			}
		case tcell.KeyBacktab:
			if editor.HasFocus() {
				form.SetFocus(1)
				p.focus(form)
				return nil
			}
		}
		return event
	})
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		_, button := form.GetFocusedItemIndex()
		if (event.Key() == tcell.KeyTab && button == 1) || (event.Key() == tcell.KeyBacktab && button == 0) {
			p.focus(editor)
			return nil
		}
		return event
	})
	p.showDialog(&dialogPanel{Primitive: layout, width: 96, height: 30})
}
