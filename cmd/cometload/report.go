package main

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

type report struct {
	p             params
	started       time.Time
	pgVersion     string
	connectTook   time.Duration
	subsEnded     int
	rssIdle       int64 // bytes, -1 when unknown
	rssLoaded     int64
	measured      time.Duration
	writes        int64
	writesSkipped int64
	callsSkipped  int64
	calls         *callStats
	callsSent     int
	callsAnswered int
	slowFn        *slowFnStats
	server        *serverStats
	dbCPU         float64 // percent of one core
	dbCPUErr      string
	dropped       int
}

// pct returns the q-th percentile (0..1) of xs by nearest rank, NaN if empty.
func pct(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	return s[min(max(i, 0), len(s)-1)]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	var t float64
	for _, x := range xs {
		t += x
	}
	return t / float64(len(xs))
}

func ms(s float64) string {
	if math.IsNaN(s) {
		return "n/a"
	}
	return strconv.FormatFloat(s*1000, 'f', 1, 64) + " ms"
}

func (r *report) markdown() string {
	var b strings.Builder
	p := r.p
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	secs := r.measured.Seconds()

	w("# cometload: %s scenario\n\n", p.scenario)
	w("Run %s at commit %s.\n\n", r.started.UTC().Format("2006-01-02 15:04 MST"), gitRev())
	w("## Machine\n\n")
	w("- CPU: %s, %d logical cores\n", cpuModel(), runtime.NumCPU())
	w("- Memory: %s\n", memTotal())
	w("- OS: %s %s, kernel %s\n", runtime.GOOS, runtime.GOARCH, kernel())
	w("- Go: %s\n", runtime.Version())
	w("- Postgres: %s (`make db`, same machine)\n\n", r.pgVersion)
	w("Driver, server and Postgres share the machine.\n\n")

	w("## Load\n\n")
	w("| Parameter | Value |\n|---|---|\n")
	w("| Clients | %d, over %d users |\n", p.clients, p.users)
	w("| Subs per client | %d (%d shared, %d per-user) |\n", p.subs, p.sharedSubs(), p.userSubs())
	w("| Shared groups / rows per result | %d / %d |\n", p.groups, p.rows)
	w("| Direct writes | %d/s target, %.0f/s done |\n", p.writes, float64(r.writes)/secs)
	w("| Calls | %d/s target, %.0f/s sent |\n", p.calls, float64(r.callsSent)/secs)
	if p.scenario == "slow" {
		w("| Slow observer | pg_sleep %s, re-run on every shared write |\n", p.slowObserver)
		w("| Slow fn | pg_sleep %s, one in flight at all times |\n", p.slowFn)
	}
	w("| Warmup / measured | %s / %s |\n\n", p.warmup, r.measured.Round(time.Millisecond))

	w("## Calls\n\n")
	w("| Method | Calls | p50 | p99 | max | lag:true |\n|---|---|---|---|---|---|\n")
	var all []float64
	var lagged int
	for _, m := range slices.Sorted(maps.Keys(r.calls.latency)) {
		l := r.calls.latency[m]
		all = append(all, l...)
		lagged += r.calls.lag[m]
		w("| %s | %d | %s | %s | %s | %s |\n", m, len(l), ms(pct(l, .5)), ms(pct(l, .99)), ms(pct(l, 1)), rate(r.calls.lag[m], len(l)))
	}
	w("| **all** | %d | **%s** | **%s** | %s | **%s** |\n\n", len(all), ms(pct(all, .5)), ms(pct(all, .99)), ms(pct(all, 1)), rate(lagged, len(all)))
	w("Latency is send to `res` on the client, for calls answered ok.")
	if n := r.callsSent - r.callsAnswered; n > 0 {
		w(" %d calls got no `res` within 15 s of the end.", n)
	}
	if r.callsSkipped > 0 {
		w(" %d calls were not sent (client at maxInflight or closed).", r.callsSkipped)
	}
	w("\n\n")
	if len(r.calls.errs) > 0 {
		w("Errors: ")
		for i, k := range slices.Sorted(maps.Keys(r.calls.errs)) {
			if i > 0 {
				w(", ")
			}
			w("%s ×%d", k, r.calls.errs[k])
		}
		w("\n\n")
	}
	if r.slowFn != nil && p.scenario == "slow" {
		w("Slow fn: %d calls, p50 %s, max %s, %d failed.\n\n", len(r.slowFn.latency), ms(pct(r.slowFn.latency, .5)), ms(pct(r.slowFn.latency, 1)), r.slowFn.failed)
	}

	s := r.server
	w("## Engine\n\n")
	w("| Metric | Value |\n|---|---|\n")
	w("| Ticks | %d (%.1f/s) |\n", len(s.Ticks), float64(len(s.Ticks))/secs)
	w("| Tick duration p50 / p99 / max | %s / %s / %s |\n", ms(pct(s.Ticks, .5)), ms(pct(s.Ticks, .99)), ms(pct(s.Ticks, 1)))
	w("| Runs per tick mean / p99 / max | %.1f / %.0f / %.0f |\n", mean(s.Runs), pct(s.Runs, .99), pct(s.Runs, 1))
	w("| Fence wait p50 / p99 | %s / %s |\n", ms(pct(s.FenceWait, .5)), ms(pct(s.FenceWait, .99)))
	w("| Observers / subscriptions | %.0f / %.0f |\n", s.Observers, s.Subscriptions)
	w("| Max queued bytes / coalescing conns | %.0f / %.0f |\n", s.MaxQueuedBytes, s.MaxCoalescing)
	w("| Slow-consumer closes / listener reconnects / dropped clients | %d / %d / %d |\n", s.SlowCloses, s.ListenReconnects, r.dropped)
	w("| Subs ended by the server at connect | %d |\n\n", r.subsEnded)

	w("## Resources\n\n")
	w("| Metric | Value |\n|---|---|\n")
	if r.rssIdle > 0 && r.rssLoaded > 0 {
		w("| Server RSS idle / loaded | %.1f MiB / %.1f MiB |\n", mib(r.rssIdle), mib(r.rssLoaded))
		w("| Server RSS per 1k connections | %.1f MiB |\n", mib(r.rssLoaded-r.rssIdle)*1000/float64(p.clients))
	} else {
		w("| Server RSS | n/a (needs /proc) |\n")
	}
	w("| Connect + initial snapshots | %s |\n", r.connectTook.Round(time.Millisecond))
	if r.dbCPUErr == "" {
		w("| Postgres CPU | %.0f%% of one core |\n", r.dbCPU)
	} else {
		w("| Postgres CPU | n/a (%s) |\n", r.dbCPUErr)
	}
	w("\nRSS per 1k connections is (loaded − idle) / clients × 1000, measured with every sub's snapshot sent. Postgres CPU is the container's cgroup CPU time over the measured window.\n")
	return b.String()
}

