# Deployment recovery, rollback and troubleshooting

## Diagnose without destroying working resources

Use `platform inspect NAME`, `history NAME` and `status NAME` with the same `--connection` and `--state-dir` used to deploy. Metadata-only list/inspect/history work offline. Runtime status requires the repository-supported Podman v6 server. F9 → `m` exposes contextual commands; build/deployment work is asynchronous and Esc cancels with independently bounded recovery.

A configured image or repository is desired state. The current release snapshot identifies the configuration actually deployed. A failed candidate must not become current. Check immutable image identity, commit SHA, deployment stage, readiness outcome and error in history before retrying.

| Symptom | Check / safe action |
| --- | --- |
| No configured connection | Select an existing Podman connection; initialize/start Podman Machine separately on macOS |
| Metadata appears missing | Check exact endpoint URI and state directory; connection nicknames do not merge hosts |
| Operation already active | Wait for the writer or cancel its UI operation; do not delete its lock file |
| Unsupported state schema or unsafe file mode | Preserve the original state/backup; repair permissions deliberately, never reset to empty state |
| Git fetch fails | Verify exact branch/tag/SHA, credential helper/SSH keys and verified known-hosts; no plaintext URL credentials |
| No root build file | Provide a regular root Containerfile or Dockerfile; nested/symlink-only instructions are rejected |
| Untrusted repository | Review its build instructions before explicit trust; never bypass hooks/filter protections |
| HTTP readiness fails | Check explicit backend reachability, published container port and a 2xx path; redirects do not count |
| Health readiness fails | The image must define HEALTHCHECK and reach healthy before timeout |
| Fixed ports or exclusive writable storage | Use explicitly authorized maintenance downtime; do not silently overlap database writers |
| Nginx activation fails | Inspect the explicit main config/include, conflict and certificate permissions; old generated route is restored |
| Missing rollback image | Restore the retained immutable image; never substitute the current moving tag |
| Service starts but protocol not ready | Running-container readiness is not SQL/Redis readiness; inspect private service diagnostics and application-level readiness |
| Quadlet unavailable | Keep manual operation; check local Linux Unix socket, systemd, cgroup v2 and installed generator |

## Manual rollback

```sh
podman-tui platform history web --connection production
podman-tui platform rollback web RELEASE-ID --connection production
podman-tui platform status web --connection production
```

Only retained successful releases are eligible. Rollback redeploys immutable image/configuration, runs readiness again and transactionally switches the route. It keeps a current renewed certificate when it covers the restored domains. It does not rewind database migrations, delete data or restore historical volume contents. Backups and backward-compatible schema changes are operator responsibilities.

For an application whose fixed host port or exclusive storage cannot overlap the current release, use the documented `--maintenance` path after explicitly accepting downtime. Source/image preparation occurs before stopping the old container. The old release is retained and restarted if the candidate fails. Active supervised Quadlet maintenance requires the stop/remove-startup/redeploy/reinstall procedure in the rootless guide.

## Interrupted operation journal

If routing/runtime recovery cannot be proven, the platform reports `recovery-required` and retains resources instead of deleting a potentially healthy routed candidate. Investigate the known-good release and candidate, then:

```sh
podman-tui platform recover-abort web --yes --connection production
```

This restores the committed release deliberately and can require maintenance downtime. It stops an interrupted writer before recreating a missing/stopped predecessor. Remote-host failure, broken Nginx configuration or unavailable retained images can still prevent recovery; the error is explicit. Never remove working containers manually merely to clear an error.

## Retention and removal

```sh
podman-tui platform cleanup web --yes --connection production
podman-tui platform delete web --yes --connection production
```

Cleanup applies configured successful-release retention, skips current/running/in-progress releases and removes only eligible stopped owned containers/image pins. Old immutable images remain until safe operator-controlled image cleanup; the platform never globally prunes shared images. Deletion preserves named volumes, networks and secrets. Stop/remove any operator-installed cron entry associated with a deleted application; preserve backups before explicit storage cleanup.

## Verification limits

Real Podman/Git/Nginx/TLS-import/service/Quadlet scenarios use isolated resources on a dedicated rootless Linux arm64 VM plus local Nginx. Public ACME HTTP-01 and scheduler-driven renewal require owned DNS/public port-80 infrastructure that is not implied by local fixture tests. Cross-built binaries do not prove execution on physical AMD64, Raspberry Pi or every Linux distribution. Consult the measured compatibility/release record; `2.1.0-dev` is not a release certification.
