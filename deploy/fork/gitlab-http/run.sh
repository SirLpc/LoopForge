#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' 'Usage: bash deploy/fork/gitlab-http/run.sh --check | --deploy | --deploy-wsl | --exec COMMAND [ARG ...]'
}

mode="${1:---check}"
case "$mode" in
  --help|-h) usage; exit 0 ;;
  --check|--deploy|--deploy-wsl) (( $# <= 1 )) || { usage >&2; exit 2; } ;;
  --exec) (( $# >= 2 )) || { usage >&2; exit 2; }; shift ;;
  *) usage >&2; exit 2 ;;
esac

patch_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(git -C "$patch_dir" rev-parse --show-toplevel)"
revision="$(git -C "$repo_root" rev-parse HEAD)"
patch_file="$patch_dir/gitlab-http.patch"

if [[ "$mode" == --deploy || "$mode" == --deploy-wsl ]]; then
  if [[ ! -r "$repo_root/.env" ]] || ! (
    source "$repo_root/.env"
    [[ "${LOOPFORGE_DATA_DIR:-}" = /* ]]
  ); then
    printf '%s\n' 'This deployment uses an existing .env with an absolute LOOPFORGE_DATA_DIR.' >&2
    exit 1
  fi
fi

staging="$(mktemp -d "${TMPDIR:-/tmp}/loopforge-gitlab-http.XXXXXX")"
trap 'rm -rf -- "$staging"' EXIT
git -C "$repo_root" archive "$revision" | tar -xf - -C "$staging"
cd "$staging"
export GIT_CEILING_DIRECTORIES="$(dirname -- "$staging")"
if ! git apply --check --whitespace=error "$patch_file"; then
  printf 'GitLab HTTP patch needs updating for commit %s.\n' "$revision" >&2
  exit 1
fi

if [[ "$mode" == --check ]]; then
  printf 'GitLab HTTP patch applies to %s.\n' "$revision"
  exit 0
fi

git apply --whitespace=error "$patch_file"
if [[ "$mode" == --exec ]]; then
  "$@"
else
  ln -s -- "$repo_root/.env" .env
  export LOOPFORGE_COMMIT="$revision"
  if [[ "$mode" == --deploy-wsl ]]; then
    bash scripts/redeploy-wsl.sh
  else
    bash start.sh dev
  fi
fi
