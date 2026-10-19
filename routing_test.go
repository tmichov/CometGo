package cometgo

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
)

// The docs/server.md "Routing to nodes" nginx recipe, end to end: two nodes
// behind docker/nginx/nginx.conf, which hashes on the route parameter.
// Needs the db and nginx (make test-routing).
func TestRoutingByRouteKey(t *testing.T) {
	base := os.Getenv("COMETGO_TEST_NGINX_URL")
	if base == "" {
		t.Skip("COMETGO_TEST_NGINX_URL not set")
	}
	// The nodes nginx proxies to, on the host ports its config names.
	var nodes [2]*testEnv
	for i, port := range []int{54341, 54342} {
		nodes[i] = setup(t)
		// All interfaces, not loopback: nginx reaches it over the docker bridge.
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			t.Fatalf("port %d busy (docker/nginx/nginx.conf names it): %v", port, err)
		}
		srv := &http.Server{Handler: nodes[i].app}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
	}
	// nodeOf reports which node holds session sid.
	nodeOf := func(sid string) int {
		t.Helper()
		for i, n := range nodes {
			if n.app.lookupSession(sid) != nil {
				return i
			}
		}
		t.Fatalf("no node holds session %s", sid)
		return -1
	}
	url := func(key string) string { return base + "/?route=" + key }

	// A reconnect with the same key reaches the same node and resumes.
	a, w := dialURL(t, url("acme"), resumeHello(nil))
	sid := w["session"].(string)
	home := nodeOf(sid)
	a.kill()
	_, w2 := dialURL(t, url("acme"), resumeHello(map[string]any{"resume": map[string]any{"session": sid, "seq": 0, "subs": []int{}}}))
	if w2["resumed"] != true || w2["session"] != sid {
		t.Fatalf("reconnect did not resume on node %d: %v", home, w2)
	}

	// Different keys spread over both nodes.
	seen := map[int]bool{}
	for i := 0; i < 32 && len(seen) < 2; i++ {
		_, w := dialURL(t, url(fmt.Sprintf("tenant%d", i)), resumeHello(nil))
		seen[nodeOf(w["session"].(string))] = true
	}
	if len(seen) < 2 {
		t.Fatal("32 keys all routed to one node")
	}
}
