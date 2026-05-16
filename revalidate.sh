#!/bin/sh

set -e

echo "::group::version info"
echo "wpm version: $(wpm --version)"
echo "svn version: $(svn --version --quiet)"
echo "::endgroup::"

echo "::group::revalidate-closures"
revalidate-wpm
echo "::endgroup::"
