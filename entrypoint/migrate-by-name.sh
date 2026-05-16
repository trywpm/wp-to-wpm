#!/bin/sh

set -e

echo "::add-mask::${WPM_TOKEN}"

echo "::group::version info"
echo "wpm version: $(wpm --version)"
echo "svn version: $(svn --version --quiet)"
echo "::endgroup::"

echo "::group::wpm login"
wpm auth login --token ${WPM_TOKEN}
echo "::endgroup::"

if [ -z "${NAMES}" ]; then
	echo "::error::NAMES is empty; nothing to migrate"
	exit 1
fi

echo "::group::run migration (${PACKAGE_TYPE})"
echo "${NAMES}" | xargs migrate-wpm --type ${PACKAGE_TYPE} --concurrency ${CONCURRENCY:-2}
echo "::endgroup::"
