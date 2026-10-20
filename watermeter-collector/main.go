package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// rtlamr emits one JSON object per line, e.g. (SCM+):
//   {"Time":"2026-10-20T10:00:00.123456789-04:00","Offset":0,"Type":"SCM+",
//    "Message":{"FrameSync":44,"ProtocolID":30,"ApplicationID":2,
//               "ERTType":11,"ERTSerialNumber":81623781,"CommodityType":11,
//               "Consumption":123456,"TamperPhy":0,"TamperEnc":0,...}}
// Field names vary between protocols (ID / EndpointID / ERTSerialNumber),
// so Message is decoded generically and probed for known aliases.
type rtlamrLine struct {
	Time    string          `json:"Time"`
	Type    string          `json:"Type"`
	Message json.RawMessage `json:"Message"`
}

func extractUint(m map[string]any, keys ...string) (uint64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return uint64(n), true
			case string:
				if u, err := strconv.ParseUint(n, 10, 64); err == nil {
					return u, true
				}
			}
		}
	}
	return 0, false
}

// persistedState survives collector restarts so that usage during a restart
// gap is still counted when the next reading arrives (the meter's register
// keeps accumulating; without this, that span would be lost).
type persistedState struct {
	FirstReading bool      `json:"first_reading"`
	LastReading  uint64    `json:"last_reading"`
	UsageCounts  uint64    `json:"usage_counts"`
	SavedAt      time.Time `json:"saved_at"`
}

type collector struct {
	meterID    uint64
	rolloverAt uint64
	rawLogPath  string
	statePath   string

	mu           sync.Mutex
	state        persistedState
	// In-memory episode tracking for ambiguous decreases. A single low
	// reading is quarantined: it is only acted on once confirmed (climbing
	// from the low value, or repeated identical low readings), and a
	// confirmed reset can still be reverted as a bad-decode episode if a
	// later reading returns to the pre-reset baseline.
	pending       bool
	pendingValue  uint64
	pendingStreak int
	// Post-reset confirmation window (readings, not time).
	preResetReading uint64 // 0 = no active post-reset window
	postResetUsage  uint64 // usage buffered during the window
	episodeReadings int

	gotReading atomic.Bool
	lastMsgAt  atomic.Int64 // unix seconds of last decoded message

	reading     *prometheus.GaugeVec  // watermeter_register
	usageTotal  prometheus.Counter    // watermeter_usage_counts_total
	msgCount    prometheus.Counter    // watermeter_messages_total
	lastMsgTime prometheus.Gauge      // watermeter_last_message_timestamp_seconds
	resets      prometheus.Counter    // watermeter_resets_total
	outOfOrder  prometheus.Counter    // watermeter_out_of_order_total
	childRestart *prometheus.CounterVec
	childUp      *prometheus.GaugeVec
}

func newCollector(meterID, rolloverAt uint64, rawLogPath, statePath string, reg *prometheus.Registry) *collector {
	c := &collector{
		meterID:    meterID,
		rolloverAt: rolloverAt,
		rawLogPath: rawLogPath,
		statePath:   statePath,
		reading: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "watermeter_register",
			Help: "Raw cumulative register value reported by the meter (odometer).",
		}, []string{"meter_id"}),
		usageTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "watermeter_usage_counts_total",
			Help: "Cumulative consumption in register counts computed from positive deltas. Excludes reset/rollover gaps; state is persisted across restarts.",
		}),
		msgCount: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "watermeter_messages_total",
			Help: "Total ERT messages decoded from the configured meter.",
		}),
		lastMsgTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "watermeter_last_message_timestamp_seconds",
			Help: "Unix time of the most recent decoded message.",
		}),
		resets: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "watermeter_resets_total",
			Help: "Confirmed register decreases (rollover, reset, or meter swap).",
		}),
		outOfOrder: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "watermeter_out_of_order_total",
			Help: "Readings that arrived below the running baseline and were later contradicted (treated as bad decodes).",
		}),
		childRestart: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "watermeter_child_restarts_total",
			Help: "Restarts of supervised child processes.",
		}, []string{"process"}),
		childUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "watermeter_child_up",
			Help: "1 if the child process is currently running.",
		}, []string{"process"}),
	}
	reg.MustRegister(c.reading, c.usageTotal, c.msgCount, c.lastMsgTime, c.resets, c.outOfOrder, c.childRestart, c.childUp)
	c.loadState()
	return c
}

