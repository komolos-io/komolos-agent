// Package redact removes secret-looking substrings from text the collector
// reads from customer hosts, before anything is written to the inventory file.
//
// The wrapper already avoids the obvious sources (command lines, file contents,
// environment variables). This is the second line of defence for the fields
// that remain free text: container names and images, port strings, service
// names, mount sources.
package redact

import "regexp"

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private key material.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), "[PRIVATE KEY]"},
	// Credentials embedded in URLs: scheme://user:pass@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}[REDACTED]@"},
	// AWS access key IDs.
	{regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "[AWS KEY]"},
	// JWTs and bearer tokens.
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), "[JWT]"},
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9\-._~+/]+=*`), "bearer [REDACTED]"},
	// key=value or key: value where the key names a secret.
	{regexp.MustCompile(`(?i)\b([a-z0-9_.\-]*(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)[a-z0-9_.\-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`), "${1}${3}[REDACTED]"},
	// GitHub, Slack and generic long hex/base64 tokens.
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|xox[abprs]-[A-Za-z0-9\-]{10,})\b`), "[TOKEN]"},
}

// String redacts sensitive-looking substrings.
func String(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
