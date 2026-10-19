package main

import (
	"fmt"
	"maps"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	// A walsender: its command name holds spaces and parentheses.
	line := "4242 (postgres: walsender comet 172.18.0.1(5432) START_REPLICATION) S 1 1 1 0 -1 4194624 512 0 0 0 250 75 0 0 20 0 1 0 100 0 0"
	if got, ok := parseStat(line); !ok || got != 3.25 {
		t.Fatalf("parseStat = %v, %v; want 3.25 (250 + 75 ticks)", got, ok)
	}
	for _, bad := range []string{"", "4242 (x) S 1", "no paren at all", "1 (x) S 1 1 1 0 -1 0 0 0 0 0 a b"} {
		if _, ok := parseStat(bad); ok {
			t.Errorf("parseStat(%q) accepted", bad)
		}
	}
}

func TestParseProc(t *testing.T) {
	stat := func(pid int, ticks int) string {
		return fmt.Sprintf("%d (postgres) S 1 1 1 0 -1 4194624 512 0 0 0 %d 0 0 0 20 0 1 0 100 0 0\n", pid, ticks)
	}
	// Postgres pads its retitled cmdline with NULs; the title holds parentheses and digits.
	ws := "postgres: walsender comet 172.18.0.1(54321) START_REPLICATION\x00\x00"
	out := stat(41, 100) + ws + stat(1, 250) + ws + stat(12, 50) + ws
	cpu, err := parseProc(out, []int32{41, 1, 12})
	if err != nil || !maps.Equal(cpu, map[int32]float64{41: 1, 1: 2.5, 12: 0.5}) {
		t.Fatalf("parseProc = %v, %v", cpu, err)
	}
	for name, bad := range map[string]string{
		"not a walsender": stat(41, 100) + "/usr/bin/bash\x00" + stat(1, 250) + ws,
		"last not one":    stat(41, 100) + ws + stat(1, 250) + "sshd\x00",
		"wrong PID":       stat(41, 100) + ws + stat(2, 250) + ws,
		"truncated":       stat(41, 100) + ws,
	} {
		if _, err := parseProc(bad, []int32{41, 1}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWalsenderCPU(t *testing.T) {
	before := map[int32]float64{1: 1, 2: 2}
	if c, ok := walsenderCPU(before, map[int32]float64{1: 1.5, 2: 3}, 2); !ok || c != 1.5 {
		t.Errorf("same PIDs: %v, %v", c, ok)
	}
	for name, after := range map[string]map[int32]float64{
		"PID changed": {1: 1.5, 3: 0.1},
		"PID gone":    {1: 1.5},
		"unreadable":  nil,
	} {
		if _, ok := walsenderCPU(before, after, 2); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok := walsenderCPU(map[int32]float64{1: 1}, map[int32]float64{1: 2}, 2); ok {
		t.Error("1 PID for 2 nodes: accepted")
	}
}

func TestCPUMedian(t *testing.T) {
	r := func(cpu float64) result { return result{cpu: cpu, cpuTook: time.Second} }
	if c, ok := cpuMedian([]result{r(-1), r(0.5), r(-1), r(0.25), r(0.75)}); !ok || c != 0.5 {
		t.Errorf("cpuMedian = %v, %v; want 0.5 over the 3 valid rounds", c, ok)
	}
	if _, ok := cpuMedian([]result{r(-1), r(-1)}); ok {
		t.Error("cpuMedian of no valid rounds: ok")
	}
	if cores(-1) != "n/a" {
		t.Errorf("cores(-1) = %q", cores(-1))
	}
}
