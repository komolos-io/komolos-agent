package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/komolos-io/komolos-agent/internal/ui"
)

// Config is one migration: one namespace, source cluster to destination.
type Config struct {
	Source, Dest  Kubectl
	Options       Options
	HelperImage   string        // image for the temporary restore pod on the destination
	Wait          time.Duration // how long to wait for workloads to become ready
	SkipData      bool
	Overwrite     bool
	OutDir        string
	UI            *ui.UI // progress output; nil means silent
	PodReadyLimit time.Duration
	PollInterval  time.Duration // readiness polling; default 2s
}

// VolumeCopy describes one persistent volume's data move.
type VolumeCopy struct {
	PVC         string `json:"pvc"`
	SourcePod   string `json:"source_pod,omitempty"`
	Container   string `json:"container,omitempty"`
	MountPath   string `json:"mount_path,omitempty"`
	Bytes       int64  `json:"stream_bytes"`
	StreamSHA   string `json:"stream_sha256,omitempty"`
	SourceFiles int64  `json:"source_files"`
	DestFiles   int64  `json:"dest_files"`
	SourceData  int64  `json:"source_data_bytes"`
	DestData    int64  `json:"dest_data_bytes"`
	Verified    string `json:"verified"` // "match", "changed-during-copy", "not-checked: ..."
	Error       string `json:"error,omitempty"`
}

// Rollout is the readiness of one workload on the destination.
type Rollout struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Note  string `json:"note,omitempty"`
}

// Report is written to report.json and summarised on screen.
type Report struct {
	SourceContext string       `json:"source_context"`
	DestContext   string       `json:"dest_context"`
	Namespace     string       `json:"namespace"`
	ToNamespace   string       `json:"to_namespace"`
	StartedAt     time.Time    `json:"started_at"`
	FinishedAt    time.Time    `json:"finished_at"`
	Objects       []string     `json:"objects"`
	Skipped       []Skipped    `json:"skipped"`
	Warnings      []Warning    `json:"warnings"`
	Volumes       []VolumeCopy `json:"volumes"`
	Rollouts      []Rollout    `json:"rollouts"`
	Result        string       `json:"result"` // "success", "completed-with-issues", "failed"
	Error         string       `json:"error,omitempty"`
}

// Plan is what a migration would do. Building it only reads.
type Plan struct {
	Manifests []Object
	Namespace Object
	Volumes   []VolumeCopy // planned copies (source pod chosen)
	Report    Report
}

func (c *Config) ui() *ui.UI {
	if c.UI == nil {
		c.UI = ui.Plain(io.Discard)
	}
	return c.UI
}

