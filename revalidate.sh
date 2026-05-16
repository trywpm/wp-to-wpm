#!/bin/sh

set -e

echo "::group::revalidate-closures"
revalidate-wpm
echo "::endgroup::"
