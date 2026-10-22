package cfprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type trafficState struct {
	RXPrev      uint64
	TXPrev      uint64
	RXPeriod    uint64
	TXPeriod    uint64
	LastCheck   int64
	PeriodStart int64
	Interface   string
}

func readTrafficState(path string) trafficState {
	values, err := parseKVFile(path)
	if err != nil {
		return trafficState{}
	}
	return trafficState{
		RXPrev:      parseUintDefault(values["RX_PREV"], 0),
		TXPrev:      parseUintDefault(values["TX_PREV"], 0),
		RXPeriod:    parseUintDefault(values["RX_PERIOD"], 0),
		TXPeriod:    parseUintDefault(values["TX_PERIOD"], 0),
		LastCheck:   atoi64Default(values["LAST_CHECK"], 0),
		PeriodStart: atoi64Default(values["PERIOD_START"], 0),
		Interface:   values["INTERFACE"],
	}
}

// readMonthlyTrafficTotals restores matching totals without changing the
// persisted counter baseline when a fresh network sample is unavailable.
func readMonthlyTrafficTotals(path string, resetDay int, iface string, now time.Time) (uint64, uint64) {
	st := readTrafficState(path)
	if st.LastCheck == 0 || st.Interface != iface {
		return 0, 0
	}
	if currentPeriod := periodStartTS(now, resetDay); currentPeriod != 0 {
		savedPeriod := st.PeriodStart
		if savedPeriod == 0 {
			// Older states and traffic corrections may not record a period start.
			savedPeriod = periodStartTS(time.Unix(st.LastCheck, 0).In(now.Location()), resetDay)
		}
		if savedPeriod != currentPeriod {
			return 0, 0
		}
	}
	return st.RXPeriod, st.TXPeriod
}

func writeTrafficState(path string, st trafficState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data := fmt.Sprintf("RX_PREV=%d\nTX_PREV=%d\nRX_PERIOD=%d\nTX_PERIOD=%d\nLAST_CHECK=%d\nPERIOD_START=%d\nINTERFACE=%s\n",
		st.RXPrev, st.TXPrev, st.RXPeriod, st.TXPeriod, st.LastCheck, st.PeriodStart, st.Interface)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func calcMonthlyTraffic(path string, current NetBytes, resetDay int, iface string) (uint64, uint64, error) {
	nowTime := time.Now()
	now := nowTime.Unix()
	st := readTrafficState(path)
	if st.Interface != iface {
		st = trafficState{}
	}
	periodStart := periodStartTS(nowTime, resetDay)
	if st.LastCheck == 0 {
		st.RXPrev = current.RX
		st.TXPrev = current.TX
		st.LastCheck = now
		st.PeriodStart = periodStart
		st.Interface = iface
		return st.RXPeriod, st.TXPeriod, writeTrafficState(path, st)
	}

	var rxDelta, txDelta uint64
	if current.RX >= st.RXPrev {
		rxDelta = current.RX - st.RXPrev
	}
	if current.TX >= st.TXPrev {
		txDelta = current.TX - st.TXPrev
	}
	if periodStart != 0 && st.PeriodStart != 0 && periodStart != st.PeriodStart {
		st.RXPeriod = rxDelta
		st.TXPeriod = txDelta
	} else {
		st.RXPeriod += rxDelta
		st.TXPeriod += txDelta
	}
	st.RXPrev = current.RX
	st.TXPrev = current.TX
	st.LastCheck = now
	st.PeriodStart = periodStart
	st.Interface = iface
	return st.RXPeriod, st.TXPeriod, writeTrafficState(path, st)
}

func applyTrafficCorrectionFromNetwork(path, iface, rxGB, txGB string) error {
	current, err := readNetBytes(iface)
	if err != nil {
		return fmt.Errorf("read network counters before traffic correction: %w", err)
	}
	return applyTrafficCorrection(path, current, iface, rxGB, txGB)
}

func applyTrafficCorrection(path string, current NetBytes, iface string, rxGB, txGB string) error {
	rx, err := parseTrafficCorrectionGB(rxGB)
	if err != nil {
		return err
	}
	tx, err := parseTrafficCorrectionGB(txGB)
	if err != nil {
		return err
	}
	st := readTrafficState(path)
	st.RXPrev = current.RX
	st.TXPrev = current.TX
	st.RXPeriod = rx
	st.TXPeriod = tx
	st.LastCheck = time.Now().Unix()
	st.Interface = iface
	return writeTrafficState(path, st)
}

func periodStartTS(now time.Time, resetDay int) int64 {
	if resetDay == 0 {
		return 0
	}
	if resetDay < 1 || resetDay > 31 {
		return now.Unix()
	}
	return lastResetDate(now, resetDay).Unix()
}

func lastResetDate(now time.Time, resetDay int) time.Time {
	year, month, _ := now.Date()
	loc := now.Location()
	thisMonth := actualResetDate(year, month, resetDay, loc)
	if !now.Before(thisMonth) {
		return thisMonth
	}
	prevMonth := month - 1
	prevYear := year
	if prevMonth < time.January {
		prevMonth = time.December
		prevYear--
	}
	return actualResetDate(prevYear, prevMonth, resetDay, loc)
}

func actualResetDate(year int, month time.Month, resetDay int, loc *time.Location) time.Time {
	firstDayOfNextMonth := time.Date(year, month+1, 1, 0, 0, 0, 0, loc)
	lastDayOfMonth := firstDayOfNextMonth.AddDate(0, 0, -1).Day()
	if resetDay <= lastDayOfMonth {
		return time.Date(year, month, resetDay, 0, 0, 0, 0, loc)
	}
	nextMonth := month + 1
	nextYear := year
	if nextMonth > time.December {
		nextMonth = time.January
		nextYear++
	}
	return time.Date(nextYear, nextMonth, 1, 0, 0, 0, 0, loc)
}

func parseUintDefault(raw string, def uint64) uint64 {
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return n
}
