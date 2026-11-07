package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunFailsWhenTheAddressIsTaken checks that a collector that cannot
// start exits with code 1 and says why.
func TestRunFailsWhenTheAddressIsTaken(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	data, err := os.ReadFile(filepath.Join(examples, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := strings.Replace(string(data), "listen: 127.0.0.1:9790", "listen: "+ln.Addr().String(), 1)
	config = strings.Replace(config, "path: ../../data/tapyr.db", "path: "+filepath.Join(dir, "tapyr.db"), 1)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	res := execute(t, map[string]string{"TAPYR_EXAMPLE_API_KEY": "sim-api-key"}, "run", "--config", dir)
	if res.code != exitFailure || !strings.Contains(res.stderr, "address already in use") {
		t.Errorf("exit %d, want %d and the cause:\n%s", res.code, exitFailure, res.stderr)
	}
}
