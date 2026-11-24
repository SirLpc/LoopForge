#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

usage() {
  echo "Usage: bash scripts/redeploy-wsl.sh [--check]"
  echo "Before deploying, pause the affected warm slots and let active workspaces return."
}

case "${1:-}" in
  '') ;;
  --check) ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac
if (( $# > 1 )); then usage >&2; exit 2; fi

fail() {
  printf '%s\n' "$*" >&2
  exit 1
}

runner_unit="${LOOPFORGE_RUNNER_UNIT:-loopforge-native-runner.service}"
runner_binary="${LOOPFORGE_RUNNER_BINARY:-$HOME/.local/share/loopforge-native/agentflow-runner}"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail "Unsupported WSL architecture: $(uname -m)" ;;
esac

[[ -r .env ]] || fail "Run from the existing LoopForge Compose installation with a readable .env."
[[ -f "$runner_binary" && -x "$runner_binary" ]] ||
  fail "Runner binary is missing or not executable: $runner_binary"
runner_binary="$(realpath -- "$runner_binary")"
runner_dir="$(dirname "$runner_binary")"
[[ -w "$runner_dir" ]] || fail "Runner directory is not writable: $runner_dir"
[[ "$(systemctl --user show --property=LoadState --value "$runner_unit")" == loaded ]] ||
  fail "Runner user service is unavailable: $runner_unit"

source scripts/docker-access.sh
select_docker_access || fail "Cannot access Docker from this WSL distribution."
compose=(compose --profile platform)
platform_id="$(docker_access "${compose[@]}" ps -a -q platform)"
[[ -n "$platform_id" ]] || fail "No platform container exists for this Compose installation."
if [[ "${1:-}" == --check ]]; then
  printf 'Preflight passed for the Compose platform and %s.\n' "$runner_unit"
  exit 0
fi

exec 9>"$runner_dir/.redeploy.lock"
flock -n 9 || fail "Another deployment is already running."
staging="$(mktemp -d "$runner_dir/.redeploy.XXXXXX")"
trap 'rm -rf -- "$staging"' EXIT
trap 'printf "Deployment stopped while %s. Review the output above before retrying.\n" "$step" >&2' ERR

step="building and starting the platform"
echo "Building and starting the platform from the current checkout..."
bash ./start.sh dev

step="extracting the matching Runner"
echo "Extracting the Runner from the updated platform..."
docker_access "${compose[@]}" exec -T platform cat "/app/bin/agentflow-runner-linux-$arch" > "$staging/runner"
[[ -s "$staging/runner" ]] || fail "The updated platform returned an empty Runner binary."
if ! cmp -s "$staging/runner" "$runner_binary"; then
  chmod 0755 "$staging/runner"
  cp -p -- "$runner_binary" "$staging/previous"
  mv -fT -- "$staging/previous" "$runner_binary.previous"
  mv -fT -- "$staging/runner" "$runner_binary"
fi

step="restarting $runner_unit"
echo "Restarting $runner_unit..."
systemctl --user restart "$runner_unit"
systemctl --user is-active --quiet "$runner_unit"
echo "Platform health check passed and the Runner service is active."
echo "Verify the Runner is online in LoopForge and test one slot before resuming the others."
