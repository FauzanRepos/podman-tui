// Package quadlet provides optional, local Linux boot recovery using Podman
// Quadlet. Install never starts or enables a service: Quadlet's generator uses
// the [Install] section at boot. Do not simultaneously deploy with podman-tui
// and supervise the same container with systemd. Stop either supervisor before
// handing control to the other. Environment values are for non-secret data;
// use Podman Secret references for secrets. No sudo or elevation is performed.
package quadlet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/containers/podman-tui/deploy"
)

const marker = "# podman-tui quadlet v1 app="

var (
	idPattern     = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	imagePattern  = regexp.MustCompile(`^(sha256:)?[0-9a-fA-F]{64}$`)
	envPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type runner func(context.Context, string, []string, []string) ([]byte, error)

// Manager manages optional systemd supervision without creating a platform daemon.
// Operations use the deployment store's cross-process advisory lock, released on process exit.
type Manager struct {
	local                  bool
	platform               string
	uid                    int
	dir                    string
	configErr              error
	systemdDir, cgroupFile string
	generators             []string
	systemctl              string
	run                    runner
	// transactionStep is an interruption seam used by crash-recovery tests.
	transactionStep func(string, int)
}

// New does not probe the host or modify files. local must describe the actual
// Podman connection; remote Podman connections are never supported.
func New(local bool) *Manager {
	m := &Manager{
		local: local, platform: runtime.GOOS, uid: os.Geteuid(),
		systemdDir: "/run/systemd/system", cgroupFile: "/sys/fs/cgroup/cgroup.controllers",
		generators: []string{
			"/usr/lib/systemd/system-generators/podman-system-generator",
			"/usr/libexec/systemd/system-generators/podman-system-generator",
			"/lib/systemd/system-generators/podman-system-generator",
			"/usr/libexec/podman/quadlet", "/usr/lib/podman/quadlet",
		},
		run: execute,
	}
	if m.uid == 0 {
		m.dir = "/etc/containers/systemd"
	} else {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			var home string
			home, m.configErr = os.UserHomeDir()
			base = filepath.Join(home, ".config")
		}
		if !filepath.IsAbs(base) {
			m.configErr = errors.New("XDG_CONFIG_HOME must be absolute")
		}
		m.dir = filepath.Join(base, "containers", "systemd")
	}
	return m
}

func execute(ctx context.Context, name string, args, overrides []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	for _, value := range overrides {
		key := strings.SplitN(value, "=", 2)[0] + "="
		filtered := cmd.Env[:0]
		for _, old := range cmd.Env {
			if !strings.HasPrefix(old, key) {
				filtered = append(filtered, old)
			}
		}
		cmd.Env = append(filtered, value)
	}
	cmd.WaitDelay = 2 * time.Second
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.data, err
}

func (m *Manager) probe() (string, string, error) {
	if !m.local || m.platform != "linux" {
		return "", "", errors.New("Quadlet requires a local Linux Podman connection")
	}
	if m.configErr != nil {
		return "", "", m.configErr
	}
	if err := cleanValue(m.dir); err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(m.dir) {
		return "", "", errors.New("Quadlet directory must be absolute")
	}
	if fi, err := os.Stat(m.systemdDir); err != nil || !fi.IsDir() {
		return "", "", errors.New("systemd is not running on this host")
	}
	if fi, err := os.Stat(m.cgroupFile); err != nil || !fi.Mode().IsRegular() {
		return "", "", errors.New("Quadlet requires cgroup v2")
	}
	ctl := m.systemctl
	if ctl == "" {
		var err error
		ctl, err = exec.LookPath("systemctl")
		if err != nil {
			return "", "", fmt.Errorf("systemctl: %w", err)
		}
	}
	for _, path := range m.generators {
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0 {
			return ctl, path, nil
		}
	}
	return "", "", errors.New("no executable Podman Quadlet generator found")
}

