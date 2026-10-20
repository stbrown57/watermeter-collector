# watermeter-collector

Reads an Itron 100-series water ERT endpoint (SCM+ messages, 900 MHz ISM band) with a Nooelec RTL-SDR and exposes usage as Prometheus-style metrics for your existing VictoriaMetrics (Talos) / Prometheus (K3s) + Grafana stack.

**Status: compile-tested, unit-tested, and smoke-tested with simulated meter traffic. Awaiting RF/USB validation on real hardware.** The container recipe has not been built end-to-end yet (the static `rtl_tcp` stage is the known finicky part — see below).

## What it is

One Go supervisor binary plus the two helper executables it manages:

```
Nooelec RTL-SDR ─USB─► rtl_tcp ─TCP─► rtlamr (JSON) ─► watermeter-collector ─► :9101/metrics
                (C helper)        (Go, static)          (Go, this project)   └► /data/raw.jsonl
                                                                           └► /data/state.json
```

- `rtl_tcp` — the only non-Go piece (C, from osmocom/rtl-sdr); links the USB dongle to TCP.
- `rtlamr` — the ERT decoder (pure Go, static binary); run with `-format=json -filterid=<meter ID>`.
- `watermeter-collector` — this project: supervises both children, parses rtlamr's JSON, filters your meter, computes usage deltas with out-of-order/rollover/reset handling, persists state across restarts, archives raw messages, and serves metrics.

Note: this is a multi-binary scratch image, not a single self-contained executable — a fully static C `rtl_tcp` is the pragmatic compromise for RTL-SDR USB access from a scratch container.

## Meter identification

Photos show an **Itron 100-series (100W line) water ERT module** with printed endpoint ID **81623781**. Itron documents the 100W+ as transmitting the SCM+ message, and `rtlamr` decodes SCM+ — but the exact variant and operating mode of your module must be confirmed from **received messages** on day one (the label number is the candidate ID; the decoded `ERTSerialNumber`/`EndpointID` is authoritative):

```sh
rtlamr -server=127.0.0.1:1234 -format=json -msgtype=all
```

Watch for ID 81623781 and note which message type (SCM, SCM+, IDM) and consumption value appear; verify the value against the meter's dial. Then confirm the **volume-per-count** by comparing decoded changes with observed consumption — the smallest dial increment alone doesn't establish the transmitted scale.

## Metrics

| Metric | Meaning |
|---|---|
| `watermeter_register{meter_id}` | Raw cumulative register value (matches the meter odometer) |
| `watermeter_usage_counts_total` | Cumulative usage in register counts, rollover/reset-safe, persisted across restarts |
| `watermeter_messages_total` | Decoded messages from your meter |
| `watermeter_last_message_timestamp_seconds` | Unix time of last decode (use for stall alerts) |
| `watermeter_resets_total` | Confirmed register decreases (rollover, reset, meter swap) |
| `watermeter_out_of_order_total` | Bad decodes that were later contradicted |
| `watermeter_child_up` / `watermeter_child_restarts_total` | Supervision health |

### Delta handling (tested)

Ambiguous decreases are quarantined, never auto-applied:

- Duplicates: ignored.
- A single low reading: held for confirmation — no usage change, no baseline change (a configured rollover bound does **not** make a lone low reading an automatic wrap).
- Confirmation paths: a climb from the low value confirms a wrap (with `-rollover-at`) or a reset (without); repeated **identical** low readings confirm a reset conservatively — the gap is excluded, never fabricated as usage.
- Spurious-reset recovery: after a confirmed reset, a 10-reading confirmation window buffers post-reset usage; if any reading returns to the pre-reset baseline, the whole episode is reclassified as bad decodes — buffered usage is discarded and only the true delta is counted.
- State (`-state-file`) persists the baseline and cumulative usage, so a restart gap is recovered when the next reading arrives.

Run `go test ./...` for the full matrix: zero start, duplicates, out-of-order blips, spurious resets (with and without post-reset climbs), confirmed resets with window expiry, known rollover, stale low readings with a bound configured, and restart-gap recovery.

