# Persistent services, profiles and secrets

Backing services are optional applications with managed labels, an application-scoped private network and an explicitly owned named data volume. No database ports are published. PostgreSQL, MariaDB/MySQL and Redis are not metadata dependencies of the platform itself.

## Provision and link

Supply a database password to `platform secret-create NAME` on stdin from a trusted password manager or private input file; never put its value in a command argument, URL, repository or application JSON. Then provision one service:

```sh
podman-tui platform service-create database --type postgres --secret database-password --connection production
podman-tui platform service-create cache --type redis --connection production
podman-tui platform service-link web database --inject-host --connection production
podman-tui platform deploy web --connection production
podman-tui platform status database --connection production
```

`--type` also accepts `mariadb` and `mysql`. Database presets use supported image `_FILE` environment settings and root-owned read-only Podman secret mounts. They create database/user `app`; MySQL/MariaDB restrict root access to localhost. Verify image versions, backups, migrations and resource requirements before production use. General OCI backing services can use application JSON with explicit image/storage/network configuration.

Service linking authorizes network membership. `--inject-host` separately authorizes non-sensitive host/port/database/user settings; the stable host is `ptui-SERVICE-APPLICATION-ID`. Linking does **not** copy passwords, secret references or data volumes into the application. Explicitly authorize appropriate application secret references separately. Inspect desired configuration and redeploy after linking.

For authenticated Redis, `--secret NAME` references an existing **complete Redis ACL file**, not a bare password. Prefer `user default off` and hashed password entries for a narrowly scoped user. The mount is `/run/secrets/redis.acl`, UID/GID 999, mode 0400 for the selected `redis:7.4-bookworm` image. The Redis command contains the ACL file path, never its credential. Without an ACL, Redis is unauthenticated on its private service network; do not authorize untrusted containers onto that network.

Real rootless Linux arm64 integration exercised unauthenticated/authenticated Redis, private ports, unauthorized-client rejection, wrong-password rejection, data persistence, retained-volume recreation and ownership collisions. PostgreSQL, MariaDB and MySQL also exercised real SQL writes/reads, invalid credentials and persistence across restart. These checks are not database hardening, backup or hardware-capacity certifications.

## Configuration and profiles

Applications have development, staging and production profiles. The selected `profile` must exist in `profiles`. Configuration changes are desired state, not an implicit live restart. Use F9 → `m` → configure, Ctrl+S to save JSON, then deploy; or:

```sh
podman-tui platform export web --connection production
podman-tui platform configure web --file web.json --connection production
podman-tui platform deploy web --connection production
```

The `env` map is explicitly **non-sensitive**. A secret reference is:

```json
{"name":"database-password","target":"DB_PASSWORD","type":"env"}
```

For a mounted secret use an absolute target and optional container UID/GID:

```json
{"name":"database-password","target":"/run/secrets/database-password","type":"mount","uid":1000,"gid":1000}
```

Exports include non-sensitive configuration and secret **references**, not Podman secret values. The original secret-inspection screen requests metadata only. Managed-container inspection redacts runtime environment values. Applications and build tools can still disclose credentials in their own logs; the platform cannot infer or revoke secrets printed by application code.

## Lifecycle and data preservation

```sh
podman-tui platform stop database --connection production
podman-tui platform start database --connection production
podman-tui platform restart database --connection production
podman-tui platform delete database --yes --connection production
```

Deletion removes only managed release containers/metadata/routing/startup definitions. Named volumes, service networks and secrets are deliberately retained. Inspect labels and backups before operator-controlled storage removal. Network or volume collisions with unmanaged/another application's resources are rejected; no global prune is used.

Writable persistent storage defaults to exclusive writers. Redeploying a running service normally requires explicitly authorized maintenance downtime. Do not enable `concurrent_safe` for a database merely to make a blue-green deployment pass. Rollback restores image/configuration, never database migrations or volume contents.

Running-container readiness is not a database protocol readiness guarantee. Consumers should use their application's retry/readiness handling; integration tests explicitly waited for the actual Redis/SQL protocol. Tune memory/CPU limits and retain enough storage for data, logs, images and backups.
