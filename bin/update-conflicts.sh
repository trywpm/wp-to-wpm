#!/usr/bin/env bash

set -e

comm -12 \
  <(svn list https://themes.svn.wordpress.org | sed 's:/$::' | sort) \
  <(svn list https://plugins.svn.wordpress.org | sed 's:/$::' | sort) \
  | jq -R -s 'split("\n") | map(select(length > 0))' > conflicts.json

echo "updated conflicts.json"