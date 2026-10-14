package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/deploy/platform"
	podmanRuntime "github.com/containers/podman-tui/deploy/podman"
	"github.com/rs/zerolog"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func decodeApplication(file string, input io.Reader) (deploy.Application, error) {
	var a deploy.Application
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return a, err
		}
		defer f.Close()
		input = f
	}
	decoder := json.NewDecoder(io.LimitReader(input, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return a, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return a, errors.New("application file has trailing content")
	}
	return a, nil
}

func emit(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}

func platformOpen(cmd *cobra.Command, connect bool) (*deploy.Manager, *podmanRuntime.Runtime, func(), error) {
	name, _ := cmd.Flags().GetString("connection")
	root, _ := cmd.Flags().GetString("state-dir")
	local, _ := cmd.Flags().GetBool("local-integrations")
	conn, err := platform.Resolve(name)
	if err != nil {
		return nil, nil, nil, err
	}
	return platform.Open(cmd.Context(), root, conn, local, connect)
}

type safePlatformError struct{ cause error }

func (e safePlatformError) Error() string { return deploy.Sanitize(e.cause.Error()) }
func (e safePlatformError) Unwrap() error { return e.cause }

func operation(use, short string, argc int, connect bool, run func(*cobra.Command, []string, *deploy.Manager, *podmanRuntime.Runtime) error) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(argc), RunE: func(cmd *cobra.Command, args []string) (retErr error) {
		defer func() {
			if retErr != nil {
				retErr = safePlatformError{cause: retErr}
			}
		}()
		timeout, _ := cmd.Flags().GetDuration("timeout")
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		cmd.SetContext(ctx)
		manager, adapter, close, err := platformOpen(cmd, connect)
		if err != nil {
			return err
		}
		defer close()
		return run(cmd, args, manager, adapter)
	}}
}

