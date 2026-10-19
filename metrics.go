package cometgo

import (
	"runtime/debug"
	"time"
)

// tenantGaugeEvery is how often the per-tenant gauges are reported (engine
// run): per tick they would cost one hook call per tenant on the engine
// goroutine.
const tenantGaugeEvery = time.Second

// Metric is one measurement passed to Config.Metrics. Durations are in
// seconds; counts and gauges are plain numbers.
type Metric struct {
	Name   string // one of the Metric* constants
	Pub    string // the publication, for MetricPubRun, MetricStale and MetricFn; "" otherwise
	Method string // the method, for MetricCall; "" otherwise
	// Tenant is the tenant (Config.Tenant) the measurement belongs to, on
	// the metrics whose comment says so. "" for an anonymous or untenanted
	// identity, and for an observer whose subscriptions span tenants.
	Tenant string
	Value  float64
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
	MetricFenceWait       = "fence_wait_seconds" // duration: commit to fence release, per call, with Tenant
	MetricFenceLag        = "fence_lag"          // counter: a call answered with lag:true, with Tenant
	MetricSlowClose       = "slow_closes"        // counter: a socket closed with 4429, with Tenant
	MetricListenReconnect = "listen_reconnects"  // counter: the LISTEN connection dropped
	MetricPubRun          = "pub_run_seconds"    // duration of one observer query, with Pub and Tenant
	MetricStale           = "stale_seconds"      // duration: notification drained to its observer's result delivered, with Pub and Tenant
	MetricDetached        = "detached_observers" // gauge: observer queries running detached from their tick
	MetricFeedSnapshots   = "feed_snapshots"     // counter: a successful pg_current_snapshot() query of the logical feed (its visibility gate, and restarts)
	MetricCall            = "call_seconds"       // duration: a method call, received to res sent, with Method and Tenant
	MetricFn              = "fn_seconds"         // duration: one publication fn call, with Pub and Tenant
	// Per-tenant gauges, reported once a second, one per Tenant with
	// sessions or subscriptions; a tenant that has none left gets one 0.
	MetricTenantSubs   = "tenant_subscriptions" // gauge: bound subscriptions, with Tenant
	MetricTenantQueued = "tenant_queued_bytes"  // gauge: bytes queued to the tenant's sockets, with Tenant
)

// metric reports v under name if a hook is set.
func (a *App) metric(name, pub string, v float64) {
	a.report(Metric{Name: name, Pub: pub, Value: v})
}

// report passes m to the hook, if set. A panicking hook is logged instead
// of taking down the engine or connection goroutine it runs on.
func (a *App) report(m Metric) {
	h := a.cfg.Metrics
	if h == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			a.log.Error("metrics hook panicked", "metric", m.Name, "panic", p, "stack", string(debug.Stack()))
		}
	}()
	h(m)
}

// tenantOf is ident's tenant (Config.Tenant, else its ScopeKey). "" for
// nil, and always without Metrics, so the hook never runs for nothing.
func (a *App) tenantOf(ident *Identity) (t string) {
	if ident == nil || a.cfg.Metrics == nil {
		return ""
	}
	if a.cfg.Tenant == nil {
		return ident.ScopeKey
	}
	defer func() {
		if p := recover(); p != nil {
			a.log.Error("tenant hook panicked", "panic", p, "stack", string(debug.Stack()))
			t = ""
		}
	}()
	return a.cfg.Tenant(ident)
}

// tenant is the tenant all of the observer's subs share, or "" when they
// span tenants.
func (obs *observer) tenant() string {
	if len(obs.tenants) == 1 {
		for t := range obs.tenants {
			return t
		}
	}
	return ""
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
	a.metric(MetricDetached, "", float64(e.detachedN))
}

// tenantGauges reports subscriptions and queued bytes per tenant, and a 0
// for each tenant reported last time that has neither now, so an exporter
// does not keep its last value. Engine goroutine only.
func (e *engine) tenantGauges() {
	subs, queued := map[string]int{}, map[string]int{}
	for _, ss := range e.conns {
		for _, sub := range ss {
			subs[sub.tenant]++
		}
	}
	for _, c := range e.a.liveConns() {
		q, _ := c.queuedBytes(0)
		queued[c.tenantName()] += q
	}
	seen := map[string]bool{}
	for t := range e.tenantsReported {
		seen[t] = true
	}
	for t := range subs {
		seen[t] = true
	}
	for t := range queued {
		seen[t] = true
	}
	now := map[string]bool{}
	for t := range seen {
		_, s := subs[t]
		_, q := queued[t]
		if s || q {
			now[t] = true
		}
		e.a.report(Metric{Name: MetricTenantSubs, Tenant: t, Value: float64(subs[t])})
		e.a.report(Metric{Name: MetricTenantQueued, Tenant: t, Value: float64(queued[t])})
	}
	e.tenantsReported = now
}
