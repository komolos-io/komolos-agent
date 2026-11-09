package redact

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	cases := []struct{ in, mustNotContain, mustContain string }{
		{"postgres://app:hunter2@db:5432/x", "hunter2", "[REDACTED]@db"},
		{"key AKIAABCDEFGHIJKLMNOP here", "AKIAABCDEFGHIJKLMNOP", "[AWS KEY]"},
		{"DB_PASSWORD=s3cr3t", "s3cr3t", "DB_PASSWORD=[REDACTED]"},
		{"api_key: 'abc def'", "abc def", "[REDACTED]"},
		{"Authorization: Bearer abc.def.ghi", "abc.def.ghi", "[REDACTED]"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----", "MIIE", "[PRIVATE KEY]"},
		{"ghp_" + strings.Repeat("a", 36), strings.Repeat("a", 36), "[TOKEN]"},
	}
	for _, c := range cases {
		got := String(c.in)
		if strings.Contains(got, c.mustNotContain) || !strings.Contains(got, c.mustContain) {
			t.Errorf("String(%q) = %q", c.in, got)
		}
	}
	// Ordinary inventory text passes through untouched.
	for _, s := range []string{"nginx:1.25-alpine", "0.0.0.0:80->80/tcp, :::80->80/tcp", "postgresql.service", "/dev/nvme0n1p1"} {
		if got := String(s); got != s {
			t.Errorf("String(%q) changed to %q", s, got)
		}
	}
}
