package k8s

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake kubectl: when FAKE_KUBECTL_ROOT is set it
// answers like kubectl for two contexts, "src" and "dst", streams real tar
// archives between two folders standing in for the volumes, and logs every
// call so the test can check what was sent to each cluster.
func TestMain(m *testing.M) {
	if root := os.Getenv("FAKE_KUBECTL_ROOT"); root != "" {
		os.Exit(fakeKubectl(root, os.Args[1:]))
	}
	os.Exit(m.Run())
}

const sourceFixture = `{
 "namespace": {"apiVersion":"v1","kind":"Namespace","metadata":{"name":"shop","labels":{"team":"storefront","kubernetes.io/metadata.name":"shop"}}},
 "objects": [
  {"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"default","namespace":"shop"}},
  {"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"api","namespace":"shop","uid":"u1","annotations":{"eks.amazonaws.com/role-arn":"arn:aws:iam::123456789012:role/api"}}},
  {"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"kube-root-ca.crt","namespace":"shop"},"data":{"ca.crt":"x"}},
  {"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"web-config","namespace":"shop","uid":"u2","resourceVersion":"77","managedFields":[{}],"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}","keep":"me"}},"data":{"a":"b"}},
  {"apiVersion":"v1","kind":"Secret","metadata":{"name":"payment-keys","namespace":"shop"},"type":"Opaque","data":{"K":"c2VjcmV0"}},
  {"apiVersion":"v1","kind":"Secret","metadata":{"name":"api-token","namespace":"shop"},"type":"kubernetes.io/service-account-token"},
  {"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"site-content","namespace":"shop","annotations":{"pv.kubernetes.io/bind-completed":"yes"},"finalizers":["kubernetes.io/pvc-protection"]},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}},"storageClassName":"gp3","volumeName":"pvc-123"},"status":{"phase":"Bound"}},
  {"apiVersion":"v1","kind":"Service","metadata":{"name":"web","namespace":"shop"},"spec":{"type":"LoadBalancer","clusterIP":"10.0.0.9","clusterIPs":["10.0.0.9"],"ports":[{"port":80,"nodePort":31000}],"selector":{"app":"web"}},"status":{"loadBalancer":{}}},
  {"apiVersion":"v1","kind":"Service","metadata":{"name":"db","namespace":"shop"},"spec":{"clusterIP":"None","ports":[{"port":5432}]}},
  {"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"shop","annotations":{"deployment.kubernetes.io/revision":"4"}},"spec":{"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"nginx","image":"123456789012.dkr.ecr.eu-west-1.amazonaws.com/web:1"}],"volumes":[{"name":"content","persistentVolumeClaim":{"claimName":"site-content"}}]}}},"status":{"readyReplicas":1}}
 ],
 "pods": [
  {"metadata":{"name":"web-abc"},"spec":{"containers":[{"name":"nginx","volumeMounts":[{"name":"content","mountPath":"/usr/share/nginx/html"}]}],"volumes":[{"name":"content","persistentVolumeClaim":{"claimName":"site-content"}}]},"status":{"phase":"Running"}},
  {"metadata":{"name":"web-old"},"spec":{"containers":[{"name":"nginx"}],"volumes":[{"name":"content","persistentVolumeClaim":{"claimName":"site-content"}}]},"status":{"phase":"Failed"}}
 ]
}`

var resourceKind = map[string]string{}

func init() {
	for _, k := range kinds {
		resourceKind[k.Resource] = k.Kind
	}
}

