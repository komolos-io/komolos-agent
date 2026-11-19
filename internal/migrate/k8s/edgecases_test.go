package k8s

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/komolos-io/komolos-agent/internal/ui"
)

// Edge cases from real EKS namespaces that a demo app never hits.

func cleanOne(t *testing.T, raw string, o Options) (Object, []Warning) {
	t.Helper()
	var obj Object
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	if o.Namespace == "" {
		o.Namespace = "shop"
	}
	keep, _, warns := Clean([]Object{obj}, o)
	if len(keep) != 1 {
		t.Fatalf("object dropped: %s", raw)
	}
	return keep[0], warns
}

func joinWarnings(w []Warning) string {
	var s []string
	for _, x := range w {
		s = append(s, x.Message)
	}
	return strings.Join(s, " | ")
}

func TestDualStackAndAWSLoadBalancerServices(t *testing.T) {
	svc, warns := cleanOne(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"api","annotations":{"service.beta.kubernetes.io/aws-load-balancer-type":"nlb"}},
	  "spec":{"type":"LoadBalancer","loadBalancerClass":"service.k8s.aws/nlb","ipFamilies":["IPv6","IPv4"],"ipFamilyPolicy":"PreferDualStack","clusterIP":"10.0.0.1","ports":[{"port":443}]}}`, Options{})
	spec := m(svc, "spec")
	for _, f := range []string{"ipFamilies", "ipFamilyPolicy", "loadBalancerClass", "clusterIP"} {
		if _, ok := spec[f]; ok {
			t.Errorf("%s kept; a single-stack or non-AWS destination rejects it", f)
		}
	}
	all := joinWarnings(warns)
	for _, want := range []string{"load balancer class service.k8s.aws/nlb dropped", "AWS load balancer annotations", "LoadBalancer gets a new address"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing warning %q in %q", want, all)
		}
	}
}

func TestIngressClassMapping(t *testing.T) {
	raw := `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"web","annotations":{"alb.ingress.kubernetes.io/scheme":"internet-facing"}},"spec":{"ingressClassName":"alb"}}`
	ing, warns := cleanOne(t, raw, Options{})
	if str(ing, "spec", "ingressClassName") != "alb" || !strings.Contains(joinWarnings(warns), "--ingress-class alb=") {
		t.Errorf("unmapped ALB ingress should be kept and flagged: %v %v", ing, warns)
	}
	ing, warns = cleanOne(t, raw, Options{IngressClass: map[string]string{"alb": "nginx"}})
	if str(ing, "spec", "ingressClassName") != "nginx" {
		t.Errorf("ingress class not mapped: %v", m(ing, "spec"))
	}
	if !strings.Contains(joinWarnings(warns), "ALB annotations") {
		t.Errorf("ALB annotations not flagged: %v", warns)
	}
	// The legacy annotation form maps too, and the annotation is replaced by the field.
	ing, _ = cleanOne(t, `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"old","annotations":{"kubernetes.io/ingress.class":"alb"}},"spec":{}}`,
		Options{IngressClass: map[string]string{"alb": "nginx"}})
	if str(ing, "spec", "ingressClassName") != "nginx" || str(ing, "metadata", "annotations", "kubernetes.io/ingress.class") != "" {
		t.Errorf("legacy ingress class not mapped: %v", ing)
	}
}

func TestCronJobsSuspendedUntilCutover(t *testing.T) {
	raw := `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"invoices"},"spec":{"schedule":"0 * * * *","jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"c","image":"x"}]}}}}}}`
	cj, warns := cleanOne(t, raw, Options{})
	if m(cj, "spec")["suspend"] != true || !strings.Contains(joinWarnings(warns), "suspended") {
		t.Errorf("CronJob must be created suspended so it does not run in both clusters: %v", m(cj, "spec"))
	}
	cj, _ = cleanOne(t, raw, Options{KeepCronJobsActive: true})
	if m(cj, "spec")["suspend"] == true {
		t.Error("--keep-cronjobs-active ignored")
	}
}

func TestQuotasAndLimitRangesAreMigrated(t *testing.T) {
	have := map[string]bool{}
	for _, k := range kinds {
		have[k.Kind] = true
		if (k.Kind == "ResourceQuota" || k.Kind == "LimitRange") && k.Phase != phaseIdentity {
			t.Errorf("%s must be applied before workloads", k.Kind)
		}
	}
	if !have["ResourceQuota"] || !have["LimitRange"] {
		t.Error("ResourceQuota and LimitRange are not migrated")
	}
}

