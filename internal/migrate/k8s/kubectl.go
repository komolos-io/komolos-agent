// Package k8s moves one namespace from a source Kubernetes cluster to a
// destination cluster: configuration, secrets, services, workloads and the
// data in their persistent volumes.
//
// It drives the customer's own kubectl, the same way the SSH collector drives
// their own ssh: their kubeconfig, contexts, EKS/GKE/AKS auth plugins and RBAC
// all apply unchanged, and every command it runs can be printed and reviewed.
//
// The source cluster is never modified. Every source command passes through
// sourceGuard, which only allows reads (get, version, auth can-i) and an exec
// of the exact read-only commands this package builds (tar to stream a volume
// out, and a file-count check). A test enforces the guard.
package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Kubectl runs kubectl against one context.
type Kubectl struct {
	Binary     string // default "kubectl"
	Kubeconfig string // optional --kubeconfig
	Context    string // --context
	readOnly   bool   // source cluster: only allow-listed reads
}

func (k Kubectl) bin() string {
	if k.Binary == "" {
		return "kubectl"
	}
	return k.Binary
}

func (k Kubectl) argv(args []string) []string {
	a := []string{}
	if k.Kubeconfig != "" {
		a = append(a, "--kubeconfig", k.Kubeconfig)
	}
	if k.Context != "" {
		a = append(a, "--context", k.Context)
	}
	return append(a, args...)
}

// CommandLine is what Run would execute, for --plan output and logs.
func (k Kubectl) CommandLine(args ...string) string {
	return k.bin() + " " + strings.Join(k.argv(args), " ")
}

var errNotAllowed = errors.New("refused: the source cluster is read-only")

// verifyScript counts files and bytes under a directory. It is the only shell
// command the source cluster may run; "$1" is the volume's mount path.
const verifyScript = `cd "$1" && find . -type f | wc -l && find . -type f -exec cat {} + | wc -c`

// sourceGuard allows only reads. exec is allowed solely for the two commands
// this package builds: "tar cf - -C <path> ." and the verify script.
func sourceGuard(args []string) error {
	if len(args) == 0 {
		return errNotAllowed
	}
	switch args[0] {
	case "get", "version", "api-resources":
		for _, a := range args {
			if a == "--watch" || a == "-w" {
				return errNotAllowed
			}
		}
		return nil
	case "auth":
		if len(args) > 1 && args[1] == "can-i" {
			return nil
		}
	case "exec":
		i := indexOf(args, "--")
		if i < 0 {
			return errNotAllowed
		}
		for _, a := range args[1:i] {
			if a == "-i" || a == "--stdin" || a == "-t" || a == "--tty" || strings.HasPrefix(a, "--stdin=") {
				return errNotAllowed // nothing is ever sent into a source pod
			}
		}
		cmd := args[i+1:]
		if len(cmd) == 6 && cmd[0] == "tar" && cmd[1] == "cf" && cmd[2] == "-" && cmd[3] == "-C" && cmd[5] == "." {
			return nil
		}
		if len(cmd) == 5 && cmd[0] == "sh" && cmd[1] == "-c" && cmd[2] == verifyScript && cmd[3] == "sh" {
			return nil
		}
	}
	return errNotAllowed
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// Run executes kubectl with the given stdin and stdout; stderr is captured and
// returned in the error. On a read-only (source) client, non-read commands are
// refused before anything runs.
func (k Kubectl) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	if k.readOnly {
		if err := sourceGuard(args); err != nil {
			return fmt.Errorf("%w: kubectl %s", err, strings.Join(args, " "))
		}
	}
	cmd := exec.CommandContext(ctx, k.bin(), k.argv(args)...)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 600 {
			msg = msg[:600] + "..."
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("kubectl --context %s %s: %s", k.Context, args[0], msg)
	}
	return nil
}

// Output runs kubectl and returns stdout.
func (k Kubectl) Output(ctx context.Context, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := k.Run(ctx, nil, &out, args...)
	return out.Bytes(), err
}
