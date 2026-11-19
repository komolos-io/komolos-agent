package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	k8s "github.com/komolos-io/komolos-agent/internal/migrate/k8s"
	"github.com/komolos-io/komolos-agent/internal/ui"
)

type kvFlag map[string]string

func (f kvFlag) String() string { return "" }
func (f kvFlag) Set(v string) error {
	from, to, ok := strings.Cut(v, "=")
	if !ok || from == "" || to == "" {
		return fmt.Errorf("want SOURCE=DESTINATION, got %q", v)
	}
	f[from] = to
	return nil
}

// runMigrate moves one namespace between clusters. With plan=true it only
// reads both clusters and prints what it would do.
func runMigrate(ctx context.Context, args []string, plan bool, stdin io.Reader) error {
	name := "migrate k8s"
	if plan {
		name = "plan k8s"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	from := fs.String("from", "", "source kubeconfig context (read only)")
	to := fs.String("to", "", "destination kubeconfig context")
	ns := fs.String("n", "", "namespace to move")
	fs.StringVar(ns, "namespace", "", "namespace to move (same as -n)")
	toNS := fs.String("to-namespace", "", "namespace on the destination (default: same name)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig file (default: kubectl's own: $KUBECONFIG or ~/.kube/config)")
	kubectl := fs.String("kubectl", "kubectl", "kubectl binary to run")
	classes := kvFlag{}
	fs.Var(classes, "storage-class", "map a source storage class to a destination one: gp3=local-path (repeatable)")
	ingress := kvFlag{}
	fs.Var(ingress, "ingress-class", "map a source ingress class to a destination one: alb=nginx (repeatable)")
	cronActive := fs.Bool("keep-cronjobs-active", false, "create CronJobs active; by default they are suspended until cutover so jobs never run in both clusters")
	secrets := fs.Bool("include-secrets", true, "migrate Secrets (they are written to the destination and to the local bundle)")
	helper := fs.String("helper-image", "busybox:1.36", "image for the temporary restore pod on the destination")
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for workloads to become ready")
	skipData := fs.Bool("skip-data", false, "move configuration and workloads only, not volume data")
	overwrite := fs.Bool("overwrite", false, "replace objects that already exist on the destination")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	out := fs.String("o", "", "folder for manifests.json and report.json (default: komolos-migration-<namespace>-<time>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" || *ns == "" {
		return errors.New("--from, --to and -n are required")
	}
	if _, err := exec.LookPath(*kubectl); err != nil {
		return fmt.Errorf("%s not found: install kubectl (https://kubernetes.io/docs/tasks/tools/)", *kubectl)
	}
	u := ui.New(os.Stdout)
	dir := *out
	if dir == "" {
		dir = fmt.Sprintf("komolos-migration-%s-%s", *ns, time.Now().Format("20060102-150405"))
	}
	cfg := &k8s.Config{
		Source: k8s.Kubectl{Binary: *kubectl, Kubeconfig: *kubeconfig, Context: *from},
		Dest:   k8s.Kubectl{Binary: *kubectl, Kubeconfig: *kubeconfig, Context: *to},
		Options: k8s.Options{Namespace: *ns, ToNamespace: *toNS, StorageClass: classes, IngressClass: ingress,
			IncludeSecret: *secrets, KeepCronJobsActive: *cronActive},
		HelperImage: *helper, Wait: *wait, SkipData: *skipData, Overwrite: *overwrite, OutDir: dir, UI: u,
	}

	title := "Kubernetes migration"
	if plan {
		title = "Migration plan"
	}
	u.Banner(title, version)
	u.KV("From", *from, "read only")
	u.KV("To", *to, "")
	target := *ns
	if *toNS != "" && *toNS != *ns {
		target = *ns + " → " + *toNS
	}
	u.KV("Namespace", target, "")

	reading := u.Start("Reading", "both clusters")
	p, err := k8s.Build(ctx, cfg)
	reading.Clear()
	if err != nil {
		u.Blank()
		u.Note(ui.Fail, err.Error())
		u.Blank()
		return errReported{err}
	}
	p.Summary(u)

	if plan {
		u.Blank()
		u.Text("Nothing was written. Run with --migrate k8s to apply.")
		u.Blank()
		return nil
	}
	if !*yes {
		u.Prompt("Apply to " + *to + "?")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "yes" {
			u.Blank()
			return errReported{errors.New("not applied")}
		}
	}
	if err := k8s.WriteBundle(dir, p); err != nil {
		return err
	}

	u.Section("Migrating")
	r, err := k8s.Run(ctx, cfg, p)
	if r != nil {
		printResult(u, r, dir)
	}
	if err != nil {
		return errReported{err}
	}
	if r.Result != "success" {
		return errIssues
	}
	return nil
}

// errIssues means the migration ran but something needs attention (a volume
// not copied or not verified, a workload not ready). It exits with code 3.
var errIssues = errors.New("migration completed with issues; see the report")

func printResult(u *ui.UI, r *k8s.Report, dir string) {
	took := ui.Duration(r.FinishedAt.Sub(r.StartedAt))
	switch r.Result {
	case "success":
		u.Section("Done in " + took)
	case "failed":
		u.Section("Failed after " + took)
	default:
		u.Section("Finished with issues in " + took)
	}
	u.Text("Source unchanged. Report: " + dir + string(os.PathSeparator) + "report.json")
	u.Blank()
}

// errReported is an error already shown to the user in styled form; main
// prints nothing more for it, only sets the exit code.
type errReported struct{ err error }

func (e errReported) Error() string { return e.err.Error() }
func (e errReported) Unwrap() error { return e.err }
