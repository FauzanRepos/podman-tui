// Package podman adapts the existing Podman Go bindings to managed deployments.
package podman

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
	"regexp"
	"strconv"

	"github.com/containers/podman-tui/deploy"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	buildah "go.podman.io/buildah/define"
	nettypes "go.podman.io/common/libnetwork/types"
	"go.podman.io/podman/v6/libpod/define"
	"go.podman.io/podman/v6/pkg/bindings"
	"go.podman.io/podman/v6/pkg/bindings/containers"
	"go.podman.io/podman/v6/pkg/bindings/images"
	"go.podman.io/podman/v6/pkg/bindings/network"
	"go.podman.io/podman/v6/pkg/bindings/secrets"
	"go.podman.io/podman/v6/pkg/bindings/volumes"
	"go.podman.io/podman/v6/pkg/domain/entities"
	"go.podman.io/podman/v6/pkg/specgen"
)

type Runtime struct{ connection context.Context }

func New(ctx context.Context) *Runtime { return &Runtime{connection: ctx} }
func (r *Runtime) operation(ctx context.Context) (context.Context, func()) {
	joined, cancel := context.WithCancel(r.connection)
	if ctx.Err() != nil {
		cancel()
	}
	stop := context.AfterFunc(ctx, cancel)
	if deadline, ok := ctx.Deadline(); ok {
		timed, done := context.WithDeadline(joined, deadline)
		return timed, func() { stop(); done(); cancel() }
	}
	return joined, func() { stop(); cancel() }
}
func missing(err error) bool {
	code, _ := bindings.CheckResponseCode(err)
	return code == http.StatusNotFound
}

var immutablePattern = regexp.MustCompile(`^(sha256:)?[a-f0-9]{64}$`)

func (r *Runtime) ResolveImage(ctx context.Context, image string, out io.Writer) (string, error) {
	ctx, done := r.operation(ctx)
	defer done()
	exists, err := images.Exists(ctx, image, nil)
	if err != nil {
		return "", err
	}
	if !exists {
		if immutablePattern.MatchString(image) {
			return "", errors.New("retained immutable image is missing; restore it before rollback")
		}
		if out == nil {
			out = io.Discard
		}
		writer := io.Writer(deploy.SafeWriter{Writer: out})
		if _, err = images.Pull(ctx, image, new(images.PullOptions).WithPolicy("always").WithProgressWriter(writer)); err != nil {
			return "", err
		}
	}
	report, err := images.GetImage(ctx, image, nil)
	if err != nil {
		return "", err
	}
	return report.ID, nil
}

func (r *Runtime) Build(ctx context.Context, tree deploy.SourceTree, tag string, out io.Writer) (string, error) {
	ctx, done := r.operation(ctx)
	defer done()
	if out == nil {
		out = io.Discard
	}
	writer := deploy.SafeWriter{Writer: out}
	opts := entities.BuildOptions{BuildOptions: buildah.BuildOptions{ContextDirectory: tree.Directory, Output: tag, Out: writer, Err: writer, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true, PullPolicy: buildah.PullIfMissing, Labels: []string{deploy.OwnerLabel + "=" + deploy.OwnerValue}}}
	report, err := images.Build(ctx, []string{tree.Containerfile}, opts)
	if err != nil {
		return "", err
	}
	inspected, err := images.GetImage(ctx, report.ID, nil)
	if err != nil {
		return "", err
	}
	return inspected.ID, nil
}

