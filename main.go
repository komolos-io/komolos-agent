// komolos-agent is the customer-side komolos binary. What it does is chosen by
// a mode flag: --collect inventories an environment (an AWS account, or Linux
// servers anywhere) into one JSON file, read-only; --migrate k8s moves a
// Kubernetes namespace to another cluster, reading the source and writing only
// to the destination you name.
//
// Every mode runs once and exits. Despite the name there is nothing to install
// on the machine that runs it: no service, no background process, no telemetry,
// no auto-update. It contacts only the AWS API or the SSH targets you name,
// writes a file you review, and sends nothing anywhere unless you run --upload.
//
//	komolos-agent --plan aws                           # show what will be called, run nothing
//	komolos-agent --collect aws --region eu-west-1     # AWS API, read-only
//	komolos-agent --collect ssh --targets hosts.txt    # Linux servers over SSH
//	komolos-agent --upload -f inventory-aws.json       # optional, after review
//	komolos-agent --print-wrapper > komolos-discover   # script for target servers
//	komolos-agent --migrate k8s --from eks-prod --to lagos -n shop
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/komolos-io/komolos-agent/internal/inventory"
	awsinv "github.com/komolos-io/komolos-agent/internal/providers/aws"
	sshhost "github.com/komolos-io/komolos-agent/internal/providers/ssh"
	"github.com/komolos-io/komolos-agent/internal/ui"
	"github.com/komolos-io/komolos-agent/internal/upload"
)

// version is set at release time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

// The wrapper and the IAM policy sit at the repository root and are embedded, so the
// binary prints exactly what was reviewed.
var (
	//go:embed komolos-discover.sh
	wrapper []byte
	//go:embed aws-iam-policy.json
	awsPolicy []byte
)

const usage = `komolos-agent: customer-side komolos tasks. Each one runs once and exits.

Usage:
  komolos-agent --collect aws [--profile NAME] [--region R1,R2] [-o FILE]
  komolos-agent --collect ssh (--targets FILE | --host [USER@]HOST[:PORT] ...) [options] [-o FILE]
  komolos-agent --plan aws [--policy]           show the API calls (or IAM policy); runs nothing
  komolos-agent --plan ssh (--targets FILE | --host H ...) [options]
                                                show the exact ssh commands; runs nothing
  komolos-agent --plan k8s --from CTX --to CTX -n NAMESPACE
                                                show what a migration would move; changes nothing
  komolos-agent --migrate k8s --from CTX --to CTX -n NAMESPACE [options]
                                                move a namespace (config, secrets, workloads, volume data)
                                                to another cluster; the source is only read
  komolos-agent --upload -f FILE [--endpoint URL] [--token-file FILE] [--yes]
  komolos-agent --print-wrapper                 the script target servers run
  komolos-agent --version

The mode flag comes first; the options for that mode follow it.
The file stays on this machine. Nothing is sent anywhere unless you run --upload.
Run "komolos-agent --collect aws -h" (and so on) for the options of each mode.
`

// splitMode reads the mode flag, which must be the first argument, so that a
// mode's own options (--region, --host, ...) can follow it unchanged. It
// accepts "--collect aws" and "--collect=aws".
func splitMode(args []string) (mode, target string, rest []string, err error) {
	if len(args) == 0 || !strings.HasPrefix(args[0], "-") {
		return "", "", nil, errors.New("start with a mode flag such as --collect")
	}
	mode, target, hasValue := strings.Cut(strings.TrimLeft(args[0], "-"), "=")
	rest = args[1:]
	switch mode {
	case "collect", "plan", "migrate":
		if !hasValue && len(rest) > 0 {
			target, rest = rest[0], rest[1:]
		}
		switch {
		case mode == "collect" && (target == "aws" || target == "ssh"):
		case mode == "plan" && (target == "aws" || target == "ssh" || target == "k8s"):
		case mode == "migrate" && target == "k8s":
		case mode == "migrate":
			return "", "", nil, errors.New("--migrate needs a target: k8s")
		case mode == "plan":
			return "", "", nil, errors.New("--plan needs a target: aws, ssh or k8s")
		default:
			return "", "", nil, fmt.Errorf("--%s needs a target: aws or ssh", mode)
		}
	case "upload", "print-wrapper", "version", "v", "help", "h":
		if hasValue {
			return "", "", nil, fmt.Errorf("--%s takes no value", mode)
		}
	default:
		return "", "", nil, fmt.Errorf("unknown mode %q", args[0])
	}
	return mode, target, rest, nil
}

