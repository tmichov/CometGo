package main

import (
	"math"
	"testing"
)

func TestPct(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3, 6, 7, 8, 9, 10}
	for _, c := range []struct{ q, want float64 }{{.5, 5}, {.99, 10}, {1, 10}, {0, 1}, {.1, 1}, {.11, 2}} {
		if got := pct(xs, c.q); got != c.want {
			t.Errorf("pct(%v) = %v, want %v", c.q, got, c.want)
		}
	}
	if !math.IsNaN(pct(nil, .5)) {
		t.Error("pct of nothing should be NaN")
	}
	if xs[0] != 5 {
		t.Error("pct sorted its input")
	}
}

func TestProcField(t *testing.T) {
	b := []byte("Name:\tx\nVmRSS:\t  1234 kB\nusage_usec 99\n")
	if got := procField(b, "VmRSS:"); got != 1234 {
		t.Errorf("VmRSS = %d", got)
	}
	if got := procField(b, "usage_usec"); got != 99 {
		t.Errorf("usage_usec = %d", got)
	}
	if got := procField(b, "VmHWM:"); got != -1 {
		t.Errorf("missing field = %d", got)
	}
}

func TestSubsFor(t *testing.T) {
	p := params{subs: 5, sharedRatio: 0.4, groups: 3}
	subs := subsFor(p, 1)
	if len(subs) != 5 {
		t.Fatalf("got %d subs", len(subs))
	}
	shared := 0
	for _, s := range subs {
		if s["name"] == "shared.group" {
			if g := s["args"].(map[string]any)["g"].(int); g < 0 || g >= 3 {
				t.Errorf("group %d out of range", g)
			}
			shared++
		}
	}
	if shared != 2 {
		t.Errorf("shared subs = %d, want 2", shared)
	}
}
