// Package applications extends the existing tview screens with managed applications.
package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/deploy/platform"
	"github.com/containers/podman-tui/pdcs/registry"
	"github.com/containers/podman-tui/ui/dialogs"
	"github.com/containers/podman-tui/ui/style"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type Applications struct {
	*tview.Pages
	table      *tview.Table
	output     *tview.TextView
	status     *tview.TextView
	apps       []deploy.Application
	origin     registry.Connection
	queue      func(func())
	focus      func(tview.Primitive)
	connection func() registry.Connection
	dialog     string
	mu         sync.Mutex
	busy       bool
	refreshing bool
	cancel     context.CancelFunc
}

type target struct {
	deploy.Application
	origin registry.Connection
}

func sameEndpoint(a, b registry.Connection) bool {
	return a.URI == b.URI && a.Identity == b.Identity
}

func (a target) key() string { return "@" + a.ID }

func New(queue func(func()), focus func(tview.Primitive), connection func() registry.Connection) *Applications {
	p := &Applications{Pages: tview.NewPages(), table: tview.NewTable(), output: tview.NewTextView(), status: tview.NewTextView(), queue: queue, focus: focus, connection: connection}
	p.table.SetBorder(true).SetTitle("APPLICATIONS / SERVICES").SetBorderColor(style.BorderColor).SetBackgroundColor(style.BgColor)
	p.table.SetSelectable(true, false).SetFixed(1, 0).SetSelectedStyle(style.TableSelectedStyle)
	p.output.SetBorder(true).SetTitle("DEPLOYMENT OUTPUT (bounded, Esc cancels)").SetBorderColor(style.BorderColor).SetBackgroundColor(style.BgColor)
	p.output.SetDynamicColors(false).SetWrap(true).SetTextColor(style.FgColor)
	p.status.SetText("m commands • Enter status • j/k select • F9 applications • Esc close/cancel").SetTextColor(style.FgColor).SetBackgroundColor(style.BgColor)
	layout := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(p.table, 0, 2, true).AddItem(p.output, 0, 1, false).AddItem(p.status, 2, 0, false)
	p.AddPage("main", layout, true, true)
	p.table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEnter:
			p.action("status")
			return nil
		case tcell.KeyDelete:
			p.action("delete")
			return nil
		case tcell.KeyEsc:
			p.Cancel()
			return nil
		}
		switch event.Rune() {
		case 'm':
			p.menu()
			return nil
		case 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return event
	})
	p.render(nil)
	return p
}
func (p *Applications) GetTitle() string        { return "applications" }
func (p *Applications) SubDialogHasFocus() bool { return p.dialog != "" }
func (p *Applications) Cancel() {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (p *Applications) RequestExit() bool {
	p.mu.Lock()
	busy := p.busy
	cancel := p.cancel
	p.mu.Unlock()
	if busy && cancel != nil {
		cancel()
		return false
	}
	return true
}
func (p *Applications) ClearData() {
	p.Cancel()
	p.queue(func() { p.origin = registry.Connection{}; p.closeDialog(); p.render(nil) })
}
func (p *Applications) HideAllDialogs() { p.queue(p.closeDialog) }
func (p *Applications) closeDialog() {
	if p.dialog != "" {
		p.RemovePage(p.dialog)
		p.dialog = ""
	}
	p.focus(p.table)
}
func (p *Applications) showDialog(primitive tview.Primitive) {
	p.closeDialog()
	p.dialog = "dialog"
	p.AddPage(p.dialog, primitive, true, true)
	p.focus(primitive)
}
func (p *Applications) selected() (target, error) {
	if !sameEndpoint(p.origin, p.connection()) {
		return target{}, errors.New("connection changed; refresh applications before selecting")
	}
	row, _ := p.table.GetSelection()
	if row < 1 || row > len(p.apps) {
		return target{}, errors.New("select an application first")
	}
	return target{Application: p.apps[row-1], origin: p.origin}, nil
}
func (p *Applications) render(apps []deploy.Application) {
	p.apps = apps
	p.table.Clear()
	for i, h := range []string{"name", "kind", "profile", "release"} {
		p.table.SetCell(0, i, tview.NewTableCell(h).SetSelectable(false).SetTextColor(style.FgColor))
	}
	for row, a := range apps {
		release := a.CurrentRelease
		if len(release) > 12 {
			release = release[:12]
		}
		for col, text := range []string{a.Name, a.Kind, a.Profile, release} {
			p.table.SetCell(row+1, col, tview.NewTableCell(deploy.Sanitize(text)).SetTextColor(style.FgColor))
		}
	}
	if len(apps) > 0 {
		p.table.Select(1, 0)
	}
}

func (p *Applications) runTarget(a target, connect bool, action func(context.Context, *deploy.Manager, io.Writer) error) {
	if !sameEndpoint(a.origin, p.connection()) {
		p.status.SetText("Connection changed; canceled stale application action")
		return
	}
	p.run(connect, action)
}

func (p *Applications) queueFor(conn registry.Connection, update func()) {
	p.queue(func() {
		if sameEndpoint(conn, p.connection()) {
			update()
		}
	})
}
func (p *Applications) run(connect bool, action func(context.Context, *deploy.Manager, io.Writer) error) {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	p.busy = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	p.cancel = cancel
	p.mu.Unlock()
	conn := p.connection() // freeze endpoint before starting the worker
	p.output.SetText("")
	p.status.SetText("Working on " + deploy.Sanitize(conn.Name) + "; Esc cancels safely")
	go func() {
		var logs deploy.LogBuffer
		writer := &screenWriter{buffer: &logs, page: p, origin: conn}
		var m *deploy.Manager
		var close func()
		var err error
		if connect {
			frozen, connectionErr := registry.GetCachedConnection(conn)
			if connectionErr != nil {
				err = connectionErr
			} else {
				m, close, err = platform.OpenFrozen(ctx, "", conn, false, frozen)
			}
		} else {
			m, _, close, err = platform.Open(ctx, "", conn, false, false)
		}
		if err == nil {
			err = action(ctx, m, writer)
			close()
		}
		var apps []deploy.Application
		if m != nil {
			apps, _ = m.List()
		}
		cancel()
		p.mu.Lock()
		p.busy = false
		p.cancel = nil
		p.mu.Unlock()
		p.queueFor(conn, func() {
			p.origin = conn
			p.render(apps)
			p.output.SetText(logs.String())
			if err != nil {
				p.status.SetText("Error: " + deploy.Sanitize(err.Error()))
			} else {
				p.status.SetText("Complete • m commands • Enter runtime health • Esc close/cancel")
			}
		})
	}()
}
func (p *Applications) UpdateData() {
	p.mu.Lock()
	if p.busy || p.refreshing {
		p.mu.Unlock()
		return
	}
	p.refreshing = true
	p.mu.Unlock()
	conn := p.connection()
	go func() {
		manager, _, close, err := platform.Open(context.Background(), "", conn, false, false)
		var apps []deploy.Application
		if err == nil {
			apps, err = manager.List()
			close()
		}
		p.mu.Lock()
		p.refreshing = false
		p.mu.Unlock()
		p.queue(func() {
			if !sameEndpoint(p.connection(), conn) {
				return
			}
			if err != nil {
				p.status.SetText("Error: " + deploy.Sanitize(err.Error()))
				return
			}
			row, _ := p.table.GetSelection()
			p.origin = conn
			p.render(apps)
			if row > 0 && row <= len(apps) {
				p.table.Select(row, 0)
			}
		})
	}()
}

type screenWriter struct {
	buffer *deploy.LogBuffer
	page   *Applications
	origin registry.Connection
	last   time.Time
	mu     sync.Mutex
}

func (w *screenWriter) Write(data []byte) (int, error) {
	n, err := w.buffer.Write(data)
	w.mu.Lock()
	if time.Since(w.last) > 100*time.Millisecond {
		w.last = time.Now()
		text := w.buffer.String()
		w.page.queueFor(w.origin, func() { w.page.output.SetText(text) })
	}
	w.mu.Unlock()
	return n, err
}

func (p *Applications) menu() {
	origin := p.connection()
	command := dialogs.NewCommandDialog([][]string{
		{"create", "Create an application"},
		{"configure", "Edit desired configuration"},
		{"inspect", "Inspect application configuration"},
		{"status", "Inspect runtime health"},
		{"deploy", "Deploy the configured source"},
		{"deploy-maintenance", "Deploy with authorized downtime"},
		{"rebuild", "Build and deploy again"},
		{"rollback", "Select a successful release"},
		{"history", "View deployment history"},
		{"start", "Start the current release"},
		{"stop", "Stop the current release"},
		{"restart", "Restart the current release"},
		{"rename", "Rename the application"},
		{"service-create", "Create a private backing service"},
		{"service-link", "Authorize a service connection"},
		{"tls-status", "Inspect certificate status"},
		{"tls-issue", "Request an ACME certificate"},
		{"tls-import", "Import an existing certificate"},
		{"tls-renew", "Renew the managed certificate"},
		{"quadlet-status", "Inspect startup unit status"},
		{"quadlet-enable", "Configure automatic startup"},
		{"quadlet-disable", "Disable automatic startup"},
		{"delete", "Delete managed application resources"},
		{"cleanup", "Clean eligible release artifacts"},
		{"recover-abort", "Recover an interrupted deployment"},
	})
	command.SetCancelFunc(p.closeDialog)
	command.SetSelectedFunc(func() {
		action := command.GetSelectedItem()
		p.closeDialog()
		if !sameEndpoint(origin, p.connection()) {
			p.status.SetText("Connection changed; canceled stale command")
			return
		}
		p.action(action)
	})
	command.Display()
	p.showDialog(command)
}
func (p *Applications) action(action string) {
	if action == "create" {
		p.createForm()
		return
	}
	if action == "service-create" {
		p.serviceForm()
		return
	}
	a, err := p.selected()
	if err != nil {
		p.status.SetText(err.Error())
		return
	}
	if p.integrationAction(a, action) {
		return
	}
	if action == "deploy-maintenance" {
		p.confirm("Authorize maintenance downtime for "+a.Name+"? Persistent data/migrations are unchanged.", func() { p.execute(a, action, true) })
		return
	}
	if action == "configure" {
		p.configureForm(a)
		return
	}
	if action == "rename" {
		p.renameForm(a)
		return
	}
	if action == "delete" || action == "cleanup" || action == "recover-abort" {
		p.confirm(action+" "+a.Name+"? Persistent volumes and unmanaged resources are preserved.", func() { p.execute(a, action, true) })
		return
	}
	if action == "rollback" {
		p.rollback(a)
		return
	}
	p.execute(a, action, false)
}
func (p *Applications) confirm(message string, action func()) {
	dialog := dialogs.NewConfirmDialog().SetFocusOnCancel()
	dialog.SetTitle("confirm")
	dialog.SetText(tview.Escape(deploy.Sanitize(message)))
	dialog.SetCancelFunc(p.closeDialog)
	dialog.SetSelectedFunc(func() { p.closeDialog(); action() })
	dialog.Display()
	p.showDialog(dialog)
}
func (p *Applications) execute(a target, action string, confirmed bool) {
	connect := action != "inspect" && action != "history"
	p.runTarget(a, connect, func(ctx context.Context, m *deploy.Manager, out io.Writer) error {
		switch action {
		case "inspect":
			value, err := m.Get(a.key())
			if err != nil {
				return err
			}
			return json.NewEncoder(out).Encode(value)
		case "history":
			value, err := m.History(a.key())
			if err != nil {
				return err
			}
			return json.NewEncoder(out).Encode(value)
		case "status":
			value, err := m.Status(ctx, a.key())
			if err != nil {
				return err
			}
			return json.NewEncoder(out).Encode(value)
		case "start", "stop", "restart":
			return m.Lifecycle(ctx, a.key(), action)
		case "delete":
			return m.Delete(ctx, a.key(), confirmed)
		case "cleanup":
			return m.Cleanup(ctx, a.key(), confirmed)
		case "recover-abort":
			return m.RecoverAbort(ctx, a.key(), confirmed)
		default:
			_, err := m.Deploy(ctx, a.key(), deploy.DeployOptions{Rebuild: action == "rebuild", Maintenance: action == "deploy-maintenance" && confirmed}, func(e deploy.Event) {
				p.queueFor(a.origin, func() { p.status.SetText("Deployment: " + e.Stage + " • Esc cancels") })
			}, out)
			return err
		}
	})
}
func (p *Applications) createForm() {
	form := newForm()
	origin := p.connection()
	name, image, repository, revision := "", "", "", "main"
	trusted := false
	form.AddInputField("name", "", 0, nil, func(v string) { name = v }).AddInputField("image", "", 0, nil, func(v string) { image = v }).AddInputField("repository", "", 0, nil, func(v string) { repository = v }).AddInputField("revision", revision, 0, nil, func(v string) { revision = v }).AddCheckbox("Trust build", false, func(v bool) { trusted = v })
	form.AddButton("Cancel", p.closeDialog).AddButton("Create", func() {
		if !sameEndpoint(origin, p.connection()) {
			p.closeDialog()
			return
		}
		p.closeDialog()
		p.run(false, func(_ context.Context, m *deploy.Manager, _ io.Writer) error {
			a := deploy.Application{Name: name, Image: image}
			if repository != "" {
				a.Repository = &deploy.GitConfig{URL: repository, Revision: revision, Trusted: trusted}
			}
			_, err := m.Create(a)
			return err
		})
	})
	form.SetBorder(true).SetTitle("CREATE APPLICATION")
	form.SetCancelFunc(p.closeDialog)
	p.showForm(form)
}
func (p *Applications) configureForm(a target) {
	data, _ := json.MarshalIndent(a.Application, "", "  ")
	editor := tview.NewTextArea().SetText(string(data), false).SetWrap(true)
	p.showEditor(editor, func() error {
		var updated deploy.Application
		if err := json.Unmarshal([]byte(editor.GetText()), &updated); err != nil {
			return err
		}
		p.closeDialog()
		p.runTarget(a, false, func(_ context.Context, m *deploy.Manager, _ io.Writer) error {
			return m.Configure(a.key(), updated)
		})
		return nil
	})
}
func (p *Applications) renameForm(a target) {
	name := a.Name
	form := newForm().AddInputField("new name", name, 0, nil, func(v string) { name = v })
	form.AddButton("Cancel", p.closeDialog).AddButton("Rename", func() {
		p.closeDialog()
		p.runTarget(a, false, func(_ context.Context, m *deploy.Manager, _ io.Writer) error { return m.Rename(a.key(), name) })
	})
	form.SetBorder(true).SetTitle("RENAME APPLICATION")
	form.SetCancelFunc(p.closeDialog)
	p.showForm(form)
}
func (p *Applications) rollback(a target) {
	p.runTarget(a, false, func(_ context.Context, m *deploy.Manager, _ io.Writer) error {
		history, err := m.History(a.key())
		if err != nil {
			return err
		}
		p.queueFor(a.origin, func() {
			options := make([][]string, 0, len(history))
			for i := len(history) - 1; i >= 0; i-- {
				r := history[i]
				if r.Status != "successful" {
					continue
				}
				options = append(options, []string{r.ID, fmt.Sprintf("%s %s", r.Timestamp.Format(time.RFC3339), r.ImageID)})
			}
			if len(options) == 0 {
				p.status.SetText("No successful release is available for rollback")
				return
			}
			command := dialogs.NewCommandDialog(options)
			command.SetCancelFunc(p.closeDialog)
			command.SetSelectedFunc(func() {
				releaseID := command.GetSelectedItem()
				p.closeDialog()
				p.confirm("Rollback "+a.Name+" to "+releaseID+"? Database migrations are not reverted.", func() {
					p.runTarget(a, true, func(ctx context.Context, m *deploy.Manager, out io.Writer) error {
						_, err := m.Deploy(ctx, a.key(), deploy.DeployOptions{RollbackRelease: releaseID}, nil, out)
						return err
					})
				})
			})
			command.Display()
			p.showDialog(command)
		})
		return nil
	})
}