func containerSpec(a deploy.Application, release deploy.Release) (*specgen.SpecGenerator, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	s := specgen.NewSpecGenerator(release.ImageID, false)
	s.ImageVolumeMode = "ignore"
	s.Name = "ptui-" + a.ID + "-" + release.ID
	s.Labels = map[string]string{deploy.OwnerLabel: deploy.OwnerValue, deploy.AppLabel: a.ID, deploy.ReleaseLabel: release.ID, deploy.RoleLabel: "runtime"}
	c := a.Config()
	s.Env = c.Env
	s.Command = c.Command
	s.RestartPolicy = c.RestartPolicy
	yes := true
	no := false
	s.NoNewPrivileges = &yes
	s.Privileged = &no
	s.HTTPProxy = &no
	s.LogConfiguration = &specgen.LogConfig{Driver: "k8s-file", Size: 4 << 20}
	for _, p := range c.Ports {
		ip := p.IP
		if ip == "" {
			ip = "127.0.0.1"
		}
		protocol := p.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		s.PortMappings = append(s.PortMappings, nettypes.PortMapping{ContainerPort: p.Container, HostPort: p.Host, HostIP: ip, Protocol: protocol, Range: 1})
	}
	for _, v := range c.Volumes {
		options := []string{"rw"}
		if v.ReadOnly {
			options = []string{"ro"}
		}
		if path.IsAbs(v.Source) {
			s.Mounts = append(s.Mounts, spec.Mount{Type: "bind", Source: v.Source, Destination: v.Target, Options: append(options, "rbind")})
		} else {
			s.Volumes = append(s.Volumes, &specgen.NamedVolume{Name: v.Source, Dest: v.Target, Options: options})
		}
	}
	s.EnvSecrets = map[string]string{}
	for _, secret := range c.Secrets {
		if secret.Type == "env" {
			s.EnvSecrets[secret.Target] = secret.Name
		} else {
			s.Secrets = append(s.Secrets, specgen.Secret{Source: secret.Name, Target: secret.Target, UID: secret.UID, GID: secret.GID, Mode: 0400})
		}
	}
	if len(c.Networks) > 0 {
		s.NetNS = specgen.Namespace{NSMode: specgen.Bridge}
		s.Networks = map[string]nettypes.PerNetworkOptions{}
		for _, name := range c.Networks {
			s.Networks[name] = nettypes.PerNetworkOptions{Aliases: []string{a.Name, "ptui-" + a.ID}}
		}
	}
	if c.MemoryBytes > 0 || c.CPUQuota > 0 {
		s.ResourceLimits = &spec.LinuxResources{}
		if c.MemoryBytes > 0 {
			s.ResourceLimits.Memory = &spec.LinuxMemory{Limit: &c.MemoryBytes}
		}
		if c.CPUQuota > 0 {
			period := uint64(100000)
			s.ResourceLimits.CPU = &spec.LinuxCPU{Period: &period, Quota: &c.CPUQuota}
		}
	}
	return s, nil
}

func (r *Runtime) Create(ctx context.Context, a deploy.Application, release deploy.Release) (deploy.Candidate, error) {
	ctx, done := r.operation(ctx)
	defer done()
	s, err := containerSpec(a, release)
	if err != nil {
		return deploy.Candidate{}, err
	}
	for _, v := range a.Config().Volumes {
		if !path.IsAbs(v.Source) {
			if err = r.ensureVolume(ctx, a, v.Source); err != nil {
				return deploy.Candidate{}, err
			}
		}
	}
	for _, name := range a.Config().Networks {
		if _, err = network.Inspect(ctx, name, nil); err != nil {
			return deploy.Candidate{}, err
		}
	}
	report, err := containers.CreateWithSpec(ctx, s, nil)
	candidate := deploy.Candidate{ID: report.ID}
	return candidate, err
}

func owned(data *define.InspectContainerData, a deploy.Application, id string) error {
	if data == nil || data.Config == nil || data.Config.Labels[deploy.OwnerLabel] != deploy.OwnerValue || data.Config.Labels[deploy.AppLabel] != a.ID || data.Config.Labels[deploy.ReleaseLabel] == "" || data.ID != id {
		return errors.New("refusing operation on a resource without exact managed application ownership")
	}
	return nil
}

