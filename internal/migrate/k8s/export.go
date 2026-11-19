package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Object is one Kubernetes object as JSON.
type Object = map[string]any

// kind is a resource the migration moves, in apply order.
type kind struct {
	Resource string // what kubectl get takes
	Kind     string
	Phase    int // apply order; data is copied between phaseStorage and phaseWorkloads
}

const (
	phaseIdentity  = 1 // service accounts, RBAC
	phaseConfig    = 2 // config maps, secrets
	phaseStorage   = 3 // PVCs (data is copied into them next)
	phaseNetwork   = 4 // services
	phaseWorkloads = 5 // deployments, statefulsets, daemonsets, cronjobs
	phaseEdge      = 6 // ingress, autoscaling, disruption budgets, network policy
)

// kinds lists what is migrated. Pods, ReplicaSets, Jobs, Endpoints and events
// are not: their controllers recreate them on the destination.
var kinds = []kind{
	{"serviceaccounts", "ServiceAccount", phaseIdentity},
	{"roles.rbac.authorization.k8s.io", "Role", phaseIdentity},
	{"rolebindings.rbac.authorization.k8s.io", "RoleBinding", phaseIdentity},
	{"resourcequotas", "ResourceQuota", phaseIdentity},
	{"limitranges", "LimitRange", phaseIdentity},
	{"configmaps", "ConfigMap", phaseConfig},
	{"secrets", "Secret", phaseConfig},
	{"persistentvolumeclaims", "PersistentVolumeClaim", phaseStorage},
	{"services", "Service", phaseNetwork},
	{"deployments.apps", "Deployment", phaseWorkloads},
	{"statefulsets.apps", "StatefulSet", phaseWorkloads},
	{"daemonsets.apps", "DaemonSet", phaseWorkloads},
	{"cronjobs.batch", "CronJob", phaseWorkloads},
	{"ingresses.networking.k8s.io", "Ingress", phaseEdge},
	{"horizontalpodautoscalers.autoscaling", "HorizontalPodAutoscaler", phaseEdge},
	{"poddisruptionbudgets.policy", "PodDisruptionBudget", phaseEdge},
	{"networkpolicies.networking.k8s.io", "NetworkPolicy", phaseEdge},
}

func phaseOf(k string) int {
	for _, x := range kinds {
		if x.Kind == k {
			return x.Phase
		}
	}
	return phaseEdge
}

// Skipped records an object that is deliberately not migrated, and why.
type Skipped struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Warning is something the operator should check on the destination.
type Warning struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Options control how objects are rewritten for the destination.
type Options struct {
	Namespace     string            // source namespace
	ToNamespace   string            // destination namespace (default: same)
	StorageClass  map[string]string // source class -> destination class
	DestClasses   map[string]bool   // storage classes present on the destination
	DefaultClass  string            // destination default storage class, if any
	IngressClass  map[string]string // source ingress class -> destination class
	IncludeSecret bool
	// KeepCronJobsActive creates CronJobs unsuspended. By default they are
	// suspended so a job does not run in both clusters before cutover.
	KeepCronJobsActive bool
}

func (o Options) toNS() string {
	if o.ToNamespace != "" {
		return o.ToNamespace
	}
	return o.Namespace
}