func main() {
	mode, target, rest, err := splitMode(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch mode {
	case "collect":
		if target == "aws" {
			err = runAWS(ctx, rest)
		} else {
			err = runSSH(ctx, rest)
		}
	case "plan":
		if target == "k8s" {
			err = runMigrate(ctx, rest, true, os.Stdin)
		} else {
			err = runPlan(append([]string{target}, rest...))
		}
	case "migrate":
		err = runMigrate(ctx, rest, false, os.Stdin)
	case "upload":
		err = runUpload(ctx, rest, os.Stdin)
	case "print-wrapper":
		_, err = os.Stdout.Write(wrapper)
	case "version", "v":
		fmt.Println("komolos-agent", version, "schema", inventory.SchemaVersion, "wrapper", sshhost.WrapperVersion)
	case "help", "h":
		fmt.Print(usage)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		var shown errReported
		if !errors.As(err, &shown) && !errors.Is(err, errIssues) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		if errors.Is(err, errIssues) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func newInventory(provider string) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion, CollectorVersion: version,
		Provider: provider, CollectedAt: time.Now().UTC(), Gaps: []inventory.Gap{},
	}
}

func finish(inv *inventory.Inventory, path string) error {
	if err := inventory.WriteFile(path, inv); err != nil {
		return err
	}
	u := ui.New(os.Stderr)
	u.Section("Inventory")
	if inv.AWS != nil {
		for _, r := range inv.AWS.Regions {
			u.Row(ui.Item, r.Region, fmt.Sprintf("%d instances  ·  %d volumes  ·  %d VPCs  ·  %d clusters",
				len(r.Compute.Instances), len(r.Compute.Volumes), len(r.Network.VPCs), len(r.Compute.EKSClusters)+len(r.Compute.ECSClusters)))
		}
	}
	for _, h := range inv.Hosts {
		desc := h.OS.PrettyName
		if desc == "" {
			desc = "unreachable"
		}
		u.Row(ui.Item, h.Address, fmt.Sprintf("%s  ·  %d services  ·  %d listening ports", desc, len(h.Services), len(h.Listeners)))
	}
	if len(inv.Gaps) > 0 {
		u.Blank()
		const show = 6
		for i, g := range inv.Gaps {
			if i == show {
				u.Text(fmt.Sprintf("   … %d more gaps listed in the file", len(inv.Gaps)-show))
				break
			}
			u.Row(ui.Warn, strings.TrimPrefix(strings.TrimPrefix(g.Scope, "aws:"), "host:"), humanGap(g.Area, g.Code))
		}
	}
	u.Section("Saved")
	u.Row(ui.OK, "File", path)
	u.Text("Nothing was sent. Review the file, then: komolos-agent --upload -f " + path)
	u.Blank()
	return nil
}

// hostCounter turns the SSH collector's per-host progress lines into a live
// "n of total hosts" status.
type hostCounter struct {
	live  *ui.Live
	total int
	n     int
}

func (h *hostCounter) Write(b []byte) (int, error) {
	h.n += strings.Count(string(b), "\n")
	h.live.Set(fmt.Sprintf("%d of %d hosts", h.n, h.total))
	return len(b), nil
}

func firstLineOf(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runAWS(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("--collect aws", flag.ContinueOnError)
	profile := fs.String("profile", "", "AWS shared-config profile (default: standard credential chain)")
	regions := fs.String("region", "", "comma-separated regions (default: the profile's region)")
	out := fs.String("o", "inventory-aws.json", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u := ui.New(os.Stderr)
	u.Banner("AWS inventory", version)
	where := *regions
	if where == "" {
		where = "profile default"
	}
	if *profile != "" {
		u.KV("Profile", *profile, "")
	}
	u.KV("Regions", where, "read only")
	u.Blank()
	l := u.Start("Collecting", "Describe and List calls")
	awsInv, gaps, err := awsinv.Collect(ctx, awsinv.Options{Profile: *profile, Regions: splitList(*regions)})
	if err != nil {
		l.Done(ui.Fail, firstLineOf(err))
		u.Blank()
		return errReported{err}
	}
	l.Done(ui.OK, "account "+awsInv.AccountID)
	inv := newInventory("aws")
	inv.AWS = awsInv
	inv.Gaps = append(inv.Gaps, gaps...)
	return finish(inv, *out)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type sshOptions struct {
	cfg     sshhost.Config
	targets []sshhost.Target
	out     string
}

func parseSSH(name string, args []string) (*sshOptions, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	targetsFile := fs.String("targets", "", "file with one [user@]host[:port] per line")
	var hosts multiFlag
	fs.Var(&hosts, "host", "target [user@]host[:port] (repeatable)")
	user := fs.String("user", "komolos-discovery", "login for targets that do not name one")
	identity := fs.String("i", "", "private key or certificate file (default: your ssh config and agent)")
	jump := fs.String("J", "", "jump host, as for ssh -J")
	acceptNew := fs.Bool("accept-new-host-keys", false, "trust host keys not yet in known_hosts (default: refuse unknown hosts)")
	parallel := fs.Int("parallel", 4, "hosts to inventory at once")
	sshBin := fs.String("ssh", "ssh", "OpenSSH client to run")
	out := fs.String("o", "inventory-ssh.json", "output file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	var targets []sshhost.Target
	if *targetsFile != "" {
		f, err := os.Open(*targetsFile)
		if err != nil {
			return nil, err
		}
		targets, err = sshhost.ParseTargets(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", *targetsFile, err)
		}
	}
	for _, h := range hosts {
		t, err := sshhost.ParseTarget(h)
		if err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return nil, errors.New("no targets: pass --targets FILE or --host HOST")
	}
	return &sshOptions{
		cfg: sshhost.Config{
			SSHBinary: *sshBin, DefaultUser: *user, IdentityFile: *identity, JumpHost: *jump,
			AcceptNewKeys: *acceptNew, Parallel: *parallel,
		},
		targets: targets, out: *out,
	}, nil
}

func runSSH(ctx context.Context, args []string) error {
	o, err := parseSSH("--collect ssh", args)
	if err != nil {
		return err
	}
	if err := sshhost.LookPath(o.cfg.SSHBinary); err != nil {
		return err
	}
	u := ui.New(os.Stderr)
	u.Banner("Server inventory", version)
	u.KV("Hosts", fmt.Sprint(len(o.targets)), "read only, through komolos-discover")
	u.Blank()
	l := u.Start("Collecting", fmt.Sprintf("0 of %d hosts", len(o.targets)))
	found, gaps := sshhost.Collect(ctx, o.cfg, o.targets, &hostCounter{live: l, total: len(o.targets)})
	l.Done(ui.OK, fmt.Sprintf("%d hosts", len(o.targets)))
	inv := newInventory("ssh")
	inv.Hosts = found
	inv.Gaps = append(inv.Gaps, gaps...)
	return finish(inv, o.out)
}

// runPlan prints what a run would do, without credentials, network or targets
// being touched, so a security team can approve the exact behaviour.
func runPlan(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: komolos-agent --plan aws [--policy] | --plan ssh (--targets FILE | --host H ...)")
	}
	switch args[0] {
	case "aws":
		fs := flag.NewFlagSet("--plan aws", flag.ContinueOnError)
		policy := fs.Bool("policy", false, "print the minimal IAM policy as JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *policy {
			_, err := os.Stdout.Write(awsPolicy)
			return err
		}
		var p struct {
			Statement []struct{ Action []string }
		}
		if err := json.Unmarshal(awsPolicy, &p); err != nil {
			return err
		}
		fmt.Println("komolos-agent --collect aws makes only these read-only AWS API calls, in each region you pass:")
		for _, st := range p.Statement {
			for _, a := range st.Action {
				fmt.Println("  " + a)
			}
		}
		fmt.Println("\nCredentials come from the standard AWS chain (environment, profile, SSO, instance role).")
		fmt.Println("Only the Name tag is read. Nothing is created, changed or deleted.")
		fmt.Println("Output: a local JSON file. Network: AWS API endpoints only.")
		fmt.Println("Grant exactly this with: komolos-agent --plan aws --policy > policy.json")
		return nil
	case "ssh":
		o, err := parseSSH("--plan ssh", args[1:])
		if err != nil {
			return err
		}
		fmt.Println("komolos-agent --collect ssh would run, from this machine:")
		for _, t := range o.targets {
			fmt.Println("  " + strings.Join(o.cfg.Command(t), " "))
		}
		fmt.Println("\nOn each server, sshd runs the forced command /usr/local/bin/komolos-discover with")
		fmt.Println("SSH_ORIGINAL_COMMAND=all. The wrapper accepts only: all " + strings.Join(sshhost.Keywords, " "))
		fmt.Println("and refuses anything else (exit 126). Read it with: komolos-agent --print-wrapper")
		fmt.Println("Output: a local JSON file. Network: the SSH targets (and jump host) above only.")
		return nil
	default:
		return fmt.Errorf("unknown plan %q (want aws or ssh)", args[0])
	}
}

func runUpload(ctx context.Context, args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("--upload", flag.ContinueOnError)
	file := fs.String("f", "", "inventory file to upload")
	endpoint := fs.String("endpoint", envOr("KOMOLOS_ENDPOINT", "https://control.komolos.com"), "control plane URL")
	tokenFile := fs.String("token-file", "", "read the upload token from this file (default: $KOMOLOS_UPLOAD_TOKEN, else prompt)")
	yes := fs.Bool("yes", false, "upload without asking for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("pass -f FILE")
	}
	doc, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	inv, err := inventory.Decode(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}
	inventory.Summary(os.Stderr, inv)
	fmt.Fprintf(os.Stderr, "sha256:       %s\nendpoint:     %s\n", inventory.SHA256(doc), *endpoint)

	in := bufio.NewReader(stdin)
	token := os.Getenv("KOMOLOS_UPLOAD_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		token = string(b)
	}
	if strings.TrimSpace(token) == "" {
		fmt.Fprint(os.Stderr, "Upload token (from the komolos dashboard, Migrations > New upload token): ")
		token, _ = in.ReadString('\n')
	}
	if !*yes {
		fmt.Fprint(os.Stderr, "Upload this file? [y/N] ")
		answer, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return errors.New("not uploaded")
		}
	}
	res, err := upload.Send(ctx, *endpoint, token, doc)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Uploaded. Inventory %s, sha256 %s\n", res.InventoryID, res.SHA256)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// humanGap says in plain words what could not be collected. The file keeps
// the exact codes.
func humanGap(area, code string) string {
	area = strings.ReplaceAll(area, "_", " ")
	switch {
	case strings.HasPrefix(code, "unavailable:"):
		return area + " not available on this host"
	case strings.HasPrefix(code, "exit_"):
		return area + " could not be read"
	case code == "missing" || code == "truncated":
		return area + " incomplete"
	case code == "UnauthorizedOperation" || code == "AccessDenied" || code == "AccessDeniedException":
		return area + ": no permission"
	case code == "auth_failed":
		return "SSH sign-in refused"
	case code == "host_key_unverified":
		return "host key not trusted (check known_hosts)"
	case code == "timeout" || code == "connect_timeout":
		return "timed out"
	case code == "refused":
		return "komolos-discover refused the request"
	case code == "connection_refused" || code == "unreachable" || code == "dns_failed":
		return "could not connect"
	}
	return area + ": " + strings.ReplaceAll(code, "_", " ")
}
