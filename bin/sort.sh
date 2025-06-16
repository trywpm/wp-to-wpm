#!/usr/bin/bash

files=(
  plugins.json
  themes.json
  migrated-plugins.json
  migrated-themes.json
  ignored-plugins.json
  ignored-themes.json
)

for file in "${files[@]}"; do
  if [ -f "$file" ]; then
    cat "$file" | jq -S . > "sorted-$file" && mv "sorted-$file" "$file"
  else
    echo "File $file does not exist, skipping."
  fi
done

echo "All files sorted successfully."
