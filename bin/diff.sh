#!/usr/bin/bash

if [ -z "$1" ]; then
  echo "Usage: $0 <theme|plugin>"
  exit 1
fi

type=$1

if [ "$type" != "theme" ] && [ "$type" != "plugin" ]; then
  echo "Error: Type must be 'theme' or 'plugin'."
  echo "Usage: $0 <theme|plugin>"
  exit 1
fi

old_file="${type}s.json"
output_file="ignored-${type}s.json"
migrated_file="migrated-${type}s.json"

if [ ! -f "$old_file" ] || [ ! -f "$migrated_file" ]; then
  echo "Error: Input files not found."
  echo "Missing: $old_file or $migrated_file"
  exit 1
fi

echo "Finding entries in '$old_file' that are not in '$migrated_file'..."

jq --slurp '
  (.[0] | to_entries) as $old_entries |
  (.[1]) as $new |
  reduce $old_entries[] as $entry (
    {};

    if $new[$entry.key] == null then
      . + { ($entry.key): $entry.value }
    else
      .
    end
  )
' "$old_file" "$migrated_file" > "$output_file"

echo "Result saved to '$output_file'."