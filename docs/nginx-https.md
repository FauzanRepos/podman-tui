# How to route applications through Nginx and HTTPS

This guide describes the implemented optional host Nginx integration for the **v2.1.0 target**, not a released-platform announcement. Deployment does not require Nginx. When enabled, podman-tui writes one owned include file per application, validates the whole Nginx configuration, and reloads the existing Nginx instance. It does not install Nginx, start it automatically, replace its main configuration, or escalate privileges.

**Supported topology:** Nginx runs on the controller host, where the `podman-tui` command runs. Containerized Nginx configuration/reload orchestration is **not implemented**. A remote Podman backend is possible with explicit local-integration authorization and a backend address reachable from both the controller and Nginx. This flag does not run commands or copy certificates to the Podman server.

See [platform overview](platform.md), [deployment architecture](deployment-design.md), and [Git deployment](git-deployment.md) for the application/release workflow.

## Prerequisites and privilege boundaries

- Install a maintained Nginx binary on the controller. For ACME issuance, also install a maintained `certbot` executable on that host.
- Use the same OS account for the CLI, its state, and a user-owned Nginx master in the isolated example below. This account must be able to read the configuration and keys, write the managed include directory and logs, and signal the intended Nginx master.
- A local Unix Podman connection authorizes local Nginx integration automatically. For SSH/other remote connections, pass `--local-integrations` explicitly. `PODMAN_TUI_LOCAL_INTEGRATIONS=1` is the environment alternative, but explicit flags make operational scripts easier to audit.
- The CLI never invokes `sudo`, changes privileged-port policy, installs firewall rules, or fixes permissions automatically. Ports 80/443 can require administrator-managed listener/forwarding arrangements. The isolated example uses 8088/8443; it needs no change to global privileged-port settings.
- Do not expose the Podman socket to Nginx or the public network. Nginx forwards ordinary HTTP to published application ports, not the Podman API.
- Paths in proxy configuration must be clean absolute paths without control characters, `$`, or semicolons. The managed directory must not be a symlink. Keep it private and exclusively managed by this integration.

A root-owned system Nginx cannot be assumed compatible with an unprivileged CLI: validation, reload signals, include traversal, and private-key access all need an explicitly designed permission model. Do not make certificate keys world-readable to work around this. Prefer an isolated same-user instance, or have the host administrator provision a suitable arrangement separately.

## Start an isolated host instance without changing global configuration

Run these commands in one shell on the controller, with an installed `nginx` and an already selected working Podman connection. They create a new private directory, not `/etc/nginx/nginx.conf`. Ports 8088 and 8443 must be free.

```sh
umask 077
PROXY_ROOT=$(mktemp -d /tmp/ptui-nginx.XXXXXX)
NGINX_BIN=$(command -v nginx)
export PROXY_ROOT NGINX_BIN
mkdir "$PROXY_ROOT/managed" "$PROXY_ROOT/webroot"
cat > "$PROXY_ROOT/nginx.conf" <<EOF
pid "$PROXY_ROOT/nginx.pid";
error_log "$PROXY_ROOT/nginx.error";
events {}
http {
    access_log off;
    include "$PROXY_ROOT/managed/*.conf";
}
EOF
"$NGINX_BIN" -t -c "$PROXY_ROOT/nginx.conf"
"$NGINX_BIN" -c "$PROXY_ROOT/nginx.conf"
```

For an existing host instance, an administrator can instead add a dedicated `include "/absolute/managed/directory/*.conf";` **inside its existing `http` block**, validate that configuration, and arrange access before using podman-tui. Do not replace the main configuration with the isolated example. `proxy.config_file` must identify the actual configuration used by that running master. The CLI runs the selected binary with `-T`, `-t`, and `-s reload`, each with `-c` pointing to that file. It does not provide arbitrary reload commands or Nginx container-exec hooks.

## Create a domain route with a dynamic backend port

The following application serves a small HTTP page using BusyBox. The example domain `web.example.invalid` deliberately needs no public DNS. Use the `Host` header when checking it locally.

```sh
cat > "$PROXY_ROOT/web.json" <<EOF
{
  "name": "web",
  "image": "docker.io/library/busybox:1.37",
  "profile": "production",
  "profiles": {
    "production": {
      "ports": [{"container": 8080, "ip": "127.0.0.1", "protocol": "tcp"}],
      "command": ["sh", "-c", "mkdir -p /www && echo ready > /www/index.html && exec httpd -f -p 8080 -h /www"]
    }
  },
  "domains": ["web.example.invalid"],
  "proxy": {
    "managed_dir": "$PROXY_ROOT/managed",
    "config_file": "$PROXY_ROOT/nginx.conf",
    "binary": "$NGINX_BIN",
    "backend_host": "127.0.0.1",
    "container_port": 8080,
    "http_port": 8088,
    "https_port": 8443,
    "webroot": "$PROXY_ROOT/webroot"
  },
  "readiness": {"mode": "http", "path": "/", "timeout_seconds": 30}
}
EOF
podman-tui platform create web --file "$PROXY_ROOT/web.json"
podman-tui platform deploy web --local-integrations
curl --fail -H 'Host: web.example.invalid' http://127.0.0.1:8088/
podman-tui platform status web
```