func (r *Runtime) inspect(ctx context.Context, a deploy.Application, id string) (*define.InspectContainerData, error) {
	if !immutablePattern.MatchString(id) {
		return nil, errors.New("container operations require a full immutable container ID")
	}
	report, err := containers.Inspect(ctx, id, nil)
	if err != nil {
		return nil, err
	}
	if err = owned(report, a, id); err != nil {
		return nil, err
	}
	return report, nil
}
func (r *Runtime) Inspect(ctx context.Context, a deploy.Application, id string) (deploy.RuntimeStatus, error) {
	ctx, done := r.operation(ctx)
	defer done()
	report, err := r.inspect(ctx, a, id)
	if missing(err) {
		return deploy.RuntimeStatus{}, errors.Join(deploy.ErrNotFound, err)
	}
	if err != nil {
		return deploy.RuntimeStatus{}, err
	}
	status := deploy.RuntimeStatus{ImageID: report.Image}
	if report.State != nil {
		status.Running = report.State.Running
		if report.State.Health != nil {
			status.Health = report.State.Health.Status
		}
	}
	return status, nil
}
func (r *Runtime) Start(ctx context.Context, a deploy.Application, id string) error {
	ctx, done := r.operation(ctx)
	defer done()
	report, err := r.inspect(ctx, a, id)
	if err != nil {
		return err
	}
	if report.State != nil && report.State.Running {
		return nil
	}
	return containers.Start(ctx, id, nil)
}
func (r *Runtime) Stop(ctx context.Context, a deploy.Application, id string) error {
	ctx, done := r.operation(ctx)
	defer done()
	report, err := r.inspect(ctx, a, id)
	if err != nil {
		return err
	}
	if report.State != nil && !report.State.Running {
		return nil
	}
	return containers.Stop(ctx, id, new(containers.StopOptions).WithTimeout(10))
}
func (r *Runtime) Remove(ctx context.Context, a deploy.Application, id string) error {
	ctx, done := r.operation(ctx)
	defer done()
	_, err := r.inspect(ctx, a, id)
	if missing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	reports, err := containers.Remove(ctx, id, new(containers.RemoveOptions).WithForce(true).WithVolumes(false))
	if err != nil {
		return err
	}
	for _, report := range reports {
		if report != nil && report.Err != nil {
			return report.Err
		}
	}
	return nil
}

// Endpoint resolves the published port only after start; no remote localhost assumptions.
func (r *Runtime) Endpoint(ctx context.Context, a deploy.Application, id string) (string, error) {
	if a.Proxy == nil {
		return "", nil
	}
	ctx, done := r.operation(ctx)
	defer done()
	report, err := r.inspect(ctx, a, id)
	if err != nil {
		return "", err
	}
	if report.NetworkSettings == nil {
		return "", errors.New("container has no network settings")
	}
	key := strconv.Itoa(int(a.Proxy.ContainerPort)) + "/tcp"
	ports := report.NetworkSettings.Ports[key]
	if len(ports) != 1 {
		return "", errors.New("proxy container port must have exactly one published host port")
	}
	port := ports[0].HostPort
	if _, err = strconv.ParseUint(port, 10, 16); err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(a.Proxy.BackendHost, port), nil
}

func (r *Runtime) ensureVolume(ctx context.Context, a deploy.Application, name string) error {
	existing, err := volumes.Inspect(ctx, name, nil)
	if err == nil {
		if existing.Labels[deploy.OwnerLabel] != deploy.OwnerValue || existing.Labels[deploy.AppLabel] != a.ID {
			return errors.New("named volume collision with unmanaged or another application's volume")
		}
		return nil
	}
	if !missing(err) {
		return err
	}
	_, err = volumes.Create(ctx, entities.VolumeCreateOptions{Name: name, Labels: map[string]string{deploy.OwnerLabel: deploy.OwnerValue, deploy.AppLabel: a.ID}}, nil)
	return err
}
func (r *Runtime) EnsureNetwork(ctx context.Context, a deploy.Application, name string) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.ID == "" || a.Kind != "service" {
		return errors.New("service network requires a managed service identity")
	}
	if err := deploy.ValidateName(name); err != nil {
		return err
	}
	ctx, done := r.operation(ctx)
	defer done()
	existing, err := network.Inspect(ctx, name, nil)
	if err == nil {
		if existing.Labels[deploy.OwnerLabel] != deploy.OwnerValue || existing.Labels[deploy.AppLabel] != a.ID || existing.Labels[deploy.RoleLabel] != "service-network" {
			return errors.New("network collision with unmanaged or another service's resource")
		}
		return nil
	}
	if !missing(err) {
		return err
	}
	_, err = network.Create(ctx, &nettypes.Network{Name: name, Driver: "bridge", DNSEnabled: true, Labels: map[string]string{deploy.OwnerLabel: deploy.OwnerValue, deploy.AppLabel: a.ID, deploy.RoleLabel: "service-network"}})
	return err
}
func (r *Runtime) CreateSecret(ctx context.Context, name string, reader io.Reader) error {
	if err := deploy.ValidateName(name); err != nil {
		return err
	}
	ctx, done := r.operation(ctx)
	defer done()
	// No replacement or secret inspection: credentials are supplied over a reader, never arguments.
	_, err := secrets.Create(ctx, io.LimitReader(reader, 512001), new(secrets.CreateOptions).WithName(name).WithLabels(map[string]string{deploy.OwnerLabel: deploy.OwnerValue}))
	return err
}

