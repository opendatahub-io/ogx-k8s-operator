#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

COMPONENT_NAME="ogx"
SOURCE_PATH="config"
DST_MANIFESTS_DIR="${PROJECT_ROOT}/config/manifests/${COMPONENT_NAME}"

if [[ "${ODH_PLATFORM_TYPE:-OpenDataHub}" == "OpenDataHub" ]]; then
  echo "Downloading manifests for ODH"
  REPO_URL="https://github.com/opendatahub-io/ogx-k8s-operator"
  COMMIT_SHA="54ce7ea2e3501040c33c1d1b5ab9a69ef51ceadf"
else
  echo "Downloading manifests for RHOAI"
  REPO_URL="https://github.com/red-hat-data-services/ogx-k8s-operator"
  COMMIT_SHA="b86a2f4db30306aad25ce539ff16fb5a14e1fb6e"
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

TMP_DIR="$(mktemp -d -t "odh-ogx-manifests.XXXXXXXXXX")"
trap 'rm -rf -- "${TMP_DIR}"' EXIT

git -C "${TMP_DIR}" init -q
git -C "${TMP_DIR}" remote add origin "${REPO_URL}"
git -C "${TMP_DIR}" fetch --depth 1 -q origin "${COMMIT_SHA}"
git -C "${TMP_DIR}" reset -q --hard "${COMMIT_SHA}"

rm -rf "${DST_MANIFESTS_DIR}"
mkdir -p "${DST_MANIFESTS_DIR}"
cp -a "${TMP_DIR}/${SOURCE_PATH}/." "${DST_MANIFESTS_DIR}/"

echo "Manifests downloaded to ${DST_MANIFESTS_DIR}"
