package cmd

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/deploy/platform"
	podmanRuntime "github.com/containers/podman-tui/deploy/podman"
	"github.com/containers/podman-tui/deploy/proxy"
	"github.com/containers/podman-tui/deploy/quadlet"
	"github.com/spf13/cobra"
)

func proxyRouter(manager *deploy.Manager) (*proxy.Router, error) {
	router, ok := manager.Router.(*proxy.Router)
	if !ok {
		return nil, errors.New("Nginx integration is unavailable")
	}
	return router, nil
}

func addPlatformIntegrations(parent *cobra.Command) {
	status := operation("tls-status NAME", "Inspect certificate expiry and renewal attempts without reading key material", 1, false, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		a, _, err := m.Active(args[0])
		if err != nil {
			return err
		}
		router, err := proxyRouter(m)
		if err != nil {
			return err
		}
		value, err := proxy.NewTLS(router).Inspect(a)
		if err != nil {
			return err
		}
		return emit(cmd, value)
	})
	parent.AddCommand(status)
	for _, action := range []string{"tls-import", "tls-issue", "tls-renew"} {
		action := action
		argc := 1
		use := action + " NAME"
		if action == "tls-import" {
			argc = 3
			use += " CERTIFICATE PRIVATE-KEY"
		}
		command := operation(use, "Activate validated TLS with safe Nginx reload; no privilege escalation", argc, true, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
			name := args[0]
			byID, _ := cmd.Flags().GetBool("application-id")
			if byID {
				a, err := m.GetByID(name)
				if err != nil {
					return err
				}
				name = a.Name
			}
			router, err := proxyRouter(m)
			if err != nil {
				return err
			}
			tls := proxy.NewTLS(router)
			return m.ApplyProxy(cmd.Context(), name, func(a deploy.Application, c deploy.Candidate) (deploy.Application, error) {
				switch action {
				case "tls-import":
					return tls.Import(cmd.Context(), a, c, args[1], args[2])
				case "tls-issue":
					email, _ := cmd.Flags().GetString("email")
					staging, _ := cmd.Flags().GetBool("staging")
					agree, _ := cmd.Flags().GetBool("agree-tos")
					return tls.Issue(cmd.Context(), a, c, email, staging, agree)
				default:
					return tls.Renew(cmd.Context(), a, c)
				}
			})
		})
		command.Flags().Bool("application-id", false, "Select stable application ID (used by managed renewal schedules)")
		if action == "tls-issue" {
			command.Flags().String("email", "", "ACME account contact email")
			command.Flags().Bool("staging", false, "Use Let's Encrypt staging CA")
			command.Flags().Bool("agree-tos", false, "Explicitly agree to the selected CA terms")
		}
		parent.AddCommand(command)
	}
	schedule := operation("tls-schedule NAME", "Install a scoped Linux user renewal timer, or print a portable cron command", 1, false, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		a, _, err := m.Active(args[0])
		if err != nil {
			return err
		}
		router, err := proxyRouter(m)
		if err != nil {
			return err
		}
		tlsStatus, err := proxy.NewTLS(router).Inspect(a)
		if err != nil {
			return err
		}
		if !tlsStatus.Renewable {
			return errors.New("imported certificates have no ACME renewal request")
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		root, _ := cmd.Flags().GetString("state-dir")
		if root == "" {
			root, err = platform.DefaultStateDir()
			if err != nil {
				return err
			}
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return err
		}
		connection, _ := cmd.Flags().GetString("connection")
		if connection == "" {
			conn, err := platform.Resolve("")
			if err != nil {
				return err
			}
			connection = conn.Name
		}
		cron, _ := cmd.Flags().GetBool("cron")
		if cron {
			line, err := proxy.RenewalCron(binary, root, connection, a)
			if err != nil {
				return err
			}
			cmd.Println(line)
			return nil
		}
		yes, _ := cmd.Flags().GetBool("yes")
		if !yes {
			return errors.New("timer installation requires --yes; --cron prints an uninstalled alternative")
		}
		return proxy.InstallRenewalTimer(cmd.Context(), binary, root, connection, a)
	})
	schedule.Flags().Bool("yes", false, "Confirm scoped user timer installation")
	schedule.Flags().Bool("cron", false, "Print a quoted daily cron entry without changing crontab")
	parent.AddCommand(schedule)
	for _, action := range []string{"quadlet-enable", "quadlet-disable", "quadlet-remove", "quadlet-status"} {
		action := action
		command := operation(action+" NAME", "Manage optional local Linux startup; manual/remote container operation remains available", 1, action == "quadlet-enable", func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
			switch action {
			case "quadlet-enable":
				return m.EnableStartup(cmd.Context(), args[0], true)
			case "quadlet-disable":
				return m.DisableStartup(cmd.Context(), args[0], false, false)
			case "quadlet-remove":
				yes, _ := cmd.Flags().GetBool("yes")
				return m.DisableStartup(cmd.Context(), args[0], true, yes)
			default:
				a, err := m.Get(args[0])
				if err != nil {
					return err
				}
				status, err := quadlet.New(m.LocalPodman).Status(cmd.Context(), a)
				if err != nil {
					return err
				}
				return emit(cmd, status)
			}
		})
		if action == "quadlet-remove" {
			command.Flags().Bool("yes", false, "Confirm removal of this owned inactive startup unit")
		}
		parent.AddCommand(command)
	}
}