func appID(a deploy.Application) error {
	if !idPattern.MatchString(a.ID) {
		return errors.New("application ID must be 32 hexadecimal characters")
	}
	return nil
}

func cleanValue(s string) error {
	for _, r := range s {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return errors.New("Quadlet values must not contain control characters or line separators")
		}
	}
	return nil
}

// quote uses systemd syntax, not shell syntax. Percent is a systemd specifier.
func quote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "%", "%%")
	return "\"" + s + "\""
}

type metadata struct {
	Version     int
	Application deploy.Application
	Release     deploy.Release
	Enabled     bool
}

func (m *Manager) names(id string) (string, string, string) {
	private := filepath.Join(m.dir, ".ptui-quadlet")
	return filepath.Join(m.dir, "ptui-"+id+".container"),
		filepath.Join(private, id+".env"), filepath.Join(private, id+".json")
}

// The stable environment remains for compatibility, but boot-visible units use
// immutable bytes so an interrupted replacement cannot pair releases and envs.
func environmentGeneration(stable string, data []byte) string {
	return strings.TrimSuffix(stable, ".env") + fmt.Sprintf(".%x.env", sha256.Sum256(data))
}

func render(a deploy.Application, r deploy.Release, enabled, rootless bool, envPath string) ([]byte, []byte, error) {
	if err := a.Validate(); err != nil {
		return nil, nil, err
	}
	if err := appID(a); err != nil {
		return nil, nil, err
	}
	if !idPattern.MatchString(r.ID) {
		return nil, nil, errors.New("release ID must be 32 hexadecimal characters")
	}
	if !imagePattern.MatchString(r.ImageID) {
		return nil, nil, errors.New("release image must be a full pinned image ID")
	}
	cfg := a.Config()
	values := []string{a.Name, a.Image, a.Profile, a.CurrentRelease, r.ContainerID, envPath,
		deploy.OwnerLabel, deploy.OwnerValue, deploy.AppLabel, deploy.ReleaseLabel, cfg.RestartPolicy}
	values = append(values, cfg.Networks...)
	values = append(values, cfg.Command...)
	for k, v := range cfg.Env {
		values = append(values, k, v)
	}
	for _, s := range cfg.Secrets {
		values = append(values, s.Name, s.Target, s.Type)
	}
	for _, p := range cfg.Ports {
		values = append(values, p.IP, p.Protocol)
	}
	for _, v := range cfg.Volumes {
		values = append(values, v.Source, v.Target)
	}
	for _, v := range values {
		if err := cleanValue(v); err != nil {
			return nil, nil, err
		}
	}
	if cfg.MemoryBytes < 0 || cfg.CPUQuota < 0 {
		return nil, nil, errors.New("resource limits must not be negative")
	}
	var unit, env strings.Builder
	unit.WriteString(marker + a.ID + "\n[Unit]\nDescription=" + quote("podman-tui "+a.Name) + "\n\n[Container]\n")
	directive := func(key, value string) { unit.WriteString(key + "=" + quote(value) + "\n") }
	directive("Image", r.ImageID)
	directive("ContainerName", "ptui-"+a.ID+"-"+r.ID)
	directive("Label", deploy.OwnerLabel+"="+deploy.OwnerValue)
	directive("Label", deploy.AppLabel+"="+a.ID)
	directive("Label", deploy.ReleaseLabel+"="+r.ID)
	directive("Label", deploy.RoleLabel+"=runtime")
	directive("PodmanArgs", "--security-opt=no-new-privileges")
	directive("ImageVolume", "ignore")
	// Publish EnvironmentFile only after its content-addressed path is known.
	env.WriteString(marker + a.ID + "\n")
	keys := make([]string, 0, len(cfg.Env))
	for key := range cfg.Env {
		if !envPattern.MatchString(key) {
			return nil, nil, fmt.Errorf("invalid environment name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// [Container] EnvironmentFile is passed to Podman's --env-file parser,
		// which preserves values literally rather than unquoting shell syntax.
		env.WriteString(key + "=" + cfg.Env[key] + "\n")
	}
	envData := []byte(env.String())
	directive("EnvironmentFile", environmentGeneration(envPath, envData))
	for _, s := range cfg.Secrets {
		if !secretPattern.MatchString(s.Name) {
			return nil, nil, errors.New("invalid secret name")
		}
		switch s.Type {
		case "env":
			if !envPattern.MatchString(s.Target) {
				return nil, nil, errors.New("invalid secret environment target")
			}
		case "mount":
			if s.Target == "" || strings.ContainsAny(s.Target, ",=") {
				return nil, nil, errors.New("invalid secret mount target")
			}
		default:
			return nil, nil, errors.New("secret type must be env or mount")
		}
		value := s.Name + ",type=" + s.Type + ",target=" + s.Target
		if s.Type == "mount" {
			value += ",uid=" + strconv.FormatUint(uint64(s.UID), 10) + ",gid=" + strconv.FormatUint(uint64(s.GID), 10) + ",mode=0400"
		}
		directive("Secret", value)
	}
	for _, p := range cfg.Ports {
		if p.Host == 0 && a.Proxy != nil && p.Container == a.Proxy.ContainerPort {
			endpoint, err := url.Parse(r.Endpoint)
			if err != nil || endpoint.Hostname() != a.Proxy.BackendHost {
				return nil, nil, errors.New("Quadlet proxy requires the release's published endpoint")
			}
			port, err := strconv.ParseUint(endpoint.Port(), 10, 16)
			if err != nil {
				return nil, nil, err
			}
			p.Host = uint16(port)
		}
		if p.Container == 0 || p.Host == 0 {
			return nil, nil, errors.New("Quadlet needs explicit host ports or the release's fixed proxy endpoint")
		}
		protocol := p.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" && protocol != "udp" && protocol != "sctp" {
			return nil, nil, errors.New("invalid port protocol")
		}
		ip := p.IP
		if ip == "" {
			ip = "127.0.0.1"
		}
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return nil, nil, errors.New("published port address must be an IP literal")
		}
		if parsed.To4() == nil {
			ip = "[" + ip + "]"
		}
		directive("PublishPort", ip+":"+strconv.Itoa(int(p.Host))+":"+strconv.Itoa(int(p.Container))+"/"+protocol)
	}
	for _, v := range cfg.Volumes {
		if v.Source == "" || !filepath.IsAbs(v.Target) || strings.ContainsAny(v.Source+v.Target, ":") {
			return nil, nil, errors.New("volume source must be nonempty and target absolute; colons are not allowed")
		}
		value := v.Source + ":" + v.Target
		if v.ReadOnly {
			value += ":ro"
		}
		directive("Volume", value)
	}
	for _, network := range cfg.Networks {
		if network == "" || strings.ContainsAny(network, ",:=") {
			return nil, nil, errors.New("invalid network name")
		}
		directive("Network", network)
	}
	if len(cfg.Networks) > 0 {
		directive("NetworkAlias", "ptui-"+a.ID)
	}
	if len(cfg.Command) > 0 {
		argv := make([]string, len(cfg.Command))
		for i, arg := range cfg.Command {
			// systemd ExecStart expands dollars; preserve literal command arguments.
			argv[i] = quote(strings.ReplaceAll(arg, "$", "$$"))
		}
		unit.WriteString("Exec=" + strings.Join(argv, " ") + "\n")
	}
	unit.WriteString("\n[Service]\n")
	restart := cfg.RestartPolicy
	switch restart {
	case "", "no":
		restart = "no"
	case "always", "on-failure":
	case "unless-stopped":
		restart = "always"
	default:
		return nil, nil, errors.New("unsupported restart policy")
	}
	unit.WriteString("Restart=" + restart + "\n")
	if cfg.MemoryBytes > 0 {
		unit.WriteString("MemoryMax=" + strconv.FormatInt(cfg.MemoryBytes, 10) + "\n")
	}
	// Podman's CPU quota is microseconds per default 100000us period.
	if cfg.CPUQuota > 0 {
		unit.WriteString("CPUQuota=" + strconv.FormatInt(cfg.CPUQuota/1000, 10) + "." + fmt.Sprintf("%03d", cfg.CPUQuota%1000) + "%\n")
	}
	if enabled {
		target := "multi-user.target"
		if rootless {
			target = "default.target"
		}
		unit.WriteString("\n[Install]\nWantedBy=" + target + "\n")
	}
	return []byte(unit.String()), envData, nil
}

