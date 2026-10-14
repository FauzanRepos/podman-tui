# Platform verification and measured requirements

These are observed local results, not hardware guarantees or a v2.1.0 release certification. Native controller: Darwin 25.4 arm64, checksum-verified Go 1.25.9. Dedicated test server: rootless Podman 6.1.4 Linux arm64 on Podman Machine, 1 CPU, 2048 MiB RAM, 10 GiB disk. Nginx 1.31.6 ran in isolated local configurations. No production containers were used.

## Compatibility

| Target | Build evidence | Runtime evidence / limit |
| --- | --- | --- |
| Linux ARM64 | Vendored remote-tag, CGO-disabled build passed | Real rootless applications/Git/services/Quadlet VM scenarios passed |
| Linux AMD64 | Same cross-build passed | No physical AMD64 runtime tested |
| macOS Apple Silicon | Native build passed | Actual connected TUI/CLI through Podman Machine passed |
| Darwin AMD64 | Existing target cross-build passed | No Intel Mac runtime tested |
| Windows AMD64 | Existing target cross-build passed | No Windows runtime tested; Quadlet/systemd unavailable |
| Raspberry Pi-class ARM | Linux ARM64 binary builds | Best effort; no physical Pi or resource-constrained device tested |
| Rootful Linux/system-wide units | Code handles root-owned local units | Not runtime-tested; no automatic privilege elevation |

Build flags: `-mod=vendor -tags 'exclude_graphdriver_btrfs containers_image_openpgp remote'`, `CGO_ENABLED=0` for cross targets. No new Go dependencies were added. Optional Git, Nginx, Certbot and systemd remain external dependencies selected only for their features.

## Actual controller measurements

The connected comparison rebuilt original commit `8908354e19e97030a073fd96717300eaee08f467` in a disposable archive directory. Both versions used real 100×30 tmux terminals, the same dedicated connection and existing server resources. One ten-second observation per case; RSS is process resident memory, CPU percentages are instantaneous `ps` samples, not a statistical capacity specification.

The enhanced measurements were taken before the final security/recovery stabilization fixes, at implementation commit `a6dfd94b2875b7062e138a23b273dd1ddaa9d5ca`. They must not be presented as a multi-run benchmark of the final release.

| Scenario | Original RSS at 10s (KiB) | Enhanced RSS at 10s (KiB) | Original CPU time at 10s | Enhanced CPU time at 10s | Idle CPU samples at 5s/10s |
| --- | ---: | ---: | --- | --- | --- |
| Connected, F2 requested | 35,584 | 36,432 | 0.12s | 0.15s | 0.0% / 0.0% both |
| Connected container monitoring, F4 requested | 35,520 | 37,584 | 0.11s | 0.12s | 0.0% / 0.0% both |

The observed RSS differences are 848 KiB and 2064 KiB. These small single-run differences do not establish a general regression or memory budget. The separate unchanged disconnected baseline was 32,640 KiB at ten seconds, CPU time 0.07s, first terminal output 0.0890s.

With a real `script` pseudo-terminal wrapper (wrapper startup included), original first output/connected status were 0.1194s/2.1482s; enhanced were 0.1131s/2.1225s. These measure terminal output and the first `STATUS_OK`, not completion of every lazy screen initialization.

| Enhanced F9 catalog scenario | RSS at 10s (KiB) | CPU time at 10s | CPU at 5s/10s |
| --- | ---: | --- | --- |
| 0 managed metadata applications | 37,856 | 0.14s | 0.0% / 0.0% |
| 50 metadata applications | 38,176 | 0.14s | 0.0% / 0.0% |
| 50 metadata applications, 5 running managed containers | 38,064 | 0.14s | 0.0% / 0.0% |

One real 100-cycle F9/F4 navigation observation sampled RSS at 0/50/100 cycles: 33,536 / 36,704 / 28,000 KiB. No persistent growth was observed over that bounded run; it is not proof of leak freedom under arbitrary workloads.

After final stabilization, an actual connected 100×30 Containers screen (F4 selected and visually observed) sampled 37,696 KiB RSS, 0.0% CPU and 0.17s accumulated CPU time after ten seconds on that screen. SIGTERM exited normally. This additional sample does not replace the controlled original/enhanced comparison above.

