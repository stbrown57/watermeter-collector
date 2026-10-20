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
        /src/build/librtlsdr.a /src/build/libconvenience_static.a \
        /usr/lib/libusb-1.0.a \
        -lpthread -lm \
    && if readelf -l /out/rtl_tcp | grep -q INTERP \
         || readelf -d /out/rtl_tcp | grep -q NEEDED; \
       then echo "ERROR: rtl_tcp is not fully static; scratch image will not run" && exit 1; \
       else echo "rtl_tcp is fully static: OK"; \
       fi

# Final stage: scratch, nothing but static binaries.
FROM scratch
COPY --from=collector /out/watermeter-collector /watermeter-collector
COPY --from=rtltcp /out/rtl_tcp /usr/local/bin/rtl_tcp
COPY --from=rtlamr /out/rtlamr /usr/local/bin/rtlamr
ENTRYPOINT ["/watermeter-collector"]
