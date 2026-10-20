# UNVERIFIED CONTAINER RECIPE — compile-tested locally, but the image build
# (especially the static rtl_tcp stage) has NOT been run end-to-end here.
# Build on the Pi itself (arm64) or with `podman buildx --platform linux/arm64`.
#
# The image is deliberately multi-binary: one Go supervisor
# (watermeter-collector) plus the two helper executables it supervises —
# rtlamr (Go, static) and rtl_tcp (C, the only non-Go piece, from
# osmocom/rtl-sdr). All three must be fully static for the scratch stage.

# Stage 1: the Go collector (static, CGO disabled).
FROM golang:alpine AS collector
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/watermeter-collector .

# Stage 2: rtlamr (pure Go, static binary).
# If @latest fails, download a release tarball from
# https://github.com/bemasher/rtlamr/releases and unpack the linux-arm64
# binary into /out instead.
FROM golang:alpine AS rtlamr
RUN CGO_ENABLED=0 go install github.com/bemasher/rtlamr@latest \
    && mkdir -p /out \
    && cp /go/bin/rtlamr /out/rtlamr

# Stage 3: rtl_tcp (C, from osmocom/rtl-sdr) — the finicky part.
# BUILD_SHARED_LIBS=OFF builds librtlsdr as a static library; with Alpine's
# libusb-dev providing libusb.a this *should* link fully static. The check
# at the end FAILS the build if the binary has an ELF interpreter or any
# DT_NEEDED entries (checked with readelf — more reliable than ldd wording
# under musl), so a broken stage cannot silently produce a scratch image
# that won't run.
# Fallback if this fights you: skip this stage, run rtl_tcp as a tiny
# Alpine sidecar, and start the collector with -rtltcp-bin= (empty) and
# -rtltcp-addr=<sidecar>:1234.
FROM alpine:3.20 AS rtltcp
RUN apk add --no-cache build-base binutils cmake git libusb-dev linux-headers
RUN git clone --depth 1 https://github.com/osmocom/rtl-sdr /src
WORKDIR /src/build
RUN cmake -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DINSTALL_UDEV_RULES=OFF .. \
    && make -j"$(nproc)" rtl_tcp \
    && if readelf -l /src/build/rtl_tcp | grep -q INTERP \
         || readelf -d /src/build/rtl_tcp | grep -q NEEDED; \
       then echo "ERROR: rtl_tcp is not fully static; scratch image will not run" && exit 1; \
       else echo "rtl_tcp is fully static: OK"; \
       fi \
    && mkdir -p /out && cp /src/build/rtl_tcp /out/rtl_tcp

# Final stage: scratch, nothing but static binaries.
FROM scratch
COPY --from=collector /out/watermeter-collector /watermeter-collector
COPY --from=rtltcp /out/rtl_tcp /usr/local/bin/rtl_tcp
COPY --from=rtlamr /out/rtlamr /usr/local/bin/rtlamr
ENTRYPOINT ["/watermeter-collector"]
