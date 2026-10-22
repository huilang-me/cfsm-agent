package cfprobe

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadMonthlyTrafficTotals(t *testing.T) {
	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	currentPeriod := periodStartTS(now, 1)
	previousPeriod := periodStartTS(now.AddDate(0, -1, 0), 1)
	for _, tt := range []struct {
		name        string
		resetDay    int
		periodStart int64
		lastCheck   int64
		iface       string
		wantRX      uint64
		wantTX      uint64
	}{
		{"current period", 1, currentPeriod, now.Unix(), "eth0", 500, 600},
		{"previous period", 1, previousPeriod, now.AddDate(0, -1, 0).Unix(), "eth0", 0, 0},
		{"different interface", 1, currentPeriod, now.Unix(), "eth1", 0, 0},
		{"uninitialized state", 1, currentPeriod, 0, "eth0", 0, 0},
		{"no reset", 0, previousPeriod, now.AddDate(0, -1, 0).Unix(), "eth0", 500, 600},
		{"legacy current period", 1, 0, now.Unix(), "eth0", 500, 600},
		{"legacy previous period", 1, 0, now.AddDate(0, -1, 0).Unix(), "eth0", 0, 0},
		{"legacy local midnight", 1, 0, time.Date(2026, time.October, 1, 0, 30, 0, 0, now.Location()).Unix(), "eth0", 500, 600},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "traffic.dat")
			st := trafficState{RXPrev: 100, TXPrev: 200, RXPeriod: 500, TXPeriod: 600, LastCheck: tt.lastCheck, PeriodStart: tt.periodStart, Interface: tt.iface}
			if err := writeTrafficState(path, st); err != nil {
				t.Fatal(err)
			}
			rx, tx := readMonthlyTrafficTotals(path, tt.resetDay, "eth0", now)
			if rx != tt.wantRX || tx != tt.wantTX {
				t.Fatalf("totals=%d/%d, want %d/%d", rx, tx, tt.wantRX, tt.wantTX)
			}
			if got := readTrafficState(path); got != st {
				t.Fatalf("read changed persisted state: %+v", got)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "missing", "traffic.dat")
	if rx, tx := readMonthlyTrafficTotals(path, 1, "eth0", now); rx != 0 || tx != 0 {
		t.Fatalf("missing state returned totals=%d/%d", rx, tx)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("read created a directory or failed unexpectedly: %v", err)
	}
}

func TestMonthlyTrafficReportsCheckpointFailure(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "traffic.dat")
		if initialized {
			if err := writeTrafficState(path, trafficState{RXPrev: 100, RXPeriod: 500, LastCheck: 1}); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(path+".tmp", 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := calcMonthlyTraffic(path, NetBytes{RX: 110}, 0, ""); err == nil {
			t.Fatal("checkpoint failure was discarded")
		}
		if initialized {
			st := readTrafficState(path)
			if st.RXPrev != 100 || st.RXPeriod != 500 {
				t.Fatalf("failed checkpoint overwrote persisted state: %+v", st)
			}
		}
	}
}

func TestPeriodStartTSClampsResetDay(t *testing.T) {
	now := time.Date(2026, time.February, 15, 12, 0, 0, 0, time.UTC)
	got := time.Unix(periodStartTS(now, 31), 0).UTC()
	want := time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestPeriodStartTSRollsMissingResetDayToNextMonth(t *testing.T) {
	now := time.Date(2026, time.April, 30, 12, 0, 0, 0, time.UTC)
	got := time.Unix(periodStartTS(now, 31), 0).UTC()
	want := time.Date(2026, time.March, 31, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestPeriodStartTSNoReset(t *testing.T) {
	if got := periodStartTS(time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC), 0); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}
