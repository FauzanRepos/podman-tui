# Managed applications (v2.1.0 development)

The existing Podman screens remain available. F9 opens Applications / Services; `m` opens contextual commands, Enter inspects runtime health, and Esc cancels an active operation or closes a dialog. Configuration editing uses JSON in the TUI (Ctrl+S saves), including non-sensitive environment variables, profiles, ports, persistent mappings and secret references. Saved configuration is desired state; apply it with deploy. Existing F1–F8 and h/l/j/k navigation remain assigned as before.

Application dialogs reuse the existing theme, centered command tables and right-aligned buttons. Forms keep Cancel before the action button; destructive confirmations initially focus Cancel. Arrow keys navigate commands even when no letter shortcut is available. Short titles and viewport-clamped panels keep controls reachable on narrow terminals.

In the configuration editor, Tab / Shift+Tab cycle between JSON and Cancel / Save; Ctrl+S saves and Esc closes. Invalid JSON stays in the editor with an error so it can be corrected rather than losing the draft. Only non-sensitive configuration and secret references belong here.

Application rows, command menus and delayed dialogs are bound to the connection URI/identity that produced them. Results from a previous host cannot replace the current catalog, and mutations use immutable application IDs so rename/name reuse cannot redirect an action.

Ctrl+C requests safe cancellation while application work is active; retry exit once recovery completes. SIGTERM/interrupt cancellation waits for the application's bounded recovery, then stops the terminal event loop. Neither path bypasses release-state reconciliation.

The deployment module has no application daemon or metadata database. It uses the repository's existing Podman v6 Go bindings, Git CLI, private connection-scoped JSON and optional host integrations. Podman socket activation/Machine, Nginx and host schedulers are separate platform dependencies, not a Podman-TUI daemon.

## Start from an image

Select a configured Podman v6 connection. Use a dedicated host/state directory when experimenting:

```sh
podman-tui platform create web --image docker.io/library/nginx:stable --connection my-host
podman-tui platform deploy web --connection my-host
podman-tui platform status web --connection my-host
podman-tui platform history web --connection my-host
```

Metadata-only create/list/inspect/configure/rename/export do not contact the Podman server. `--state-dir` overrides the private metadata root; `PODMAN_TUI_STATE_DIR` selects the root for both CLI and TUI. The default is the OS user configuration directory plus `podman-tui/deploy`; each exact endpoint has its own hashed directory. Do not share this directory across untrusted users or multiple machines with independent filesystem locking.

Transport initialization captures the selected connection generation under a lock. Switching or canceling the connection cancels an unfinished handshake; a completed old-host binding cannot be published into the new selection. Cached TUI bindings must match the selected URI and identity.

Application JSON input is limited to 1 MiB, with oversize input rejected before decoding. Unknown fields and trailing JSON are errors; whitespace cannot hide an additional payload beyond the read limit.

For ports, limits, storage or environment configuration, create from a JSON file (`--file path`, or `--file -` for stdin):

```json
{
  "name": "web",
  "image": "docker.io/library/nginx:stable",
  "profile": "production",
  "profiles": {
    "production": {
      "ports": [{"container": 80, "host": 8080, "ip": "127.0.0.1"}],
      "env": {"APP_MODE": "production"},
      "restart_policy": "unless-stopped",
      "memory_bytes": 134217728,
      "cpu_quota": 50000
    }
  },
  "readiness": {"mode": "running", "timeout_seconds": 30},
  "retention": 3
}
```

```sh
podman-tui platform create web --file web.json --connection my-host
podman-tui platform configure web --file web.json --connection my-host
podman-tui platform deploy web --connection my-host
```

Profile names are development, staging and production. `env` is explicitly **non-sensitive** configuration. Secrets belong in Podman and metadata contains only `{"name":"db-password","target":"DB_PASSWORD","type":"env"}` or a `mount` reference. Do not put credentials in image/Git URLs, JSON, command arguments, build instructions or terminal previews. Create a Podman secret with `platform secret-create NAME` reading stdin, or use the existing Secrets screen/Podman tools. Environment secret values are not returned by platform status/history/export. Images and application processes remain responsible for not logging their own credentials.

The original Secrets inspection screen now requests metadata only. Managed-container inspection redacts **all runtime environment values** while retaining their variable names; unmanaged container inspection is unchanged. Container/secret create debug logs no longer dump configuration or secret input. A dedicated rootless integration scenario verifies these original inspection paths with a real environment-secret deployment.