func fakeKubectl(root string, args []string) int {
	ctx := ""
	for len(args) >= 2 && (args[0] == "--context" || args[0] == "--kubeconfig") {
		if args[0] == "--context" {
			ctx = args[1]
		}
		args = args[2:]
	}
	logf, _ := os.OpenFile(filepath.Join(root, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	fmt.Fprintf(logf, "%s\t%s\n", ctx, strings.Join(args, " "))
	logf.Close()

	var fx struct {
		Namespace Object
		Objects   []Object
		Pods      []Object
	}
	fixture := []byte(sourceFixture)
	if f := os.Getenv("FAKE_FIXTURE"); f != "" {
		fixture, _ = os.ReadFile(f)
	}
	json.Unmarshal(fixture, &fx)
	emit := func(v any) int { json.NewEncoder(os.Stdout).Encode(v); return 0 }
	appliedPath := filepath.Join(root, "applied.json")
	applied := func() []Object {
		var a []Object
		b, _ := os.ReadFile(appliedPath)
		json.Unmarshal(b, &a)
		return a
	}
	volume := filepath.Join(root, ctx+"-volume")

	switch args[0] {
	case "version":
		return emit(Object{"serverVersion": Object{"gitVersion": "v1.31.0"}})
	case "get":
		res := args[1]
		switch {
		case res == "storageclasses":
			if ctx == "dst" && os.Getenv("FAKE_NO_DEFAULT_SC") != "" {
				return emit(Object{"items": []any{}})
			}
			if ctx == "dst" {
				return emit(Object{"items": []any{Object{"metadata": Object{"name": "local-path", "annotations": Object{"storageclass.kubernetes.io/is-default-class": "true"}}}}})
			}
			return emit(Object{"items": []any{}})
		case res == "namespace":
			if ctx == "src" {
				if strings.Contains(strings.Join(args, " "), "-o name") {
					fmt.Println("namespace/shop")
					return 0
				}
				return emit(fx.Namespace)
			}
			for _, o := range applied() {
				if kindOf(o) == "Namespace" {
					fmt.Println("namespace/shop")
				}
			}
			return 0
		case res == "pods":
			return emit(Object{"items": fx.Pods})
		case ctx == "dst" && len(args) > 2 && !strings.HasPrefix(args[2], "-"):
			// A single workload: report it ready after FAKE_READY_AFTER polls.
			want, name := resourceKind[res], args[2]
			polls := 0
			if b, err := os.ReadFile(filepath.Join(root, "polls")); err == nil {
				fmt.Sscan(string(b), &polls)
			}
			polls++
			os.WriteFile(filepath.Join(root, "polls"), []byte(fmt.Sprint(polls)), 0o600)
			after := 0
			fmt.Sscan(os.Getenv("FAKE_READY_AFTER"), &after)
			for _, o := range applied() {
				if kindOf(o) == want && nameOf(o) == name {
					ready := int64(0)
					if polls > after {
						ready = 1
					}
					o["metadata"].(map[string]any)["generation"] = 1
					o["status"] = Object{"observedGeneration": 1, "replicas": 1, "updatedReplicas": ready, "availableReplicas": ready, "readyReplicas": ready}
					return emit(o)
				}
			}
			fmt.Fprintf(os.Stderr, "Error from server (NotFound): %s %q not found\n", res, name)
			return 1
		default:
			want := resourceKind[res]
			items := []Object{}
			pool := fx.Objects
			if ctx == "dst" {
				pool = applied()
			}
			for _, o := range pool {
				if kindOf(o) == want {
					items = append(items, o)
				}
			}
			return emit(Object{"items": items})
		}
	case "apply":
		var l struct{ Items []Object }
		json.NewDecoder(os.Stdin).Decode(&l)
		all := append(applied(), l.Items...)
		b, _ := json.Marshal(all)
		os.WriteFile(appliedPath, b, 0o600)
		for _, o := range l.Items {
			fmt.Printf("%s/%s serverside-applied\n", strings.ToLower(kindOf(o)), nameOf(o))
		}
		return 0
	case "wait", "delete", "rollout":
		return 0
	case "exec":
		cmd := args[indexOf(args, "--")+1:]
		switch {
		case cmd[0] == "tar" && cmd[1] == "cf":
			return writeTar(volume, os.Stdout)
		case cmd[0] == "tar" && cmd[1] == "xf":
			return readTar(volume, os.Stdin)
		case cmd[0] == "sh":
			var files, size int64
			filepath.Walk(volume, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi.Mode().IsRegular() {
					files++
					size += fi.Size()
				}
				return nil
			})
			fmt.Printf("%d\n%d\n", files, size)
			return 0
		}
	}
	fmt.Fprintf(os.Stderr, "fake kubectl: unhandled %v\n", args)
	return 1
}

func writeTar(dir string, w io.Writer) int {
	tw := tar.NewWriter(w)
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h, _ := tar.FileInfoHeader(fi, "")
		h.Name = "./" + filepath.ToSlash(rel)
		tw.WriteHeader(h)
		if fi.Mode().IsRegular() {
			f, _ := os.Open(p)
			io.Copy(tw, f)
			f.Close()
		}
		return nil
	})
	tw.Close()
	return 0
}

