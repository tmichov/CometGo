package main

import "testing"

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