// Export lists every migrated kind in the source namespace. A kind the source
// does not serve, or that the caller may not list, becomes a warning, so one
// missing permission does not stop the migration.
func Export(ctx context.Context, src Kubectl, ns string) ([]Object, []Warning, error) {
	if _, err := src.Output(ctx, "get", "namespace", ns, "-o", "name"); err != nil {
		return nil, nil, fmt.Errorf("source namespace %q: %w", ns, err)
	}
	var objs []Object
	var warns []Warning
	for _, k := range kinds {
		out, err := src.Output(ctx, "get", k.Resource, "-n", ns, "-o", "json")
		if err != nil {
			warns = append(warns, Warning{Kind: k.Kind, Message: "could not read from source (" + firstLine(err.Error()) + ")"})
			continue
		}
		var list struct {
			Items []Object `json:"items"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", k.Resource, err)
		}
		for _, it := range list.Items {
			if it["kind"] == nil {
				it["kind"] = k.Kind // kubectl lists sometimes omit kind on items
			}
			objs = append(objs, it)
		}
	}
	return objs, warns, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- small JSON helpers ----

func m(o Object, path ...string) Object {
	cur := o
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

func str(o Object, path ...string) string {
	if len(path) == 0 {
		return ""
	}
	parent := m(o, path[:len(path)-1]...)
	if parent == nil {
		return ""
	}
	s, _ := parent[path[len(path)-1]].(string)
	return s
}

func list(o Object, path ...string) []any {
	parent := m(o, path[:len(path)-1]...)
	if parent == nil {
		return nil
	}
	l, _ := parent[path[len(path)-1]].([]any)
	return l
}

func kindOf(o Object) string { s, _ := o["kind"].(string); return s }
func nameOf(o Object) string { return str(o, "metadata", "name") }

// annotations removed because they describe the source cluster's state.
var dropAnnotation = regexp.MustCompile(`^(kubectl\.kubernetes\.io/last-applied-configuration|deployment\.kubernetes\.io/revision|pv\.kubernetes\.io/.*|volume\.beta\.kubernetes\.io/.*|volume\.kubernetes\.io/.*|control-plane\.alpha\.kubernetes\.io/leader|endpoints\.kubernetes\.io/.*|autoscaling\.alpha\.kubernetes\.io/(conditions|current-metrics))$`)

var ecrImage = regexp.MustCompile(`^\d{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/`)

// Clean turns exported objects into destination manifests, dropping objects
// the destination creates itself and fields that only make sense on the
// source. It never invents values: anything that cannot carry over unchanged
// is dropped and reported.
func Clean(objs []Object, o Options) (keep []Object, skipped []Skipped, warns []Warning) {
	for _, obj := range objs {
		k, name := kindOf(obj), nameOf(obj)
		if reason := skipReason(obj); reason != "" {
			skipped = append(skipped, Skipped{Kind: k, Name: name, Reason: reason})
			continue
		}
		if k == "Secret" && !o.IncludeSecret {
			skipped = append(skipped, Skipped{Kind: k, Name: name, Reason: "secrets excluded (--include-secrets=false)"})
			continue
		}
		if k == "PersistentVolumeClaim" {
			if ph := str(obj, "status", "phase"); ph != "" && ph != "Bound" {
				warns = append(warns, Warning{Kind: k, Name: name, Message: "unbound on source, created empty"})
			}
		}
		meta := m(obj, "metadata")
		clean := Object{"name": name, "namespace": o.toNS()}
		if l, ok := meta["labels"].(map[string]any); ok && len(l) > 0 {
			clean["labels"] = l
		}
		if a, ok := meta["annotations"].(map[string]any); ok {
			kept := map[string]any{}
			for key, v := range a {
				if !dropAnnotation.MatchString(key) {
					kept[key] = v
				}
			}
			if len(kept) > 0 {
				clean["annotations"] = kept
			}
		}
		if refs, ok := meta["ownerReferences"].([]any); ok && len(refs) > 0 {
			warns = append(warns, Warning{Kind: k, Name: name, Message: "owner reference dropped"})
		}
		obj["metadata"] = clean
		delete(obj, "status")

		switch k {
		case "Service":
			warns = append(warns, cleanService(obj)...)
		case "Ingress":
			warns = append(warns, cleanIngress(obj, o)...)
		case "CronJob":
			if !o.KeepCronJobsActive {
				if spec := m(obj, "spec"); spec != nil && spec["suspend"] != true {
					spec["suspend"] = true
					setAnnotation(obj, suspendedAnnotation, "true")
					warns = append(warns, Warning{Kind: k, Name: name, Message: "created suspended, resume it at cutover"})
				}
			}
		case "PersistentVolumeClaim":
			warns = append(warns, cleanPVC(obj, o)...)
		case "StatefulSet":
			for _, t := range list(obj, "spec", "volumeClaimTemplates") {
				if tm, ok := t.(map[string]any); ok {
					delete(tm, "status")
					if md := m(tm, "metadata"); md != nil {
						for _, f := range []string{"creationTimestamp", "uid", "resourceVersion", "managedFields"} {
							delete(md, f)
						}
					}
					warns = append(warns, mapStorageClass(tm, o, "StatefulSet", name)...)
				}
			}
		case "ServiceAccount":
			delete(obj, "secrets") // token secrets are created per cluster
			if a := str(obj, "metadata", "annotations", "eks.amazonaws.com/role-arn"); a != "" {
				warns = append(warns, Warning{Kind: k, Name: name, Message: "uses AWS IAM role (IRSA), grant access on destination"})
			}
		case "RoleBinding":
			for _, s := range list(obj, "subjects") {
				if sm, ok := s.(map[string]any); ok && sm["namespace"] == o.Namespace {
					sm["namespace"] = o.toNS()
				}
			}
		}
		if spec := podSpec(obj); spec != nil {
			warns = append(warns, inspectPodSpec(spec, k, name)...)
		}
		keep = append(keep, obj)
	}
	sort.SliceStable(keep, func(i, j int) bool { return phaseOf(kindOf(keep[i])) < phaseOf(kindOf(keep[j])) })
	return keep, skipped, warns
}

func skipReason(obj Object) string {
	k, name := kindOf(obj), nameOf(obj)
	switch {
	case k == "ServiceAccount" && name == "default":
		return "created automatically in every namespace"
	case k == "ConfigMap" && (name == "kube-root-ca.crt" || name == "openshift-service-ca.crt"):
		return "created automatically in every namespace"
	case k == "Secret" && str(obj, "type") == "kubernetes.io/service-account-token":
		return "service account token; the destination issues its own"
	case str(obj, "metadata", "deletionTimestamp") != "":
		return "being deleted on the source"
	}
	return ""
}

func cleanService(obj Object) []Warning {
	spec := m(obj, "spec")
	if spec == nil {
		return nil
	}
	var warns []Warning
	if spec["clusterIP"] != "None" { // keep headless services headless
		delete(spec, "clusterIP")
		delete(spec, "clusterIPs")
	}
	delete(spec, "healthCheckNodePort")
	delete(spec, "loadBalancerIP")
	// IP families describe the source network (for example dual-stack EKS) and
	// are rejected by a single-stack destination; let the destination choose.
	delete(spec, "ipFamilies")
	delete(spec, "ipFamilyPolicy")
	if lbc, _ := spec["loadBalancerClass"].(string); lbc != "" {
		delete(spec, "loadBalancerClass") // its controller (e.g. AWS) is not on the destination
		warns = append(warns, Warning{Kind: "Service", Name: nameOf(obj), Message: "load balancer class " + lbc + " dropped, destination default used"})
	}
	for key := range m(obj, "metadata", "annotations") {
		if strings.HasPrefix(key, "service.beta.kubernetes.io/aws-load-balancer") {
			warns = append(warns, Warning{Kind: "Service", Name: nameOf(obj), Message: "AWS load balancer annotations have no effect on destination"})
			break
		}
	}
	t, _ := spec["type"].(string)
	for _, p := range list(obj, "spec", "ports") {
		if pm, ok := p.(map[string]any); ok {
			delete(pm, "nodePort")
		}
	}
	if t == "LoadBalancer" || t == "NodePort" {
		warns = append(warns, Warning{Kind: "Service", Name: nameOf(obj), Message: t + " gets a new address, update DNS at cutover"})
	}
	return warns
}

// cleanIngress maps the ingress class (for example AWS "alb" to "nginx") and
// warns about AWS load balancer annotations that do nothing elsewhere.
func cleanIngress(obj Object, o Options) []Warning {
	var warns []Warning
	name := nameOf(obj)
	class := str(obj, "spec", "ingressClassName")
	annotated := str(obj, "metadata", "annotations", "kubernetes.io/ingress.class")
	if class == "" {
		class = annotated
	}
	if to, ok := o.IngressClass[class]; ok && class != "" {
		if spec := m(obj, "spec"); spec != nil {
			spec["ingressClassName"] = to
		}
		if annotated != "" {
			delete(m(obj, "metadata", "annotations"), "kubernetes.io/ingress.class")
		}
	} else if class == "alb" {
		warns = append(warns, Warning{Kind: "Ingress", Name: name, Message: "uses AWS ALB ingress class, map it with --ingress-class alb=<class>"})
	}
	for key := range m(obj, "metadata", "annotations") {
		if strings.HasPrefix(key, "alb.ingress.kubernetes.io/") {
			warns = append(warns, Warning{Kind: "Ingress", Name: name, Message: "ALB annotations have no effect on destination"})
			break
		}
	}
	return warns
}

func cleanPVC(obj Object, o Options) []Warning {
	spec := m(obj, "spec")
	if spec == nil {
		return nil
	}
	delete(spec, "volumeName")
	delete(spec, "dataSource")
	delete(spec, "dataSourceRef")
	return mapStorageClass(obj, o, "PersistentVolumeClaim", nameOf(obj))
}

// suspendedAnnotation marks CronJobs that komolos-agent suspended, so they can
// be told apart from ones already suspended on the source.
const suspendedAnnotation = "komolos.io/suspended-for-cutover"

func setAnnotation(obj Object, k, v string) {
	md := m(obj, "metadata")
	if md == nil {
		return
	}
	a, _ := md["annotations"].(map[string]any)
	if a == nil {
		a = map[string]any{}
		md["annotations"] = a
	}
	a[k] = v
}

// mapStorageClass rewrites spec.storageClassName for the destination: an
// explicit mapping wins; a class that exists there is kept; otherwise the
// field is removed so the destination's default class is used.
func mapStorageClass(obj Object, o Options, k, name string) []Warning {
	spec := m(obj, "spec")
	if spec == nil {
		return nil
	}
	sc, set := spec["storageClassName"].(string)
	if sc == "" {
		// No class (default) or "" (bound to a static PV on the source). Neither
		// binds on the destination without a class, so "default=<class>" maps
		// both; otherwise the destination default provisions them.
		delete(spec, "storageClassName")
		if to, ok := o.StorageClass["default"]; ok {
			spec["storageClassName"] = to
			return nil
		}
		if set && k == "PersistentVolumeClaim" {
			return []Warning{{Kind: k, Name: name, Message: "bound to a static volume on the source, provisioned by the destination default class"}}
		}
		return nil
	}
	if to, ok := o.StorageClass[sc]; ok {
		spec["storageClassName"] = to
		return nil
	}
	if o.DestClasses[sc] {
		return nil
	}
	delete(spec, "storageClassName")
	target := "cluster default"
	if o.DefaultClass != "" {
		target = o.DefaultClass
	}
	return []Warning{{Kind: k, Name: name, Message: fmt.Sprintf("storage class %s → %s", sc, target)}}
}

// podSpec returns the pod spec inside a workload, if it has one.
func podSpec(obj Object) Object {
	switch kindOf(obj) {
	case "Deployment", "StatefulSet", "DaemonSet":
		return m(obj, "spec", "template", "spec")
	case "CronJob":
		return m(obj, "spec", "jobTemplate", "spec", "template", "spec")
	}
	return nil
}

func inspectPodSpec(spec Object, k, name string) []Warning {
	var warns []Warning
	seen := map[string]bool{}
	for _, field := range []string{"containers", "initContainers"} {
		for _, c := range list(spec, field) {
			cm, _ := c.(map[string]any)
			img, _ := cm["image"].(string)
			if ecrImage.MatchString(img) && !seen[img] {
				seen[img] = true
				warns = append(warns, Warning{Kind: k, Name: name, Message: "image in Amazon ECR, destination needs pull access"})
			}
		}
	}
	if ns := m(spec, "nodeSelector"); ns != nil {
		if _, ok := ns["kubernetes.io/hostname"]; ok {
			warns = append(warns, Warning{Kind: k, Name: name, Message: "pinned to a source node, edit nodeSelector"})
		}
		for key := range ns {
			if strings.HasPrefix(key, "eks.amazonaws.com/") || strings.HasPrefix(key, "node.kubernetes.io/instance-type") {
				warns = append(warns, Warning{Kind: k, Name: name, Message: "source-specific node selector " + key})
			}
		}
	}
	if s, _ := spec["nodeName"].(string); s != "" {
		delete(spec, "nodeName")
		warns = append(warns, Warning{Kind: k, Name: name, Message: "nodeName dropped, scheduler places it"})
	}
	return warns
}

// Namespaces of every object are rewritten by Clean; this builds the
// destination namespace object itself, copying the source's labels.
func NamespaceObject(src Object, toNS string) Object {
	labels := map[string]any{}
	if l := m(src, "metadata", "labels"); l != nil {
		for k, v := range l {
			if k != "kubernetes.io/metadata.name" {
				labels[k] = v
			}
		}
	}
	labels["app.kubernetes.io/managed-by"] = "komolos-agent"
	return Object{"apiVersion": "v1", "kind": "Namespace", "metadata": Object{"name": toNS, "labels": labels}}
}
