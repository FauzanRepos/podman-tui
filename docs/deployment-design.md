# Deployment platform design and release gates

Target: v2.1.0. This document is a design and evidence record, not a claim that the platform is released.

## Repository baseline

Starting commit: `8908354e19e97030a073fd96717300eaee08f467`, branch `main`, clean worktree and index. Existing tags range through `v2.0.0`; `cmd/version.go` already reports `2.1.0-dev`. The fork remote is `origin`; the initial `git ls-remote --tags origin refs/tags/v2.1.0 refs/tags/v2.1.0^{}` returned no matching tag. Repeat immediately before any release tag.

No root AGENTS.md, CLAUDE.md, CONTRIBUTING.md or project-local agent command directory was present. `docs/CONTRIBUTING.md` requires DCO sign-off. Keep existing source, vendoring, build tags, screens and F1–F8 assignments.

Structure: `cmd/` Cobra entry points; `app/` tview application, navigation and refresh; `ui/` tcell/tview screens and dialogs; `pdcs/` Podman resource adapters; `pdcs/registry/` selected connection and binding context; `system/` connection health and event stream; `config/` containers.conf connection configuration; `test/` Bats/tmux functionality suite; `docs/`, `rpm/`, `hack/`, `.github/`; vendored dependencies in `vendor/`.

Go requirement is **1.25.9**, including CI. Major existing dependencies: Podman bindings v6.1.1, common v0.69.1, Buildah v1.45.0, tview v0.42.0, tcell v2.13.10, Cobra v1.10.2, Ginkgo v2.32.0 and Gomega v1.42.1. Existing compatibility policy is release-2.x with Podman v6.x. Vendored Docker-related image/client libraries are existing transitive dependencies, not a requirement to run Docker Engine.

Reusable features: local/SSH Podman bindings, connection selector, container/image/pod/volume/network/secret management, image build output, command/confirmation/error dialogs, keyboard help, simulation-screen unit tests, and Podman event-driven resource refresh. Deployment operations need explicit contexts rather than the existing wrappers that log entire create options and fetch a mutable global connection for each action. Freeze one connection context for each deployment and verify ownership before every mutation.

### Verification before production changes

Commands, using checksum-verified Go 1.25.9 under `/tmp/podman-tui-toolchain/go/bin`:

```
go test -mod=vendor -tags 'exclude_graphdriver_btrfs containers_image_openpgp remote' ./...
make binary GO=/tmp/podman-tui-toolchain/go/bin/go
go vet -mod=vendor -tags 'exclude_graphdriver_btrfs containers_image_openpgp remote' ./...
```

The initial native build passed. Initial unit tests passed: 8 tested packages, 26 packages without tests. Initial `go vet` passed without diagnostics (2.53 seconds).

The host initially had no Go executable on PATH. Installed Podman CLI: 5.8.2, no connections or machines; `podman version` failed because no socket exists. Nginx, Certbot, systemd, Bats and golangci-lint were not found. These are **baseline environment limitations**, not introduced regressions. Existing Bats setup assumes root, systemctl and `/root` connection configuration and the test guide recommends `podman system reset`; do not run it against an uncontrolled or production host.

An unchanged real TUI was launched on a controlling pseudo-terminal at 100x24, disconnected. First terminal output: 0.0890 seconds (not time to connected readiness). RSS/instantaneous CPU samples: 1s 31,472 KiB/0.4%; 5s 31,904 KiB/0.0%; 10s 32,640 KiB/0.0%. Process CPU time at 10s: 0.07s. Single run, no connected monitoring or deployments; not a hardware guarantee. The owned smoke process required forced termination after Ctrl+C/TERM did not finish within 5 seconds; retain as a baseline observation, not a new regression.

After the unchanged baseline, a dedicated rootless test machine `podman-tui-v210-test` was initialized from `quay.io/podman/machine-os:6.1` (1 CPU, 2048 MiB RAM, 10 GiB disk). Its server reports Podman **6.1.4**, Linux arm64. Do not install the macOS Docker socket helper or switch the machine rootful. All integration resources must use this explicit connection and a unique ownership label.

## Dokku gap matrix

