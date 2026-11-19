package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPlainLayout(t *testing.T) {
	var b bytes.Buffer
	u := Plain(&b)
	u.Banner("Kubernetes migration", "v0.2.0")
	u.KV("From", "eks-prod", "read only")
	u.Section("Plan")
	u.Row(OK, "Configuration", "3 objects")
	u.Note(Warn, "storage class gp3 → local-path")
	l := u.Start("site-content", "copying")
	l.Write(make([]byte, 2500))
	l.Done(OK, "2 files · 2.5 KB")
	out := b.String()
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape codes in plain output: %q", out)
	}
	if strings.Contains(out, "/_/") {
		t.Error("logo printed when not a terminal")
	}
	for _, want := range []string{"Kubernetes migration", "From       eks-prod  read only", "── Plan ──",
		"✓  Configuration       3 objects", "!  storage class gp3", "✓  site-content        2 files · 2.5 KB"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if l.Bytes() != 2500 {
		t.Errorf("bytes %d", l.Bytes())
	}
}

func TestColorBanner(t *testing.T) {
	var b bytes.Buffer
	u := &UI{w: &b, color: true, live: true}
	u.Banner("t", "v1")
	if !strings.Contains(b.String(), "\x1b[38;5;") || !strings.Contains(b.String(), "/_/ |_") {
		t.Errorf("no styled banner: %q", b.String())
	}
}

func TestFormatters(t *testing.T) {
	if Bytes(6100) != "6.1 KB" || Bytes(12) != "12 B" || Bytes(2_400_000) != "2.4 MB" {
		t.Error("Bytes")
	}
	if Duration(41*time.Second) != "41s" || Duration(184*time.Second) != "3m 4s" {
		t.Error("Duration")
	}
}