The expected application response is `ready`. These are instructions, not a claim that this exact shell session has been executed.

`ports[].host` is intentionally omitted (equivalent to zero): Podman chooses a free host TCP port for each candidate. `proxy.container_port` must match a dynamically published TCP port in the selected profile. A fixed host port does not satisfy proxy validation. `backend_host` accepts an IP address or `localhost`, not an arbitrary DNS name. It is the address of the **Podman backend's published port as reachable by the controller and Nginx**. The runtime discovers the assigned port; do not insert a guessed port in the include.

With a local backend, keep the published port on loopback as shown. With a remote backend, `127.0.0.1` is not automatically the remote machine: configure a reachable backend IP and an appropriate publish IP, restrict network access separately, and pass the same `--connection` and `--local-integrations` on operations. A Podman machine's port forwarding is an additional topology requirement, not something this module configures. Certificates, webroot, Nginx config and managed files remain on the controller.

### Proxy configuration reference

| JSON field | Meaning |
| --- | --- |
| `managed_dir` | Required absolute directory for owned `ptui-APPLICATION_ID.conf` files and private TLS state. |
| `config_file` | Required absolute main configuration path of the running host Nginx. |
| `binary` | Nginx executable; defaults to `nginx` when omitted. Prefer an absolute executable path for scheduled operations. |
| `backend_host` | Required IP address or `localhost` reachable by the controller and Nginx. |
| `container_port` | Required container port with dynamic TCP publication in the active profile. |
| `http_port`, `https_port` | Listener ports; omitted/zero means 80 and 443 respectively. |
| `webroot` | Optional absolute directory served for HTTP-01 challenges. Required for managed ACME issuance. |
| `certificate`, `private_key` | Absolute paths; both must be present together. Prefer validated `tls-import` over manually setting these fields. |
| `redirect_https` | Defaults false. Requires a certificate/key; emits an HTTP 308 redirect while preserving the challenge location. |

The generated upstream uses HTTP/1.1 and forwards `Host`, `X-Real-IP`, `X-Forwarded-For`, and `X-Forwarded-Proto`. It sets `Upgrade` and `Connection: upgrade` for WebSockets. There is no configurable timeout/header template API here; applications must trust forwarded headers only from their intended proxy boundary.

## What a safe switch guarantees, and what it does not

The candidate starts before routing changes. HTTP readiness requests the configured path at its published endpoint and requires a 2xx response. `health` readiness requires an image `HEALTHCHECK` and a healthy result. The default `running` mode proves only that the process is running; it is not an application-health guarantee.

After readiness, the router obtains a managed-directory lock, reads the old owned include, checks `nginx -T` for conflicting domains, atomically replaces the include, runs `nginx -t`, and reloads. On activation failure it attempts to restore and reactivate the previous include. It refuses to overwrite a file without the application's ownership marker, and refuses a stale undo when routing changed after the switch.

This supports **health-based, conditional zero-downtime cutover**, not an unconditional availability promise. Concurrent releases need separate published ports and sufficient resources. Writable shared volumes require explicit `concurrent_safe` authorization or a maintenance deployment; never authorize overlapping writers unless the application actually supports them. Maintenance deployments can interrupt service. Readiness does not prove every user path works, and stopping the previous container can interrupt long-lived requests or WebSockets; connection draining is not implemented as an independent guarantee.

Domains must be unique in application configuration. The Nginx conflict scan rejects exact matches and matching wildcard/dot-prefix names in other included server blocks. External regex `server_name` entries require manual conflict resolution. It does not silently take over another virtual host, even on a different listener. Resolve ownership deliberately rather than deleting an unrelated include.

## Import an existing certificate

`tls-import` is the CLI operation corresponding to certificate import; there is no `importCert` CLI flag. Use a PEM certificate/full chain and its matching private key. Both input files must be regular files no larger than 4 MiB. Private-key permissions must be `0600` or stricter. The leaf certificate must be currently valid and cover **every** application domain through hostname validation, including its Subject Alternative Names (SANs).

Set these variables to existing certificate files you own, then run:

```sh
: "${CERTIFICATE_FILE:?Set CERTIFICATE_FILE to an existing PEM certificate/full chain}"
: "${PRIVATE_KEY_FILE:?Set PRIVATE_KEY_FILE to its matching private key}"
podman-tui platform tls-import web "$CERTIFICATE_FILE" "$PRIVATE_KEY_FILE" --local-integrations
podman-tui platform tls-status web --local-integrations
```

Do not import a public-domain certificate into the `.invalid` example unless it also covers that domain. For a real application, configure its actual domains before deployment/import. Import checks the pair, validity dates and hostname coverage; it does not establish public CA trust or run a full external trust-chain audit. A staging/self-signed certificate can pass these local checks but still be rejected by browsers.

Import copies the validated pair to a private versioned directory under `managed_dir/.certificates/APPLICATION_ID/`, switches Nginx transactionally, and persists the new certificate paths only on successful application. Certificate directories below `managed_dir` must be private and non-symlink; generated files are private. The running Nginx master must be able to traverse/read them. Retain old versions for recovery rather than deleting files still referenced by a live route.

Import enables HTTPS alongside HTTP; it does **not** automatically enable redirects. To enable redirects, export the application after successful import, change only `proxy.redirect_https` to `true` in that JSON (preserving the imported paths), configure it, and deploy:

```sh
podman-tui platform export web > "$PROXY_ROOT/web-https.json"
# Edit web-https.json: set proxy.redirect_https to true, retaining the certificate/key paths.
podman-tui platform configure web --file "$PROXY_ROOT/web-https.json"
podman-tui platform deploy web --local-integrations
```

TLS listeners allow TLS 1.2 and 1.3. The HTTP redirect is 308, preserves the request URI, and includes a nonstandard HTTPS port such as 8443 when configured. The ACME challenge path remains served from the webroot rather than redirected.

## Issue through Certbot, starting with staging

**Public ACME issuance and renewal have not been live-verified for this target.** No owned public DNS plus public port-80 environment was available. Local HTTPS import, redirect and invalid-certificate rejection do not prove HTTP-01 works on a public network. Treat a successful owned-domain staging issue **and renewal** as a public-release gate before relying on this path.

Before running issuance for a real application:

1. Configure actual owned public domain names, not `web.example.invalid`. All requested names need correct public DNS reaching this Nginx host; check both IPv4 and IPv6 records where present.
2. Make the HTTP-01 challenge reachable from the public Internet on **port 80**, including firewall/NAT/load-balancer forwarding. Setting `http_port` to 8088 alone does not satisfy the CA. DNS-01, wildcard issuance, and standalone Certbot listener management are not implemented.
3. Configure an absolute `webroot`, deploy the HTTP route, and ensure Certbot can write it and Nginx can serve it. A file beneath `webroot/.well-known/acme-challenge/` must be retrievable at the corresponding public URL for every domain. Do not use a broad webroot containing secrets.
4. Have a working contact email and review the selected CA's terms before explicitly accepting them.

For an already deployed application named `web` that meets those requirements:

```sh
: "${ACME_EMAIL:?Set ACME_EMAIL to your ACME account contact email}"
podman-tui platform tls-issue web --email "$ACME_EMAIL" --staging --agree-tos --local-integrations
podman-tui platform tls-status web --local-integrations
```

`--agree-tos` is required and has no implicit default acceptance. `--staging` selects Let's Encrypt's staging directory; omit it only when intentionally requesting a production certificate after staging checks. Staging certificates are not publicly trusted. There is no arbitrary CA URL flag.

The implementation calls Certbot `certonly` in webroot/noninteractive HTTP-01 mode with application-scoped config/work/log directories, an explicit empty client config, no directory hooks, and `--keep-until-expiring`. It then imports and activates the resulting pair using the same validation/reload path. It does not let Certbot edit the global Nginx configuration. Certbot output is not printed; failure points to private managed logs. Request state records domains, email, staging selection, webroot, the absolute Certbot executable path, and a hash of the managed renewal configuration.

## Renew and schedule

```sh
podman-tui platform tls-renew web --local-integrations
podman-tui platform tls-status web --local-integrations
```

Renewal reuses the saved ACME request and Certbot's keep-until-expiring behavior. An import-only certificate has no managed ACME request and cannot be automatically renewed; obtain a replacement through its issuer and import it again. `tls-status` reports expiry, last attempt/success/error, staging and whether request state exists, without returning key material. `renewable` means saved request state exists, not that a public renewal has succeeded.

Changed domains or webroot require explicit `tls-issue` again. A changed managed Certbot renewal file is rejected to avoid possible hook injection. Do not edit the lineage configuration or bypass its stored hash to suppress that error.

