package containers

import (
	"strings"

	"github.com/containers/podman-tui/deploy"
	"github.com/containers/podman-tui/pdcs/registry"
	"github.com/containers/podman-tui/pdcs/utils"
	"github.com/rs/zerolog/log"
	"go.podman.io/podman/v6/pkg/bindings/containers"
)

// Inspect returns inspect results of the specific container.
func Inspect(id string) (string, error) {
	log.Debug().Msgf("pdcs: podman container inspect %s", id)

	var report string

	conn, err := registry.GetConnection()
	if err != nil {
		return report, err
	}

	response, err := containers.Inspect(conn, id, new(containers.InspectOptions))
	if err != nil {
		return report, err
	}
	// Podman expands environment-secret references in inspect output. Managed
	// applications deliberately reveal names, never runtime environment values.
	if response.Config != nil && response.Config.Labels[deploy.OwnerLabel] == deploy.OwnerValue {
		for i, value := range response.Config.Env {
			name, _, _ := strings.Cut(value, "=")
			response.Config.Env[i] = name + "=<redacted>"
		}
	}

	report, err = utils.GetJSONOutput(response)
	if err != nil {
		return report, err
	}

	return report, nil
}
