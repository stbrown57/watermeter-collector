# GitHub Repository Setup — watermeter-collector

Step-by-step instructions to publish the project to `github.com/stbrown57/watermeter-collector`, get CI building the image to ghcr.io, and bootstrap the edge Pi.

## 1. Prepare the Project Locally

The zip contains a nested `watermeter-collector/` folder — the **inner** directory is the repo root (the workflow expects `go.mod` at the repo root):

```sh
unzip watermeter-collector.zip
cd watermeter-collector

git init -b main
git add -A
git commit -m "Initial commit: ERT collector, tests, CI, deploy files"
```

Check the hidden CI directory was committed: `git ls-files | grep github` should show `.github/workflows/release.yml`.

## 2. Create the Repository and Push

**Option A — gh CLI** (requires `gh auth login` done once):

```sh
gh repo create stbrown57/watermeter-collector \
  --public --source . --push \
  --description "Itron water ERT collector (RTL-SDR → Prometheus metrics)"
```

**Option B — Web UI:**

1. Create the repo at https://github.com/new (account: stbrown57).
2. Name: **watermeter-collector** — the name matters: CI derives the image path from it (`ghcr.io/stbrown57/watermeter-collector`).
3. Visibility: **Public** or private (package visibility is separate — see section 4).
4. Leave "Add a README" unchecked; create.
5. Push:

```sh
git remote add origin git@github.com:stbrown57/watermeter-collector.git   # SSH (needs a key on the account)
# or: git remote add origin https://github.com/stbrown57/watermeter-collector.git  (HTTPS + PAT/credential helper)
git push -u origin main
```

## 3. First CI Run

Pushing to `main` triggers `.github/workflows/release.yml`:

1. **test job** — `go test ./...` (the 9 unit tests).
2. **build job** — builds `linux/arm64` under QEMU and pushes to `ghcr.io/stbrown57/watermeter-collector` (tags: `latest`, branch name, `sha-<shortsha>`; `v*` tags add semver tags).

Watch it: https://github.com/stbrown57/watermeter-collector/actions (URL live once the repo exists).

Two things can fail on the first run, both with known fallbacks — but neither fallback is turnkey:

- **The static `rtl_tcp` stage.** The build deliberately fails if `rtl_tcp` isn't fully static. Fallback (requires real deployment changes): remove that Dockerfile stage **and its final `COPY`**, deploy `rtl_tcp` as a separate Alpine sidecar with its own USB device access, and start the collector with `-rtltcp-bin=` (empty) and `-rtltcp-addr=<sidecar>:1234`.
- **`go install github.com/bemasher/rtlamr@latest`** breaking on a future Go version. Fallback: download a prebuilt release binary from https://github.com/bemasher/rtlamr/releases into the build stage.

## 4. GHCR Package Visibility and Authentication

A **public repository does not automatically make its container package public** — new ghcr.io packages from Actions are private by default, and package visibility is set separately from repo visibility:

1. Go to https://github.com/stbrown57?tab=packages → **watermeter-collector** (appears after the first successful build).
2. Package settings → Danger Zone → **Change visibility** → Public (then the Pi pulls anonymously).

If you keep the package **private**, authenticate on the Pi with a **persistent auth file** (a plain `podman login` may store credentials under the ephemeral runtime dir):

```sh
mkdir -p ~/.config/containers
podman login --authfile ~/.config/containers/auth.json ghcr.io -u stbrown57
# Password: a classic PAT with read:packages scope
# Create at: Settings → Developer settings → Personal access tokens → Tokens (classic)
```

If auto-update doesn't find the credentials, export `REGISTRY_AUTH_FILE=$HOME/.config/containers/auth.json` in the user environment (e.g., via `systemctl --user set-environment` or an `Environment=` drop-in for the service).

## 5. Bootstrap the Edge Pi (Fedora IoT, rootless Podman)

One-time setup:

```sh
# Linger: user services start at boot without a login session
loginctl enable-linger $USER

# Install the Quadlet
mkdir -p ~/watermeter/data ~/.config/containers/systemd
cp deploy/watermeter-collector.container ~/.config/containers/systemd/

# No-touch updates
systemctl --user enable --now podman-auto-update.timer

# If firewalld is active, allow the metrics port (scraped by the Talos cluster)
sudo firewall-cmd --add-port=9101/tcp --permanent && sudo firewall-cmd --reload

# Start it
systemctl --user daemon-reload
systemctl --user start watermeter-collector.service
```