On a Linux host with systemd and a working user manager:

```sh
podman-tui platform tls-schedule web --yes --local-integrations
```

This installs only owned `ptui-tls-APPLICATION_ID.service` and `.timer` files in `$XDG_CONFIG_HOME/systemd/user` (or `~/.config/systemd/user`), reloads the user manager, and enables/starts the timer. The schedule is daily, with up to one hour randomized delay and `Persistent=true`. It refuses unmanaged unit-file replacements and attempts to restore prior files if installation fails. It does not enable login lingering or promise execution after logout/reboot without a functioning user manager. Coordinate that host lifecycle separately.

For a portable scheduler entry:

```sh
podman-tui platform tls-schedule web --cron --local-integrations
```

This **prints**, but does not install, a shell-quoted daily 03:00 cron entry. Review it and add it to that account's scheduler using normal host procedures. Do not install both a timer and cron entry for the same application. Non-Linux hosts must use a suitable host scheduler; a systemd user timer cannot be installed there.

Both forms capture the current podman-tui executable's absolute path, absolute state root, resolved connection name and stable application ID, and invoke `platform tls-renew ... --application-id --local-integrations`. Renames therefore retain the renewal identity. Keep the binary location, state directory, named connection, credentials, SSH agent/authentication, Nginx executable/config and saved Certbot executable available to the scheduler's noninteractive environment. Reinstall/update schedules after relocating the binary or changing the state/connection selection. Use explicit `--state-dir` and `--connection` when generating schedules if operational scripts must not follow changing defaults. The timer does not transfer state or credentials to another machine.

## Troubleshooting and recovery

| Symptom | Action |
| --- | --- |
| `nginx command failed` / invalid configuration | Run the configured binary with `-t -c` against the same absolute config and inspect its error log. Check that the managed include is inside `http`, ports are free, paths are accessible, and the config belongs to the running master. Correct the actual error; do not disable validation. |
| Route exists in metadata but no traffic arrives | Check public DNS/Host header, listener/firewall reachability, the main config's include, and the published endpoint from `platform status`. Both controller readiness and Nginx must reach `backend_host`. `--local-integrations` does not create a tunnel. |
| Domain conflict or regex warning | Inspect the configured instance with `nginx -T -c` and resolve the existing virtual host's ownership manually. Do not paste unreviewed config dumps into public reports. |
| Invalid PEM / mismatched key / expired certificate | Supply the matching currently valid PEM pair. Keep the private key private. Check host clock and input-file access; import rejects invalid pairs before switching. |
| Certificate does not cover every domain | Obtain a certificate whose SANs cover all configured names, or deliberately correct the desired domain set. Changing only the Common Name is not a substitute for valid hostname coverage. |
| Nginx cannot read imported key | Use a compatible master/account permission model. Do not loosen the generated private directories or keys to world-readable permissions. |
| Certbot fails | Inspect the private `logs` directory under the application's certificate root. Check maintained client installation, saved executable, DNS, every public HTTP-01 path, port 80, and CA policy/rate limits. No real public staging/renewal success is claimed here. |
| Renewal rejects domains/webroot changes | Explicitly issue for the new desired domain/webroot configuration after restoring challenge reachability. |
| Renewal configuration changed | Investigate tampering or manual lineage edits. Recover trusted private state from an operational backup rather than editing the stored hash. |
| Timer/cron does not run | Inspect the scoped user timer/service or host scheduler logs. Check stable executable paths, state and connection selection, noninteractive authentication, user-manager lifecycle, and Certbot availability. A successful installation is not evidence of a completed renewal. |
| Recovery-required error | Treat the live route as uncertain. Preserve the old and replacement containers, include and certificate versions, and metadata. Inspect the configured Nginx instance and `platform status`/`history` before further mutations. Fix the underlying permission/config/reload error and reconcile actual live routing with the intended release; do not delete files or blindly retry cutovers. |

For the isolated example only, stop its Nginx master when finished with `"$NGINX_BIN" -s quit -c "$PROXY_ROOT/nginx.conf"`. This does not stop a separately configured system instance. Do not remove its directory while an application or scheduler still references it.

## Evidence and remaining gates

The implementation was inspected in `deploy/proxy`, application validation/runtime/readiness code, and `cmd/platform_integrations.go`. Existing real isolated host-Nginx checks exercised HTTP switching/undo, domain conflict rejection, invalid TLS rejection, certificate import, HTTPS redirect and WebSocket frames. Those checks are local integration evidence, not public CA or production-load evidence. Public owned-domain staging issuance, real renewal, and scheduler-driven end-to-end renewal remain deployment gates. Host port privileges, certificate access and remote/backend reachability must be established for each actual topology.
