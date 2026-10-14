# Git deployment and external CI

The platform fetches an explicitly trusted repository with the installed Git CLI, extracts one immutable revision without `.git`, and builds a root `Containerfile` (preferred) or `Dockerfile` using the existing Podman v6 bindings. Builds execute repository instructions inside Podman; inspect those instructions before granting trust. No application webhook daemon is required.

## Manual deployment

```sh
podman-tui platform create web --repository https://github.com/your-team/your-app.git \
  --revision main --trust-build --connection production
podman-tui platform deploy web --connection production
podman-tui platform history web --connection production
podman-tui platform redeploy web --connection production
```

Select a branch, tag, explicit `refs/heads/...` / `refs/tags/...`, or commit SHA. Refspecs, wildcard revisions, force prefixes and revision expressions are rejected. Redeploy retrieves the configured revision again; rollback uses the retained immutable image, not the current branch tip. Configure a different revision through the application JSON editor or `platform configure NAME --file FILE`.

Private repositories use normal Git SSH keys/agents and existing credential helpers. SSH host-key checking is strict; provision and verify known hosts independently. HTTPS credentials must come from a helper, never a credential-bearing URL. Git hooks, checkout/archive conversion filters, automatic submodules and inherited `GIT_*` overrides are disabled. Submodules and Git LFS retrieval are not implemented: vendor required build inputs or explicitly fetch them through reviewed build instructions. Build output is sanitized and bounded; repository tools can still print their own secrets, so do not put credentials in build instructions or build arguments.

Local TLS Git fixtures and real rootless Podman tests cover revision updates, authenticated credential-helper retrieval, build/deploy/history, failed revision preservation and immutable rollback. A live SSH Git-server integration has not been exercised.

## CI-triggered deployment

Keep controller metadata on the deployment host. Serialize deployment jobs for the same host/state directory; its lock rejects concurrent writers rather than racing releases. Install the same binary and supported Podman v6 connection on that host. A CI runner can invoke the host's CLI over existing verified SSH infrastructure:

```sh
ssh -o BatchMode=yes -o StrictHostKeyChecking=yes deploy@deployment-host \
  podman-tui platform redeploy web --connection production
```

The repository and branch are configured on the host; CI does not send arbitrary shell fragments or repository URLs. For immutable CI releases, configure the reviewed commit SHA before deploying using a trusted host-side JSON update. Do not interpolate untrusted branch names, PR titles or payload fields into remote shell commands.

For GitHub Actions, use a deployment job with a protected environment, a narrowly scoped deployment identity and a `concurrency` group for this host/application. On GitLab CI, use protected deployment variables and `resource_group` for serialization. Both can execute the command above after provisioning known-hosts and an SSH agent through their secret mechanisms. Do not enable SSH host-key bypasses, print secrets, persist credentials in the repository, expose the Podman socket publicly or run untrusted fork jobs with deployment access.

The platform does not register CI provider accounts, secrets or runners for you. These are operator-controlled trust boundaries. No GitHub/GitLab hosted pipeline was executed during local verification.
