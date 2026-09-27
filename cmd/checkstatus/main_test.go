package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	in := "ok\r\n\x1b[31mred\x1b[0m\x00\x07 ‮evil\ttab\n"
	want := "ok\nred evil\ttab\n"
	if got := sanitize(in); got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
}

func TestStdoutBlockStopsCommands(t *testing.T) {
	out := stdoutBlock("tok", "line\n::error::injected\r\n::tok2::")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "::stop-commands::tok" || lines[len(lines)-1] != "::tok::" {
		t.Fatalf("bad framing: %q", out)
	}
	if strings.Count(out, "::tok::") != 1 {
		t.Fatalf("text closes block early: %q", out)
	}
}

func TestSummaryBlockEscapes(t *testing.T) {
	s := summaryBlock("task_1", "FAIL", "", "</pre><img src=x onerror=alert(1)>\n```\n[link](http://x)")
	if strings.Count(s, "<pre>") != 1 || strings.Count(s, "</pre>") != 1 {
		t.Fatalf("pre not closed exactly once: %s", s)
	}
	if strings.Contains(s, "<img") {
		t.Fatalf("html not escaped: %s", s)
	}
}

func TestRunnerOutput(t *testing.T) {
	dir := t.TempDir()
	var log strings.Builder
	for i := 0; i < 100; i++ {
		log.WriteString("line\n")
	}
	log.WriteString("last \x1b[1mline\x1b[0m\n")
	if err := os.WriteFile(filepath.Join(dir, "task_1.log"), []byte(log.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	body, note := runnerOutput(dir, "task_1")
	if note == "" || !strings.HasSuffix(body, "last line") || strings.Count(body, "\n") != logTailLines-1 {
		t.Fatalf("log fallback: note=%q body=%q", note, body)
	}

	if err := os.MkdirAll(filepath.Join(dir, "task_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task_1", "task_1.txt"), []byte("Задание 1\nS1 PASS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, note = runnerOutput(dir, "task_1")
	if note != "" || body != "Задание 1\nS1 PASS" {
		t.Fatalf("report: note=%q body=%q", note, body)
	}
}