// Build reads the source and destination and produces the plan. Nothing is
// written anywhere.
func Build(ctx context.Context, c *Config) (*Plan, error) {
	c.Source.readOnly = true
	o := &c.Options
	p := &Plan{Report: Report{
		SourceContext: c.Source.Context, DestContext: c.Dest.Context,
		Namespace: o.Namespace, ToNamespace: o.toNS(), StartedAt: time.Now().UTC(),
		Skipped: []Skipped{}, Warnings: []Warning{}, Volumes: []VolumeCopy{}, Rollouts: []Rollout{},
	}}

	if c.Source.Context != "" && c.Source.Context == c.Dest.Context && o.toNS() == o.Namespace {
		return nil, errors.New("source and destination are the same context and namespace")
	}
	for _, k := range []Kubectl{c.Source, c.Dest} {
		if _, err := k.Output(ctx, "version", "-o", "json"); err != nil {
			return nil, fmt.Errorf("cannot reach cluster: %w", err)
		}
	}

	// Destination storage classes, so PVCs land on a class that exists.
	o.DestClasses = map[string]bool{}
	classesListed := false
	if out, err := c.Dest.Output(ctx, "get", "storageclasses", "-o", "json"); err == nil {
		classesListed = true
		var l struct{ Items []Object }
		if json.Unmarshal(out, &l) == nil {
			for _, sc := range l.Items {
				o.DestClasses[nameOf(sc)] = true
				if str(sc, "metadata", "annotations", "storageclass.kubernetes.io/is-default-class") == "true" ||
					str(sc, "metadata", "annotations", "storageclass.beta.kubernetes.io/is-default-class") == "true" {
					o.DefaultClass = nameOf(sc)
				}
			}
		}
	}

	nsOut, err := c.Source.Output(ctx, "get", "namespace", o.Namespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("source namespace %q: %w", o.Namespace, err)
	}
	var nsObj Object
	if err := json.Unmarshal(nsOut, &nsObj); err != nil {
		return nil, err
	}
	p.Namespace = NamespaceObject(nsObj, o.toNS())

	raw, warns, err := Export(ctx, c.Source, o.Namespace)
	if err != nil {
		return nil, err
	}
	p.Report.Warnings = append(p.Report.Warnings, warns...)
	keep, skipped, warns := Clean(raw, *o)
	p.Manifests = keep
	p.Report.Skipped = append(p.Report.Skipped, skipped...)
	p.Report.Warnings = append(p.Report.Warnings, warns...)
	for _, obj := range keep {
		p.Report.Objects = append(p.Report.Objects, kindOf(obj)+"/"+nameOf(obj))
	}

	if classesListed && o.DefaultClass == "" {
		if names := needDefaultClass(keep); len(names) > 0 {
			return nil, fmt.Errorf("the destination has no default storage class, so %s would never be provisioned; pass --storage-class <source>=<destination-class>, or --storage-class default=<destination-class> for claims with no class",
				strings.Join(names, ", "))
		}
	}

	if !c.SkipData {
		vols, w, err := planVolumes(ctx, c.Source, o.Namespace, keep)
		if err != nil {
			return nil, err
		}
		p.Volumes = vols
		p.Report.Warnings = append(p.Report.Warnings, w...)
	}

	// A destination namespace that already enforces "restricted" pod security
	// (labels set by someone else, which apply cannot remove) rejects the
	// restore pod. Say so before anything is written.
	if len(p.Volumes) > 0 {
		if out, err := c.Dest.Output(ctx, "get", "namespace", o.toNS(), "-o", "json", "--ignore-not-found"); err == nil && len(strings.TrimSpace(string(out))) > 0 {
			var dns Object
			if json.Unmarshal(out, &dns) == nil && str(dns, "metadata", "labels", "pod-security.kubernetes.io/enforce") == "restricted" {
				p.Report.Warnings = append(p.Report.Warnings, Warning{Kind: "Namespace", Name: o.toNS(),
					Message: "destination enforces restricted pod security, the restore pod may be rejected; relax it during the copy or use --skip-data"})
			}
		}
	}

	// Refuse to replace anything already on the destination unless asked.
	if !c.Overwrite {
		clash, err := existing(ctx, c.Dest, o.toNS(), keep)
		if err != nil {
			return nil, err
		}
		if len(clash) > 0 {
			return nil, fmt.Errorf("the destination already has %s in namespace %q; pass --overwrite to replace them",
				strings.Join(clash, ", "), o.toNS())
		}
	}
	return p, nil
}

// needDefaultClass lists the claims (and StatefulSet claim templates) that
// rely on the destination's default storage class.
func needDefaultClass(objs []Object) []string {
	var names []string
	for _, obj := range objs {
		switch kindOf(obj) {
		case "PersistentVolumeClaim":
			if str(obj, "spec", "storageClassName") == "" {
				names = append(names, "PVC "+nameOf(obj))
			}
		case "StatefulSet":
			for _, t := range list(obj, "spec", "volumeClaimTemplates") {
				if tm, ok := t.(map[string]any); ok && str(tm, "spec", "storageClassName") == "" {
					names = append(names, "StatefulSet "+nameOf(obj)+" volumes")
					break
				}
			}
		}
	}
	return names
}