func readTar(dir string, r io.Reader) int {
	os.MkdirAll(dir, 0o755)
	tr := tar.NewReader(bufio.NewReader(r))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if h.Typeflag == tar.TypeDir {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		f, _ := os.Create(p)
		io.Copy(f, tr)
		f.Close()
	}
}

func newFakeConfig(t *testing.T) (*Config, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FAKE_KUBECTL_ROOT", root)
	vol := filepath.Join(root, "src-volume")
	os.MkdirAll(filepath.Join(vol, "img"), 0o755)
	os.WriteFile(filepath.Join(vol, "index.html"), []byte("<h1>shop</h1>"), 0o644)
	os.WriteFile(filepath.Join(vol, "img", "logo.svg"), []byte(strings.Repeat("x", 5000)), 0o644)
	exe, _ := os.Executable()
	return &Config{
		Source:      Kubectl{Binary: exe, Context: "src"},
		Dest:        Kubectl{Binary: exe, Context: "dst"},
		Options:     Options{Namespace: "shop", IncludeSecret: true, StorageClass: map[string]string{}},
		HelperImage: "busybox:1.36", Wait: 5 * time.Second, PollInterval: 10 * time.Millisecond, OutDir: filepath.Join(root, "bundle"),
	}, root
}

func TestMigrateEndToEndWithFakeKubectl(t *testing.T) {
	cfg, root := newFakeConfig(t)
	ctx := context.Background()
	p, err := Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBundle(cfg.OutDir, p); err != nil {
		t.Fatal(err)
	}
	r, err := Run(ctx, cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Result != "success" {
		t.Fatalf("result %s: %+v", r.Result, r)
	}

	// The data arrived intact.
	got, _ := os.ReadFile(filepath.Join(root, "dst-volume", "img", "logo.svg"))
	if len(got) != 5000 {
		t.Errorf("logo.svg on destination: %d bytes", len(got))
	}
	v := r.Volumes[0]
	if v.Verified != "match" || v.DestFiles != 2 || v.SourcePod != "web-abc" || v.MountPath != "/usr/share/nginx/html" {
		t.Errorf("volume: %+v", v)
	}

	// What was applied, in what order, and how it was cleaned.
	var applied []Object
	b, _ := os.ReadFile(filepath.Join(root, "applied.json"))
	json.Unmarshal(b, &applied)
	var order []string
	byName := map[string]Object{}
	for _, o := range applied {
		order = append(order, kindOf(o)+"/"+nameOf(o))
		byName[kindOf(o)+"/"+nameOf(o)] = o
	}
	joined := strings.Join(order, " ")
	for _, gone := range []string{"ServiceAccount/default", "ConfigMap/kube-root-ca.crt", "Secret/api-token"} {
		if strings.Contains(joined, gone) {
			t.Errorf("%s should not be migrated", gone)
		}
	}
	idx := func(s string) int { return strings.Index(joined, s) }
	if !(idx("Namespace/shop") < idx("ConfigMap/web-config") && idx("PersistentVolumeClaim/site-content") < idx("Pod/komolos-restore") &&
		idx("Pod/komolos-restore") < idx("Deployment/web")) {
		t.Errorf("apply order: %s", joined)
	}
	cm := byName["ConfigMap/web-config"]
	if md := m(cm, "metadata"); md["uid"] != nil || md["resourceVersion"] != nil || md["managedFields"] != nil ||
		str(cm, "metadata", "annotations", "keep") != "me" || str(cm, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration") != "" {
		t.Errorf("configmap metadata not cleaned: %v", md)
	}
	pvc := byName["PersistentVolumeClaim/site-content"]
	if str(pvc, "spec", "volumeName") != "" || str(pvc, "spec", "storageClassName") != "" || m(pvc, "metadata")["finalizers"] != nil {
		t.Errorf("pvc not cleaned: %v", pvc)
	}
	svc := byName["Service/web"]
	if str(svc, "spec", "clusterIP") != "" || m(svc, "status") != nil || list(svc, "spec", "ports")[0].(map[string]any)["nodePort"] != nil {
		t.Errorf("service not cleaned: %v", svc)
	}
	if str(byName["Service/db"], "spec", "clusterIP") != "None" {
		t.Error("headless service lost clusterIP: None")
	}
	if l := m(byName["Namespace/shop"], "metadata", "labels"); l["team"] != "storefront" || l["kubernetes.io/metadata.name"] != nil {
		t.Errorf("namespace labels: %v", l)
	}

	// Warnings an operator needs before cutover.
	var msgs []string
	for _, w := range r.Warnings {
		msgs = append(msgs, w.Message)
	}
	all := strings.Join(msgs, "|")
	for _, want := range []string{"Amazon ECR", "IRSA", "LoadBalancer", "storage class gp3 → local-path"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing warning about %s in %q", want, all)
		}
	}

	// The source only ever saw reads.
	calls, _ := os.ReadFile(filepath.Join(root, "calls.log"))
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		ctxName, cmd, _ := strings.Cut(line, "\t")
		if ctxName == "src" {
			if err := sourceGuard(strings.Fields(cmd)); err != nil && !strings.Contains(cmd, "sh -c") {
				t.Errorf("non-read command reached the source: %s", cmd)
			}
			if strings.HasPrefix(cmd, "apply") || strings.HasPrefix(cmd, "delete") || strings.HasPrefix(cmd, "scale") {
				t.Errorf("write sent to source: %s", cmd)
			}
		}
	}

	// The bundle on disk is private: it contains secrets.
	if fi, err := os.Stat(filepath.Join(cfg.OutDir, "manifests.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("manifests.json mode: %v %v", fi, err)
	}

	// A second run refuses to overwrite what is now on the destination.
	if _, err := Build(ctx, cfg); err == nil || !strings.Contains(err.Error(), "--overwrite") {
		t.Errorf("rerun: %v", err)
	}
}

