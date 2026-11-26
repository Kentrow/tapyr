package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderNew(t *testing.T) {
	for _, name := range []string{"acme", "my-api", "example-sim"} {
		t.Run(name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "not", "yet")
			res := execute(t, nil, "provider", "new", name, "--dir", parent)
			if res.code != 0 {
				t.Fatalf("provider new: exit %d\n%s%s", res.code, res.stdout, res.stderr)
			}
			dir := filepath.Join(parent, name)
			for _, want := range []string{dir, "tapyr provider lint " + dir, "tapyr sim --provider-path " + parent + " --provider " + name} {
				if !strings.Contains(res.stdout, want) {
					t.Errorf("output lacks %q:\n%s", want, res.stdout)
				}
			}
			res = execute(t, nil, "provider", "lint", "--catalog", dir)
			if res.code != 0 || !strings.Contains(res.stdout, name+": ok") {
				t.Fatalf("lint of the skeleton: exit %d\n%s%s", res.code, res.stdout, res.stderr)
			}
			expected, err := os.ReadFile(filepath.Join(dir, "tests", "expected.prom"))
			if err != nil {
				t.Fatal(err)
			}
			prefix := strings.ReplaceAll(name, "-", "_")
			for _, want := range []string{prefix + `_device_up{serial="SIM-0001"} 1`, prefix + `_device_up{serial="SIM-0002"} 0`} {
				if !strings.Contains(string(expected), want) {
					t.Errorf("expected.prom lacks %q:\n%s", want, expected)
				}
			}
		})
	}
}

func TestProviderNewLongName(t *testing.T) {
	name := strings.Repeat("a", 30) + "-" + strings.Repeat("b", 24)
	parent := t.TempDir()
	if res := execute(t, nil, "provider", "new", name, "--dir", parent); res.code != 0 {
		t.Fatalf("provider new: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	data, err := os.ReadFile(filepath.Join(parent, name, "dashboards", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// Grafana rejects a dashboard uid longer than 40 characters.
	uid := "tapyr-" + name[:34]
	if !strings.Contains(string(data), `"uid": "`+uid+`"`) {
		t.Errorf("dashboard uid is not %q", uid)
	}
}

func TestProviderNewErrors(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "taken")
	if err := os.MkdirAll(existing, 0o750); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(existing, "mine.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "existing directory", args: []string{"taken", "--dir", parent}, want: "already exists"},
		{name: "invalid name", args: []string{"Acme_1", "--dir", parent}, want: "must be a DNS label"},
		{name: "too long", args: []string{strings.Repeat("a", 56), "--dir", parent}, want: "at most 55 characters"},
		{name: "no name", args: []string{"--dir", parent}, want: "accepts 1 arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := execute(t, nil, append([]string{"provider", "new"}, tt.args...)...)
			if res.code == 0 || !strings.Contains(res.stderr, tt.want) {
				t.Fatalf("exit %d, want an error with %q\n%s%s", res.code, tt.want, res.stdout, res.stderr)
			}
		})
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "mine" {
		t.Errorf("the existing directory was changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "Acme_1")); !os.IsNotExist(err) {
		t.Errorf("an invalid name left a directory: %v", err)
	}
}

func TestWriteSkeletonCleansUp(t *testing.T) {
	// A skeleton that fails its own lint is removed, not left half done.
	parent := t.TempDir()
	dir := filepath.Join(parent, "acme")
	err := newPackage(t.Context(), dir, "acme", func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "tests", "contract.yaml"), []byte("kind: Nope\n"), 0o600)
	})
	if err == nil {
		t.Fatal("a broken skeleton passed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the broken skeleton was left behind: %v", err)
	}
}