func directory(path string, mode os.FileMode, private bool) error {
	var created []string
	for parent := path; ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		created = append(created, parent)
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe directory %s", path)
	}
	if private && (fi.Mode().Perm() != 0700 || !ownedByProcess(fi)) {
		return fmt.Errorf("directory %s must be owned by this user with mode 0700", path)
	}
	for _, dir := range created {
		if err := syncDirectory(dir); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.configErr != nil {
		return nil, m.configErr
	}
	if err := cleanValue(m.dir); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(m.dir) {
		return nil, errors.New("Quadlet directory must be absolute")
	}
	if err := directory(m.dir, 0755, false); err != nil {
		return nil, err
	}
	private := filepath.Join(m.dir, ".ptui-quadlet")
	if err := directory(private, 0700, true); err != nil {
		return nil, err
	}
	store, err := deploy.NewStore(filepath.Join(private, "locks"), "quadlet")
	if err != nil {
		return nil, err
	}
	session, err := store.Begin()
	if err != nil {
		return nil, err
	}
	return func() { _ = session.Close() }, nil
}

func (m *Manager) lockRecovered(ctx context.Context, ctl string) (func(), error) {
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			unlock()
		}
	}()
	if err := m.recoverTransaction(ctl); err != nil {
		return nil, fmt.Errorf("Quadlet transaction recovery: %w", err)
	}
	handedOff = true
	return unlock, nil
}

