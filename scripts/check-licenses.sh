#!/usr/bin/env sh
set -eu

GO_LICENSES_VERSION="${GO_LICENSES_VERSION:-v2.0.1}"
GO_LICENSES="github.com/google/go-licenses/v2@${GO_LICENSES_VERSION}"
PACKAGES="${THREADPOINT_LICENSE_PACKAGES:-./...}"
ALLOWED_LICENSES="${THREADPOINT_ALLOWED_LICENSES:-Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC}"
REPORT_DIR="${THREADPOINT_LICENSE_REPORT_DIR:-}"

if [ -n "${REPORT_DIR}" ]; then
  mkdir -p "${REPORT_DIR}"
  go run "${GO_LICENSES}" report --include_tests ${PACKAGES} >"${REPORT_DIR}/licenses.csv"
fi

go run "${GO_LICENSES}" check --include_tests ${PACKAGES} --allowed_licenses="${ALLOWED_LICENSES}"
