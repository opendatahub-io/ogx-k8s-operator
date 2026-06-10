#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

COMPONENT_NAME="ogx"
SOURCE_PATH="${SOURCE_PATH:-config}"
DST_MANIFESTS_DIR="${PROJECT_ROOT}/config/manifests/${COMPONENT_NAME}"

if [[ "${ODH_PLATFORM_TYPE:-OpenDataHub}" == "OpenDataHub" ]]; then
  PLATFORM_LABEL="ODH"
  REPO_URL="${OGX_OPERATOR_REPO_URL:-https://github.com/opendatahub-io/ogx-k8s-operator}"
  REPO_REF="${OGX_OPERATOR_REF:-${OGX_OPERATOR_ODH_REF:-odh}}"
else
  PLATFORM_LABEL="RHOAI"
  REPO_URL="${OGX_OPERATOR_REPO_URL:-https://github.com/red-hat-data-services/ogx-k8s-operator}"
  REPO_REF="${OGX_OPERATOR_REF:-${OGX_OPERATOR_RHOAI_REF:-rhoai-3.5-ea.2}}"
fi

# In this repository layout the module subtree lives alongside the root OGX
# operator sources, so local development can stage manifests directly from the
# current checkout instead of a sibling clone.
if [[ "${USE_LOCAL:-}" == "true" ]] && [[ -d "${PROJECT_ROOT}/../config" ]]; then
  echo "Copying manifests from local ogx-k8s-operator checkout"
  rm -rf "${DST_MANIFESTS_DIR}"
  mkdir -p "${DST_MANIFESTS_DIR}"
  cp -a "${PROJECT_ROOT}/../${SOURCE_PATH}/." "${DST_MANIFESTS_DIR}/"
  echo "Manifests copied to ${DST_MANIFESTS_DIR}"
  exit 0
fi

try_fetch_ref() {
  local repo="$1"
  local ref_type="$2"
  local ref="$3"
  local git_ref="refs/${ref_type}/${ref}"

  if git ls-remote --exit-code "${repo}" "${git_ref}" &>/dev/null; then
    if git fetch -q --depth 1 "${repo}" "${git_ref}" && git reset -q --hard FETCH_HEAD; then
      return 0
    fi
  fi

  return 1
}

git_fetch_ref() {
  local repo="$1"
  local ref="$2"
  local dir="$3"

  mkdir -p "${dir}"
  pushd "${dir}" >/dev/null
  git init -q

  # Support branch@sha in the same way the older ODH get_all_manifests flow does.
  if [[ "${ref}" =~ ^([a-zA-Z0-9_./-]+)@([a-f0-9]{7,40})$ ]]; then
    local commit_sha="${BASH_REMATCH[2]}"
    git remote add origin "${repo}"
    git fetch --depth 1 -q origin "${commit_sha}"
    git reset -q --hard "${commit_sha}"
    popd >/dev/null
    return 0
  fi

  if try_fetch_ref "${repo}" "tags" "${ref}" || try_fetch_ref "${repo}" "heads" "${ref}"; then
    popd >/dev/null
    return 0
  fi

  popd >/dev/null
  return 1
}

TMP_DIR="$(mktemp -d -t "odh-ogx-manifests.XXXXXXXXXX")"
trap 'rm -rf -- "${TMP_DIR}"' EXIT

REPO_DIR="${TMP_DIR}/repo"
echo "Downloading manifests for ${PLATFORM_LABEL} from ${REPO_URL}@${REPO_REF}"
git_fetch_ref "${REPO_URL}" "${REPO_REF}" "${REPO_DIR}"

rm -rf "${DST_MANIFESTS_DIR}"
mkdir -p "${DST_MANIFESTS_DIR}"
cp -a "${REPO_DIR}/${SOURCE_PATH}/." "${DST_MANIFESTS_DIR}/"

echo "Manifests downloaded to ${DST_MANIFESTS_DIR}"