func TestReadiness(t *testing.T) {
	cases := []struct {
		name, obj string
		ready     bool
		note      string
	}{
		{"deployment rolling", `{"kind":"Deployment","metadata":{"generation":2},"spec":{"replicas":3},"status":{"observedGeneration":2,"updatedReplicas":3,"availableReplicas":1}}`, false, "1 of 3 replicas ready"},
		{"deployment old pods available", `{"kind":"Deployment","metadata":{"generation":2},"spec":{"replicas":2},"status":{"observedGeneration":2,"updatedReplicas":1,"availableReplicas":2}}`, false, "1 of 2 replicas ready"},
		{"deployment ready", `{"kind":"Deployment","metadata":{"generation":1},"spec":{"replicas":2},"status":{"observedGeneration":1,"updatedReplicas":2,"availableReplicas":2}}`, true, "2 of 2 replicas ready"},
		{"deployment default replicas", `{"kind":"Deployment","metadata":{"generation":1},"spec":{},"status":{"observedGeneration":1,"updatedReplicas":1,"availableReplicas":1}}`, true, "1 of 1 replicas ready"},
		{"controller behind", `{"kind":"Deployment","metadata":{"generation":3},"spec":{"replicas":1},"status":{"observedGeneration":2,"updatedReplicas":1,"availableReplicas":1}}`, false, "waiting for the controller"},
		{"scaled to zero", `{"kind":"Deployment","metadata":{"generation":1},"spec":{"replicas":0},"status":{"observedGeneration":1}}`, true, "scaled to 0"},
		{"statefulset OnDelete", `{"kind":"StatefulSet","metadata":{"generation":1},"spec":{"replicas":2,"updateStrategy":{"type":"OnDelete"}},"status":{"observedGeneration":1,"readyReplicas":2}}`, true, "2 of 2 replicas ready"},
		{"daemonset no nodes", `{"kind":"DaemonSet","metadata":{"generation":1},"status":{"observedGeneration":1,"desiredNumberScheduled":0}}`, true, "no matching nodes"},
		{"daemonset partial", `{"kind":"DaemonSet","metadata":{"generation":1},"status":{"observedGeneration":1,"desiredNumberScheduled":3,"numberReady":2}}`, false, "2 of 3 nodes ready"},
	}
	for _, c := range cases {
		var obj Object
		json.Unmarshal([]byte(c.obj), &obj)
		ready, note := readiness(obj)
		if ready != c.ready || note != c.note {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, ready, note, c.ready, c.note)
		}
	}
}

func TestWaitReadyPollsUntilReady(t *testing.T) {
	cfg, root := newFakeConfig(t)
	t.Setenv("FAKE_READY_AFTER", "3")
	os.WriteFile(filepath.Join(root, "applied.json"), []byte(`[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":1}}]`), 0o600)
	l := ui.Plain(io.Discard).Start("web", "waiting")
	ready, note, err := cfg.waitReady(context.Background(), "Deployment", "web", "shop", l)
	l.Clear()
	if err != nil || !ready || note != "1 of 1 replicas ready" {
		t.Fatalf("waitReady: %v %q %v", ready, note, err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "polls"))
	if string(b) != "4" {
		t.Errorf("polled %s times, want 4", b)
	}

	// Never ready: gives up at the deadline with the last progress note.
	t.Setenv("FAKE_READY_AFTER", "1000")
	cfg.Wait = 50 * time.Millisecond
	ready, note, err = cfg.waitReady(context.Background(), "Deployment", "web", "shop", l)
	if err != nil || ready || note != "0 of 1 replicas ready" {
		t.Errorf("deadline: %v %q %v", ready, note, err)
	}
}

func TestRefusesWhenDestinationHasNoDefaultStorageClass(t *testing.T) {
	cfg, _ := newFakeConfig(t)
	t.Setenv("FAKE_NO_DEFAULT_SC", "1")
	_, err := Build(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "no default storage class") || !strings.Contains(err.Error(), "PVC site-content") {
		t.Fatalf("want a clear refusal naming the claim, got %v", err)
	}
	// An explicit mapping resolves it.
	cfg.Options.StorageClass = map[string]string{"gp3": "fast-ssd"}
	if _, err := Build(context.Background(), cfg); err != nil {
		t.Fatalf("mapped class still refused: %v", err)
	}
}

func TestBlockVolumesAreReportedNotCopied(t *testing.T) {
	cfg, _ := newFakeConfig(t)
	cfg.Source.readOnly = true
	pvc := Object{"kind": "PersistentVolumeClaim", "metadata": Object{"name": "raw"}, "spec": Object{"volumeMode": "Block"}}
	vols, _, err := planVolumes(context.Background(), cfg.Source, "shop", []Object{pvc})
	if err != nil || len(vols) != 1 || !strings.Contains(vols[0].Error, "raw block volume") || vols[0].SourcePod != "" {
		t.Fatalf("block volume: %+v %v", vols, err)
	}
}

