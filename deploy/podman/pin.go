package podman

import (
	"context"
	"errors"
	"strings"

	"github.com/containers/podman-tui/deploy"
	"go.podman.io/podman/v6/pkg/bindings/containers"
	"go.podman.io/podman/v6/pkg/specgen"
)

// PinImage creates a stopped, network-less owned holder. Quadlet's --rm lifecycle cannot unpin its image.
// It carries no application volumes or credential references and is never started by this module.
func (r *Runtime) PinImage(ctx context.Context, a deploy.Application, release deploy.Release) (string, error) {
	ctx, done := r.operation(ctx)
	defer done()
	if !immutablePattern.MatchString(release.ImageID) {
		return "", errors.New("image pin requires a full immutable image identity")
	}
	id, err := r.findImagePin(ctx, a, release)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, deploy.ErrNotFound) {
		return "", err
	}
	s := specgen.NewSpecGenerator(release.ImageID, false)
	s.Name = "ptui-hold-" + a.ID + "-" + release.ID
	s.Labels = map[string]string{deploy.OwnerLabel: deploy.OwnerValue, deploy.AppLabel: a.ID, deploy.ReleaseLabel: release.ID, deploy.RoleLabel: "image-pin"}
	yes := true
	no := false
	s.NoNewPrivileges = &yes
	s.Privileged = &no
	s.HTTPProxy = &no
	s.ImageVolumeMode = "ignore"
	s.NetNS = specgen.Namespace{NSMode: specgen.NoNetwork}
	created, err := containers.CreateWithSpec(ctx, s, nil)
	return created.ID, err
}

// FindImagePin recovers a holder created before its ID reached the deployment journal.
func (r *Runtime) FindImagePin(ctx context.Context, a deploy.Application, release deploy.Release) (string, error) {
	ctx, done := r.operation(ctx)
	defer done()
	return r.findImagePin(ctx, a, release)
}

func (r *Runtime) findImagePin(ctx context.Context, a deploy.Application, release deploy.Release) (string, error) {
	filter := map[string][]string{"label": {deploy.OwnerLabel + "=" + deploy.OwnerValue, deploy.AppLabel + "=" + a.ID, deploy.ReleaseLabel + "=" + release.ID, deploy.RoleLabel + "=image-pin"}}
	existing, err := containers.List(ctx, new(containers.ListOptions).WithAll(true).WithFilters(filter))
	if err != nil {
		return "", err
	}
	if len(existing) == 0 {
		return "", deploy.ErrNotFound
	}
	if len(existing) != 1 {
		return "", errors.New("ambiguous managed image holders")
	}
	report, err := r.inspect(ctx, a, existing[0].ID)
	if err != nil {
		return "", err
	}
	if !immutablePattern.MatchString(release.ImageID) || report.Config.Labels[deploy.ReleaseLabel] != release.ID || report.Config.Labels[deploy.RoleLabel] != "image-pin" || strings.TrimPrefix(report.Image, "sha256:") != strings.TrimPrefix(release.ImageID, "sha256:") {
		return "", errors.New("image holder ownership or identity mismatch")
	}
	return report.ID, nil
}
