# Dokku and the enhanced Podman-TUI

Target release: v2.1.0; current version remains `2.1.0-dev`. This comparison concerns the requested lightweight deployment workflow, not full Dokku plugin/buildpack parity.

| Capability | Dokku | Enhanced Podman-TUI implementation |
| --- | --- | --- |
| Container runtime | Docker-oriented deployment platform | Existing Podman v6 bindings; no Docker Engine required |
| Primary interaction | CLI / Git push | Existing terminal UI plus noninteractive `platform` CLI |
| Application catalog | Named apps | Private endpoint-scoped JSON, stable application IDs, profiles/config/lifecycle |
| Git deployment | Git push and other methods | Manual/external-CI fetch of trusted HTTPS/SSH branch/tag/commit, root Containerfile/Dockerfile build |
| Buildpacks | Supported | Not implemented; explicitly outside this release's build workflow |
| Release history / rollback | Deployment/image workflows | Immutable image/commit/config snapshots; failed-release journal and retained-image rollback |
| Readiness / cutover | Deployment checks | Running, image-health and HTTP checks; conditional blue-green routing; explicit maintenance fallback |
| Domains / proxy | Nginx integration | Optional owned include directory, conflicts, HTTP/HTTPS, WebSockets, validated activation/undo |
| Certificates | ACME plugins/integration | Optional maintained external Certbot HTTP-01/staging/renewal, certificate import/status and scheduler generation; public ACME verification gate unpassed |
| Persistent backing services | Plugins | Optional image presets for PostgreSQL/MariaDB/MySQL/Redis, private networks, owned volumes, explicit linking |
| Secrets | Environment/plugin patterns | Existing Podman secret references, reader-based creation, mount UID/GID; no secret values in metadata/export/previews |
| Boot startup | Host service integration | Optional local Linux Quadlet/systemd with immutable image pins and rootless detection |
| Remote use | Host CLI/SSH | Existing supported Podman SSH connections; explicit controller/server integration boundaries |
| Required metadata database | Host configuration/state | None; bounded private JSON and filesystem locking |
| Application-specific daemon | Host components/SSH Git service | None for manual operations; optional OS schedulers and external CI |
| Plugins / clustering / multi-tenant SaaS | Extensible ecosystem | Not introduced |

Real rootless Linux arm64 tests exercised Git/image releases, failure preservation, rollback, SQL/Redis persistence and owned-resource isolation. Real isolated Nginx tests exercised HTTP route switching, invalid-config rejection, imported HTTPS/redirect and WebSocket frames. Cross-builds and actual measurements are in [verification](platform-verification.md). Local TLS tests do not imply public certificate issuance, hosted CI or universal zero downtime.

## Architecture

The `deploy.Manager` owns application metadata and release transitions independently of screen widgets. `deploy/podman` freezes one binding context and checks exact owner/application/release IDs before resource mutations. `deploy/git` uses a hardened Git CLI and bounded extraction. `deploy/proxy` owns optional Nginx/Certbot/scheduling files; `deploy/quadlet` owns optional Linux startup units. `deploy/platform` composes those services for Cobra and the asynchronous tview Applications screen.

No required browser, Kubernetes, Docker daemon, mandatory SQL/Redis metadata service, heavyweight queue, sidecar or new framework was added. Existing Pods/Containers/Volumes/Images/Networks/Secrets functionality and F1–F8 assignments remain.

[Dokku deployment methods](https://dokku.com/docs/deployment/methods/), [Dokku Nginx](https://dokku.com/docs/networking/proxies/nginx/), [Podman Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html).
