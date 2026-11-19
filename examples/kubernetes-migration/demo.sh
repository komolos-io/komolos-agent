#!/usr/bin/env bash
# komolos-agent migration demo (macOS, Linux).
#
#   ./demo.sh install   install komolos-agent the way a customer does (signed GitHub release)
#   ./demo.sh setup     before the meeting: installs the agent, two local clusters, images, sample app
#   ./demo.sh run       in the meeting: show the app, plan, migrate, show it again
#   ./demo.sh reset     remove the migrated copy so you can run again
#   ./demo.sh cleanup   delete both clusters
#
# Needs Docker (running), kind and kubectl. "source" stands in for a customer's
# existing cluster (for example EKS); "lagos" for the onshore destination.
# Set LOCAL_AGENT=1 to use a komolos-agent you built yourself instead of the
# published release.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
src=kind-source
dst=kind-lagos
images=(nginx:1.27-alpine busybox:1.36)
fwds=()

installer=https://raw.githubusercontent.com/komolos-io/komolos-agent/main/install.sh
say() { printf '\n\033[38;5;37;1m▸ %s\033[0m\n' "$*"; }
install_agent() {
	say "Installing komolos-agent from its signed GitHub release, as a customer would"
	printf '  \033[2mcurl -fsSL %s | sh\033[0m\n' "$installer"
	curl -fsSL "$installer" | sh
	export PATH="$HOME/.local/bin:$PATH"
}
pause() { printf '\n'; read -r -p "${1:-Press Enter to continue}" _; }
need() { command -v "$1" >/dev/null || { echo "$1 not found. $2" >&2; exit 1; }; }
cleanup_forwards() { for p in "${fwds[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; }
trap cleanup_forwards EXIT

find_agent() {
	if [ -n "${LOCAL_AGENT:-}" ]; then
		for p in "$here/../../komolos-agent" "$here/komolos-agent"; do
			[ -x "$p" ] && { echo "$p"; return; }
		done
		echo "No local komolos-agent. Build it in the komolos-agent folder: go build ." >&2
		exit 1
	fi
	PATH="$HOME/.local/bin:$PATH" command -v komolos-agent && return
	echo "komolos-agent is not installed. Run ./demo.sh install (or set LOCAL_AGENT=1)." >&2
	exit 1
}

show_site() { # context port label
	kubectl --context "$1" -n shop port-forward svc/web "$2:80" >/dev/null 2>&1 &
	fwds+=($!)
	sleep 3
	if out=$(curl -fsS -D /tmp/komolos-demo-headers "http://localhost:$2/orders.txt" 2>/dev/null); then
		printf '\n\033[32m%s  http://localhost:%s\033[0m\n' "$3" "$2"
		grep -i '^x-served-by' /tmp/komolos-demo-headers || true
		echo "$out"
		if command -v open >/dev/null; then open "http://localhost:$2"; elif command -v xdg-open >/dev/null; then xdg-open "http://localhost:$2" >/dev/null 2>&1 || true; fi
	else
		echo "Could not reach the site on $1" >&2
	fi
}

case "${1:-run}" in
install) install_agent ;;
setup)
	[ -n "${LOCAL_AGENT:-}" ] || install_agent
	need docker "Install Docker Desktop (or Docker Engine) and start it."
	need kind "https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
	need kubectl "https://kubernetes.io/docs/tasks/tools/"
	docker info >/dev/null 2>&1 || { echo "Docker is not running." >&2; exit 1; }

	say "Creating two local Kubernetes clusters: source and lagos (about 2 minutes)"
	for name in source lagos; do
		if kind get clusters 2>/dev/null | grep -qx "$name"; then echo "cluster $name already exists"
		else kind create cluster --name "$name" --wait 180s; fi
	done

	say "Loading images into both clusters, so the demo needs no internet"
	case "$(uname -m)" in arm64 | aarch64) platform=linux/arm64 ;; *) platform=linux/amd64 ;; esac
	for img in "${images[@]}"; do
		# One platform only: kind cannot load multi-platform images from Docker Desktop's containerd store.
		docker pull --platform "$platform" "$img"
		for name in source lagos; do
			kind load docker-image "$img" --name "$name" ||
				echo "Could not preload $img into $name; it will be downloaded instead (needs internet)." >&2
		done
	done

	say "Deploying the sample store to the source cluster"
	kubectl --context "$src" apply -f "$here/app.yaml"
	kubectl --context "$src" -n shop rollout status deploy/web --timeout=180s
	pod=$(kubectl --context "$src" -n shop get pods -l app=web -o jsonpath='{.items[0].metadata.name}')

	say "Putting the store's content and some order records on its volume"
	tar -cf - -C "$here/site" . | kubectl --context "$src" -n shop exec -i "$pod" -- tar xf - -C /usr/share/nginx/html
	stamp=$(date '+%Y-%m-%d %H:%M:%S')
	printf 'Orders recorded on the SOURCE cluster at %s\nORD-1001  Indigo adire x2         N37,000  paid\nORD-1002  Kampala wax print x1    N32,000  paid\nORD-1003  Aso-oke gele x3          N72,000  pending\n' "$stamp" |
		kubectl --context "$src" -n shop exec -i "$pod" -- sh -c 'cat > /usr/share/nginx/html/orders.txt'

	say "Ready. Rehearse with: ./demo.sh run   (then ./demo.sh reset to run it again)"
	;;
run)
	agent=$(find_agent)
	for c in "$src" "$dst"; do
		kubectl config get-contexts "$c" >/dev/null 2>&1 || { echo "context $c not found. Run ./demo.sh setup first." >&2; exit 1; }
	done

	say "1. A store running on the customer's current cluster (source)"
	show_site "$src" 8081 SOURCE
	pause

	say "2. What komolos-agent would move. Read-only on both clusters; nothing is written."
	"$agent" --plan k8s --from "$src" --to "$dst" -n shop
	pause

	say "3. Migrate: config, secrets, the volume's data and the workload, onto the Lagos cluster"
	set +e
	KOMOLOS_NO_BANNER=1 "$agent" --migrate k8s --from "$src" --to "$dst" -n shop --yes --wait 3m -o "$here/last-migration"
	code=$?
	set -e
	[ "$code" -eq 0 ] || echo "komolos-agent exited with $code; see last-migration/report.json" >&2
	pause

	say "4. The same store, now served from Lagos, with the same data"
	show_site "$dst" 8082 LAGOS
	printf '\n\033[32mThe source cluster is untouched:\033[0m\n'
	kubectl --context "$src" -n shop get deploy,pvc
	pause "Press Enter to finish (stops the port-forwards)"
	;;
reset)
	say "Removing the migrated copy from lagos"
	kubectl --context "$dst" delete namespace shop --ignore-not-found --wait=true
	echo "Done. Run ./demo.sh run again."
	;;
cleanup)
	say "Deleting both demo clusters"
	kind delete cluster --name source
	kind delete cluster --name lagos
	;;
*)
	echo "usage: $0 install|setup|run|reset|cleanup" >&2
	exit 2
	;;
esac
