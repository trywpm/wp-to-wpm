#!/bin/sh

set -e

echo "::add-mask::${WPM_TOKEN}"

echo "::group::wpm login"
wpm auth login --token ${WPM_TOKEN}
echo "::endgroup::"

echo "::group::run migration"
migrate-wpm --type ${PACKAGE_TYPE} --workers ${WORKERS}
echo "::endgroup::"
