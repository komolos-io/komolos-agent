package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestSplitMode(t *testing.T) {
	ok := []struct {
		args         []string
		mode, target string
		rest         int
	}{
		{[]string{"--collect", "aws", "--region", "eu-west-1"}, "collect", "aws", 2},
		{[]string{"--collect=ssh", "--host", "h1"}, "collect", "ssh", 2},
		{[]string{"-collect", "ssh"}, "collect", "ssh", 0},
		{[]string{"--plan", "aws", "--policy"}, "plan", "aws", 1},
		{[]string{"--upload", "-f", "x.json"}, "upload", "", 2},
		{[]string{"--print-wrapper"}, "print-wrapper", "", 0},
		{[]string{"--version"}, "version", "", 0},
	}
	for _, c := range ok {
		mode, target, rest, err := splitMode(c.args)
		if err != nil || mode != c.mode || target != c.target || len(rest) != c.rest {
			t.Errorf("splitMode(%v) = %q, %q, %v, %v", c.args, mode, target, rest, err)
		}
	}
	bad := [][]string{
		nil,
		{"aws"},
		{"--collect"},
		{"--collect", "gcp"},
		{"--collect", "--region", "eu-west-1"},
		{"--region", "eu-west-1", "--collect", "aws"},
		{"--upload=x"},
	}
	for _, args := range bad {
		if _, _, _, err := splitMode(args); err == nil {
			t.Errorf("splitMode(%v) should fail", args)
		}
	}
}

// TestIAMPolicyMatchesCode keeps aws-iam-policy.json honest: every AWS call in
// the collector must be in the policy, and the policy must grant nothing the
// collector does not call. Each SDK call takes a <service>.<Action>Input, so
// the inputs in the source are the list of calls.
func TestIAMPolicyMatchesCode(t *testing.T) {
	files, _ := filepath.Glob("internal/providers/aws/*.go")
	re := regexp.MustCompile(`\b(ec2|eks|ecs|sts)\.(\w+)Input\b`)
	called := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			called[m[1]+":"+m[2]] = true
		}
	}
	var p struct{ Statement []struct{ Action []string } }
	if err := json.Unmarshal(awsPolicy, &p); err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for _, s := range p.Statement {
		for _, a := range s.Action {
			granted[a] = true
		}
	}
	diff := func(a, b map[string]bool) (out []string) {
		for k := range a {
			if !b[k] {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	if missing := diff(called, granted); len(missing) > 0 {
		t.Errorf("called but not in aws-iam-policy.json: %v", missing)
	}
	if extra := diff(granted, called); len(extra) > 0 {
		t.Errorf("in aws-iam-policy.json but never called: %v", extra)
	}
	for a := range granted {
		verb := a[strings.Index(a, ":")+1:]
		if !strings.HasPrefix(verb, "Describe") && !strings.HasPrefix(verb, "List") && !strings.HasPrefix(verb, "Get") {
			t.Errorf("policy grants a non-read action: %s", a)
		}
	}
}

func TestWrapperEmbeddedIsTheReviewedFile(t *testing.T) {
	b, err := os.ReadFile("komolos-discover.sh")
	if err != nil || string(b) != string(wrapper) {
		t.Fatal("embedded wrapper differs from komolos-discover.sh")
	}
}