// planVolumes picks, for each PVC, a running source pod that mounts it, so its
// data can be streamed out with tar. A PVC with no running consumer is
// reported rather than copied: starting a helper pod would write to the source.
func planVolumes(ctx context.Context, src Kubectl, ns string, manifests []Object) ([]VolumeCopy, []Warning, error) {
	out, err := src.Output(ctx, "get", "pods", "-n", ns, "-o", "json")
	if err != nil {
		return nil, nil, fmt.Errorf("list source pods: %w", err)
	}
	var pods struct{ Items []Object }
	if err := json.Unmarshal(out, &pods); err != nil {
		return nil, nil, err
	}
	var vols []VolumeCopy
	var warns []Warning
	for _, obj := range manifests {
		if kindOf(obj) != "PersistentVolumeClaim" {
			continue
		}
		claim := nameOf(obj)
		v := VolumeCopy{PVC: claim, Verified: "not-checked"}
		if str(obj, "spec", "volumeMode") == "Block" {
			v.Error = "raw block volume, file copy not possible"
			vols = append(vols, v)
			continue
		}
		for _, pod := range pods.Items {
			if str(pod, "status", "phase") != "Running" {
				continue
			}
			podName := nameOf(pod)
			for _, vol := range list(pod, "spec", "volumes") {
				vm, _ := vol.(map[string]any)
				if str(vm, "persistentVolumeClaim", "claimName") != claim {
					continue
				}
				volName, _ := vm["name"].(string)
				for _, c := range list(pod, "spec", "containers") {
					cm, _ := c.(map[string]any)
					for _, mt := range list(cm, "volumeMounts") {
						mm, _ := mt.(map[string]any)
						if mm["name"] == volName && mm["subPath"] == nil && mm["subPathExpr"] == nil && v.SourcePod == "" {
							v.SourcePod, v.Container = podName, cm["name"].(string)
							v.MountPath, _ = mm["mountPath"].(string)
						}
					}
				}
			}
		}
		if v.SourcePod == "" {
			v.Error = "no running pod mounts it"
		}
		vols = append(vols, v)
	}
	return vols, warns, nil
}

func existing(ctx context.Context, dst Kubectl, ns string, manifests []Object) ([]string, error) {
	nsOut, err := dst.Output(ctx, "get", "namespace", ns, "-o", "name", "--ignore-not-found")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(nsOut)) == "" {
		return nil, nil // namespace absent: nothing can clash
	}
	var clash []string
	for _, k := range kinds {
		out, err := dst.Output(ctx, "get", k.Resource, "-n", ns, "-o", "json")
		if err != nil {
			continue
		}
		var l struct{ Items []Object }
		if json.Unmarshal(out, &l) != nil {
			continue
		}
		have := map[string]bool{}
		for _, it := range l.Items {
			have[nameOf(it)] = true
		}
		for _, obj := range manifests {
			if kindOf(obj) == k.Kind && have[nameOf(obj)] {
				clash = append(clash, k.Kind+"/"+nameOf(obj))
			}
		}
	}
	return clash, nil
}

// WriteBundle saves the cleaned manifests and the plan, so the operator can
// review or re-apply them by hand. The directory is owner-only: it holds secrets.
func WriteBundle(dir string, p *Plan) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	items := append([]Object{p.Namespace}, p.Manifests...)
	b, err := json.MarshalIndent(Object{"apiVersion": "v1", "kind": "List", "items": items}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifests.json"), b, 0o600)
}

func writeReport(dir string, r *Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.json"), append(b, '\n'), 0o600)
}

