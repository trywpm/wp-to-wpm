#!/bin/sh

set -e

echo "::group::version info"
echo "wpm version: $(wpm --version)"
echo "svn version: $(svn --version --quiet)"
echo "::endgroup::"

echo "::group::run update"
update-wpm
echo "::endgroup::"
