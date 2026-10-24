#!/bin/sh
# sdr-diag.sh - entrypoint for the watermeter-sdr-diagnostic pod
# (deploy/diagnostics/watermeter-sdr-diagnostic.container).
#
# Runs the RTL-SDR decode chain (rtl_tcp -> rtlamr, plus the raw RTL-SDR
# tools: rtl_eeprom / rtl_test) with a real shell behind it, because the
# production collector image is FROM scratch and therefore has no shell to run
# diagnostics with. This pod exists so SDR configuration / operation can be
# diagnosed from inside the pod instead of the host.
#
# Lifecycle:
#   - starts rtl_tcp on RTL_TCP_ADDR (default 127.0.0.1:1234)
#   - waits up to 60s for rtl_tcp to report "listening..." (i.e. dongle open
#     + tune + listen), prints rtl_tcp log
#   - runs rtlamr which streams decoded messages as JSON to /tmp/sdr-diag/rtlamr.log
#
# Logs land in /tmp/sdr-diag/:
#   rtl_tcp.log  - rtl_tcp output (device open, tuning, warnings, "listening...")
#   rtlamr.log   - rtlamr JSON decode stream + errors
#
# Tunables via environment (all have sane defaults):
#   RTL_TCP_ADDR       rtl_tcp listen address                 (default 127.0.0.1:1234)
#   RTL_TCP_FREQ       rtl_tcp center frequency [Hz]         (default: driver default)
#   RTL_TCP_GAIN       rtl_tcp tuner gain in dB              (default 0 = auto)
#   RTL_TCP_SAMPLERATE rtl_tcp sample rate [Hz]             (default: driver default)
#   RTL_TCP_PPM        rtl_tcp tuner ppm correction         (default: 0)
#   METER_ID           rtlamr ERT id filter                (default 81623781)
#   MSG_TYPE           rtlamr message type filter         (default all)
#   SYMBOL_LENGTH      rtlamr symbol length                (default 72)
#   RTLAMR_CENTERFREQ  rtlamr center frequency [Hz]       (default 915000000 = 915 MHz ERT band)

DIAGDIR=/tmp/sdr-diag
mkdir -p "$DIAGDIR"

ADDR=${RTL_TCP_ADDR:-127.0.0.1:1234}
FREQ=${RTL_TCP_FREQ:-}
GAIN=${RTL_TCP_GAIN:-0}
SAMPLERATE=${RTL_TCP_SAMPLERATE:-}
PPM=${RTL_TCP_PPM:-}
METER_ID=${METER_ID:-81623781}
MSG_TYPE=${MSG_TYPE:-all}
SYMBOL_LENGTH=${SYMBOL_LENGTH:-72}
CENTERFREQ=${RTLAMR_CENTERFREQ:-915000000}

rtl_tcp_args="-a $ADDR"
[ -n "$FREQ" ] && rtl_tcp_args="$rtl_tcp_args -f $FREQ"
[ -n "$SAMPLERATE" ] && rtl_tcp_args="$rtl_tcp_args -s $SAMPLERATE"
[ -n "$PPM" ] && rtl_tcp_args="$rtl_tcp_args -P $PPM"
[ "$GAIN" != "0" ] && rtl_tcp_args="$rtl_tcp_args -g $GAIN"

rtl_tcp $rtl_tcp_args >"$DIAGDIR/rtl_tcp.log" 2>&1 &
RTL_PID=$!
AMR_PID=""

cleanup() {
  [ -n "$AMR_PID" ] && kill -TERM "$AMR_PID" 2>/dev/null
  kill -TERM "$RTL_PID" 2>/dev/null
  wait "$AMR_PID" 2>/dev/null
  wait "$RTL_PID" 2>/dev/null
}
trap cleanup TERM INT

i=0
while [ "$i" -lt 60 ]; do
  grep -qi 'listening' "$DIAGDIR/rtl_tcp.log" 2>/dev/null && break
  i=$((i + 1))
  sleep 1
done

echo "--- [sdr-diag] rtl_tcp log ---"
cat "$DIAGDIR/rtl_tcp.log"

rtlamr -server=127.0.0.1:1234 -format=json -msgtype="$MSG_TYPE" -filterid="$METER_ID" \
  -symbollength="$SYMBOL_LENGTH" -unique=false -centerfreq="$CENTERFREQ" \
  >"$DIAGDIR/rtlamr.log" 2>&1 &
AMR_PID=$!
echo "--- [sdr-diag] rtlamr streaming to $DIAGDIR/rtlamr.log (msgtype=$MSG_TYPE filterid=$METER_ID) ---"
wait "$AMR_PID"
STATUS=$?

cleanup
echo "--- [sdr-diag] rtlamr exited with status $STATUS ---"
exit "$STATUS"