| Capability | Existing Podman-TUI | Required deployment layer |
| --- | --- | --- |
| Container/image/volume/network/secret operations | Existing | Reuse bindings and screens |
| Named application configuration | Absent | Connection-scoped persistent application catalog |
| Git clone/revision/build workflow | Manual image build only | Hardened Git CLI and Podman build |
| Release/readiness/rollback | Absent | Persisted lifecycle and immutable image snapshots |
| Domain routing/WebSockets | Absent | Optional managed Nginx include |
| Let's Encrypt and renewal | Absent | Optional external Certbot and host scheduler |
| Boot recovery | Podman restart options | Optional Linux Quadlet |
| Backing services | Generic containers/volumes/networks | Optional private service provisioning |
| CI-triggered release | No deployment command | Noninteractive CLI, no webhook daemon |
| Buildpacks, clustering, marketplace | Absent | Out of scope; Containerfile/Dockerfile only |

Reference: [Dokku Git deployment](https://dokku.com/docs/deployment/methods/git/), [Dokku Nginx proxy](https://dokku.com/docs/networking/proxies/nginx/), [Quadlet manual](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html), [Certbot user guide](https://eff-certbot.readthedocs.io/en/stable/using.html).

## Architecture and invariants

Keep a reusable Go `deploy` module independent of tview and Cobra. Internal adapters own Podman runtime, Git retrieval, optional Nginx/TLS and Quadlet operations. CLI and TUI call the same manager. No application daemon, database, Kubernetes, global prune, automatic privilege escalation or public Podman socket.

Private connection-scoped state under the user's configuration directory uses versioned JSON, atomic replacement and cross-process advisory locking. Application IDs survive rename; container labels identify owner, application and release. State and source directories use 0700, state and credential files 0600. Bind mounts refer to paths on the Podman server, not necessarily the workstation. Never delete volumes on application deletion; database rollback changes image/configuration only, never data/migrations.

A deployment validates configuration, retrieves source/image, builds when needed, records immutable image identity, creates a separately named candidate, waits for bounded readiness, switches optional routing, records the successful release, then retires the previous container. Failure before commit leaves the previous container and routing intact. Journal stages allow inspection of interrupted operations; reconcile uncertain container/proxy/state results explicitly rather than guessing success. Retained containers pin images for rollback; cleanup checks ownership and never globally prunes.

Overlapping blue-green deployment requires a distinct candidate port, routable backend and application support for concurrent use of persistent storage. Fixed host ports or unsafe concurrent writers must fail safely or use an explicitly selected maintenance procedure; no unconditional zero-downtime claim. Proxy host, Podman server and workstation are distinct environments. Host Nginx/Certbot/Quadlet operations are local-only unless run on the server itself; remote container operations remain supported. Reject incompatible combinations rather than manipulating the wrong host.

Git supports HTTPS/SSH and existing credential helpers/SSH agents, rejects embedded HTTP credentials and unsafe protocols, disables hooks/submodules/interactive prompts, uses argument arrays and isolated source/build directories. Building a Containerfile executes repository-supplied build instructions in Podman: it requires explicit repository trust. Render output through bounded control-sequence sanitization; never persist secret values in configuration, arguments, previews or build logs. Podman secret references are the supported sensitive configuration mechanism.

TUI adds applications after existing screens without changing F1–F8, uses asynchronous workers and event-loop updates, bounded output and explicit cancellation. Optional integrations initialize only when selected. Keep one active deployment per connection/app; host-state mutations serialize across processes.

## Compatibility and release gates

Linux amd64/arm64 and Darwin arm64 builds must pass with existing remote build tags and CGO disabled. Existing Windows/Darwin amd64 build targets remain intact. Rootless Linux is preferred. Quadlet additionally needs Linux, systemd, cgroup v2 and an installed generator; generated units use `[Install]`, not `systemctl enable` on transient units. Rootless boot requires administrator-enabled lingering when needed. macOS uses Podman Machine; local host integrations must not be confused with VM integrations.

Every phase requires targeted successful/failure tests, build and static checks, an exercised changed path, documentation and isolated Conventional Commits. Real integration tests use only uniquely named owned resources and an isolated state directory. Release gates include existing tests, real Podman build/deploy/failure/rollback, actual Nginx validation/routing, TLS staging/renewal, Quadlet reboot behavior, isolation/secrets, responsive/narrow TUI, compatible binaries and measured idle/monitoring/app/deployment overhead. Unavailable integration environments remain explicit unpassed gates.

Keep version at `2.1.0-dev` and do not create `v2.1.0` until every mandatory capability and gate passes. Never overwrite a tag, push or publish automatically.