func (c *collector) loadState() {
	if c.statePath == "" {
		return
	}
	b, err := os.ReadFile(c.statePath)
	if err != nil {
		return // no state yet
	}
	var s persistedState
	if err := json.Unmarshal(b, &s); err != nil {
		log.Printf("could not parse state file %s (starting fresh): %v", c.statePath, err)
		return
	}
	c.state = s
	if s.UsageCounts > 0 {
		c.usageTotal.Add(float64(s.UsageCounts))
	}
	log.Printf("restored state: last_reading=%d usage_counts=%d (saved %s)",
		s.LastReading, s.UsageCounts, s.SavedAt.Format(time.RFC3339))
}

func (c *collector) saveLocked() {
	if c.statePath == "" {
		return
	}
	c.state.LastReading = c.lastReadingLocked()
	c.state.SavedAt = time.Now().UTC()
	b, err := json.Marshal(c.state)
	if err != nil {
		return
	}
	tmp := c.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("state write failed: %v", err)
		return
	}
	_ = os.Rename(tmp, c.statePath)
}

// caller must hold c.mu
func (c *collector) lastReadingLocked() uint64 { return c.state.LastReading }

// applyReading updates the baseline/usage state machine. Caller must hold c.mu.
//
// Semantics:
//   - duplicates: ignored.
//   - a single low reading: quarantined (no usage change, no baseline change).
//   - a low reading followed by a climb (still below baseline): with a
//     rollover bound configured this confirms a wrap (counted as usage);
//     without one it confirms a reset (gap excluded, usage from the new
//     level buffered during a confirmation window).
//   - repeated identical low readings: confirmed reset (gap excluded),
//     even with a rollover bound — the conservative reading that never
//     fabricates usage.
//   - any reading back at/above the pre-reset baseline within the window:
//     the whole episode is reclassified as bad decodes; buffered usage is
//     discarded and the true delta from the pre-reset baseline is counted.
//   - the window expires (10 further readings) without contradiction:
//     the reset is accepted and buffered usage is committed.
func (c *collector) applyReading(r uint64) {
	s := &c.state
	if !s.FirstReading {
		s.FirstReading = true
		s.LastReading = r
		c.saveLocked()
		return
	}

	// Active post-reset confirmation window.
	if c.preResetReading > 0 {
		if r >= c.preResetReading {
			// Contradicted: the decrease episode was bad decodes.
			c.outOfOrder.Inc()
			c.addUsage(r - c.preResetReading)
			s.LastReading = r
			c.clearEpisode()
			c.saveLocked()
			return
		}
		c.episodeReadings++
		if c.episodeReadings > 10 {
			// Window expired: accept the reset, commit buffered usage.
			c.addUsage(c.postResetUsage)
			c.clearEpisode()
			// fall through to normal handling of this reading
		} else {
			if r > s.LastReading {
				c.postResetUsage += r - s.LastReading
				s.LastReading = r
			}
			c.saveLocked()
			return
		}
	}

	last := s.LastReading

	if !c.pending {
		switch {
		case r == last:
			return // duplicate reading
		case r > last:
			c.addUsage(r - last)
			s.LastReading = r
			c.saveLocked()
		default:
			// Quarantine: hold the decrease for confirmation.
			c.pending = true
			c.pendingValue = r
			c.pendingStreak = 1
		}
		return
	}

	// Pending decrease episode.
	switch {
	case r >= last:
		// Contradicted by a reading at/above the baseline: bad decode.
		c.pending = false
		c.outOfOrder.Inc()
		if r > last {
			c.addUsage(r - last)
			s.LastReading = r
		}
		c.saveLocked()

	case r == c.pendingValue:
		c.pendingStreak++
		if c.pendingStreak >= 2 {
			c.confirmReset(r)
		}

	case r > c.pendingValue:
		if c.rolloverAt > last {
			// Climbing from the low value: consistent with a wrap.
			delta := (c.rolloverAt - last) + r
			c.pending = false
			c.addUsage(delta)
			s.LastReading = r
			c.saveLocked()
		} else {
			// Climbing from the low value without a bound: reset.
			c.confirmReset(r)
		}

	default:
		// Deeper decrease: restart quarantine from the new value.
		c.pendingValue = r
		c.pendingStreak = 1
	}
}