// apply sends a set of objects to the destination with server-side apply.
func (c *Config) apply(ctx context.Context, objs []Object) error {
	if len(objs) == 0 {
		return nil
	}
	b, err := json.Marshal(Object{"apiVersion": "v1", "kind": "List", "items": objs})
	if err != nil {
		return err
	}
	args := []string{"apply", "--server-side", "--field-manager=komolos-agent", "-f", "-"}
	if c.Overwrite {
		args = append(args, "--force-conflicts")
	}
	return c.Dest.Run(ctx, bytes.NewReader(b), io.Discard, args...)
}

// Run executes a built plan: namespace and config first, then volumes and
// their data, then services and workloads, then edge objects; then waits for
// the workloads and writes report.json. The source is only read.
func Run(ctx context.Context, c *Config, p *Plan) (*Report, error) {
	r := &p.Report
	u := c.ui()
	fail := func(l *ui.Live, err error) (*Report, error) {
		l.Done(ui.Fail, firstLine(err.Error()))
		r.Result, r.Error, r.FinishedAt = "failed", err.Error(), time.Now().UTC()
		_ = writeReport(c.OutDir, r)
		return r, err
	}
	byPhase := map[int][]Object{}
	for _, obj := range p.Manifests {
		ph := phaseOf(kindOf(obj))
		byPhase[ph] = append(byPhase[ph], obj)
	}
	count := func(phases ...int) int {
		n := 0
		for _, ph := range phases {
			n += len(byPhase[ph])
		}
		return n
	}

	// Pod-security labels (for example enforce=restricted) would block the
	// temporary restore pod, which must run as root to keep file ownership.
	// The namespace is created without them and finished after the copy.
	l := u.Start("Configuration", "creating namespace "+r.ToNamespace)
	if err := c.apply(ctx, []Object{withoutPodSecurity(p.Namespace)}); err != nil {
		return fail(l, err)
	}
	for _, ph := range []int{phaseIdentity, phaseConfig, phaseStorage} {
		if err := c.apply(ctx, byPhase[ph]); err != nil {
			return fail(l, err)
		}
	}
	l.Done(ui.OK, plural(count(phaseIdentity, phaseConfig, phaseStorage), "object"))

	if !c.SkipData {
		for i := range p.Volumes {
			v := &p.Volumes[i]
			if v.SourcePod == "" {
				u.Row(ui.Warn, v.PVC, "not copied, "+v.Error)
				continue
			}
			l := u.Start(v.PVC, "preparing destination volume")
			if err := c.copyVolume(ctx, v, l); err != nil {
				v.Error = err.Error()
				l.Done(ui.Fail, firstLine(err.Error()))
				continue
			}
			summary := plural(int(v.DestFiles), "file") + "  ·  " + ui.Bytes(v.DestData)
			switch v.Verified {
			case "match":
				l.Done(ui.OK, summary+"  ·  verified")
			case "changed-during-copy":
				l.Done(ui.Warn, summary+"  ·  changed during copy")
			default:
				l.Done(ui.Warn, ui.Bytes(v.Bytes)+" copied  ·  not verified")
			}
		}
		r.Volumes = p.Volumes
	}

	l = u.Start("Workloads", "starting")
	if err := c.apply(ctx, []Object{p.Namespace}); err != nil { // restore pod-security labels
		return fail(l, err)
	}
	for _, ph := range []int{phaseNetwork, phaseWorkloads, phaseEdge} {
		if err := c.apply(ctx, byPhase[ph]); err != nil {
			return fail(l, err)
		}
	}
	l.Done(ui.OK, plural(count(phaseNetwork, phaseWorkloads, phaseEdge), "object"))

	for _, obj := range byPhase[phaseWorkloads] {
		k, name := kindOf(obj), nameOf(obj)
		if k == "CronJob" {
			note := "scheduled"
			if str(obj, "metadata", "annotations", suspendedAnnotation) == "true" {
				note = "suspended until cutover"
			} else if m(obj, "spec")["suspend"] == true {
				note = "suspended, as on the source"
			}
			r.Rollouts = append(r.Rollouts, Rollout{Kind: k, Name: name, Ready: true, Note: note})
			u.Row(ui.OK, name, note)
			continue
		}
		ro := Rollout{Kind: k, Name: name}
		l := u.Start(name, "waiting")
		ready, note, err := c.waitReady(ctx, k, name, r.ToNamespace, l)
		switch {
		case err != nil:
			ro.Note = firstLine(err.Error())
			l.Done(ui.Warn, "could not check: "+ro.Note)
		case ready:
			ro.Ready, ro.Note = true, note
			if note == "" {
				note = "ready"
			}
			l.Done(ui.OK, note)
		default:
			ro.Note = "not ready after " + ui.Duration(c.Wait) + " (" + note + ")"
			l.Done(ui.Warn, ro.Note)
		}
		r.Rollouts = append(r.Rollouts, ro)
	}

	r.Result = "success"
	for _, v := range r.Volumes {
		if v.Error != "" || v.Verified != "match" {
			r.Result = "completed-with-issues"
		}
	}
	for _, ro := range r.Rollouts {
		if !ro.Ready {
			r.Result = "completed-with-issues"
		}
	}
	r.FinishedAt = time.Now().UTC()
	return r, writeReport(c.OutDir, r)
}

