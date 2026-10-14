package proxy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/containers/podman-tui/deploy"
)

func renewalArguments(executable, stateDir, connection string, a deploy.Application) ([]string, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if len(a.ID) != 32 {
		return nil, errors.New("renewal requires managed application identity")
	}
	for _, value := range []string{executable, stateDir} {
		if err := safePath(value); err != nil {
			return nil, err
		}
	}
	if connection == "" || strings.ContainsAny(connection, "\x00\r\n") {
		return nil, errors.New("invalid renewal connection")
	}
	return []string{executable, "platform", "tls-renew", a.ID, "--application-id", "--state-dir", stateDir, "--connection", connection, "--local-integrations"}, nil
}
func RenewalCron(executable, stateDir, connection string, a deploy.Application) (string, error) {
	argv, err := renewalArguments(executable, stateDir, connection, a)
	if err != nil {
		return "", err
	}
	args := make([]string, len(argv))
	for i, value := range argv {
		args[i] = "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
	}
	// Cron expands percent even within shell quotes; escape it independently of shell argument quoting.
	return "0 3 * * * " + strings.ReplaceAll(strings.Join(args, " "), "%", "\\%"), nil
}
func unitControl(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	command.Stdout = nil
	command.Stderr = nil
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil {
		return errors.New("user systemd renewal operation failed; inspect the scoped timer")
	}
	return nil
}

func InstallRenewalTimer(ctx context.Context, executable, stateDir, connection string, a deploy.Application) error {
	if runtime.GOOS != "linux" {
		return errors.New("user systemd timers require Linux; use the printed cron entry with the host scheduler")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("systemd is not running; use the printed cron entry")
	}
	service, timer, err := RenewalUnits(executable, stateDir, connection, a)
	if err != nil {
		return err
	}
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return e
		}
		root = filepath.Join(home, ".config")
	}
	if err = safePath(root); err != nil {
		return err
	}
	directory := filepath.Join(root, "systemd", "user")
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	stat, err := os.Lstat(directory)
	if err != nil || !stat.IsDir() {
		return errors.New("user unit directory must be non-symlink")
	}
	lock, err := deploy.NewStore(filepath.Join(directory, ".ptui-tls-lock"), "renewal")
	if err != nil {
		return err
	}
	session, err := lock.Begin()
	if err != nil {
		return err
	}
	defer session.Close()
	unit := "ptui-tls-" + a.ID
	files := []string{filepath.Join(directory, unit+".service"), filepath.Join(directory, unit+".timer")}
	contents := []string{service, timer}
	previous := make([][]byte, 2)
	for i, file := range files {
		info, e := os.Lstat(file)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return errors.New("unsafe renewal unit file")
		}
		data, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		if !strings.HasPrefix(string(data), "# managed by podman-tui TLS application="+a.ID+"\n") {
			return errors.New("refusing to overwrite an unmanaged renewal unit")
		}
		previous[i] = data
	}
	installed := false
	for i, file := range files {
		if err = replace(file, []byte(contents[i])); err != nil {
			break
		}
	}
	if err == nil {
		err = unitControl(ctx, "daemon-reload")
	}
	if err == nil {
		err = unitControl(ctx, "enable", "--now", unit+".timer")
		installed = err == nil
	}
	if installed {
		return nil
	}
	recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var restoreErr error
	for i, file := range files {
		restoreErr = errors.Join(restoreErr, replace(file, previous[i]))
	}
	restoreErr = errors.Join(restoreErr, unitControl(recovery, "daemon-reload"))
	if previous[1] == nil {
		restoreErr = errors.Join(restoreErr, unitControl(recovery, "disable", "--now", unit+".timer"))
	}
	return errors.Join(err, restoreErr)
}