## Build and run locally

```sh
go build -o watermeter-collector .
./watermeter-collector -meter-id 81623781 -msgtype=scm+ -state-file ./state.json -raw-log ./raw.jsonl
```

Useful flags: `-health-max-age` (healthz goes unhealthy if no decode within this window), `-rtltcp-bin=` (empty = external rtl_tcp server via `-rtltcp-addr`).

## CI/CD — GitHub Container Registry, no-touch updates

`.github/workflows/release.yml` runs the unit tests first, then builds the arm64 image (QEMU-emulated on the amd64 runner) and pushes to `ghcr.io/<owner>/<repo>` on every push to `main` (tagged `latest` + git SHA) and on `v*` tags. **Name the repository `watermeter-collector`** so the image path matches the Quadlet reference (`ghcr.io/<owner>/watermeter-collector`).

On the Pi, the Quadlet references the registry image with `AutoUpdate=registry`; enable the rootless auto-update timer once:

```sh
systemctl --user enable --now podman-auto-update.timer
```

If the ghcr.io package is **private**, the Pi needs a one-time persistent login (survives reboots) with a classic PAT that has `read:packages` scope:

```sh
podman login ghcr.io -u <github-username>
```

Update flow: merge to main → CI tests, builds, and pushes to ghcr.io → the Pi's auto-update timer pulls the new image and restarts the container. Fedora IoT's automatic OS update mechanism handles the OS layer independently (verify what your release ships). The whole node is no-touch.

## Deploy on the Fedora IoT edge node (rootless Podman Quadlet)

```sh
mkdir -p ~/watermeter/data ~/.config/containers/systemd
cp deploy/watermeter-collector.container ~/.config/containers/systemd/   # edit GHCR_OWNER
loginctl enable-linger $USER              # start services at boot without a login
systemctl --user daemon-reload
systemctl --user start watermeter-collector.service
```

The collector publishes `:9101/metrics` on the LAN. **Pick one primary ingestion path** (running both duplicates series):

- **Talos / VictoriaMetrics (recommended):** `deploy/cluster/vmscrapeconfig.yaml` (VM operator `VMScrapeConfig` — verify field names against your operator version and that a VMAgent selects it), or a static target in your existing vmagent config. Dashboards in your Talos Grafana with a Prometheus-type datasource at your VM query endpoint.
- **K3s / Prometheus (alternative):** `deploy/cluster/k3s-prometheus.yaml` (scrape + watermeter-only remoteWrite into the Talos VM).

If firewalld is active on the Pi: `sudo firewall-cmd --add-port=9101/tcp --permanent && sudo firewall-cmd --reload`.

### Notes and rough edges to verify on hardware

- **USB placement:** keep the SSD on USB 3 and put the SDR on a USB 2 port (or powered hub) — RTL-SDRs draw current and radiate RF; USB 3 is a known broadband noise source at 900 MHz.
- **SELinux:** the `:Z` volume labels handle file contexts; if the container can't open the USB device, test with `--security-opt label=disable` to isolate the cause before relaxing anything.
- **Antenna:** SMA, tuned near 915 MHz; place near the meter side of the house.
- **Dockerfile:** the static `rtl_tcp` stage is **unverified until you run the first build** (CI will surface failures — the build deliberately fails if `rtl_tcp` isn't fully static, checked with `readelf` for ELF interpreter/NEEDED entries). Fallback: skip that stage, run `rtl_tcp` as a tiny Alpine sidecar, and start the collector with `-rtltcp-bin=` (empty) and `-rtltcp-addr=<sidecar>:1234`.

## Later analysis / AI

- `/data/raw.jsonl` archives every decoded message for offline ML (it does not auto-backfill VictoriaMetrics — it's a separate, full-fidelity record).
- VictoriaMetrics exports CSV/JSON over HTTP for building datasets; MetricsQL serves the same data to any AI tooling you later point at it.
