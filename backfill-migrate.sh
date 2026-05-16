#!/bin/sh

set -e

echo "::add-mask::${WPM_TOKEN}"

echo "::group::wpm login"
wpm auth login --token ${WPM_TOKEN}
echo "::endgroup::"

PENDING="pending-backfill-${PACKAGE_TYPE}s.txt"

if [ ! -s "${PENDING}" ]; then
	echo "no pending ${PACKAGE_TYPE} entries; skipping backfill"
	exit 0
fi

echo "::group::backfill migration (${PACKAGE_TYPE})"
xargs migrate-wpm --type ${PACKAGE_TYPE} --concurrency ${CONCURRENCY:-2} < "${PENDING}"
echo "::endgroup::"
