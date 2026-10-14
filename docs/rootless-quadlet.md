# Rootless operation and Quadlet

Podman-TUI uses the repository-supported Podman v6 bindings. Manual image/Git deployments do not require systemd, Quadlet, a metadata database or an application-specific daemon. The selected Podman API connection is frozen for each operation.

## Host topology

| Environment | Manual operations | Host integrations |
| --- | --- | --- |
| Local Linux with a Unix Podman socket | Supported; rootless preferred | Nginx/TLS permitted; Quadlet requires systemd/cgroup v2/generator |
| SSH remote Linux connection | Container operations on the server | Run the CLI on that server for Quadlet; controller Nginx/TLS requires explicit `--local-integrations` and reachable backend |
| macOS Apple Silicon with Podman Machine | Container/build/service operations in the Linux VM | macOS cannot manage the VM's systemd units. Execute the Linux binary inside the VM with a local Unix connection |
| Linux without systemd/Quadlet | Manual lifecycle/deploy/rollback | No generated startup units; use an operator-selected host scheduler/runtime policy |

Bind-mount source paths belong to the **Podman server**, not necessarily the workstation. Proxy backend IP/port must be reachable by both Nginx and the controller performing readiness checks. Never assume the controller, VM, remote host and Nginx container share networking or filesystems.

## Optional automatic startup

Deploy an application first, then run locally on the Linux Podman host:

```sh
podman-tui platform quadlet-enable web --connection local
podman-tui platform quadlet-status web --connection local
podman-tui platform stop web --connection local
podman-tui platform start web --connection local
podman-tui platform quadlet-disable web --connection local
podman-tui platform quadlet-remove web --yes --connection local
```

Enable installs a private, managed `.container` definition with an immutable image, release/application labels, explicit volumes/networks/secret references, resource limits and the chosen restart policy. It configures the boot target; it does not start a duplicate of the currently running manual deployment. The manager discovers container IDs recreated by systemd through exact ownership/release/runtime labels. Stopped image-pin containers retain images when systemd removes live containers.

Rootless units live in the current user's containers/systemd configuration directory; root-owned local invocations use system-wide Quadlet locations. The application does not use sudo, change ownership globally or enable lingering. An administrator may need to enable lingering for rootless services to survive logout/start at boot; that provisioning decision is outside automatic operations. Inspect the generated `ptui-APPLICATION-ID.service` through the appropriate user/system systemd manager.

Quadlet generation is checked with the installed generator before activation, followed by scoped file replacement and daemon reload. A private durable journal restores the previous complete generation after interrupted updates/removal. Units reference immutable, content-addressed environment files so an interrupted write cannot boot an old image with new environment values; migration preserves a legacy stable environment until the new unit is published. Unmanaged/foreign definitions, unexpected journal bytes and symlink targets are rejected. Removal requires an inactive unit and removes only owned definitions/generations, not persistent volumes. Disable removes boot enablement, not the running application.

The container `EnvironmentFile` is consumed by Podman's env-file parser, not shell/systemd value quoting. Non-sensitive values are written literally after validation; quotes, backslashes, spaces, percent and dollar characters retain their configured values. A real systemd-started container verifies this boundary.

Fixed host ports are required in Quadlet, except a proxied application's current dynamically allocated endpoint is pinned into its boot definition. Stable `ptui-APPLICATION-ID` network aliases preserve backing-service host settings across application rename and boot recreation.

## Cutover and recovery

An active unit's `ExecStop` changes on daemon reload. Deployment therefore waits for the candidate's readiness and switches routing, stops the old unit under its unchanged definition, then installs the next boot definition. A failed install restores the old definition and recreates the old release from its retained image. Image/config rollback does not revert database migrations or volume contents.

A live supervised application with fixed ports/exclusive writers cannot use the ordinary maintenance path while retaining its old container. Explicitly stop the application, remove its inactive startup definition, then perform the authorized maintenance deployment and reinstall startup. This is a documented downtime procedure, not zero downtime.

The dedicated rootless Linux arm64 VM exercised unit generation, start, label-based discovery and corrected active-unit cutover. Other hardware, system-wide/rootful operation and unsupported systemd distributions must not be inferred from that evidence. See the compatibility and verification records for reboot evidence and remaining gates.