// readiness reports whether a workload on the destination is fully up, and a
// short progress note. It reads status fields directly, so it works for every
// update strategy (including StatefulSet OnDelete, where "kubectl rollout
// status" refuses to answer).
func readiness(obj Object) (ready bool, note string) {
	num := func(path ...string) int64 {
		parent := m(obj, path[:len(path)-1]...)
		if parent == nil {
			return 0
		}
		switch v := parent[path[len(path)-1]].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		}
		return 0
	}
	gen, observed := num("metadata", "generation"), num("status", "observedGeneration")
	if observed < gen {
		return false, "waiting for the controller"
	}
	switch kindOf(obj) {
	case "Deployment", "StatefulSet":
		want := int64(1)
		if spec := m(obj, "spec"); spec != nil {
			if _, set := spec["replicas"]; set {
				want = num("spec", "replicas")
			}
		}
		if want == 0 {
			return true, "scaled to 0"
		}
		readyN := num("status", "readyReplicas")
		if kindOf(obj) == "Deployment" {
			readyN = min(num("status", "availableReplicas"), num("status", "updatedReplicas"))
		}
		return readyN >= want, fmt.Sprintf("%d of %d replicas ready", readyN, want)
	case "DaemonSet":
		want, readyN := num("status", "desiredNumberScheduled"), num("status", "numberReady")
		if want == 0 {
			return true, "no matching nodes"
		}
		return readyN >= want, fmt.Sprintf("%d of %d nodes ready", readyN, want)
	}
	return true, ""
}

