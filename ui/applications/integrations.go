package applications

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/deploy/platform"
	"github.com/containers/podman-tui/deploy/proxy"
	"github.com/containers/podman-tui/deploy/quadlet"
	"github.com/rivo/tview"
)

func (p *Applications) serviceForm() {
	origin := p.connection()
	name, secret, network, kind := "", "", "", "postgres"
	form := tview.NewForm().AddInputField("name", "", 0, nil, func(v string) { name = v }).AddDropDown("type", []string{"postgres", "mariadb", "mysql", "redis"}, 0, func(v string, _ int) { kind = v }).AddInputField("secret ref", "", 0, nil, func(v string) { secret = v }).AddInputField("network", "", 0, nil, func(v string) { network = v })
	form.AddButton("Create", func() {
		if !sameEndpoint(origin, p.connection()) {
			p.closeDialog()
			return
		}
		p.closeDialog()
		p.run(true, func(ctx context.Context, m *deploy.Manager, out io.Writer) error {
			_, err := platform.CreateService(ctx, m, name, kind, secret, network, out)
			return err
		})
	}).AddButton("Cancel", p.closeDialog)
	form.SetBorder(true).SetTitle("PRIVATE BACKING SERVICE (existing secret reference; no public ports)")
	form.SetCancelFunc(p.closeDialog)
	p.showDialog(form)
}
func (p *Applications) linkForm(a target) {
	name := ""
	inject := false
	form := tview.NewForm().AddInputField("service", "", 0, nil, func(v string) { name = v }).AddCheckbox("Inject host", false, func(v bool) { inject = v })
	form.AddButton("Link", func() {
		p.closeDialog()
		p.runTarget(a, false, func(_ context.Context, m *deploy.Manager, _ io.Writer) error {
			return platform.LinkService(m, a.key(), name, inject)
		})
	}).AddButton("Cancel", p.closeDialog)
	form.SetBorder(true).SetTitle("AUTHORIZE SERVICE NETWORK / NON-SENSITIVE SETTINGS")
	form.SetCancelFunc(p.closeDialog)
	p.showDialog(form)
}
func routerFor(m *deploy.Manager) (*proxy.Router, error) {
	r, ok := m.Router.(*proxy.Router)
	if !ok {
		return nil, errors.New("Nginx integration is unavailable")
	}
	return r, nil
}
func (p *Applications) tlsIssueForm(a target) {
	email := ""
	staging, agree := true, false
	form := tview.NewForm().AddInputField("email", "", 0, nil, func(v string) { email = v }).AddCheckbox("Staging CA", true, func(v bool) { staging = v }).AddCheckbox("Agree terms", false, func(v bool) { agree = v })
	form.AddButton("Issue", func() {
		p.closeDialog()
		p.runTarget(a, true, func(ctx context.Context, m *deploy.Manager, _ io.Writer) error {
			router, err := routerFor(m)
			if err != nil {
				return err
			}
			return m.ApplyProxy(ctx, a.key(), func(current deploy.Application, c deploy.Candidate) (deploy.Application, error) {
				return proxy.NewTLS(router).Issue(ctx, current, c, email, staging, agree)
			})
		})
	}).AddButton("Cancel", p.closeDialog)
	form.SetBorder(true).SetTitle("ACME HTTP-01 (requires public DNS/reachability and configured webroot)")
	form.SetCancelFunc(p.closeDialog)
	p.showDialog(form)
}
func (p *Applications) tlsImportForm(a target) {
	certificate, key := "", ""
	form := tview.NewForm().AddInputField("certificate", "", 0, nil, func(v string) { certificate = v }).AddInputField("key file", "", 0, nil, func(v string) { key = v })
	form.AddButton("Import", func() {
		p.closeDialog()
		p.runTarget(a, true, func(ctx context.Context, m *deploy.Manager, _ io.Writer) error {
			router, err := routerFor(m)
			if err != nil {
				return err
			}
			return m.ApplyProxy(ctx, a.key(), func(current deploy.Application, c deploy.Candidate) (deploy.Application, error) {
				return proxy.NewTLS(router).Import(ctx, current, c, certificate, key)
			})
		})
	}).AddButton("Cancel", p.closeDialog)
	form.SetBorder(true).SetTitle("IMPORT LOCAL CERTIFICATE (key path only; contents are never previewed)")
	form.SetCancelFunc(p.closeDialog)
	p.showDialog(form)
}
func (p *Applications) integrationAction(a target, action string) bool {
	switch action {
	case "service-link":
		p.linkForm(a)
		return true
	case "tls-issue":
		p.tlsIssueForm(a)
		return true
	case "tls-import":
		p.tlsImportForm(a)
		return true
	case "tls-status", "tls-renew", "quadlet-enable", "quadlet-disable", "quadlet-status":
		p.runTarget(a, action != "tls-status" && action != "quadlet-status" && action != "quadlet-disable", func(ctx context.Context, m *deploy.Manager, out io.Writer) error {
			switch action {
			case "quadlet-enable":
				return m.EnableStartup(ctx, a.key(), true)
			case "quadlet-disable":
				return m.DisableStartup(ctx, a.key(), false, false)
			case "quadlet-status":
				status, err := quadlet.New(m.LocalPodman).Status(ctx, a.Application)
				if err != nil {
					return err
				}
				return json.NewEncoder(out).Encode(status)
			default:
				router, err := routerFor(m)
				if err != nil {
					return err
				}
				if action == "tls-status" {
					current, _, err := m.Active(a.key())
					if err != nil {
						return err
					}
					status, err := proxy.NewTLS(router).Inspect(current)
					if err != nil {
						return err
					}
					return json.NewEncoder(out).Encode(status)
				}
				return m.ApplyProxy(ctx, a.key(), func(current deploy.Application, c deploy.Candidate) (deploy.Application, error) {
					return proxy.NewTLS(router).Renew(ctx, current, c)
				})
			}
		})
		return true
	}
	return false
}
