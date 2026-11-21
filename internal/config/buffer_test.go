package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDiskBuffer checks the defaults of a disk buffer and where it lives:
// next to the state store by default, else at its path, relative to the
// declaring file.
func TestDiskBuffer(t *testing.T) {
	dir := t.TempDir()
	conf := `apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: default }
spec:
  state: { path: ./state/tapyr.db }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: vm }
spec:
  type: remote_write
  remoteWrite: { url: "http://vm:8428/api/v1/write" }
  queue: { disk: {} }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: collector }
spec:
  type: otlp
  otlp: { endpoint: "http://collector:4318" }
  queue: { disk: { path: ./spool, maxSize: 2GiB, maxAge: 24h } }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: memory-only }
spec:
  type: otlp
  otlp: { endpoint: "http://collector:4318" }
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := newTestLoader(t).Load([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = Assemble(cfg, nil); err != nil {
		t.Fatal(err)
	}
	state := cfg.Settings.Spec.State.Path
	outputs := map[Name]*Output{}
	for _, o := range cfg.Outputs {
		outputs[o.Metadata.Name] = o
	}
	vm := outputs["vm"].Spec.Queue.Disk
	if vm.MaxSize.Bytes() != 512<<20 || vm.MaxAge.Std() != 2*time.Hour {
		t.Errorf("defaults: maxSize %s, maxAge %s", vm.MaxSize, vm.MaxAge.Std())
	}
	if got, want := outputs["vm"].BufferDir(state), filepath.Join(dir, "state", "buffer", "vm"); got != want {
		t.Errorf("default directory %s, want %s", got, want)
	}
	collector := outputs["collector"].Spec.Queue.Disk
	if collector.MaxSize.Bytes() != 2<<30 || collector.MaxAge.Std() != 24*time.Hour {
		t.Errorf("collector: maxSize %s, maxAge %s", collector.MaxSize, collector.MaxAge.Std())
	}
	if got, want := outputs["collector"].BufferDir(state), filepath.Join(dir, "spool"); got != want {
		t.Errorf("path %s, want %s", got, want)
	}
	if outputs["memory-only"].Spec.Queue.Disk != nil {
		t.Error("an output without disk has no disk buffer")
	}
	// Until the queue uses it (ADR 0015, in steps), a warning says so.
	if n := len(cfg.Warnings); n != 2 {
		t.Errorf("warnings = %v, want one per disk buffer", cfg.Warnings)
	}
}
