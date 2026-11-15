package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsVersion matches the version in a link to the documentation: the
// guide shows the one of its release, a test binary links to dev.
var docsVersion = regexp.MustCompile(`docs\.tapyr\.eu/[^/]+/`)

// TestGettingStarted replays the getting-started guide: the configuration
// it asks to write, then its validate and plan commands, the second plan
// after the edit the guide asks for (the devices every 2 minutes instead of
// every minute). Their output must be the one the guide shows, so that the
// guide never drifts from the binary. A line "..." in a block of the guide
// stands for lines it leaves out. Commands that need a running Tapyr (run,
// curl) are left to the acceptance tests.
func TestGettingStarted(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "getting-started.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(data)
	dir := t.TempDir()
	config := codeBlock(t, guide, "In another terminal, create a directory", "yaml")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{}
	plans := 0
	for _, block := range consoleBlocks(guide) {
		for _, step := range block {
			args := strings.Fields(step.command)
			switch {
			case len(args) == 2 && args[0] == "export":
				name, value, _ := strings.Cut(args[1], "=")
				vars[name] = value
				continue
			case len(args) < 2 || args[0] != "tapyr" || (args[1] != "validate" && args[1] != "plan"):
				continue
			}
			if args[1] == "plan" {
				if plans++; plans == 2 {
					edited := strings.Replace(config, "interval: 1m", "interval: 2m", 1)
					if edited == config {
						t.Fatal("the guide no longer polls the devices every minute in its configuration")
					}
					if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(edited), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			for i, a := range args {
				if a == "." && i > 0 && args[i-1] == "--config" {
					args[i] = dir
				}
			}
			res := execute(t, vars, args[1:]...)
			got := strings.ReplaceAll(res.stderr+res.stdout, dir+string(filepath.Separator), "")
			if msg := matchOutput(step.output, got); msg != "" {
				t.Errorf("$ %s\n%s\nthe binary prints:\n%s", step.command, msg, got)
			}
		}
	}
	if plans != 2 {
		t.Errorf("the guide runs tapyr plan %d times, want 2", plans)
	}
}

// codeBlock returns the first fenced block of a language after a line
// starting with marker.
func codeBlock(t *testing.T, doc, marker, lang string) string {
	t.Helper()
	_, after, ok := strings.Cut(doc, "\n"+marker)
	if !ok {
		t.Fatalf("no %q in the guide", marker)
	}
	_, after, ok = strings.Cut(after, "```"+lang+"\n")
	if !ok {
		t.Fatalf("no %s block after %q", lang, marker)
	}
	block, _, _ := strings.Cut(after, "```")
	return block
}

// consoleStep is a command of a console block and the output the guide
// shows for it.
type consoleStep struct {
	command string
	output  []string
}

// consoleBlocks returns the commands of the console blocks of a document,
// each with the lines that follow it up to the next command.
func consoleBlocks(doc string) [][]consoleStep {
	var blocks [][]consoleStep
	var current []consoleStep
	in := false
	scanner := bufio.NewScanner(strings.NewReader(doc))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case !in && line == "```console":
			in, current = true, nil
		case in && line == "```":
			in = false
			blocks = append(blocks, current)
		case in && strings.HasPrefix(line, "$ "):
			current = append(current, consoleStep{command: strings.TrimPrefix(line, "$ ")})
		case in && len(current) > 0:
			last := &current[len(current)-1]
			last.output = append(last.output, line)
		}
	}
	return blocks
}

// matchOutput compares the output the guide shows with the one of the
// binary, line by line, trailing spaces aside; a line "..." skips lines of
// the binary up to the next line of the guide. It returns what differs, or
// "".
func matchOutput(want []string, got string) string {
	got = docsVersion.ReplaceAllString(got, "docs.tapyr.eu/VERSION/")
	want = append([]string(nil), want...)
	for i := range want {
		want[i] = docsVersion.ReplaceAllString(want[i], "docs.tapyr.eu/VERSION/")
	}
	for len(want) > 0 && strings.TrimSpace(want[len(want)-1]) == "" {
		want = want[:len(want)-1]
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	j := 0
	for i := range want {
		w := strings.TrimRight(want[i], " ")
		if w == "..." {
			if i+1 == len(want) {
				return ""
			}
			next := strings.TrimRight(want[i+1], " ")
			for j < len(lines) && strings.TrimRight(lines[j], " ") != next {
				j++
			}
			continue
		}
		if j >= len(lines) {
			return "the guide shows more lines, from: " + w
		}
		if g := strings.TrimRight(lines[j], " "); g != w {
			return "the guide shows:\n  " + w + "\nthe binary:\n  " + g
		}
		j++
	}
	if j < len(lines) {
		return "the binary prints more lines, from: " + lines[j]
	}
	return ""
}

// TestConfigureAccounts checks that the configuration of the guide
// "Configure your accounts" is valid and that its plan fits.
func TestConfigureAccounts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "configure-accounts.md"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := codeBlock(t, string(data), "One file or several", "yaml")
	config = strings.Replace(config, "path: ./data/tapyr.db", "path: "+filepath.Join(dir, "tapyr.db"), 1)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"TAPYR_MERAKI_API_KEY": "guide-api-key"}
	if res := execute(t, vars, "validate", "--config", dir, "--check-secrets"); res.code != exitOK || res.stderr != "" {
		t.Errorf("validate exited with %d:\n%s%s", res.code, res.stderr, res.stdout)
	}
	if res := execute(t, vars, "plan", "--config", dir); res.code != exitOK || !strings.Contains(res.stdout, "The plan fits") {
		t.Errorf("plan exited with %d:\n%s%s", res.code, res.stderr, res.stdout)
	}
}