func TestSourceGuard(t *testing.T) {
	allowed := [][]string{
		{"get", "pods", "-n", "shop", "-o", "json"},
		{"version", "-o", "json"},
		{"auth", "can-i", "list", "pods"},
		{"exec", "web-1", "-n", "shop", "-c", "app", "--", "tar", "cf", "-", "-C", "/data", "."},
		{"exec", "web-1", "-n", "shop", "-c", "app", "--", "sh", "-c", verifyScript, "sh", "/data"},
	}
	for _, a := range allowed {
		if err := sourceGuard(a); err != nil {
			t.Errorf("refused a read: %v", a)
		}
	}
	refused := [][]string{
		{"apply", "-f", "-"},
		{"delete", "pod", "x"},
		{"scale", "deploy/web", "--replicas=0"},
		{"patch", "deploy", "web"},
		{"edit", "deploy", "web"},
		{"cp", "x", "y"},
		{"get", "pods", "--watch"},
		{"exec", "web-1", "--", "rm", "-rf", "/data"},
		{"exec", "web-1", "--", "tar", "xf", "-", "-C", "/data"},
		{"exec", "-i", "web-1", "--", "tar", "cf", "-", "-C", "/data", "."},
		{"exec", "web-1", "--", "sh", "-c", "rm -rf /", "sh", "/data"},
		{"exec", "web-1", "tar", "cf", "-"},
		{"auth", "reconcile"},
		{},
	}
	for _, a := range refused {
		if err := sourceGuard(a); err == nil {
			t.Errorf("allowed a non-read on the source: %v", a)
		}
	}
}

func TestReadOnlyKubectlRefusesBeforeRunning(t *testing.T) {
	k := Kubectl{Binary: "/nonexistent/kubectl", Context: "src", readOnly: true}
	err := k.Run(context.Background(), nil, io.Discard, "delete", "namespace", "shop")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("want a read-only refusal before exec, got %v", err)
	}
}