// waitReady polls a workload until it is ready or c.Wait passes.
func (c *Config) waitReady(ctx context.Context, k, name, ns string, l *ui.Live) (bool, string, error) {
	res := map[string]string{"Deployment": "deployments.apps", "StatefulSet": "statefulsets.apps", "DaemonSet": "daemonsets.apps"}[k]
	deadline := time.Now().Add(c.Wait)
	interval := c.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	var note string
	failures := 0
	for {
		out, err := c.Dest.Output(ctx, "get", res, name, "-n", ns, "-o", "json")
		var obj Object
		if err == nil {
			err = json.Unmarshal(out, &obj)
		}
		if err != nil {
			// A token refresh or API blip should not end the wait.
			if failures++; failures >= 3 || time.Now().After(deadline) {
				return false, note, err
			}
			select {
			case <-ctx.Done():
				return false, note, ctx.Err()
			case <-time.After(interval):
			}
			continue
		}
		failures = 0
		if obj["kind"] == nil {
			obj["kind"] = k
		}
		var ready bool
		ready, note = readiness(obj)
		if ready {
			return true, note, nil
		}
		l.Set(note)
		if time.Now().After(deadline) {
			return false, note, nil
		}
		select {
		case <-ctx.Done():
			return false, note, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// withoutPodSecurity returns the namespace without pod-security.kubernetes.io
// labels, for the phase where the root restore pod has to run.
func withoutPodSecurity(ns Object) Object {
	labels := map[string]any{}
	for k, v := range m(ns, "metadata", "labels") {
		if !strings.HasPrefix(k, "pod-security.kubernetes.io/") {
			labels[k] = v
		}
	}
	return Object{"apiVersion": "v1", "kind": "Namespace", "metadata": Object{"name": nameOf(ns), "labels": labels}}
}

func helperName(pvc string) string {
	s := sha256.Sum256([]byte(pvc))
	name := "komolos-restore-" + strings.ToLower(pvc)
	if len(name) > 50 {
		name = name[:50]
	}
	return strings.TrimRight(name, "-.") + "-" + hex.EncodeToString(s[:3])
}

// copyVolume starts a temporary pod on the destination that mounts the new
// PVC, then streams "tar cf -" from the source pod into "tar xf -" in it.
// The temporary pod is always deleted afterwards.
func (c *Config) copyVolume(ctx context.Context, v *VolumeCopy, live *ui.Live) error {
	ns := c.Options.toNS()
	helper := helperName(v.PVC)
	pod := Object{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": Object{"name": helper, "namespace": ns, "labels": Object{"app.kubernetes.io/managed-by": "komolos-agent", "komolos.io/role": "restore"}},
		"spec": Object{
			"restartPolicy":                 "Never",
			"terminationGracePeriodSeconds": 0,
			"containers": []any{Object{
				"name": "restore", "image": c.HelperImage,
				"command":      []any{"sh", "-c", "sleep 3600"},
				"volumeMounts": []any{Object{"name": "data", "mountPath": "/data"}},
				// Requests and limits keep it admissible under a namespace ResourceQuota.
				"resources": Object{
					"requests": Object{"cpu": "10m", "memory": "32Mi"},
					"limits":   Object{"cpu": "500m", "memory": "256Mi"},
				},
			}},
			"volumes": []any{Object{"name": "data", "persistentVolumeClaim": Object{"claimName": v.PVC}}},
		},
	}
	if err := c.apply(ctx, []Object{pod}); err != nil {
		return fmt.Errorf("start restore pod: %w", err)
	}
	defer func() {
		// Wait for it to go, so a ReadWriteOnce volume is free for the workload.
		_ = c.Dest.Run(context.Background(), nil, io.Discard, "delete", "pod", helper, "-n", ns, "--ignore-not-found", "--timeout=90s")
	}()
	limit := c.PodReadyLimit
	if limit == 0 {
		limit = 3 * time.Minute
	}
	live.Set("starting restore pod")
	err := c.Dest.Run(ctx, nil, io.Discard, "wait", "--for=condition=Ready", "pod/"+helper, "-n", ns, "--timeout", limit.String())
	if err != nil {
		return fmt.Errorf("restore pod did not start (image %s must be pullable on the destination): %w", c.HelperImage, err)
	}

	// Stream source tar into destination tar, hashing and counting on the way.
	pr, pw := io.Pipe()
	h := sha256.New()
	counter := &byteCounter{}
	srcErr := make(chan error, 1)
	go func() {
		err := c.Source.Run(ctx, nil, io.MultiWriter(pw, h, counter, live),
			"exec", v.SourcePod, "-n", c.Options.Namespace, "-c", v.Container, "--", "tar", "cf", "-", "-C", v.MountPath, ".")
		pw.CloseWithError(err)
		srcErr <- err
	}()
	dstErr := c.Dest.Run(ctx, pr, io.Discard, "exec", "-i", helper, "-n", ns, "--", "tar", "xf", "-", "-C", "/data")
	pr.CloseWithError(io.ErrClosedPipe)
	if err := <-srcErr; err != nil {
		return fmt.Errorf("read source volume (the container needs tar): %w", err)
	}
	if dstErr != nil {
		return fmt.Errorf("write destination volume: %w", dstErr)
	}
	v.Bytes, v.StreamSHA = counter.n, hex.EncodeToString(h.Sum(nil))
	live.Phase("verifying")

	// Verify by file count and total bytes on both sides.
	sf, sb, err1 := countFiles(ctx, c.Source, v.SourcePod, c.Options.Namespace, v.Container, v.MountPath)
	df, db, err2 := countFiles(ctx, c.Dest, helper, ns, "restore", "/data")
	v.SourceFiles, v.SourceData, v.DestFiles, v.DestData = sf, sb, df, db
	switch {
	case err1 != nil || err2 != nil:
		v.Verified = "not-checked: find/wc unavailable in a container"
	case sf == df && sb == db:
		v.Verified = "match"
	default:
		v.Verified = "changed-during-copy" // the source app kept writing; rerun or quiesce it
	}
	return nil
}

type byteCounter struct{ n int64 }

func (b *byteCounter) Write(p []byte) (int, error) { b.n += int64(len(p)); return len(p), nil }

func countFiles(ctx context.Context, k Kubectl, pod, ns, container, path string) (files, bytes int64, err error) {
	out, err := k.Output(ctx, "exec", pod, "-n", ns, "-c", container, "--", "sh", "-c", verifyScript, "sh", path)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return 0, 0, errors.New("unexpected verify output")
	}
	files, err1 := strconv.ParseInt(f[0], 10, 64)
	bytes, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, errors.New("unexpected verify output")
	}
	return files, bytes, nil
}

// short names for kinds in on-screen lists.
var shortKind = map[string]string{
	"PersistentVolumeClaim": "PVC", "HorizontalPodAutoscaler": "HPA", "PodDisruptionBudget": "PDB",
	"ServiceAccount": "ServiceAccount", "NetworkPolicy": "NetworkPolicy",
}

func short(k string) string {
	if s, ok := shortKind[k]; ok {
		return s
	}
	return k
}

// Summary renders the plan: what will be created, copied and left out, and
// the few things to check. Full detail is in the report.
func (p *Plan) Summary(u *ui.UI) {
	r := p.Report
	counts := map[string]int{}
	for _, o := range p.Manifests {
		counts[kindOf(o)]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return phaseOf(keys[i]) < phaseOf(keys[j]) || (phaseOf(keys[i]) == phaseOf(keys[j]) && keys[i] < keys[j])
	})
	var kinds []string
	for _, k := range keys {
		label := short(k)
		if counts[k] > 1 {
			label = fmt.Sprintf("%s ×%d", label, counts[k])
		}
		kinds = append(kinds, label)
	}

	u.Section("Plan")
	u.Row(ui.Item, "Create", plural(len(p.Manifests), "object"))
	if len(kinds) > 0 {
		u.Line("", u.Muted(strings.Join(kinds, ", ")))
	}
	var copyNames, missing, reasons []string
	for _, v := range p.Volumes {
		if v.SourcePod != "" {
			copyNames = append(copyNames, v.PVC)
		} else {
			missing = append(missing, v.PVC)
			reasons = append(reasons, v.Error)
		}
	}
	if len(copyNames) > 0 {
		u.Row(ui.Item, "Copy data", strings.Join(copyNames, ", "))
	}

	if len(missing)+len(r.Warnings) > 0 {
		u.Blank()
	}
	for i, name := range missing {
		why := reasons[i]
		if why == "" {
			why = "no running pod mounts it"
		}
		u.Row(ui.Warn, name, why+", data not copied")
	}
	for _, w := range r.Warnings {
		label := w.Name
		if label == "" {
			label = short(w.Kind)
		}
		u.Row(ui.Warn, label, w.Message)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
