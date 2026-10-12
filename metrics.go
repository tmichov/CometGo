package cometgo

import "time"

// Metric is one measurement passed to Config.Metrics. Durations are in
// seconds; counts and gauges are plain numbers.
type Metric struct {
	Name  string  // one of the Metric* constants
	Pub   string  // the publication, for MetricPubRun; "" otherwise
	Value float64 //
}

// Metric names. A "gauge" is the current value, reported after every engine
// tick; a "counter" reports 1 per occurrence; a "duration" reports one
// observation in seconds.
const (
	MetricTick            = "tick_seconds"       // duration of one engine tick
	MetricTickRuns        = "tick_runs"          // observer queries run in that tick
	MetricObservers       = "observers"          // gauge: live observers
	MetricSubscriptions   = "subscriptions"      // gauge: bound subscriptions
	MetricQueuedBytes     = "queued_bytes"       // gauge: bytes queued to all sockets
	MetricCoalescing      = "coalescing_conns"   // gauge: connections coalescing (§8.1)
	MetricFenceWait       = "fence_wait_seconds" // duration: commit to fence release, per call
	MetricFenceLag        = "fence_lag"          // counter: a call answered with lag:true
	MetricSlowClose       = "slow_closes"        // counter: a socket closed with 4429
	MetricListenReconnect = "listen_reconnects"  // counter: the LISTEN connection dropped
	MetricPubRun          = "pub_run_seconds"    // duration of one observer query, with Pub
)

// metric reports v under name if a hook is set. A panicking hook is logged
// instead of taking down the engine or connection goroutine it runs on.
func (a *App) metric(name, pub string, v float64) {
	m := a.cfg.Metrics
	if m == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			a.log.Error("metrics hook panicked", "metric", name, "panic", p)
		}
	}()
	m(Metric{Name: name, Pub: pub, Value: v})
}

// tickMetrics reports a finished tick and the engine's gauges. Engine
// goroutine only; callers check cfg.Metrics first.
func (e *engine) tickMetrics(start time.Time, runs int64) {
	a := e.a
	a.metric(MetricTick, "", time.Since(start).Seconds())
	a.metric(MetricTickRuns, "", float64(runs))
	a.metric(MetricObservers, "", float64(len(e.observers)))
	subs, queued := 0, 0
	for _, s := range e.conns {
		subs += len(s)
	}
	// Every live session, not only those with subs: calls and events queue too.
	for _, c := range a.liveConns() {
		q, _ := c.queuedBytes(0)
		queued += q
	}
	a.metric(MetricSubscriptions, "", float64(subs))
	a.metric(MetricQueuedBytes, "", float64(queued))
	a.metric(MetricCoalescing, "", float64(len(e.coalescing)))
}
