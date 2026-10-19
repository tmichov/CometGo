package cometgo

import (
	"context"
	"strings"
	"testing"
	"time"
)

// waitRes reads until the res for call id, failing on anything but ok.
func waitRes(t *testing.T, c *client, id int) {
	t.Helper()
	for {
		m := c.next()
		if m["t"] == "res" && m["id"] == float64(id) {
			if m["ok"] != true {
				t.Fatalf("call %d: %v", id, m)
			}
			return
		}
	}
}

// Tenants default to Identity.ScopeKey. A single-tenant observer's runs name
// its tenant; a shared observer with subs of two tenants names TenantMixed.
func TestTenantMetrics(t *testing.T) {
	log := &metricLog{}
	e := setupWith(t, func(c *Config) { c.Metrics = log.hook })
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice@acme"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.all"})
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice@globex"}})
	b.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	log.wait(t, MetricTenantSubs, func(m Metric) bool { return m.Tenant == "acme" && m.Value == 2 })
	log.wait(t, MetricTenantSubs, func(m Metric) bool { return m.Tenant == "globex" && m.Value == 1 })
	log.wait(t, MetricTenantQueued, func(m Metric) bool { return m.Tenant == "acme" })

	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "n"}})
	waitRes(t, a, 1)
	log.wait(t, MetricPubRun, func(m Metric) bool { return m.Pub == "items.mine" && m.Tenant == "acme" })
	log.wait(t, MetricPubRun, func(m Metric) bool { return m.Pub == "items.all" && m.Tenant == TenantMixed })
	log.wait(t, MetricStale, func(m Metric) bool { return m.Pub == "items.mine" && m.Tenant == "acme" })
	log.wait(t, MetricFn, func(m Metric) bool { return m.Pub == "items.mine" && m.Tenant == "acme" })
	log.wait(t, MetricFn, func(m Metric) bool { return m.Pub == "items.all" && m.Tenant == "globex" })
	log.wait(t, MetricCall, func(m Metric) bool { return m.Method == "add" && m.Tenant == "acme" && m.Value > 0 })
	log.wait(t, MetricFenceWait, func(m Metric) bool { return m.Tenant == "acme" })

	// A shared observer whose only sub is globex's again names globex.
	a.send(map[string]any{"t": "unsub", "s": 2})
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "n"}})
	waitRes(t, b, 1)
	log.wait(t, MetricPubRun, func(m Metric) bool { return m.Pub == "items.all" && m.Tenant == "globex" })

	// An unknown method is not reported: the client chooses its name.
	a.send(map[string]any{"t": "call", "id": 2, "m": "nope." + newID(4)})
	for m := a.next(); m["t"] != "res"; m = a.next() {
	}
	time.Sleep(20 * time.Millisecond)
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, m := range log.seen[MetricCall] {
		if strings.HasPrefix(m.Method, "nope.") {
			t.Fatalf("unknown method reported: %+v", m)
		}
	}
}

// Config.Tenant replaces the default. A rebind to another tenant moves the
// subs' attribution, and the tenant left behind gets a final 0.
func TestTenantMetricsRebindAndHook(t *testing.T) {
	log := &metricLog{}
	e := setupWith(t, func(c *Config) {
		c.Metrics = log.hook
		c.Tenant = func(id *Identity) string { return strings.ToUpper(id.ScopeKey) }
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice@acme"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	log.wait(t, MetricTenantSubs, func(m Metric) bool { return m.Tenant == "ACME" && m.Value == 1 })

	a.send(map[string]any{"t": "auth", "id": 1, "token": "alice@globex"})
	log.wait(t, MetricTenantSubs, func(m Metric) bool { return m.Tenant == "GLOBEX" && m.Value == 1 })
	log.wait(t, MetricTenantSubs, func(m Metric) bool { return m.Tenant == "ACME" && m.Value == 0 })
	// One 0, then the tenant is forgotten.
	time.Sleep(2200 * time.Millisecond)
	log.mu.Lock()
	var zeros, after int
	for _, m := range log.seen[MetricTenantSubs] {
		if m.Tenant == "ACME" {
			if m.Value == 0 {
				zeros++
			} else if zeros > 0 {
				after++
			}
		}
	}
	log.mu.Unlock()
	if zeros != 1 || after != 0 {
		t.Fatalf("ACME after leaving: %d zero reports, %d others; want exactly one 0", zeros, after)
	}

	a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "n"}})
	waitRes(t, a, 2)
	log.wait(t, MetricPubRun, func(m Metric) bool { return m.Pub == "items.mine" && m.Tenant == "GLOBEX" })
	log.wait(t, MetricCall, func(m Metric) bool { return m.Method == "add" && m.Tenant == "GLOBEX" })
}

