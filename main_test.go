package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestCollector(t *testing.T, rollover uint64, statePath string) *collector {
	t.Helper()
	reg := prometheus.NewRegistry()
	return newCollector(81623781, rollover, "", statePath, reg)
}

// msg builds an rtlamr-style JSON line for the given meter and consumption.
func msg(id, consumption uint64) []byte {
	return fmt.Appendf(nil, `{"Time":"2026-10-20T10:00:00-04:00","Type":"SCM+","Message":{"ERTSerialNumber":%d,"Consumption":%d}}`, id, consumption)
}

func (c *collector) usage(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(c.usageTotal)
}

func (c *collector) resetCount(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(c.resets)
}

func TestFirstReadingZeroThenIncrease(t *testing.T) {
	c := newTestCollector(t, 0, "")
	c.handleLine(msg(81623781, 0)) // legitimate zero register: baseline only
	if got := c.usage(t); got != 0 {
		t.Fatalf("zero baseline should add no usage, got %v", got)
	}
	c.handleLine(msg(81623781, 5))
	if got := c.usage(t); got != 5 {
		t.Fatalf("usage = %v, want 5", got)
	}
}

func TestDuplicatesAndOtherMetersIgnored(t *testing.T) {
	c := newTestCollector(t, 0, "")
	c.handleLine(msg(81623781, 100))
	c.handleLine(msg(81623781, 100)) // exact duplicate
	c.handleLine(msg(99999999, 500)) // neighbor's meter
	c.handleLine(msg(81623781, 107))
	if got := c.usage(t); got != 7 {
		t.Fatalf("usage = %v, want 7", got)
	}
}

func TestOutOfOrderDecreaseNotDoubleCounted(t *testing.T) {
	c := newTestCollector(t, 0, "")
	c.handleLine(msg(81623781, 100))
	c.handleLine(msg(81623781, 50))  // single bad decode / blip
	c.handleLine(msg(81623781, 107)) // contradicts the decrease
	if got := c.usage(t); got != 7 {
		t.Fatalf("usage = %v, want 7 (blip ignored)", got)
	}
	if got := c.resetCount(t); got != 0 {
		t.Fatalf("resets = %v, want 0", got)
	}
}

func TestSustainedDecreaseIsConfirmedReset(t *testing.T) {
	c := newTestCollector(t, 0, "")
	c.handleLine(msg(81623781, 100))
	c.handleLine(msg(81623781, 50)) // first lower reading: quarantined
	c.handleLine(msg(81623781, 50)) // repeated identical: reset confirmed
	c.handleLine(msg(81623781, 52)) // post-reset climb: buffered, not yet counted
	if got := c.usage(t); got != 0 {
		t.Fatalf("usage = %v, want 0 (post-reset usage buffered during window)", got)
	}
	if got := c.resetCount(t); got != 1 {
		t.Fatalf("resets = %v, want 1", got)
	}
	// Window expiry (10 further readings) commits the buffered usage;
	// subsequent readings count normally.
	for v := uint64(53); v <= 64; v++ {
		c.handleLine(msg(81623781, v))
	}
	if got := c.usage(t); got != 14 { // 64 - 50
		t.Fatalf("usage after window expiry = %v, want 14", got)
	}
}

func TestSpuriousResetRecovery(t *testing.T) {
	// 100 -> 50 -> 50 -> 107: the repeated low readings were stale/bad
	// decodes; the final reading proves the register never reset.
	c := newTestCollector(t, 0, "")
	c.handleLine(msg(81623781, 100))
	c.handleLine(msg(81623781, 50))
	c.handleLine(msg(81623781, 50))  // confirmed reset (wrongly)
	c.handleLine(msg(81623781, 107)) // contradicts: reverts to pre-reset baseline
	if got := c.usage(t); got != 7 {
		t.Fatalf("usage = %v, want 7 (spurious reset reverted)", got)
	}
	if got := c.resetCount(t); got != 1 {
		t.Fatalf("resets = %v, want 1 (event was recorded, then reverted)", got)
	}
}

func TestSpuriousResetWithClimbDiscardsBufferedUsage(t *testing.T) {
	// 100 -> 50 -> 50 -> 52 -> 107: post-reset climb is buffered, then the
	// contradiction discards it; only the true delta from 100 counts.
	c := newTestCollector(t, 0, "")
	c.handleLine(msg(81623781, 100))
	c.handleLine(msg(81623781, 50))
	c.handleLine(msg(81623781, 50))
	c.handleLine(msg(81623781, 52))
	c.handleLine(msg(81623781, 107))
	if got := c.usage(t); got != 7 {
		t.Fatalf("usage = %v, want 7 (buffered climb discarded on contradiction)", got)
	}
}

func TestRolloverWithKnownBound(t *testing.T) {
	c := newTestCollector(t, 1000, "")
	c.handleLine(msg(81623781, 995))
	c.handleLine(msg(81623781, 3)) // single low reading: quarantined, NOT auto-wrapped
	c.handleLine(msg(81623781, 4)) // climbing from the low value: wrap confirmed
	if got := c.usage(t); got != 9 {
		t.Fatalf("usage = %v, want 9 (5 wrap + 4)", got)
	}
	if got := c.resetCount(t); got != 0 {
		t.Fatalf("resets = %v, want 0", got)
	}
}

func TestRolloverStaleLowReadingNotFabricated(t *testing.T) {
	// 995 -> 3 -> 995 -> 4 with a bound configured: the lone low reading
	// was a bad decode; no wrap usage may be fabricated.
	c := newTestCollector(t, 1000, "")
	c.handleLine(msg(81623781, 995))
	c.handleLine(msg(81623781, 3))
	c.handleLine(msg(81623781, 995)) // contradicts the low reading
	c.handleLine(msg(81623781, 4))
	if got := c.usage(t); got != 0 {
		t.Fatalf("usage = %v, want 0 (no fabricated wrap)", got)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	c1 := newTestCollector(t, 0, statePath)
	c1.handleLine(msg(81623781, 100))
	c1.handleLine(msg(81623781, 107))

	// "Restart": a fresh collector with the same state file. The next
	// reading must count usage across the gap (the register kept running).
	c2 := newTestCollector(t, 0, statePath)
	if got := c2.usage(t); got != 7 {
		t.Fatalf("restored usage = %v, want 7", got)
	}
	c2.handleLine(msg(81623781, 112))
	if got := c2.usage(t); got != 12 {
		t.Fatalf("usage after restart gap = %v, want 12", got)
	}

	// And the state file is valid JSON with the expected fields.
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	want := `"last_reading":112`
	if !contains(string(b), want) {
		t.Fatalf("state file missing %s: %s", want, b)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
