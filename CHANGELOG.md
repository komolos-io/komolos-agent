# Changelog

## Unreleased

- `--migrate k8s` moves a Kubernetes namespace (configuration, secrets,
  services, workloads and volume data) to another cluster through the user's
  own kubectl. The source is only read, enforced by a guard and a test.
  `--plan k8s` shows what would move without writing anything.
- Hardened for real clusters: ResourceQuotas and LimitRanges move; CronJobs
  arrive suspended (`--keep-cronjobs-active` to opt out); `--ingress-class`
  maps ingress classes; dual-stack fields and load balancer classes are
  dropped from Services, with warnings for AWS load balancer and ALB
  annotations; pod-security labels are applied after the data copy and the
  restore pod has resource requests, so restricted or quota'd namespaces work;
  raw block volumes are reported instead of failing; a missing default storage
  class is refused up front; readiness is polled per workload instead of
  `kubectl rollout status`, handling scaled-to-0, StatefulSets with OnDelete
  and DaemonSets with no matching nodes.
- `examples/kubernetes-migration`: a laptop demo with two local clusters.
- Redesigned terminal output: KOMOLOS banner, a short header, ruled sections
  and one aligned row per step, with a live line (spinner, bytes and rate,
  replicas ready) that settles into a status mark. Detail goes to the report.
  Pipes, CI logs, `NO_COLOR` and `TERM=dumb` get plain text;
  `KOMOLOS_NO_BANNER=1` hides the logo.
- Installers: matching banner and step output; `install.ps1` now verifies the
  cosign signature when cosign is installed, like `install.sh`; both run
  `komolos-agent --version` correctly after installing.

- First public version. `komolos-agent --collect` replaces the earlier
  `komolos-collect` command, which was never released.