Verify:

```sh
systemctl --user status watermeter-collector.service
curl -s http://127.0.0.1:9101/metrics | head
curl -s http://127.0.0.1:9101/healthz
journalctl --user -u watermeter-collector.service -f
```

USB note: keep the SSD on a USB 3 port and put the RTL-SDR on a USB 2 port — USB 3 is a known broadband RF noise source at 900 MHz. If the container can't open the USB device, test with `--security-opt label=disable` to isolate whether SELinux is the cause.

## 6. Day One — Confirm the Meter Over RF

The collector's defaults (meter ID 81623781, SCM+) are candidates, not confirmed facts. The helper binaries live **inside** the container and the rtl_tcp port is not published to the host, so discovery is a separate run: stop the normal service and run an unfiltered discovery session using the image's own binaries:

```sh
systemctl --user stop watermeter-collector.service

# rtl_tcp with USB access (background container)
podman run -d --name wm-rtltcp --device /dev/bus/usb \
  --entrypoint /usr/local/bin/rtl_tcp \
  ghcr.io/stbrown57/watermeter-collector:latest -a 127.0.0.1 -p 1234

# rtlamr sharing that container's network: ALL message types, NO ID filter
podman run --rm -it --network container:wm-rtltcp \
  --entrypoint /usr/local/bin/rtlamr \
  ghcr.io/stbrown57/watermeter-collector:latest \
  -server=127.0.0.1:1234 -format=json -msgtype=all

podman rm -f wm-rtltcp
```

- Confirm endpoint **81623781** appears and note which message type actually carries it (SCM, SCM+, IDM).
- Verify the decoded consumption value against the meter's dial.
- Watch usage for a day and compare decoded deltas with observed consumption to establish **volume-per-count** (don't assume the dial's smallest increment is the transmitted scale).

If the ID or protocol differs from the defaults, update the `Exec=` line in the Quadlet, `daemon-reload`, and restart the service.

## 7. Wire Up the Talos Cluster (VictoriaMetrics)

Pick ONE primary ingestion path (running both the Talos and K3s paths duplicates series).

**Recommended — Talos VictoriaMetrics scrapes the Pi:**

- If you run the VictoriaMetrics operator: apply `deploy/cluster/vmscrapeconfig.yaml` (replace `PI_LAN_IP`), and **verify against your installed operator version** that (a) the CRD field names match and (b) one of your VMAgents actually selects the resource (`VMAgent.spec.scrapeConfigSelector`).
- If you run plain vmagent: add a static target for `PI_LAN_IP:9101` to its scrape config.

**Alternative — K3s Prometheus scrapes and remote-writes to the Talos VM:** follow `deploy/cluster/k3s-prometheus.yaml` (watermeter-job-filtered remote write).

Grafana (Talos): add a Prometheus-type datasource at your VictoriaMetrics query endpoint, then:

```promql
# Daily usage (multiply by your verified volume-per-count)
increase(watermeter_usage_counts_total[1d])
# Flow rate
rate(watermeter_usage_counts_total[5m]) * 60
# Reception stalled?
time() - watermeter_last_message_timestamp_seconds > 300
```

## 8. Verify No-Touch Updates End to End

1. Make and push a real committed change (e.g., edit a comment in main.go) — a file touch alone produces no CI run.
2. Watch CI publish a new image; note its digest on the ghcr.io package page.
3. On the Pi, either wait for `podman-auto-update.timer` (schedule: `systemctl --user list-timers podman-auto-update.timer`) or force it: `systemctl --user start podman-auto-update.service`.
4. Confirm the container restarted with the new image (`podman inspect` the running image's digest) and metrics resumed.

## Rollback

- Pin to a known-good image: set `Image=ghcr.io/stbrown57/watermeter-collector@sha256:<digest>` (record digests of known-good builds), remove `AutoUpdate=registry`, then `systemctl --user daemon-reload && systemctl --user restart watermeter-collector.service`.
- Note: rebuilding a reverted commit is not guaranteed to reproduce the old image byte-for-byte — prefer the recorded digest.
- The collector's supervision restarts crashed children with backoff, and the Quadlet's `Restart=always` restarts the container — retries continue indefinitely rather than giving up.
