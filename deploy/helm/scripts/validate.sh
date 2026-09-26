#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
trap cleanup_charts EXIT
progress 'Chart validation: preparing dependencies and configuration'
prepare_charts platform
for K8S_ENV in dev test qa; do
  NAMESPACE="go-chat-${K8S_ENV}"
  for group in infra apps observability migrations load; do
    progress "${K8S_ENV}/${group}: linting and rendering chart"
    values_args "${group}"
    "${HELM}" lint "${STAGE}/${group}" --strict "${VALUES[@]}"
    render_group "${group}" > /dev/null
  done
done
progress 'Platform: linting and rendering chart'
"${HELM}" lint "${STAGE}/platform" --strict -f "${HELM_DIR}/values/common/platform.yaml"
"${HELM}" template platform "${STAGE}/platform" -n gochat-system -f "${HELM_DIR}/values/common/platform.yaml" >/dev/null
progress 'Chart validation complete (dev, test, qa and platform)'
