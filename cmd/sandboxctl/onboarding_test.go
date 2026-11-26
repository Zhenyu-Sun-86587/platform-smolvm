package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBareCommandKeepsHelpForPipedInputAndJSON(t *testing.T) {
	for _, args := range [][]string{{}, {"--json"}, {"--no-animation"}} {
		root, _ := rootCommand()
		var stdout bytes.Buffer
		root.SetIn(strings.NewReader("machines list\n"))
		root.SetOut(&stdout)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if result := stdout.String(); !strings.Contains(result, "Usage:") ||
			strings.Contains(result, "sandboxctl>") || strings.Contains(result, "\x1b") {
			t.Fatalf("bare %q output %q", args, result)
		}
	}
}

func TestShellArgsAndCompletionReuseCommandTree(t *testing.T) {
	args, err := splitShellLine(`exec --tty "name:two words" -- sh -c 'echo "$HOME"' "" one\ two`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--tty", "name:two words", "--", "sh", "-c", `echo "$HOME"`, "", "one two"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%q, want %q", args, want)
	}
	if _, err := splitShellLine(`machines create "unclosed`); err == nil {
		t.Fatal("expected unfinished quote")
	}
	for prefix, want := range map[string]string{"machine crea": "machine create ", "machines cre": "machines create ", "snapshots capt": "snapshots capt"} {
		line, pos, handled := shellCompletion(prefix, len(prefix), '\t')
		if line != want || pos != len(want) || !handled {
			t.Fatalf("completed %q to %q at %d (%t); want %q", prefix, line, pos, handled, want)
		}
	}
}

func TestBrandOverrideAcceptsOnlyStaticCellMasks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terminal.txt")
	if err := os.WriteFile(path, []byte("## ##\n #  #\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if art := wordmark(dir, 40); !reflect.DeepEqual(art, []string{"██ ██", " █  █"}) {
		t.Fatalf("wordmark %q", art)
	}
	if art := wordmark(dir, 4); !reflect.DeepEqual(art, []string{defaultWordmark}) {
		t.Fatalf("narrow terminal wordmark %q", art)
	}
	if err := os.WriteFile(path, []byte("##\x1b[A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if art := wordmark(dir, 40); !reflect.DeepEqual(art, []string{defaultWordmark}) {
		t.Fatalf("invalid mask wordmark %q", art)
	}
	if welcomeFrame("████", 0, welcomeFrames-1) != "████" || welcomeFrame("████", 0, 0) == "████" {
		t.Fatal("animation must resolve to the mask")
	}
}