func init() {
	parent := &cobra.Command{Use: "platform", Short: "Manage applications and releases without a platform daemon", SilenceUsage: true, SilenceErrors: true}
	parent.PersistentPreRun = func(*cobra.Command, []string) {
		zerolog.SetGlobalLevel(zerolog.Disabled)
		logrus.SetOutput(io.Discard)
	}
	parent.PersistentFlags().String("connection", "", "Configured Podman connection (default connection when omitted)")
	parent.PersistentFlags().String("state-dir", "", "Private connection-scoped metadata root")
	parent.PersistentFlags().Bool("local-integrations", false, "Explicitly authorize Nginx/TLS on this controller host for a remote Podman backend")
	parent.PersistentFlags().Duration("timeout", 10*time.Minute, "Overall operation timeout; readiness has its own configuration")
	rootCmd.AddCommand(parent)
	parent.AddCommand(operation("list", "List persisted applications and services", 0, false, func(cmd *cobra.Command, _ []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		apps, err := m.List()
		if err != nil {
			return err
		}
		return emit(cmd, apps)
	}))
	create := operation("create NAME", "Create application metadata from an image, repository or configuration file", 1, false, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		file, _ := cmd.Flags().GetString("file")
		var a deploy.Application
		var err error
		if file != "" {
			a, err = decodeApplication(file, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if a.Name != "" && a.Name != args[0] {
				return errors.New("file name does not match command application")
			}
		} else {
			image, _ := cmd.Flags().GetString("image")
			repo, _ := cmd.Flags().GetString("repository")
			revision, _ := cmd.Flags().GetString("revision")
			trusted, _ := cmd.Flags().GetBool("trust-source")
			a.Image = image
			if repo != "" {
				a.Repository = &deploy.GitConfig{URL: repo, Revision: revision, Trusted: trusted}
			}
		}
		a.Name = args[0]
		created, err := m.Create(a)
		if err != nil {
			return err
		}
		return emit(cmd, created)
	})
	create.Flags().String("file", "", "Application JSON file, or - for stdin (non-sensitive configuration and secret references only)")
	create.Flags().String("image", "", "OCI image reference")
	create.Flags().String("repository", "", "HTTPS or SSH Git repository")
	create.Flags().String("revision", "main", "Git branch, tag or commit")
	create.Flags().Bool("trust-source", false, "Authorize repository-supplied Containerfile build instructions")
	parent.AddCommand(create)
	for _, action := range []string{"inspect", "export", "history", "status"} {
		action := action
		parent.AddCommand(operation(action+" NAME", "Inspect application "+action, 1, action == "status", func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
			switch action {
			case "history":
				v, err := m.History(args[0])
				if err != nil {
					return err
				}
				return emit(cmd, v)
			case "status":
				v, err := m.Status(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return emit(cmd, v)
			default:
				a, err := m.Get(args[0])
				if err != nil {
					return err
				}
				if action == "export" {
					a.ID = ""
					a.CurrentRelease = ""
					a.CreatedAt = time.Time{}
				}
				return emit(cmd, a)
			}
		}))
	}
	configure := operation("configure NAME", "Save desired configuration; apply with deploy", 1, false, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		file, _ := cmd.Flags().GetString("file")
		if file == "" {
			return errors.New("--file is required")
		}
		a, err := decodeApplication(file, cmd.InOrStdin())
		if err != nil {
			return err
		}
		if a.Name == "" {
			a.Name = args[0]
		}
		return m.Configure(args[0], a)
	})
	configure.Flags().String("file", "", "Application JSON file, or - for stdin")
	parent.AddCommand(configure)
	parent.AddCommand(operation("rename NAME NEW-NAME", "Rename metadata while retaining stable managed identity", 2, false, func(_ *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		return m.Rename(args[0], args[1])
	}))
	for _, action := range []string{"start", "stop", "restart"} {
		action := action
		parent.AddCommand(operation(action+" NAME", "Operate the current owned release", 1, true, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
			return m.Lifecycle(cmd.Context(), args[0], action)
		}))
	}
	for _, action := range []string{"deploy", "redeploy", "rebuild", "rollback"} {
		action := action
		argc := 1
		if action == "rollback" {
			argc = 2
		}
		use := action + " NAME"
		if argc == 2 {
			use += " RELEASE-ID"
		}
		command := operation(use, "Build/deploy a checked candidate; preserve the known-good release on failure", argc, true, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
			maintenance, _ := cmd.Flags().GetBool("maintenance")
			opts := deploy.DeployOptions{Maintenance: maintenance, Rebuild: action == "rebuild"}
			if action == "rollback" {
				opts.RollbackRelease = args[1]
			}
			var logs deploy.LogBuffer
			result, err := m.Deploy(cmd.Context(), args[0], opts, func(e deploy.Event) { fmt.Fprintln(cmd.ErrOrStderr(), deploy.Sanitize(e.Stage)) }, &logs)
			if text := logs.String(); text != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), text)
			}
			if err != nil {
				return err
			}
			return emit(cmd, result)
		})
		command.Flags().Bool("maintenance", false, "Authorize temporary downtime for fixed ports or exclusive persistent writers; restore old container on failure")
		parent.AddCommand(command)
	}
	for _, action := range []string{"delete", "cleanup", "recover-abort"} {
		action := action
		command := operation(action+" NAME", "Confirm an owned destructive operation; volumes and unmanaged resources remain untouched", 1, true, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
			yes, _ := cmd.Flags().GetBool("yes")
			switch action {
			case "delete":
				return m.Delete(cmd.Context(), args[0], yes)
			case "cleanup":
				return m.Cleanup(cmd.Context(), args[0], yes)
			default:
				return m.RecoverAbort(cmd.Context(), args[0], yes)
			}
		})
		command.Flags().Bool("yes", false, "Confirm this exact application operation")
		parent.AddCommand(command)
	}
	secret := operation("secret-create NAME", "Read a secret from stdin directly into Podman; never persist it in metadata", 1, true, func(cmd *cobra.Command, args []string, _ *deploy.Manager, adapter *podmanRuntime.Runtime) error {
		return adapter.CreateSecret(cmd.Context(), args[0], cmd.InOrStdin())
	})
	parent.AddCommand(secret)
	service := operation("service-create NAME", "Provision an optional private backing service with persistent storage", 1, true, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		kind, _ := cmd.Flags().GetString("type")
		secret, _ := cmd.Flags().GetString("secret")
		network, _ := cmd.Flags().GetString("network")
		var output deploy.LogBuffer
		result, err := platform.CreateService(cmd.Context(), m, args[0], kind, secret, network, &output)
		if err != nil {
			return err
		}
		return emit(cmd, result)
	})
	service.Flags().String("type", "postgres", "postgres, mariadb, mysql or redis")
	service.Flags().String("secret", "", "Existing Podman credential secret (required for database images)")
	service.Flags().String("network", "", "Private service network; no database ports published")
	parent.AddCommand(service)
	link := operation("service-link APP SERVICE", "Authorize private-network connection; optionally inject non-sensitive host settings", 2, false, func(cmd *cobra.Command, args []string, m *deploy.Manager, _ *podmanRuntime.Runtime) error {
		inject, _ := cmd.Flags().GetBool("inject-host")
		return platform.LinkService(m, args[0], args[1], inject)
	})
	link.Flags().Bool("inject-host", false, "Authorize non-sensitive SERVICE_HOST injection; secret injection must be explicit configuration references")
	parent.AddCommand(link)
	addPlatformIntegrations(parent)
}
