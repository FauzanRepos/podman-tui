# Deployment security considerations

## Trust and authority

This is a single-operator deployment tool, not an authorization server or multi-tenant sandbox. Access to a Podman socket, the connection's SSH identity, or private platform state authorizes deployment and can affect that account's workloads. Keep these capabilities out of application containers and public networks. Prefer rootless Podman. No command automatically invokes sudo or changes system firewall, privileged-port or linger policy.

Each endpoint has an isolated private catalog. Interactive rows and delayed dialogs bind to the connection URI/identity and immutable application ID; old-host operation results are discarded. An application's display name is not sufficient authorization after a connection change or name reuse. Managed container mutations inspect full identities and ownership labels. Cleanup/removal never adopts an unmanaged container, volume, network or Nginx include.

Application deletion preserves persistent volumes, networks and secret objects. Back up data and remove those resources separately only after inspecting ownership and references. Image holders protect retained release images; interrupted holders are discovered by exact application/release/role/image identity, not broad name matching. Do not use global Podman pruning as application retention management.

## Files and credentials

Keep catalog and TLS directories owned by the controller account, mode 0700; operational files and private keys are mode 0600. Do not share these directories with another local writer. Atomic writes and ownership checks are not a defense against an administrator or another process with the same account's filesystem authority. Backups of operational state may contain non-sensitive environment values and certificate material; restrict backup access and encryption independently.

Only put non-sensitive values in `env`. They appear in configuration, exports and history snapshots. Store credential values as Podman secrets and configure references. Secret creation reads stdin rather than a password argument; avoid shell history, process arguments and terminal previews when preparing that input. Generic managed-container inspection redacts environment values; generic secret inspection does not request secret data. Application/build tools can still print their own credentials: bounded terminal-safe logs are not a secret-content classifier. Do not provide credentials in Containerfile commands or build arguments.

Git SSH agents/keys and existing credential helpers provide repository authentication. Credential-bearing URLs, inherited `GIT_*` overrides, Git hooks, conversion filters, automatic submodules and unsafe retrieval protocols are rejected or disabled. HTTPS uses normal certificate verification; SSH requires preverified known hosts. Repository builds still execute explicitly trusted application instructions through Podman. Review the repository, revision and Containerfile before using `--trust-source`; this flag is not a sandbox guarantee. Do not mount the Podman socket or controller credentials into an application.

## Proxy, certificates and privileged operations

Nginx is optional and host-based. Explicitly authorize controller-host integration when the Podman endpoint is remote. Provision a dedicated include inside the intended master's existing `http` block; never replace a system-wide configuration with the tutorial's isolated example. Domains, backend addresses and paths are validated, external domain conflicts are rejected, and every generated activation runs `nginx -t` before reload. Routing recovery precedes removal of a replacement candidate. A failed restore remains an explicit recovery condition, not a successful deployment.

Root-owned Nginx requires an administrator-designed validation/reload/key-access arrangement. Do not make private keys public or expose privileged container capabilities to obtain that access. The CLI does not orchestrate containerized Nginx. Network reachability and Podman Machine forwarding must be verified for the actual topology.

Certbot is an optional maintained external ACME client. HTTP-01 requires owned public DNS and port-80 ingress. Test against staging before production; imported/self-signed certificate tests do not prove public issuance or renewal. Private renewal configuration is checked for unexpected changes and directory hooks are disabled. Review scheduler ownership and executable paths; cron output is a proposal, not automatic installation. Inspect renewal status and test real timer-driven rotation before production reliance.

## Failure recovery and availability

A candidate must pass configured readiness before route switching or predecessor retirement. `running` readiness is weaker than image health or HTTP readiness. Fixed host ports and exclusive writable volumes require explicit maintenance; blue-green availability is conditional and long-lived connection draining is not guaranteed. Rollback restores an image/configuration snapshot, not database migrations or volume contents.

Interrupted deployments remain inspectable and require confirmed recovery. Quadlet uses a private durable transaction journal to restore the prior complete unit/environment/metadata generation after an interrupted file update, without restarting a healthy process solely for file recovery. Unsafe or externally modified journal targets are rejected rather than adopted. Keep state on persistent storage for host-restart recovery. A controller signal cancels application work and waits for its bounded recovery before stopping the TUI.

## Security evidence, not certification

The retained offline CSO assessment is partial (run `1791987376401-d1e1a6587e06c356`). Independent static UI/host review identified a stale-row wrong-host risk and reliability gaps in route removal, image-pin discovery, signal shutdown, bounded JSON input, transport publication and Quadlet crash transactions. These were corrected and covered by targeted behavioral tests and local smoke scenarios; this does not convert the earlier snapshot report into a complete post-fix audit. Redacted spans and the absence of qualified runtime security scanners remain coverage limits. See [verification and unpassed release gates](platform-verification.md).

Report suspected vulnerabilities through the repository's maintainer channels without publishing credentials, private keys, private state or exploit output. No remote release/tag publication is performed by the local commit workflow.