const restrictedFixture = `{
 "namespace": {"apiVersion":"v1","kind":"Namespace","metadata":{"name":"shop","labels":{"pod-security.kubernetes.io/enforce":"restricted","team":"storefront"}}},
 "objects": [
  {"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"shop"},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}},"status":{"phase":"Bound"}},
  {"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"shop"},"spec":{"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"app","image":"nginx"}],"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"data"}}]}}}}
 ],
 "pods": [
  {"metadata":{"name":"web-1"},"spec":{"containers":[{"name":"app","volumeMounts":[{"name":"d","mountPath":"/data"}]}],"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"data"}}]},"status":{"phase":"Running"}}
 ]
}`

// A restricted namespace would reject the root restore pod, so its
// pod-security labels are applied only after the data is in place; and the
// restore pod carries resource requests so a ResourceQuota admits it.
func TestRestrictedNamespaceAndQuotaSafeRestore(t *testing.T) {
	cfg, root := newFakeConfig(t)
	fx := filepath.Join(root, "fixture.json")
	os.WriteFile(fx, []byte(restrictedFixture), 0o600)
	t.Setenv("FAKE_FIXTURE", fx)
	ctx := context.Background()
	p, err := Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBundle(cfg.OutDir, p); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, cfg, p); err != nil {
		t.Fatal(err)
	}
	var applied []Object
	b, _ := os.ReadFile(filepath.Join(root, "applied.json"))
	json.Unmarshal(b, &applied)

	var order []string
	var helper Object
	for _, o := range applied {
		switch kindOf(o) {
		case "Namespace":
			if str(o, "metadata", "labels", "pod-security.kubernetes.io/enforce") == "restricted" {
				order = append(order, "ns-restricted")
			} else {
				order = append(order, "ns-open")
			}
		case "Pod":
			order = append(order, "restore-pod")
			helper = o
		case "Deployment":
			order = append(order, "deployment")
		}
	}
	got := strings.Join(order, " ")
	if got != "ns-open restore-pod ns-restricted deployment" {
		t.Errorf("apply order %q", got)
	}
	c := list(helper, "spec", "containers")[0].(map[string]any)
	if str(c, "resources", "requests", "memory") == "" || str(c, "resources", "limits", "cpu") == "" {
		t.Errorf("restore pod has no requests/limits: %v", c)
	}
}

func TestClasslessClaimsMapWithDefault(t *testing.T) {
	sts := `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db"},"spec":{"volumeClaimTemplates":[{"metadata":{"name":"data"},"spec":{"accessModes":["ReadWriteOnce"]}}]}}`
	obj, _ := cleanOne(t, sts, Options{StorageClass: map[string]string{"default": "fast"}})
	tm := list(obj, "spec", "volumeClaimTemplates")[0].(map[string]any)
	if str(tm, "spec", "storageClassName") != "fast" {
		t.Errorf("classless template not mapped by default=: %v", tm)
	}
	if n := needDefaultClass([]Object{obj}); len(n) != 0 {
		t.Errorf("mapped template still needs a default class: %v", n)
	}
	// A claim bound to a static PV ("" class) must not keep "", or it never binds.
	pvc, warns := cleanOne(t, `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"static"},"spec":{"storageClassName":"","volumeName":"pv-1"}}`, Options{DefaultClass: "local-path"})
	if _, ok := m(pvc, "spec")["storageClassName"]; ok || !strings.Contains(joinWarnings(warns), "static volume") {
		t.Errorf("static claim: %v %v", m(pvc, "spec"), warns)
	}
}

func TestCronJobSuspendedOnSourceStaysAndIsNotMarked(t *testing.T) {
	cj, warns := cleanOne(t, `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"off"},"spec":{"suspend":true,"schedule":"* * * * *"}}`, Options{})
	if str(cj, "metadata", "annotations", suspendedAnnotation) != "" || len(warns) != 0 {
		t.Errorf("a CronJob already off on the source must not be marked for resuming: %v %v", cj, warns)
	}
	cj, _ = cleanOne(t, `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"on"},"spec":{"schedule":"* * * * *"}}`, Options{})
	if str(cj, "metadata", "annotations", suspendedAnnotation) != "true" {
		t.Errorf("suspended-for-cutover annotation missing: %v", cj)
	}
}

func TestPlanNamesTheRealReasonForUncopiedVolumes(t *testing.T) {
	var b strings.Builder
	p := &Plan{Volumes: []VolumeCopy{{PVC: "raw", Error: "raw block volume, file copy not possible"}, {PVC: "idle", Error: "no running pod mounts it"}}}
	p.Summary(ui.Plain(&b))
	out := b.String()
	if !strings.Contains(out, "raw block volume, file copy not possible, data not copied") || !strings.Contains(out, "no running pod mounts it, data not copied") {
		t.Errorf("plan output:\n%s", out)
	}
}
