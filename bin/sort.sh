#!/usr/bin/bash

files=(
  plugins.json
  themes.json
  conflicts.json
)

for file in "${files[@]}"; do
  if [ -f "$file" ]; then
    jq sort $file > "sorted-$file" && mv "sorted-$file" "$file"
  else
    echo "File $file does not exist, skipping."
  fi
done

echo "All files sorted successfully."