func (r *Runtime) Find(ctx context.Context, a deploy.Application, release string) (deploy.Candidate, error) {
	ctx, done := r.operation(ctx)
	defer done()
	list, err := containers.List(ctx, new(containers.ListOptions).WithAll(true).WithFilters(map[string][]string{"label": {deploy.OwnerLabel + "=" + deploy.OwnerValue, deploy.AppLabel + "=" + a.ID, deploy.ReleaseLabel + "=" + release, deploy.RoleLabel + "=runtime"}}))
	if err != nil {
		return deploy.Candidate{}, err
	}
	if len(list) == 0 {
		return deploy.Candidate{}, deploy.ErrNotFound
	}
	if len(list) != 1 {
		return deploy.Candidate{}, errors.New("ambiguous managed release containers")
	}
	report, err := r.inspect(ctx, a, list[0].ID)
	if err != nil {
		return deploy.Candidate{}, err
	}
	if report.Config.Labels[deploy.ReleaseLabel] != release || report.Config.Labels[deploy.RoleLabel] != "runtime" {
		return deploy.Candidate{}, errors.New("managed release identity mismatch")
	}
	return deploy.Candidate{ID: list[0].ID}, nil
}

// ServiceConfig returns image-backed services with no published ports and persistent named storage.
// Database secrets contain passwords. An optional Redis secret contains a complete Redis ACL file;
// callers should disable the default user and use hashed ACL passwords. Only secret references
// and file paths enter configuration, never credential values or password command arguments.
func ServiceConfig(kind, secret, volume, networkName string) (string, deploy.RuntimeConfig, error) {
	c := deploy.RuntimeConfig{Networks: []string{networkName}, RestartPolicy: "unless-stopped"}
	if err := deploy.ValidateName(volume); err != nil {
		return "", c, err
	}
	if err := deploy.ValidateName(networkName); err != nil {
		return "", c, err
	}
	if secret != "" {
		if err := deploy.ValidateName(secret); err != nil {
			return "", c, err
		}
	}
	const passwordFile = "/run/secrets/service-password"
	var image, target string
	switch kind {
	case "postgres":
		image = "docker.io/library/postgres:17-bookworm"
		target = "/var/lib/postgresql/data"
		c.Env = map[string]string{"POSTGRES_USER": "app", "POSTGRES_DB": "app", "POSTGRES_PASSWORD_FILE": passwordFile}
	case "mariadb":
		image = "docker.io/library/mariadb:11.8-noble"
		target = "/var/lib/mysql"
		c.Env = map[string]string{"MARIADB_DATABASE": "app", "MARIADB_USER": "app", "MARIADB_PASSWORD_FILE": passwordFile, "MARIADB_ROOT_PASSWORD_FILE": passwordFile, "MARIADB_ROOT_HOST": "localhost"}
	case "mysql":
		image = "docker.io/library/mysql:8.4-oraclelinux9"
		target = "/var/lib/mysql"
		c.Env = map[string]string{"MYSQL_DATABASE": "app", "MYSQL_USER": "app", "MYSQL_PASSWORD_FILE": passwordFile, "MYSQL_ROOT_PASSWORD_FILE": passwordFile, "MYSQL_ROOT_HOST": "localhost"}
	case "redis":
		image = "docker.io/library/redis:7.4-bookworm"
		target = "/data"
		c.Command = []string{"redis-server", "--appendonly", "yes"}
		if secret != "" {
			// The explicit bookworm variant fixes redis UID/GID at 999 in its Dockerfile.
			const aclFile = "/run/secrets/redis.acl"
			c.Secrets = []deploy.SecretRef{{Name: secret, Target: aclFile, Type: "mount", UID: 999, GID: 999}}
			c.Command = append(c.Command, "--aclfile", aclFile)
		}
	default:
		return "", c, errors.New("service must be postgres, mariadb, mysql or redis; other OCI services use application configuration")
	}
	if kind != "redis" {
		if secret == "" {
			return "", c, errors.New("database provisioning requires an existing Podman password secret reference")
		}
		// These database entrypoints read _FILE variables as root before dropping privileges.
		c.Secrets = []deploy.SecretRef{{Name: secret, Target: passwordFile, Type: "mount"}}
	}
	c.Volumes = []deploy.Volume{{Source: volume, Target: target}}
	return image, c, nil
}

var _ deploy.Runtime = (*Runtime)(nil)