// A panicking Config.Tenant counts as no tenant; the server keeps serving.
func TestTenantHookPanicIsContained(t *testing.T) {
	log := &metricLog{}
	e := setupWith(t, func(c *Config) {
		c.Metrics = log.hook
		c.Tenant = func(*Identity) string { panic("broken tenant hook") }
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice@acme"}})
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "n"}})
	waitRes(t, a, 1)
	log.wait(t, MetricCall, func(m Metric) bool { return m.Method == "add" && m.Tenant == "" })
}

// A token refresh for the same principal keeps its tenant, even when
// Config.Tenant would now name another: the subs stay bound, and every
// metric of the session keeps naming the tenant they were bound for.
func TestTenantKeptOnSamePrincipalRefresh(t *testing.T) {
	log := &metricLog{}
	e := setupWith(t, func(c *Config) {
		c.Metrics = log.hook
		c.Authenticate = func(_ context.Context, token string) (*Identity, error) {
			return &Identity{UserID: "alice", Data: token}, nil // same principal, other claims
		}
		c.Tenant = func(id *Identity) string { return id.Data.(string) }
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "one"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	log.wait(t, MetricTenantSubs, func(m Metric) bool { return m.Tenant == "one" && m.Value == 1 })
	a.send(map[string]any{"t": "auth", "id": 1, "token": "two"})
	for m := a.next(); m["t"] != "authRes"; m = a.next() {
	}
	a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "n"}})
	waitRes(t, a, 2)
	log.wait(t, MetricCall, func(m Metric) bool { return m.Method == "add" && m.Tenant == "one" })
	time.Sleep(1100 * time.Millisecond) // one more round of gauges
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, name := range []string{MetricTenantSubs, MetricTenantQueued, MetricCall, MetricFenceWait, MetricPubRun, MetricFn} {
		for _, m := range log.seen[name] {
			if m.Tenant == "two" {
				t.Fatalf("%s named the refreshed claim's tenant: %+v", name, m)
			}
		}
	}
}

// A resumed session keeps its tenant: its subs stay bound, and its calls
// and their observers' runs still name it.
func TestTenantKeptOnResume(t *testing.T) {
	log := &metricLog{}
	e := setupWith(t, func(c *Config) { c.Metrics = log.hook })
	auth := map[string]any{"token": "alice@acme"}
	a, w := e.dial(t, resumeHello(map[string]any{"auth": auth}))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	snap := a.next()
	a.kill()
	b, w2 := e.dial(t, resumeHello(map[string]any{"auth": auth, "resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if w2["resumed"] != true {
		t.Fatalf("not resumed: %v", w2)
	}
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "n"}})
	waitRes(t, b, 1)
	log.wait(t, MetricCall, func(m Metric) bool { return m.Method == "add" && m.Tenant == "acme" })
	log.wait(t, MetricPubRun, func(m Metric) bool { return m.Pub == "items.mine" && m.Tenant == "acme" })
	log.wait(t, MetricTenantSubs, func(m Metric) bool { return m.Tenant == "acme" && m.Value == 1 })
}