type savedFile struct {
	path   string
	data   []byte
	exists bool
}

func readPrivate(path string) ([]byte, bool, error) {
	return readPrivateBounded(path, maxQuadletFile)
}

func readPrivateBounded(path string, maxSize int64) ([]byte, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0600 || !ownedByProcess(fi) {
		return nil, false, fmt.Errorf("unsafe file %s: expected user-owned regular mode 0600", path)
	}
	if fi.Size() > maxSize {
		return nil, false, fmt.Errorf("Quadlet file %s exceeds size limit", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !os.SameFile(fi, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm() != 0600 || !ownedByProcess(actual) {
		return nil, false, fmt.Errorf("Quadlet file %s changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err == nil && int64(len(data)) > maxSize {
		err = fmt.Errorf("Quadlet file %s exceeds size limit", path)
	}
	return data, true, err
}

// snapshot rejects unowned unit, env and metadata targets, including symlinks.
func (m *Manager) snapshot(id string) ([]savedFile, *metadata, error) {
	unit, env, meta := m.names(id)
	files := make([]savedFile, 0, 3)
	for _, path := range []string{env, meta, unit} {
		data, exists, err := readPrivate(path)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, savedFile{path, data, exists})
	}
	var md *metadata
	if files[1].exists {
		md = new(metadata)
		if err := json.Unmarshal(files[1].data, md); err != nil {
			return nil, nil, fmt.Errorf("invalid Quadlet metadata: %w", err)
		}
		if md.Version != 1 || md.Application.ID != id {
			return nil, nil, errors.New("unowned Quadlet metadata")
		}
	}
	for _, i := range []int{0, 2} {
		if files[i].exists && !strings.HasPrefix(string(files[i].data), marker+id+"\n") {
			return nil, nil, fmt.Errorf("unmanaged Quadlet file %s", files[i].path)
		}
	}
	// Partial state without a recoverable journal must never be adopted.
	if (files[0].exists || files[1].exists || files[2].exists) &&
		!(files[0].exists && files[1].exists && files[2].exists) {
		return nil, nil, errors.New("incomplete Quadlet state; refusing to adopt files")
	}
	if files[2].exists {
		if err := m.checkEnvironment(id, files[0].data, files[2].data); err != nil {
			return nil, nil, err
		}
	}
	return files, md, nil
}

func atomicWrite(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".ptui-tmp-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (m *Manager) ctl(ctx context.Context, ctl string, args ...string) ([]byte, error) {
	if m.uid != 0 {
		args = append([]string{"--user"}, args...)
	}
	out, err := m.run(ctx, ctl, args, nil)
	if err != nil {
		text := string(out)
		if len(text) > 4096 {
			text = text[:4096]
		}
		return out, fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return out, nil
}

func (m *Manager) preflight(ctx context.Context, generator, id string, unit []byte) error {
	dir, err := os.MkdirTemp("", "ptui-quadlet-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := atomicWrite(filepath.Join(dir, "ptui-"+id+".container"), unit); err != nil {
		return err
	}
	args := []string{"--dryrun"}
	if m.uid != 0 {
		args = append(args, "--user")
	}
	out, err := m.run(ctx, generator, args, []string{"QUADLET_UNIT_DIRS=" + dir})
	if err != nil || !strings.Contains(string(out), "ptui-"+id+".service") {
		text := string(out)
		if len(text) > 4096 {
			text = text[:4096]
		}
		if err == nil {
			err = errors.New("generator did not produce the expected service")
		}
		return fmt.Errorf("Quadlet preflight: %w: %s", err, text)
	}
	return nil
}

// A single journal suffices because all operations share the cross-process lock.
// Paths are derived from the validated ID, never supplied by journal contents.
const maxQuadletFile = 4 << 20
const maxQuadletJournal = 40 << 20

type journalFile struct {
	Data   []byte
	Exists bool
}

type transactionJournal struct {
	Version int
	ID      string
	Before  [3]journalFile
	After   [3]journalFile
}

func (m *Manager) journalPath() string {
	return filepath.Join(m.dir, ".ptui-quadlet", "transaction.json")
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func durableRemove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (m *Manager) step(stage string, index int) {
	if m.transactionStep != nil {
		m.transactionStep(stage, index)
	}
}

func validateGeneration(id string, files [3]journalFile) error {
	exists := files[0].Exists
	for _, file := range files {
		if len(file.Data) > maxQuadletFile || file.Exists != exists || (!file.Exists && len(file.Data) != 0) {
			return errors.New("invalid Quadlet journal generation")
		}
	}
	if !exists {
		return nil
	}
	for _, index := range []int{0, 2} {
		if !bytes.HasPrefix(files[index].Data, []byte(marker+id+"\n")) {
			return errors.New("unowned Quadlet journal generation")
		}
	}
	var md metadata
	if err := json.Unmarshal(files[1].Data, &md); err != nil {
		return fmt.Errorf("invalid Quadlet journal metadata: %w", err)
	}
	if md.Version != 1 || md.Application.ID != id {
		return errors.New("unowned Quadlet journal metadata")
	}
	return nil
}

func (m *Manager) readJournal() (*transactionJournal, error) {
	data, exists, err := readPrivateBounded(m.journalPath(), maxQuadletJournal)
	if err != nil || !exists {
		return nil, err
	}
	var journal transactionJournal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return nil, fmt.Errorf("invalid Quadlet transaction journal: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing Quadlet transaction journal data")
	}
	if journal.Version != 1 || !idPattern.MatchString(journal.ID) {
		return nil, errors.New("invalid Quadlet transaction identity")
	}
	for _, files := range [][3]journalFile{journal.Before, journal.After} {
		if err := validateGeneration(journal.ID, files); err != nil {
			return nil, err
		}
	}
	return &journal, nil
}

func journalMatches(data []byte, exists bool, file journalFile) bool {
	return exists == file.Exists && bytes.Equal(data, file.Data)
}

func (m *Manager) transactionPaths(id string) [3]string {
	unit, env, meta := m.names(id)
	return [3]string{env, meta, unit}
}

func (m *Manager) checkEnvironment(id string, env, unit []byte) error {
	_, stable, _ := m.names(id)
	generation := environmentGeneration(stable, env)
	wantStable := "EnvironmentFile=" + quote(stable)
	wantGeneration := "EnvironmentFile=" + quote(generation)
	found := false
	for len(unit) != 0 {
		line, rest, _ := bytes.Cut(unit, []byte("\n"))
		unit = rest
		if !bytes.HasPrefix(line, []byte("EnvironmentFile=")) {
			continue
		}
		if found || (string(line) != wantStable && string(line) != wantGeneration) {
			return errors.New("unowned Quadlet environment reference")
		}
		found = true
		if string(line) == wantGeneration {
			data, exists, err := readPrivate(generation)
			if err != nil {
				return err
			}
			if !exists || !bytes.Equal(data, env) {
				return errors.New("missing or changed immutable Quadlet environment")
			}
		}
	}
	if !found {
		return errors.New("missing Quadlet environment reference")
	}
	return nil
}

func (m *Manager) prepareEnvironment(id string, env []byte) error {
	_, stable, _ := m.names(id)
	path := environmentGeneration(stable, env)
	data, exists, err := readPrivate(path)
	if err != nil {
		return err
	}
	if exists {
		if !bytes.Equal(data, env) {
			return errors.New("unmanaged immutable Quadlet environment")
		}
		return nil
	}
	return atomicWrite(path, env)
}

// Only remove the obsolete generation after both the unit and journal no longer
// reference it. A foreign/replaced path is deliberately left untouched.
func (m *Manager) cleanupEnvironment(id string, obsolete, keep journalFile) error {
	if !obsolete.Exists || (keep.Exists && bytes.Equal(obsolete.Data, keep.Data)) {
		return nil
	}
	_, stable, _ := m.names(id)
	path := environmentGeneration(stable, obsolete.Data)
	data, exists, err := readPrivate(path)
	if err != nil || !exists || !bytes.Equal(data, obsolete.Data) {
		return nil
	}
	return durableRemove(path)
}

func (m *Manager) checkTransactionPath(path string, journal *transactionJournal, index int) error {
	data, exists, err := readPrivate(path)
	if err != nil {
		return err
	}
	if !journalMatches(data, exists, journal.Before[index]) && !journalMatches(data, exists, journal.After[index]) {
		return fmt.Errorf("unmanaged or changed Quadlet transaction target %s", path)
	}
	return nil
}

// recoverTransaction always rolls back an interrupted operation. The journal is
// kept until every before-file and the daemon reload are durable/successful, so
// a second crash during recovery can safely repeat the same rollback.
func (m *Manager) recoverTransaction(ctl string) error {
	journal, err := m.readJournal()
	if err != nil || journal == nil {
		return err
	}
	if ctl == "" {
		ctl, _, err = m.probe()
		if err != nil {
			return err
		}
	}
	paths := m.transactionPaths(journal.ID)
	// Validate all targets before touching any of them, including partial installs.
	for index, path := range paths {
		if err := m.checkTransactionPath(path, journal, index); err != nil {
			return err
		}
	}
	if journal.Before[2].Exists {
		if err := m.checkEnvironment(journal.ID, journal.Before[0].Data, journal.Before[2].Data); err != nil {
			return err
		}
	}
	// The after-generation need only exist when its unit was published. Failed
	// preparation must still be able to roll back without adopting its target.
	currentUnit, unitExists, err := readPrivate(paths[2])
	if err != nil {
		return err
	}
	if unitExists && journal.After[2].Exists && bytes.Equal(currentUnit, journal.After[2].Data) {
		if err := m.checkEnvironment(journal.ID, journal.After[0].Data, currentUnit); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for index, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.checkTransactionPath(path, journal, index); err != nil {
			return err
		}
		old := journal.Before[index]
		if old.Exists {
			err = atomicWrite(path, old.Data)
		} else {
			err = durableRemove(path)
		}
		if err != nil {
			return err
		}
		m.step("recovery", index+1)
	}
	if _, err := m.ctl(ctx, ctl, "daemon-reload"); err != nil {
		return err
	}
	m.step("recovery-reload", 0)
	if err := durableRemove(m.journalPath()); err != nil {
		return err
	}
	return m.cleanupEnvironment(journal.ID, journal.After[0], journal.Before[0])
}

// commit durably records both known generations before the first file change.
// No runtime action is used, particularly no ExecStop against a replaced unit.
func (m *Manager) commit(ctx context.Context, ctl string, before []savedFile, after [][]byte, remove bool) error {
	if len(before) != 3 || (!remove && len(after) != 3) {
		return errors.New("invalid Quadlet transaction files")
	}
	id := strings.TrimSuffix(filepath.Base(before[1].path), ".json")
	if !idPattern.MatchString(id) {
		return errors.New("invalid Quadlet transaction identity")
	}
	journal := transactionJournal{Version: 1, ID: id}
	paths := m.transactionPaths(id)
	for index, old := range before {
		if old.path != paths[index] {
			return errors.New("invalid Quadlet transaction path")
		}
		journal.Before[index] = journalFile{Data: old.data, Exists: old.exists}
		if !remove {
			journal.After[index] = journalFile{Data: after[index], Exists: true}
		}
	}
	for _, files := range [][3]journalFile{journal.Before, journal.After} {
		if err := validateGeneration(id, files); err != nil {
			return err
		}
	}
	if _, exists, err := readPrivateBounded(m.journalPath(), maxQuadletJournal); err != nil {
		return err
	} else if exists {
		return errors.New("Quadlet transaction recovery required")
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(data) > maxQuadletJournal {
		return errors.New("Quadlet transaction journal too large")
	}
	if err := atomicWrite(m.journalPath(), data); err != nil {
		return err
	}
	m.step("journal", 0)
	if !remove {
		if err := m.prepareEnvironment(id, after[0]); err != nil {
			if recoveryErr := m.recoverTransaction(ctl); recoveryErr != nil {
				return fmt.Errorf("Quadlet environment preparation failed: %w; rollback: %v", err, recoveryErr)
			}
			return err
		}
	}
	m.step("commit", 0)
	var failure error
	// Migrate legacy stable-env units before changing the compatibility env.
	// Removal unpublishes the unit before deleting any referenced stable file.
	order := [3]int{1, 2, 0}
	if remove {
		order = [3]int{2, 1, 0}
	}
	for boundary, index := range order {
		path := paths[index]
		if failure = ctx.Err(); failure != nil {
			break
		}
		if failure = m.checkTransactionPath(path, &journal, index); failure != nil {
			break
		}
		if remove {
			failure = durableRemove(path)
		} else {
			failure = atomicWrite(path, after[index])
		}
		if failure != nil {
			break
		}
		m.step("commit", boundary+1)
	}
	if failure == nil {
		_, failure = m.ctl(ctx, ctl, "daemon-reload")
	}
	if failure == nil {
		if err := durableRemove(m.journalPath()); err != nil {
			return err
		}
		return m.cleanupEnvironment(id, journal.Before[0], journal.After[0])
	}
	if err := m.recoverTransaction(ctl); err != nil {
		return fmt.Errorf("Quadlet update failed: %w; rollback: %v", failure, err)
	}
	return fmt.Errorf("Quadlet update rolled back: %w", failure)
}

// Install writes a pinned boot-recovery unit. enabled controls only [Install];
// this method never starts, restarts, or enables the generated service. For
// rootless boot without login, an administrator must separately arrange linger.
func (m *Manager) Install(ctx context.Context, a deploy.Application, r deploy.Release, enabled bool) error {
	ctl, generator, err := m.probe()
	if err != nil {
		return err
	}
	unlock, err := m.lockRecovered(ctx, ctl)
	if err != nil {
		return err
	}
	defer unlock()
	return m.installLocked(ctx, ctl, generator, a, r, enabled)
}

func (m *Manager) installLocked(ctx context.Context, ctl, generator string, a deploy.Application, r deploy.Release, enabled bool) error {
	if err := a.Validate(); err != nil {
		return err
	}
	_, envPath, _ := m.names(a.ID)
	unit, env, err := render(a, r, enabled, m.uid != 0, envPath)
	if err != nil {
		return err
	}
	before, _, err := m.snapshot(a.ID)
	if err != nil {
		return err
	}
	if err := m.preflight(ctx, generator, a.ID, unit); err != nil {
		return err
	}
	md, err := json.Marshal(metadata{Version: 1, Application: a, Release: r, Enabled: enabled})
	if err != nil {
		return err
	}
	return m.commit(ctx, ctl, before, [][]byte{env, md, unit}, false)
}

// Status reports systemctl's ActiveState, SubState and Result verbatim, or
// "not-installed" when no owned unit exists. It does not start the service.
func (m *Manager) Status(ctx context.Context, a deploy.Application) (string, error) {
	ctl, _, err := m.probe()
	if err != nil {
		return "", err
	}
	if err := appID(a); err != nil {
		return "", err
	}
	unlock, err := m.lockRecovered(ctx, ctl)
	if err != nil {
		return "", err
	}
	defer unlock()
	_, md, err := m.snapshot(a.ID)
	if err != nil {
		return "", err
	}
	if md == nil {
		return "not-installed", nil
	}
	out, err := m.ctl(ctx, ctl, "show", "--property=ActiveState,SubState,Result", "ptui-"+a.ID+".service")
	return strings.TrimSpace(string(out)), err
}

// Disable removes boot activation by re-rendering saved private metadata; it
// does not stop a running service or use systemctl disable on a generated unit.
func (m *Manager) Disable(ctx context.Context, a deploy.Application) error {
	ctl, generator, err := m.probe()
	if err != nil {
		return err
	}
	if err := appID(a); err != nil {
		return err
	}
	unlock, err := m.lockRecovered(ctx, ctl)
	if err != nil {
		return err
	}
	defer unlock()
	before, md, err := m.snapshot(a.ID)
	if err != nil {
		return err
	}
	if md == nil {
		return nil
	}
	_, envPath, _ := m.names(a.ID)
	unit, env, err := render(md.Application, md.Release, false, m.uid != 0, envPath)
	if err != nil {
		return err
	}
	if err := m.preflight(ctx, generator, a.ID, unit); err != nil {
		return err
	}
	md.Enabled = false
	data, err := json.Marshal(md)
	if err != nil {
		return err
	}
	return m.commit(ctx, ctl, before, [][]byte{env, data, unit}, false)
}

// Remove deletes only owned Quadlet unit, environment and metadata files. It
// never stops/deletes containers or touches volumes, networks or secrets.
func (m *Manager) Remove(ctx context.Context, a deploy.Application) error {
	ctl, _, err := m.probe()
	if err != nil {
		return err
	}
	if err := appID(a); err != nil {
		return err
	}
	unlock, err := m.lockRecovered(ctx, ctl)
	if err != nil {
		return err
	}
	defer unlock()
	before, md, err := m.snapshot(a.ID)
	if err != nil {
		return err
	}
	if md == nil {
		return nil
	}
	return m.commit(ctx, ctl, before, nil, true)
}