Five warm BusyBox image deployments with three-observation running readiness took 0.7820, 0.6889, 0.6836, 0.6920 and 0.6913 seconds. A real remote Podman build of a tiny BusyBox/HTTP COPY context with the base image already present took 0.2311 seconds inside the build call. Its disposable controller process, including connection and scoped image cleanup, took 1.08s wall time, 0.03s user CPU, 0.02s system CPU and 29,622,272 bytes maximum RSS. These are warm-cache examples, not representative application build guarantees; image downloads and application instructions dominate cold builds.

The deployment layer has no permanent process of its own. TUI logs are bounded at 64 KiB, Podman runtime logs at 4 MiB, source extraction at 512 MiB/100,000 entries and metadata at 16 MiB. One UI operation per screen and one filesystem writer per endpoint prevent unbounded deployment concurrency. Rootless backing databases consume their own resources and are optional.

## Verification results

- Initial native tests/build/vet passed before source changes: 8 tested packages, 26 without tests.
- Stabilized full unit suite and vet passed: 17 tested packages, 24 without tests. Existing progress tests were isolated from an unsafe live event-loop setup; production dialog code was unchanged. Targeted race checks passed for deployment, Quadlet, registry and Applications; Darwin's linker emitted LC_DYSYMTAB warnings, not race failures.
- Real Podman build/deploy/failed-candidate preservation/rollback/ownership and original secret-safe inspection passed.
- Real TLS Git credential-helper/revision/hook/filter/cancellation fixtures and Git→Podman build/deploy/update/error-preservation/rollback passed (40.85s combined).
- Real isolated Nginx switching/undo/domain conflicts/invalid configuration rejection, verified HTTPS import/redirect and bidirectional WebSocket frames passed.
- A real Podman→Nginx health-gated release switch had 189 HTTP observations with responses from the old/new release and no sampled request errors; failed-candidate preservation and immutable-image HTTP rollback also passed. This is a bounded scenario, not an uninterrupted-deployment guarantee.
- Redis unauthenticated/authenticated/private network/authorization/data persistence/retained storage/unmanaged isolation passed. The two initial scenarios took 50.09s and 394.81s with cold setup/cleanup.
- PostgreSQL/MySQL/MariaDB SQL credential and restart-persistence scenarios passed individually: 140.08s, 48.64s and 15.53s. A combined cold-image invocation exhausted Go's default ten-minute package deadline; it is recorded as a failed aggregate run, not hidden.
- Real Linux Quadlet generation/start/discovery/corrected active-unit cutover passed (11.71s). The dedicated VM was then stopped/started: persisted home-directory metadata recovered a running application and `ActiveState=active`, `SubState=running`, `Result=success` (22.58s including VM restart).
- Actual F9 rendering at 40×20 preserved F1–F9 menu discoverability, and simulation/event-loop operation tests verified responsive commands and Esc cancellation. Existing Containers/Secrets inspection paths were exercised against real secrets.
- New route-recovery, pin cleanup, input-limit, transport-selection and stale-TUI-row regressions failed against the prior committed implementation and passed after correction. Real rootless unjournaled-pin/ownership/retained-image checks passed (11.04s). A real idle TUI ignored SIGTERM before correction and exited after signal cancellation was wired into shutdown.
- Final Quadlet crash/second-crash/legacy-migration tests and race checks passed. A real systemd-started container initially received literal quote characters from incorrectly quoted env-file values; the corrected Podman consumer boundary and live unit now return the exact configured value. Final unit/build/vet and all five target builds passed; the local commit helper's five tests passed.

## Unpassed release gates

Public Let's Encrypt staging issuance, real HTTP-01 renewal and scheduler-triggered certificate rotation/reload have not run: no owned publicly resolvable application domain/public port-80 ingress was supplied. Local TLS import is not evidence of public ACME success. Live SSH Git-server and hosted GitHub/GitLab pipelines were not exercised.

The trusted offline CSO helper retained a **partial** static assessment, run `1791987376401-d1e1a6587e06c356`. Independent static UI/host review subsequently identified a stale-row wrong-host risk and recovery/crash-consistency defects; local behavioral verification covers the corrections, but there is no complete post-fix security certification. Redacted spans and the absence of qualified runtime scanner witnesses remain coverage limits. No live provider credential probing was used.

Existing Bats tests require a controlled Linux/root setup and were not run on the macOS controller or against production. No physical AMD64/Pi, rootful host, Intel Mac or Windows runtime compatibility is claimed. Version remains `2.1.0-dev`; these gaps prohibit creation of the final annotated `v2.1.0` release tag.
