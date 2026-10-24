# VERIFIED CONTAINER RECIPE — the full image build (including the static
# rtl_tcp stage) has been run end-to-end: all three binaries come out
# statically linked and the scratch image runs.
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

# Stage 3: rtl_tcp (C, from osmocom/rtl-sdr).
#
# What broke the first build: cmake's default executable link prefers
# shared libusb (and the dynamic musl loader), so `make rtl_tcp` produced
# a dynamic binary and the static check correctly failed the build.
#
# Fix: use cmake ONLY to build the static libraries (rtlsdr_static,
# convenience_static), then link rtl_tcp ourselves with `gcc -static`
# against the archives. Verified: Alpine's libusb-dev ships the static
# archive /usr/lib/libusb-1.0.a (pkgs.alpinelinux.org), musl links fully
# static with build-base, and rtl_tcp.c compiles with just -I<include>.
#
# What broke the second build: the archives are NOT in the build root.
# cmake builds the libraries from src/CMakeLists.txt, so they land in
# <build>/src/ (i.e. /src/build/src/librtlsdr.a), not /src/build/. The
# link step failed with "cannot find /src/build/librtlsdr.a". The paths
# are resolved with `find` so an upstream layout change cannot silently
# break the link again.
#
# Fallback if this still fights you: drop this stage (and its final COPY),
# run rtl_tcp as a tiny Alpine sidecar with its own USB access, and start
# the collector with -rtltcp-bin= (empty) and -rtltcp-addr=<sidecar>:1234.
FROM alpine:3.20 AS rtltcp
RUN apk add --no-cache build-base binutils cmake git libusb-dev linux-headers
RUN git clone --depth 1 https://github.com/osmocom/rtl-sdr /src
WORKDIR /src/build
RUN mkdir -p /out \
    && cmake -DCMAKE_BUILD_TYPE=Release -DINSTALL_UDEV_RULES=OFF .. \
    && make -j"$(nproc)" rtlsdr_static convenience_static \
    && gcc -static -o /out/rtl_tcp \
        -I/src/include \
        /src/src/rtl_tcp.c \
        $(find /src/build -name 'librtlsdr.a' | head -1) \
        $(find /src/build -name 'libconvenience_static.a' | head -1) \
        /usr/lib/libusb-1.0.a \
        -lpthread -lm \
    && if readelf -l /out/rtl_tcp | grep -q INTERP \
         || readelf -d /out/rtl_tcp | grep -q NEEDED; \
       then echo "ERROR: rtl_tcp is not fully static; scratch image will not run" && exit 1; \
       else echo "rtl_tcp is fully static: OK"; \
       fi

# Stage 4: minimal /etc/passwd + /etc/group for the root user, for the
# scratch final image. A `FROM scratch` image ships no /etc/passwd (or /etc/
# group); the OCI runtime (crun) resolves the container user via /etc/passwd,
# so starting with an explicit user fails with:
#   "unable to find user root: no matching entries in passwd file"
# (exit 126). See the final stage below.
FROM alpine:3.20 AS rootfiles
RUN printf 'root:x:0:0:root:/root:/bin/sh\n' > /etc/passwd \
    && printf 'root:x:0:\n' > /etc/group

# Stage 5: SDR diagnostic image — Alpine-based (provides /bin/sh) plus the
# full set of osmocom RTL-SDR helper tools (rtl_tcp, rtl_test, rtl_eeprom,
# rtl_fm, rtl_adsb, rtl_power, rtl_biast) built the same way as the production
# rtl_tcp, plus the collector and rtlamr binaries.
#
# Why this exists: the final production image is FROM scratch — no shell, no
# package manager, no SDR tools — so when the collector pod runs but no water
# data arrives, there is no way to run diagnostics from inside the container.
# This diag stage is what feeds the watermeter-sdr-diagnostic debug pod
# (deploy/diagnostics/), which runs the SDR chain behind a real shell.
FROM alpine:3.20 AS diag
RUN apk add --no-cache --virtual .build-deps \
        build-base binutils cmake git libusb-dev linux-headers \
    && git clone --depth 1 https://github.com/osmocom/rtl-sdr /src \
    && mkdir -p /src/build \
    && cd /src/build \
    && cmake -DCMAKE_BUILD_TYPE=Release -DINSTALL_UDEV_RULES=OFF .. \
    && make -j"$(nproc)" all \
    && cp /src/build/rtl_tcp /src/build/rtl_test /src/build/rtl_eeprom /src/build/rtl_fm \
          /src/build/rtl_adsb /src/build/rtl_power /src/build/rtl_biast /usr/local/bin/ \
    && apk del .build-deps \
    && apk add --no-cache libusb
COPY --from=collector /out/watermeter-collector /usr/local/bin/watermeter-collector
COPY --from=rtlamr /out/rtlamr /usr/local/bin/rtlamr
COPY --from=rootfiles /etc/passwd /etc/passwd
COPY --from=rootfiles /etc/group /etc/group
COPY deploy/diagnostics/sdr-diag.sh /sdr-diag.sh
RUN chmod +x /sdr-diag.sh
EXPOSE 1234
ENTRYPOINT ["/sdr-diag.sh"]
WORKDIR /

# Final stage: scratch, nothing but static binaries + the root passwd/group files.
# Explicitly run as root - this is a rootful container deployment (rootful
# Podman / systemd Quadlet); the collector needs host-level access to the
# RTL-SDR USB bus and keeps state under /data on the host.
FROM scratch
USER root
COPY --from=collector /out/watermeter-collector /watermeter-collector
COPY --from=rtltcp /out/rtl_tcp /usr/local/bin/rtl_tcp
COPY --from=rtlamr /out/rtlamr /usr/local/bin/rtlamr
COPY --from=rootfiles /etc/passwd /etc/passwd
COPY --from=rootfiles /etc/group /etc/group
ENTRYPOINT ["/watermeter-collector"]