func (c *collector) confirmReset(r uint64) {
	c.resets.Inc()
	c.preResetReading = c.state.LastReading
	c.postResetUsage = 0
	c.episodeReadings = 0
	c.pending = false
	c.state.LastReading = r
	c.saveLocked()
}

func (c *collector) clearEpisode() {
	c.preResetReading = 0
	c.postResetUsage = 0
	c.episodeReadings = 0
	c.pending = false
	c.pendingStreak = 0
}

func (c *collector) addUsage(delta uint64) {
	c.state.UsageCounts += delta
	c.usageTotal.Add(float64(delta))
}

// handleLine processes one JSON line from rtlamr's stdout.
func (c *collector) handleLine(line []byte) {
	if len(line) == 0 {
		return
	}
	var l rtlamrLine
	if err := json.Unmarshal(line, &l); err != nil {
		log.Printf("unparsable rtlamr line: %v", err)
		return
	}
	// Raw archive for later offline/AI analysis (message + local receive time).
	if c.rawLogPath != "" {
		_ = os.MkdirAll(filepath.Dir(c.rawLogPath), 0o755)
		if f, err := os.OpenFile(c.rawLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "{\"received\":\"%s\",\"raw\":%s}\n", time.Now().UTC().Format(time.RFC3339Nano), line)
			f.Close()
		} else {
			log.Printf("raw log append failed: %v", err)
		}
	}

	var msg map[string]any
	if len(l.Message) > 0 {
		if err := json.Unmarshal(l.Message, &msg); err != nil {
			log.Printf("unparsable message body: %v", err)
			return
		}
	}

	id, _ := extractUint(msg, "EndpointID", "ERTSerialNumber", "ID")
	if id == 0 || id != c.meterID {
		return // not our meter
	}
	consumption, ok := extractUint(msg, "Consumption")
	if !ok {
		return
	}

	c.mu.Lock()
	c.applyReading(consumption)
	c.mu.Unlock()

	c.reading.WithLabelValues(strconv.FormatUint(c.meterID, 10)).Set(float64(consumption))
	c.msgCount.Inc()
	now := time.Now().Unix()
	c.lastMsgTime.Set(float64(now))
	c.lastMsgAt.Store(now)
	c.gotReading.Store(true)
}

// startChild launches cmd in its own process group and returns its Wait
// function. On ctx cancellation the whole process group is terminated
// (SIGTERM, then SIGKILL), so shell-script children (e.g. sleep) cannot be
// orphaned. Callers must call the returned wait function.
func startChild(ctx context.Context, name string, cmd *exec.Cmd, up prometheus.Gauge) (wait func() error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("%s start failed: %v", name, err)
		return func() error { return err }
	}
	up.Set(1)
	log.Printf("%s started (pid %d, pgid %d)", name, cmd.Process.Pid, cmd.Process.Pid)

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Kill the entire process group, not just the direct child.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			time.Sleep(200 * time.Millisecond)
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()

	return func() error {
		err := cmd.Wait()
		close(done)
		up.Set(0)
		return err
	}
}

// supervised keeps a child command running with restart backoff until ctx ends.
func supervised(ctx context.Context, wg *sync.WaitGroup, name string, mk func(ctx context.Context) *exec.Cmd, up prometheus.Gauge, restarts prometheus.Counter) {
	defer wg.Done()
	var backoff = []time.Duration{2, 5, 10, 20, 30} // seconds, capped at 30
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wait := startChild(ctx, name, mk(ctx), up)
		if err := wait(); err != nil && ctx.Err() == nil {
			log.Printf("%s exited: %v", name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff[min(attempt, len(backoff)-1)] * time.Second):
		}
		attempt++
		if ctx.Err() == nil {
			restarts.Inc()
		}
	}
}

// dialTimeout is a small helper for probing the rtl_tcp port.
func dialTimeout(addr string, timeout time.Duration) net.Conn {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil
	}
	return conn
}