Ports default to loopback. A host port of zero requests dynamic publication. Bind mounts name **server-side** paths; workstation paths are not implicitly available on remote servers. Named volumes are created with application ownership labels; a collision with an unmanaged/other application's volume is rejected. Application deletion never removes volumes, networks or secrets.

## Lifecycle and safe replacement

```sh
podman-tui platform stop web --connection my-host
podman-tui platform start web --connection my-host
podman-tui platform restart web --connection my-host
podman-tui platform rename web api --connection my-host
podman-tui platform delete api --yes --connection my-host
```

Containers use immutable IDs and owner/application/release labels. Every mutation inspects exact ownership; no global prune or automatic deletion of unmanaged resources is performed. Rename preserves application identity, deployment history and container labels. Interactive operations bind to immutable application identity, not a display name that could be reused.

A replacement is independently created, started and checked before routing commit and old-container retirement. Running readiness requires three consecutive running observations; it is **not an application-level readiness guarantee**. `health` requires an image HEALTHCHECK; `http` requires an explicit published proxy endpoint and a successful 2xx path response. Readiness and overall CLI operation timeouts are bounded.

Fixed host ports or exclusive writable volumes cannot be safely overlapped. Explicit `--maintenance` authorizes temporary downtime: source/image preparation happens first, the old container is stopped but retained, and a failed replacement restores it. `concurrent_safe` authorizes overlapping volume writers only when the application itself supports it. Neither flag reverts database migrations. Do not advertise universal zero downtime.

Failed candidates preserve the committed release and record failure in history. If routing recovery itself fails, both resources and an in-progress journal are preserved instead of deleting a potentially routed healthy candidate. Investigate the error and use `recover-abort NAME --yes` to restore the committed release deliberately. Recovery can require maintenance downtime. Disconnect/cancellation cannot make a broken remote host recoverable; errors remain explicit.

Confirmed interrupted recovery reconciles both the presence and absence of predecessor routing before removing the candidate. A crash-created image holder whose ID was not yet saved is discovered by exact application/release/role/image identity; retained successful holders remain protected. Recovery stops an active unchanged unit before replacing its boot definition.

## History and rollback

```sh
podman-tui platform history web --connection my-host
podman-tui platform rollback web RELEASE-ID --connection my-host
podman-tui platform cleanup web --yes --connection my-host
```

Rollback redeploys a retained successful release's immutable image and configuration snapshot. Old stopped containers and optional stopped image holders pin images, including when Quadlet recreates/removes a runtime container. A missing image is an error, not a fallback to a moving tag. Cleanup explicitly removes only eligible stopped managed release containers/holders according to retention, including crash-created holders; volumes and unrelated images remain untouched. Historical database schema/data are not restored. Application-level backups and compatible migrations are operator responsibilities.

## Verification status

The initial Go unit/build/vet baseline passed. A dedicated rootless Podman Machine server 6.1.4 (Linux arm64) has exercised image build, create/start/stop/rename, failed candidate preservation, retained-image rollback, persisted history, cross-application ownership rejection and secret-reference deployment without secret values in metadata. The opt-in integration suite uses `PODMAN_TUI_TEST_CONNECTION` and uniquely owned resources; it does not globally reset or prune. Existing Bats tests require a controlled Linux/root environment and are not safe to point at production.

This is not a release announcement. Remaining integration/security/performance/platform gates and optional integration documentation are tracked in [deployment design](deployment-design.md). The version stays `2.1.0-dev`; no release tag is justified by unit tests alone.

### Dialog refinement verification

Live terminal checks used an isolated metadata catalog and a dedicated rootless Podman Machine connection, at 100×30 and 40×20. Create, rename, service creation/linking, TLS issue/import, configuration and destructive-confirmation dialogs were exercised without provisioning services or certificates. The editor's Cancel, Save, invalid-JSON retention and terminal-theme rendering were checked. A synthetic history entry exercised rollback selection and cancellation only, not an actual rollback.

The full Go test suite, `go vet`, native build and Linux AMD64/ARM64, Darwin AMD64/ARM64 and Windows AMD64 cross-builds passed. Application tests and the command-navigation, Cancel-first confirmation and synchronous input-dialog tests passed under the race detector. The complete legacy dialog race run still reports background-event-loop races in existing command/confirmation/message/top tests; it is not a clean race result. The input-dialog test now drives keyboard events synchronously instead of racing a background renderer.
