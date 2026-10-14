# Podman-TUI v2.1.0 development release notes

**Unreleased.** Current version: `2.1.0-dev`. No final `v2.1.0` tag or published release is authorized by these notes. Local implementation and verification are recorded separately from unpassed production-release gates.

## Implemented

- Connection-scoped, private JSON application/service catalog with stable IDs, safe rename, image/Git sources, lifecycle, profiles, non-sensitive environment, ports, volumes, limits and restart policies.
- Podman-native managed release containers with exact ownership checks; unchanged generic resource-management screens remain. Application deletion never removes unmanaged resources or persistent data volumes.
- Trusted Git CLI revision retrieval, hardened hooks/filter/protocol/archive handling, credential-helper/SSH support, Containerfile/Dockerfile builds and immutable commit/image history.
- Readiness-gated replacement, persisted deployment stages/failures/config snapshots, rollback, retention, cancellation and explicit interrupted-deployment recovery. Blue-green is conditional; fixed ports/exclusive writers require authorized maintenance.
- Optional managed host Nginx domains, external-domain conflicts, proxy headers, WebSockets, HTTP/HTTPS, redirects and validation before reload with route restoration on failure.
- Optional maintained external Certbot HTTP-01/staging/renewal, private imported certificates, SAN/expiry/key checks, renewal status and systemd timer/cron generation. Public ACME/runtime scheduler verification remains unpassed.
- Optional Linux Quadlet/systemd startup, rootless detection, scoped file ownership, immutable image pins, boot-recreated container discovery and corrected active-unit cutover ordering.
- Optional private persistent PostgreSQL, MariaDB/MySQL and Redis services; mounted password/ACL secret references and explicitly authorized network/non-sensitive connection-setting injection.
- F9 Applications/Services, contextual commands, JSON configuration editing, bounded sanitized progress/build output, asynchronous operations and safe Esc cancellation. F1–F8 retain their assignments; narrow menus wrap.
- Noninteractive `platform` CLI for manual/external-CI deployment without a webhook server or platform-specific background daemon.

## Security and compatibility changes

Managed-container inspection redacts runtime environment values; original secret inspection requests metadata rather than values. Container/secret create debug logs no longer dump sensitive options. Privileged container capabilities, automatic elevation and public Podman sockets are not introduced. Git instructions require explicit trust; application code remains responsible for credentials it prints in its own logs.

Stabilization binds delayed TUI actions to their originating endpoint and immutable application ID, rejects stale transport publication and oversize configuration input, reconciles interrupted route absence and image holders, and makes idle-TUI signal shutdown graceful. Behavioral regressions fail against the prior implementation and pass after correction. Quadlet file transactions have private crash-recovery journals; boot-visible generation consistency is verified separately from normal successful cutover.

Supported-target cross-builds passed for Linux AMD64/ARM64, Darwin AMD64/ARM64 and existing Windows AMD64. Actual runtime evidence is rootless Linux ARM64/Podman Machine and Darwin ARM64, not every hardware/distribution. No mandatory browser, Kubernetes, Docker Engine, SQL/Redis metadata service, message queue or new Go dependency was added.

## Verification and known limits

See [actual measurements and verification](platform-verification.md), [baseline/design](deployment-design.md), [Dokku comparison](dokku-comparison.md) and [recovery guide](deployment-troubleshooting.md). The existing dialog-test background race was removed without changing production dialog code. Real active-Quadlet testing exposed and fixed a cutover defect that could delete the new candidate after daemon reload.

Public Let's Encrypt staging issuance/renewal and scheduler-driven rotation require an owned public domain and HTTP-01 ingress; they were not proven by local TLS import tests. The security helper's retained static report is partial, not vulnerability-free assurance. Existing Bats/rootful/physical AMD64/Pi/Windows/Intel Mac runtime and hosted CI/live SSH Git-server coverage are not implied by builds or local fixtures.

Final local `v2.1.0` tag lookup and exact remote tag/peeled-tag lookup returned no matches. No tag was created because the mandatory public ACME/renewal acceptance gate is unpassed; the version remains `2.1.0-dev`. Commits and tags were not pushed.

The release tag must remain absent while any mandatory feature or gate is incomplete. Never overwrite an existing tag, push commits/tags or publish a remote release without explicit authorization.
