package main

import (
	"context"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
)

// TestSimAll serves every built-in simulator at its port offset from
// --listen-base, with the scenario applied to those that have it.
func TestSimAll(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]string{} // requested address -> real address
	listen := func(ctx context.Context, network, address string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, network, "127.0.0.1:0")
		if err == nil {
			mu.Lock()
			asked[address] = ln.Addr().String()
			mu.Unlock()
		}
		return ln, err
	}
	var stderr syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"sim", "--all", "--listen-base", "0.0.0.0:9101", "--scenario", "quota-tight", "--log-format", "text"},
			&env{stdout: &stderr, stderr: &stderr, lookupEnv: func(string) (string, bool) { return "", false }, clock: clock.Real(), listen: listen})
	}()
	want := map[string]string{
		"0.0.0.0:9101": "aruba-central-classic", "0.0.0.0:9102": "example-sim", "0.0.0.0:9103": "meraki-dashboard",
		"0.0.0.0:9104": "teltonika-rms", "0.0.0.0:9105": "teltonika-rutos", "0.0.0.0:9106": "juniper-mist",
		"0.0.0.0:9107": "cisco-thousandeyes",
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(asked)
		mu.Unlock()
		if n == len(want) && strings.Count(stderr.String(), "simulator listening") == len(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d simulators listening:\n%s", n, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for addr, provider := range want {
		if !strings.Contains(stderr.String(), "provider="+provider+" address="+asked[addr]) {
			t.Errorf("%s is not served on %s:\n%s", provider, addr, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "provider=example-sim address="+asked["0.0.0.0:9102"]+" scenario=quota-tight") {
		t.Errorf("the scenario is not applied:\n%s", stderr.String())
	}
	if status, _ := httpGet(t, "http://"+asked["0.0.0.0:9103"]+"/_sim/status"); status != http.StatusOK {
		t.Errorf("meraki simulator status: %d", status)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Errorf("exit %d:\n%s", code, stderr.String())
	}
}

// TestPortOffsets: declared offsets are kept, the packages without one take
// the offsets after the largest declared one in name order, and two
// packages may not declare the same offset.
func TestPortOffsets(t *testing.T) {
	pkg := func(name string, offset ...int) *config.Package {
		p := &config.Package{
			Provider:   &config.Provider{Metadata: config.ProviderMeta{Name: config.Name(name)}},
			Simulation: &config.Simulation{},
		}
		if len(offset) > 0 {
			p.Simulation.Spec.PortOffset = &offset[0]
		}
		return p
	}
	tests := []struct {
		name string
		pkgs []*config.Package
		want map[string]int
		err  string
	}{
		{"none declared", []*config.Package{pkg("a"), pkg("b")}, map[string]int{"a": 0, "b": 1}, ""},
		{"a new package does not move the others", []*config.Package{pkg("a", 0), pkg("aa"), pkg("b", 1)}, map[string]int{"a": 0, "b": 1, "aa": 2}, ""},
		{"gaps are kept", []*config.Package{pkg("a", 5), pkg("b"), pkg("c", 0)}, map[string]int{"a": 5, "c": 0, "b": 6}, ""},
		{"duplicate offsets", []*config.Package{pkg("a", 1), pkg("b", 1)}, nil, "simulators a and b both declare portOffset 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := portOffsets(tt.pkgs, simulated(tt.pkgs))
			if tt.err != "" {
				if err == nil || err.Error() != tt.err {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("offsets = %v, want %v", got, tt.want)
			}
		})
	}
}