func main() {
	var (
		meterID      = flag.Uint64("meter-id", 81623781, "ERT endpoint ID to filter on (from the meter label / listen mode)")
		rolloverAt   = flag.Uint64("rollover-at", 0, "register rollover bound; 0 treats decreases as pending resets")
		rtltcpBin    = flag.String("rtltcp-bin", "/usr/local/bin/rtl_tcp", "path to rtl_tcp binary; empty string = external rtl_tcp server (use -rtltcp-addr)")
		rtltcpAddr   = flag.String("rtltcp-addr", "127.0.0.1:1234", "address of the rtl_tcp server to use/connect to")
		rtlamrBin    = flag.String("rtlamr-bin", "/usr/local/bin/rtlamr", "path to rtlamr binary")
		msgTypes     = flag.String("msgtype", "scm+", "rtlamr message types (comma separated: scm,scm+,idm,...)")
		rawLog       = flag.String("raw-log", "", "path to a JSONL file archiving every decoded message (optional)")
		stateFile    = flag.String("state-file", "", "path to a JSON state file persisting the reading baseline across restarts (recommended)")
		listenAddr   = flag.String("listen", ":9101", "HTTP listen address for /metrics and /healthz")
		healthMaxAge = flag.Duration("health-max-age", 15*time.Minute, "healthz reports unhealthy if the last decoded message is older than this")
	)
	flag.Parse()

	reg := prometheus.NewRegistry()
	c := newCollector(*meterID, *rolloverAt, *rawLog, *stateFile, reg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	upTCP := c.childUp.WithLabelValues("rtl_tcp")
	upAMR := c.childUp.WithLabelValues("rtlamr")
	rstTCP := c.childRestart.WithLabelValues("rtl_tcp")
	rstAMR := c.childRestart.WithLabelValues("rtlamr")

	// rtl_tcp owns the USB device; the collector keeps it alive. Leave
	// -rtltcp-bin empty to use an externally managed rtl_tcp instead.
	var wg sync.WaitGroup
	if *rtltcpBin != "" {
		host, port, err := net.SplitHostPort(*rtltcpAddr)
		if err != nil {
			log.Fatalf("-rtltcp-addr must be host:port: %v", err)
		}
		wg.Add(1)
		go supervised(ctx, &wg, "rtl_tcp", func(ctx context.Context) *exec.Cmd {
			return exec.Command(*rtltcpBin, "-a", host, "-p", port)
		}, upTCP, rstTCP)
	}

	// Wait for the rtl_tcp port before starting rtlamr.
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn := dialTimeout(*rtltcpAddr, 2*time.Second)
		if conn != nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			log.Printf("warning: rtl_tcp not reachable at %s yet; starting rtlamr anyway", *rtltcpAddr)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	amrCmd := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, *rtlamrBin,
			"-server="+*rtltcpAddr,
			"-format=json",
			"-msgtype="+*msgTypes,
			"-filterid="+strconv.FormatUint(*meterID, 10),
			"-unique=false",
		)
		cmd.Stderr = os.Stderr
		return cmd
	}

	// Main decode loop: run rtlamr, read its stdout, restart on exit.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			cmd := amrCmd()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				log.Fatalf("stdout pipe: %v", err)
			}
			wait := startChild(ctx, "rtlamr", cmd, upAMR)
			if cmd.Process == nil {
				// start failed; back off and retry
			} else {
				sc := bufio.NewScanner(stdout)
				sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
				for sc.Scan() {
					c.handleLine(sc.Bytes())
				}
				if err := sc.Err(); err != nil && ctx.Err() == nil {
				log.Printf("rtlamr stdout read error: %v", err)
				}
				if err := wait(); err != nil && ctx.Err() == nil {
					log.Printf("rtlamr exited: %v", err)
				}
				if ctx.Err() == nil {
					rstAMR.Inc()
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !c.gotReading.Load() || time.Now().Unix()-c.lastMsgAt.Load() > int64(*healthMaxAge/time.Second) {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "no recent meter reading")
			return
		}
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{Addr: *listenAddr, Handler: mux}
	go func() {
		log.Printf("metrics listening on %s/metrics", *listenAddr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	// Wait (bounded) for the supervised children to be torn down before
	// exiting, so no process groups are left behind.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		log.Println("children terminated")
	case <-time.After(5 * time.Second):
		log.Println("warning: children still running after 5s")
	}
}
