package platform

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/containers/podman-tui/deploy"
	podmanRuntime "github.com/containers/podman-tui/deploy/podman"
)

// CreateService provisions one optional private network and an owned persistent service.
// Failed startup retains inspectable metadata/storage; it never deletes data to make provisioning succeed.
func CreateService(ctx context.Context, m *deploy.Manager, name, kind, credentialSecret, network string, output io.Writer) (deploy.Release, error) {
	if err := deploy.ValidateName(name); err != nil {
		return deploy.Release{}, err
	}
	if _, err := m.Get(name); err == nil {
		return deploy.Release{}, errors.New("application/service name already exists")
	} else if !errors.Is(err, deploy.ErrNotFound) {
		return deploy.Release{}, err
	}
	adapter, ok := m.Runtime.(*podmanRuntime.Runtime)
	if !ok {
		return deploy.Release{}, errors.New("service provisioning requires the Podman runtime")
	}
	id, err := deploy.NewID()
	if err != nil {
		return deploy.Release{}, err
	}
	if network == "" {
		network = "ptui-svc-" + id[:16]
	}
	image, cfg, err := podmanRuntime.ServiceConfig(kind, credentialSecret, "ptui-data-"+id, network)
	if err != nil {
		return deploy.Release{}, err
	}
	application, err := m.Create(deploy.Application{Name: name, Kind: "service", Image: image, Profile: "production", Profiles: map[string]deploy.RuntimeConfig{"production": cfg}})
	if err != nil {
		return deploy.Release{}, err
	}
	if err = adapter.EnsureNetwork(ctx, application, network); err != nil {
		return deploy.Release{}, err
	}
	return m.Deploy(ctx, application.Name, deploy.DeployOptions{}, nil, output)
}

// LinkService explicitly authorizes network membership and, separately, non-sensitive connection
// settings. It never copies the service's credential secrets or data volume to the application.
func LinkService(m *deploy.Manager, applicationName, serviceName string, injectHost bool) error {
	application, err := m.Get(applicationName)
	if err != nil {
		return err
	}
	service, err := m.Get(serviceName)
	if err != nil {
		return err
	}
	if application.Kind == "service" || application.ID == service.ID {
		return errors.New("only applications can link to backing services")
	}
	if service.Kind != "service" {
		return errors.New("target is not a backing service")
	}
	cfg := application.Config()
	for _, network := range service.Config().Networks {
		if !slices.Contains(cfg.Networks, network) {
			cfg.Networks = append(cfg.Networks, network)
		}
	}
	if injectHost {
		if cfg.Env == nil {
			cfg.Env = map[string]string{}
		}
		host := "ptui-" + service.ID
		cfg.Env["SERVICE_HOST"] = host
		switch {
		case strings.HasPrefix(service.Image, "docker.io/library/postgres:"):
			cfg.Env["PGHOST"] = host
			cfg.Env["PGPORT"] = "5432"
			cfg.Env["PGDATABASE"] = "app"
			cfg.Env["PGUSER"] = "app"
		case strings.HasPrefix(service.Image, "docker.io/library/mariadb:"), strings.HasPrefix(service.Image, "docker.io/library/mysql:"):
			cfg.Env["MYSQL_HOST"] = host
			cfg.Env["MYSQL_PORT"] = "3306"
			cfg.Env["MYSQL_DATABASE"] = "app"
			cfg.Env["MYSQL_USER"] = "app"
		case strings.HasPrefix(service.Image, "docker.io/library/redis:"):
			cfg.Env["REDIS_HOST"] = host
			cfg.Env["REDIS_PORT"] = "6379"
		}
	}
	application.Profiles[application.Profile] = cfg
	return m.Configure("@"+application.ID, application)
}
