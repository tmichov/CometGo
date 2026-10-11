package cometgo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// metricLog records every Metric the hook receives.
type metricLog struct {
	mu   sync.Mutex
	seen map[string][]Metric
}

func (l *metricLog) hook(m Metric) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]Metric{}
	}
	l.seen[m.Name] = append(l.seen[m.Name], m)
}

// wait returns the first Metric named name with match true, failing after 5s.
func (l *metricLog) wait(t *testing.T, name string, match func(Metric) bool) Metric {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		l.mu.Lock()
		for _, m := range l.seen[name] {
			if match == nil || match(m) {
				l.mu.Unlock()
				return m
			}
		}
		l.mu.Unlock()
	}
	t.Fatalf("metric %s never reported", name)
	return Metric{}
}

func TestMetricsReportEachSignal(t *testing.T) {
	log := &metricLog{}
	appName := "cometgo_test_" + newID(4)
	e := setupWith(t, func(c *Config) {
		c.Metrics = log.hook
		cfg := c.DB.Config()
		cfg.ConnConfig.RuntimeParams["application_name"] = appName
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.DB = pool
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.next()
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "a", "Name": "n"}})
	a.next()
	if r := a.next(); r["t"] != "res" || r["lag"] != nil {
		t.Fatalf("res %v", r)
	}

	log.wait(t, MetricTick, func(m Metric) bool { return m.Value > 0 })
	log.wait(t, MetricTickRuns, func(m Metric) bool { return m.Value >= 1 })
	log.wait(t, MetricObservers, func(m Metric) bool { return m.Value == 1 })
	log.wait(t, MetricSubscriptions, func(m Metric) bool { return m.Value == 1 })
	log.wait(t, MetricQueuedBytes, nil)
	log.wait(t, MetricCoalescing, func(m Metric) bool { return m.Value == 0 })
	log.wait(t, MetricFenceWait, func(m Metric) bool { return m.Value > 0 })
	log.wait(t, MetricPubRun, func(m Metric) bool { return m.Pub == "items.all" && m.Value > 0 })

	// A dropped LISTEN connection.
	e.waitListening(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND query LIKE 'LISTEN %'`, appName); err != nil {
		t.Fatal(err)
	}
	log.wait(t, MetricListenReconnect, func(m Metric) bool { return m.Value == 1 })

	// Every 4429 close goes through socket.fail; once per socket.
	c := e.app.liveConns()[0]
	c.mu.Lock()
	s := c.sock
	c.mu.Unlock()
	s.fail(closeSlow, "SLOW_CONSUMER", "test")
	s.fail(closeSlow, "SLOW_CONSUMER", "test")
	log.wait(t, MetricSlowClose, nil)
	time.Sleep(50 * time.Millisecond)
	log.mu.Lock()
	n := len(log.seen[MetricSlowClose])
	log.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d slow closes reported for one socket", n)
	}
}

// A call whose fence times out is reported as lag.
func TestMetricsReportFenceLag(t *testing.T) {
	log := &metricLog{}
	e := setupWith(t, func(c *Config) {
		c.Metrics = log.hook
		c.FenceTimeout = time.Nanosecond
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "a", "Name": "n"}})
	if r := a.next(); r["t"] != "res" || r["lag"] != true {
		t.Fatalf("expected a lagged res, got %v", r)
	}
	log.wait(t, MetricFenceLag, func(m Metric) bool { return m.Value == 1 })
}
