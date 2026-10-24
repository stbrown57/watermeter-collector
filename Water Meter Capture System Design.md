# Residential Water Meter Capture System — Design (Prototype)

Capture, store, and analyze water usage from a residential Itron water ERT endpoint, using a Nooelec RTL-SDR, a Raspberry Pi 4B running Fedora IoT (atomic OS) with Podman container workloads, and a Go-first collector exporting Prometheus-style metrics into an existing Talos-cluster VictoriaMetrics + Grafana stack (with a K3s Pi 5 Prometheus/Grafana as a secondary option). Images publish to the user's GitHub Container Registry for no-touch updates. The software is tested with simulated meter traffic; RF/USB validation on the Pi is the remaining step.

## Meter Identification (Pending RF Confirmation)

Photos of the meter show an **Itron water ERT module from the 100W/60W series** (model text partially obscured) with printed endpoint ID **81623781** — a candidate ID, not yet a confirmed one. Itron documents the 100W+ variant as transmitting the **SCM+ message**, which `rtlamr` decodes natively, but this module's exact variant, operating mode, and decoded endpoint ID must be confirmed from **received messages** on day one: run rtlamr in listen-all mode, find the ID, note the message type and consumption value, and check the value against the meter's dial. (Sources: https://na.itron.com/products/100w-plus-water-module , https://github.com/bemasher/rtlamr)

- ERT endpoints use passive "bubble-up" broadcasting in the 902–928 MHz ISM band; no handshake is needed and nothing is transmitted to the meter. (Source: https://meterradio.com/protocols/)
- Transmission cadence is set by the endpoint's configuration (typically on the order of seconds between broadcasts); the collector's metrics expose `watermeter_last_message_timestamp_seconds` so any stall is visible.
- The register's **volume-per-count** must be established by comparing decoded changes with observed consumption — the smallest dial increment alone doesn't establish the transmitted scale.

## Architecture

The edge node is a pure collector: it runs one scratch container and exposes metrics on the LAN. Storage and analysis live in the existing Talos cluster's VictoriaMetrics and Grafana; the K3s Pi 5 Prometheus/Grafana can serve as a secondary scraper or fallback path.

```
┌── Edge: Raspberry Pi 4B 4GB — Fedora IoT 44 (USB SSD boot) ─────────────────────┐
│                                                                                │
│  Nooelec RTL-SDR (USB 2 port, 915 MHz antenna)                                 │
│         │                                                                     │
│  ┌──────▼────────────────────────────────────────────┐                        │
│  │ watermeter-collector  (scratch container)          │                        │
│  │  ├─ rtl_tcp          (static C helper, supervised) │                        │
│  │  ├─ rtlamr           (static Go decoder, supervised)│                        │
│  │  └─ Go collector      parses JSON, deltas, metrics  │                        │
│  │      :9101/metrics   /healthz   /data/raw.jsonl   │                        │
│  └────────────────────────────────────────────────────┘                        │
└─────────────────────────┬──────────────────────────────────────────────────────┘
                          │ scrape :9101 (LAN)
          ┌────────────────┼───────────────────────┐
          ▼                                        ▼
┌── Talos cluster (amd64) ──────────┐   ┌── K3s Pi 5 cluster (secondary) ──┐
│  vmagent / VictoriaMetrics        │   │  Prometheus (optional fallback   │
│  (VMScrapeConfig static target)   │   │  scraper, remoteWrite to VM)     │
│         │                         │   └──────────────────────────────────┘
│  Grafana :3000  ◄─ datasource     │
│  dashboards + alerts              │
└───────────────────────────────────┘
```

## Continuous Delivery — No-Touch Updates

All artifacts flow through the user's GitHub Container Registry (ghcr.io), account `stbrown57`. **Nothing has been published yet** — the workflow and edge configuration below become active when the repository `watermeter-collector` is created and pushed:

```
git push to main (or tag vX.Y.Z)
  └► GitHub Actions: go test ─► cross-compile arm64 (native runner, no QEMU) ─► ghcr.io/stbrown57/watermeter-collector
        └► Pi: podman-auto-update.timer (rootless) pulls + restarts
               (Quadlet AutoUpdate=registry)
OS layer: Fedora IoT's automatic OS update mechanism (verify what your release ships).
```

- The edge Quadlet references `ghcr.io/stbrown57/watermeter-collector:latest` with `AutoUpdate=registry`; enabling `podman-auto-update.timer` (user unit, requires lingering) makes the Pi self-update whenever CI publishes.
- If the package is private, the Pi needs a one-time `podman login ghcr.io` with a read:packages PAT (persists across reboots).
- Net effect: application and OS updates both roll out without touching the Pi — once the repo exists and the first image is published.

## Component Details

### Radio
- Nooelec NESDR SMArt-class RTL-SDR (RTL2832U + R820T2, 100 kHz–1.75 GHz) covers the ERT band with margin. (Source: https://www.nooelec.com/store/sdr/sdr-receivers/smart.html)
- Use an SMA antenna tuned near 915 MHz rather than the stock telescoping whip.
- Put the SDR on a USB 2 port and keep it physically away from the USB 3 SSD — USB 3 is a documented broadband RF noise source in the 900 MHz range.

### Collecting node — Fedora IoT on Pi 4B
- The Pi 4 (4B/CM4) is a **tested reference platform** for Fedora IoT, which is exactly your atomic-OS requirement: image-based, signed, transactional ostree updates, Podman-first, no package drift. (Source: https://docs.fedoraproject.org/en-US/iot/reference-platforms/)
- Booting from USB 3 SSD avoids SD-card wear — a good fit for a long-lived data collector.
- Container workloads are declared as Quadlet `.container` files under `~/.config/containers/systemd/`, started natively by systemd at boot (with `loginctl enable-linger`). (Source: https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html)

### Capture — watermeter-collector (Go, scratch container)
A single Go binary supervises the pipeline and owns the data:

1. Spawns **rtl_tcp** (the one C helper; links the USB dongle to TCP) and restarts it with backoff.
2. Spawns **rtlamr** (pure Go, static binary) with `-format=json -msgtype=scm+ -filterid=81623781` and reads its stdout line by line.
3. Parses the JSON (handles ID/EndpointID/ERTSerialNumber field variants across protocols), computes usage deltas with explicit rollover/reset handling, and serves Prometheus metrics on `:9101`.
4. Appends every decoded raw message to `/data/raw.jsonl` — a durable archive for later analysis.

rtlamr is a Go project that ships static binaries and is installable via `go install`; its decoder covers SCM/SCM+/IDM and it talks to rtl_tcp over TCP. (Sources: https://github.com/bemasher/rtlamr , https://github.com/bemasher/rtlamr/releases)

### Storage — VictoriaMetrics in the Talos cluster (existing)
- Chosen over Prometheus: single small static binary, no dependencies, excellent compression, Prometheus-compatible query and ingestion. (Source: https://docs.victoriametrics.com/victoriametrics/single-server-victoriametrics/)
- Scrape the Pi directly from the Talos side: a `VMScrapeConfig` (VictoriaMetrics operator) or a static target in your vmagent config pointing at `PI_LAN_IP:9101`. Example manifest in `deploy/cluster/vmscrapeconfig.yaml`.
- If the K3s Prometheus is preferred as the scraper, it can remote-write into the same VictoriaMetrics (`deploy/cluster/k3s-prometheus.yaml`), keeping one source of truth for dashboards.
- Grafana (Talos) uses a Prometheus-type datasource at the VM endpoint.

### Display — Grafana (Talos, existing)
- Dashboards and alerting in the existing Talos Grafana; the K3s Grafana can be pointed at the same VictoriaMetrics if you want water data in both UIs.

### Later AI analysis
- `/data/raw.jsonl` preserves full-fidelity decoded messages for offline training/analysis.
- VictoriaMetrics exports CSV/JSON over HTTP for building datasets, and MetricsQL gives the same data to any AI tooling you later point at it.

## Data Handling
- `watermeter_register` mirrors the meter's odometer (verify against the physical dial on day one).
- `watermeter_usage_counts_total` counts only verified positive deltas; decreases are counted in `watermeter_resets_total` and logged — or, with `-rollover-at <bound>`, handled as a proper rollover.
- Raw JSONL + metrics retention are independent: metrics drive dashboards, raw archive feeds future analysis.

## Staged Validation
0. **Identify (RF):** run rtlamr in listen-all mode on the Pi; confirm endpoint 81623781 appears, note the actual message type (SCM/SCM+/IDM) and consumption value, and verify the value against the meter's dial. This closes the compatibility question and fixes the meter ID/protocol configuration.
1. **Baseline:** confirm the decoded consumption value tracks the physical dial over a day; establish volume-per-count.
2. **Collector:** run the scratch container; confirm `:9101/metrics`, `/healthz` ok, `watermeter_register` matches the dial, and the raw log grows.
3. **Storage:** add the scrape target in the Talos cluster (VMScrapeConfig or vmagent static config); confirm the `up` metric and dashboards in the existing Grafana.
4. **Resilience:** reboot the Pi (Fedora IoT transactional update test optional); verify containers self-start, data volumes persist, and reception resumes.
5. **Alerts:** add Grafana alerting or vmalert for reception stalls and continuous-flow (leak) patterns.

## Files in this project
- `watermeter-collector/` — Go collector: `main.go`, `main_test.go`, `go.mod`, `Dockerfile` (scratch, cross-compiled with no QEMU — the rtl_tcp static-link recipe verified end-to-end on amd64), `README.md`, `.github/workflows/release.yml` (ghcr.io publish: tests → arm64 build), edge Quadlet in `deploy/`, cluster-side templates in `deploy/cluster/` (verify against your operator/CRD versions).
- Tested in sandbox: 9 unit tests (duplicates, out-of-order blips, spurious resets with and without post-reset climbs, confirmed resets with window expiry, known rollover, stale-low readings with a bound, restart-gap recovery), amd64 + arm64 builds, and smoke tests including a child crash/restart cycle and SIGTERM teardown with no orphaned process groups (verified via process-group kill).
- Not yet done: first successful ghcr.io publish (three failed CI runs: the naive static link, a deterministic archive-path bug — cmake writes subdirectory targets to `/src/build/src/`, the link step referenced `/src/build/` — and then the QEMU question; the current Dockerfile abandons cmake entirely, compiling the rtl-sdr sources directly with a musl cross toolchain on the native runner, so neither the path bug nor QEMU applies) and RF/USB validation on the Pi.
- Consumption semantics: ambiguous decreases are quarantined (never auto-applied); confirmed resets can be reverted within a 10-reading window if the register returns to baseline, so bad decodes cannot fabricate or lose usage.

## Sources
- Itron 100W+ water module (SCM+): https://na.itron.com/products/100w-plus-water-module
- rtlamr (ERT decoder, SCM/SCM+/IDM): https://github.com/bemasher/rtlamr
- rtlamr releases (static binaries): https://github.com/bemasher/rtlamr/releases
- ERT protocols overview: https://meterradio.com/protocols/
- Fedora IoT (immutable, container-first): https://docs.fedoraproject.org/en-US/iot/
- Fedora IoT reference platforms (Pi 4 tested): https://docs.fedoraproject.org/en-US/iot/reference-platforms/
- Podman Quadlet units: https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html
- VictoriaMetrics single-node (self-scraping): https://docs.victoriametrics.com/victoriametrics/single-server-victoriametrics/
- Nooelec NESDR SMArt: https://www.nooelec.com/store/sdr/sdr-receivers/smart.html
