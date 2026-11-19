# Demo: migrate a Kubernetes app to another cluster

This folder runs a complete `komolos-agent --migrate k8s` on your laptop, using
two local clusters: `source` stands in for a customer's existing cluster (for
example on EKS) and `lagos` for the onshore destination. A sample store
(`app.yaml`) with its configuration, a secret, a service and a persistent volume
holding its website and order records moves from one to the other.

komolos-agent only talks to clusters through kubectl contexts, so the same
command works with a real EKS source:

```sh
aws eks update-kubeconfig --name prod --region eu-west-1 --alias eks-prod
komolos-agent --migrate k8s --from eks-prod --to lagos -n shop
```

## One-time setup on Windows

1. Install the tools (PowerShell, then restart the terminal):

   ```powershell
   winget install Docker.DockerDesktop Kubernetes.kind Kubernetes.kubectl
   ```

2. Start Docker Desktop and wait until it says it is running.
3. In this folder:

   ```powershell
   Set-ExecutionPolicy -Scope Process Bypass   # this window only
   .\demo.ps1 setup
   ```

   It installs komolos-agent exactly as a customer would, from the signed
   GitHub release (`irm .../install.ps1 | iex`: download, signature and
   checksum check, install to your user folder). Then it creates both clusters,
   loads the two images into them (so the demo does not need the venue's
   internet), deploys the store and puts its content and order records on the
   volume. Takes a few minutes the first time.

   No release published yet, or no internet? Add `-LocalAgent` to use a
   `komolos-agent.exe` built in the `komolos-agent` folder
   (`go build -o komolos-agent.exe .`).

macOS or Linux: use `./demo.sh` with the same steps.

## Rehearse, then present

```powershell
.\demo.ps1 run      # the live demo, pauses between steps
.\demo.ps1 reset    # removes the copy from lagos so you can run it again
```

Run it at least twice before the meeting. `.\demo.ps1 cleanup` deletes both
clusters when you are done for good.

### Optional opening: install it live

`.\demo.ps1 install` runs the public installer on screen: the KOMOLOS banner,
the download from GitHub, the checksum (and signature, if cosign is installed)
and the installed version. *"This is all a customer's admin runs. One signed
binary, nothing else installed, no agent left running."* Needs internet.

### The four steps and what to say

1. **The store on the source cluster.** The browser opens
   `http://localhost:8081`. *"This is a customer's app running on their current
   cloud. The orders at the bottom live on a persistent disk."*
2. **The plan.** *"Before anything moves, komolos-agent shows exactly what it
   will create and what it deliberately leaves out, and flags what the operator
   must check, such as images in Amazon ECR or AWS-specific storage. It only
   reads both clusters."*
3. **The migration.** Config, secrets, the volume's data (streamed and checked
   by file count and size) and the workload are created on `lagos`, in order,
   and it waits until the app is ready. *"One command. The source is never
   modified, so the customer can switch DNS when they are ready and roll back
   by doing nothing."*
4. **The same store, served from Lagos.** The browser opens
   `http://localhost:8082` with the same orders, still saying they were recorded
   on the source. The source is shown still running.

## If something goes wrong on stage

- **Port-forward errors in step 1:** the source app is not ready yet. Run
  `kubectl --context kind-source -n shop get pods` and wait for `Running`.
- **Step 3 exits with code 3:** the migration finished but something needs a
  look; `last-migration\report.json` says what. The usual cause is the
  destination pod still pulling an image: run `.\demo.ps1 setup` again (it
  reloads the images), then `.\demo.ps1 reset` and `run`.
- **"already has ... pass --overwrite":** you ran it before without a reset.
  Run `.\demo.ps1 reset`.

## What this demo does not show

- Live databases: the volume is copied while the app runs, so files are
  crash-consistent, not transactionally consistent. For a database, dump and
  restore it, or stop writes before copying.
- Cutover: DNS or load-balancer changes are left to the operator.
- Images in private registries: the destination must be able to pull them;
  the plan warns about ECR images.

`last-migration\` holds `manifests.json` (including the Secrets) and
`report.json`. It is ignored by git; delete it after the demo.