func rate(n, of int) string {
	if of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", float64(n)/float64(of)*100)
}

func mib(b int64) float64 { return float64(b) / (1 << 20) }

// rss is a process's resident set size in bytes from /proc, -1 if unknown.
func rss(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	return procField(b, "VmRSS:") * 1024
}

// procField reads a "Name:  123 kB" line's number, -1 if absent.
func procField(b []byte, name string) int64 {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), name); ok {
			n, err := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return -1
}

// dbCPU is the Postgres container's total CPU time in microseconds, from
// its cgroup v2 cpu.stat.
func dbCPU(container string) (int64, error) {
	if container == "" {
		out, err := exec.Command("docker", "compose", "ps", "-q", "postgres").Output()
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			return 0, fmt.Errorf("no postgres container from docker compose; pass -pg-container")
		}
		container = string(bytes.TrimSpace(out))
	}
	out, err := exec.Command("docker", "exec", container, "cat", "/sys/fs/cgroup/cpu.stat").Output()
	if err != nil {
		return 0, fmt.Errorf("read cpu.stat in %s: %v", container, err)
	}
	n := procField(out, "usage_usec")
	if n < 0 {
		return 0, fmt.Errorf("no usage_usec in cpu.stat (cgroup v1?)")
	}
	return n, nil
}

func gitRev() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	rev := strings.TrimSpace(string(out))
	if st, _ := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output(); len(st) > 0 {
		rev += "-dirty"
	}
	return rev
}

func cpuModel() string {
	b, _ := os.ReadFile("/proc/cpuinfo")
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, "model name"); ok {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), ":"))
		}
	}
	return "unknown"
}

func memTotal() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f GiB", float64(procField(b, "MemTotal:"))/(1<<20))
}

func kernel() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}
