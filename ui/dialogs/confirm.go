package dialogs

import (
	"strings"

	"github.com/containers/podman-tui/ui/style"
	"github.com/containers/podman-tui/ui/utils"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rs/zerolog/log"
)

// ConfirmDialog is a simple confirmation dialog primitive.
type ConfirmDialog struct {
	*tview.Box

	layout        *tview.Flex
	textview      *tview.TextView
	form          *tview.Form
	x             int
	y             int
	width         int
	height        int
	message       string
	display       bool
	firstDisplay  bool
	focusCancel   bool
	cancelHandler func()
	selectHandler func()
}

// NewConfirmDialog returns new confirm dialog primitive.
func NewConfirmDialog() *ConfirmDialog {
	dialog := &ConfirmDialog{
		Box:          tview.NewBox(),
		display:      false,
		firstDisplay: false,
	}

	dialog.textview = tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(true).
		SetTextAlign(tview.AlignLeft)

	dialog.textview.SetBackgroundColor(style.DialogBgColor)
	dialog.textview.SetTextColor(style.DialogFgColor)

	dialog.form = tview.NewForm().
		AddButton("Cancel", nil).
		AddButton("  OK  ", nil).
		SetButtonsAlign(tview.AlignRight)
	dialog.form.SetBackgroundColor(style.DialogBgColor)
	dialog.form.SetButtonBackgroundColor(style.ButtonBgColor)
	dialog.form.SetButtonTextColor(style.ButtonFgColor)
	dialog.form.SetButtonActivatedStyle(style.ButtonActivatedStyle)

	dialog.layout = tview.NewFlex().SetDirection(tview.FlexRow)
	dialog.layout.SetBorder(true)
	dialog.layout.SetBorderColor(style.DialogBorderColor)
	dialog.layout.SetBackgroundColor(style.DialogBgColor)

	return dialog
}

// Display displays this primitive.
func (d *ConfirmDialog) Display() {
	d.display = true
	d.firstDisplay = true
}

// IsDisplay returns true if primitive is shown.
func (d *ConfirmDialog) IsDisplay() bool {
	return d.display
}

// Hide stops displaying this primitive.
func (d *ConfirmDialog) Hide() {
	d.textview.SetText("")
	d.message = ""
	d.display = false
	d.firstDisplay = false
}

// SetTitle sets dialog title.
func (d *ConfirmDialog) SetTitle(title string) {
	d.layout.SetTitle(strings.ToUpper(title))
	d.layout.SetTitleColor(style.DialogFgColor)
}

// SetFocusOnCancel starts each display on Cancel without changing other callers.
func (d *ConfirmDialog) SetFocusOnCancel() *ConfirmDialog {
	d.focusCancel = true
	return d
}

// SetText sets dialog title.
func (d *ConfirmDialog) SetText(message string) {
	d.message = message
	d.textview.Clear()

	msg := "\n" + message

	d.textview.SetText(msg)
	d.textview.ScrollToBeginning()
	d.setRect()
}

// Focus is called when this primitive receives focus.
func (d *ConfirmDialog) Focus(delegate func(p tview.Primitive)) {
	if d.firstDisplay {
		d.firstDisplay = false

		index := 1
		if d.focusCancel {
			index = 0
		}
		d.form.SetFocus(index)
		delegate(d.form)

		return
	}

	delegate(d.form)
}

// HasFocus returns whether or not this primitive has focus.
func (d *ConfirmDialog) HasFocus() bool {
	return d.form.HasFocus()
}

// SetRect set rects for this primitive.
func (d *ConfirmDialog) SetRect(x, y, width, height int) {
	padding := DialogPadding
	if width < 2*DialogMinWidth || height < 15 {
		padding = 0
	}
	d.x = x + padding
	d.y = y + padding
	d.width = max(0, width-2*padding)
	d.height = max(0, height-2*padding)
	d.setRect()
}

// Draw draws this primitive onto the screen.
func (d *ConfirmDialog) Draw(screen tcell.Screen) {
	if !d.display {
		return
	}

	d.DrawForSubclass(screen, d)

	x, y, width, height := d.GetInnerRect()
	d.layout.SetRect(x, y, width, height)
	d.layout.Draw(screen)
}

// InputHandler returns input handler function for this primitive.
func (d *ConfirmDialog) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return d.WrapInputHandler(func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
		log.Debug().Msgf("confirm dialog: event %v received", event)

		if event.Key() == tcell.KeyEsc {
			d.cancelHandler()

			return
		}
		if event.Key() == tcell.KeyPgDn || event.Key() == tcell.KeyPgUp {
			d.textview.InputHandler()(event, setFocus)
			return
		}

		if formHandler := d.form.InputHandler(); formHandler != nil {
			formHandler(event, setFocus)

			return
		}
	})
}

// SetCancelFunc sets form cancel button selected function.
func (d *ConfirmDialog) SetCancelFunc(handler func()) *ConfirmDialog {
	d.cancelHandler = handler
	cancelButton := d.form.GetButton(d.form.GetButtonCount() - 2) //nolint:mnd
	cancelButton.SetSelectedFunc(handler)

	return d
}

// SetSelectedFunc sets form select button selected function.
func (d *ConfirmDialog) SetSelectedFunc(handler func()) *ConfirmDialog {
	d.selectHandler = handler
	enterButton := d.form.GetButton(d.form.GetButtonCount() - 1)
	enterButton.SetSelectedFunc(handler)

	return d
}

func (d *ConfirmDialog) setRect() {
	maxHeight := d.height
	maxWidth := d.width
	messageWidth := getMessageWidth(d.message)
	d.width = min(maxWidth, max(DialogMinWidth+4, messageWidth+6))
	messageHeight := len(tview.WordWrap(d.message, max(1, d.width-4)))
	layoutHeight := min(max(0, maxHeight-DialogFormHeight-2), messageHeight+2)
	d.height = min(maxHeight, layoutHeight+DialogFormHeight+2)
	d.x += (maxWidth - d.width) / 2
	d.y += (maxHeight - d.height) / 2

	msgLayout := tview.NewFlex().SetDirection(tview.FlexColumn)
	msgLayout.AddItem(utils.EmptyBoxSpace(style.DialogBgColor), 1, 0, false)
	msgLayout.AddItem(d.textview, 0, 1, true)
	msgLayout.AddItem(utils.EmptyBoxSpace(style.DialogBgColor), 1, 0, false)

	d.layout.Clear()
	d.layout.AddItem(msgLayout, layoutHeight, 0, true)
	d.layout.AddItem(d.form, DialogFormHeight, 0, true)

	d.Box.SetRect(d.x, d.y, d.width, d.height)
}
