#!/usr/bin/env bash
set -e

readonly PKG_NAME_REGEX='^[a-z0-9]+(-[a-z0-9]+)*$'
readonly THEMES_INFO_API='https://api.wordpress.org/themes/info/1.2/?action=theme_information&slug='
readonly PLUGINS_INFO_API='https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&slug='

readonly MAX_RETRIES=3
readonly BASE_BACKOFF_SECONDS=5

themes_list=$(mktemp)
plugins_list=$(mktemp)

cleanup() {
  echo "cleaning up..."
  rm -f "$themes_list" "$plugins_list"
  pkill -P $$
}

trap cleanup EXIT INT TERM

if ! command -v svn &> /dev/null; then
  echo "error: svn command not found." >&2
  exit 1
fi

if [[ ! -f "resolved.json" ]]; then
  echo "error: resolved.json not found." >&2
  exit 1
fi

check_slug_exists() {
  local type="$1"
  local name="$2"
  local http_code
  local attempt=1

  if [[ "$type" != "theme" && "$type" != "plugin" ]]; then
      echo "error: invalid type '$type' provided to check_slug_exists" >&2
      return 1
  fi

  local url
  if [[ "$type" == "theme" ]]; then
      url="${THEMES_INFO_API}${name}"
  else
      url="${PLUGINS_INFO_API}${name}"
  fi

  while (( attempt <= MAX_RETRIES )); do
      http_code=$(curl --silent --head --location \
          --write-out "%{http_code}" --output /dev/null "$url")

      if [[ "$http_code" -eq 200 ]]; then
          return 0
      fi

      if (( http_code >= 500 && http_code < 600 )); then
          echo "server error ($http_code) for ${url}., retrying..." >&2
          local sleep_duration=$((attempt * BASE_BACKOFF_SECONDS))
          sleep "$sleep_duration"
      else
          # return since we only want 200 to include the plugin or theme in the list
          return 1
      fi
      ((attempt++))
  done

  echo "error: exceeded maximum retries for ${url}, falling back to previous data." >&2

  local json_file
  if [[ "$type" == "theme" ]]; then
      json_file="themes.json"
  else
      json_file="plugins.json"
  fi

  if [[ -f "$json_file" ]] && jq -e --arg name "$name" 'index($name)' "$json_file" > /dev/null; then
      return 0
  fi

  return 1
}

echo "fetching themes and plugins lists..."
svn list https://themes.svn.wordpress.org | sed 's:/$::' | sort > "$themes_list" &
svn_themes_pid=$!
svn list https://plugins.svn.wordpress.org | sed 's:/$::' | sort > "$plugins_list" &
svn_plugins_pid=$!

wait "$svn_themes_pid"
wait "$svn_plugins_pid"

if [[ ! -s "$themes_list" ]]; then
  echo "error: fetched themes list is empty." >&2
  exit 1
fi

if [[ ! -s "$plugins_list" ]]; then
  echo "error: fetched plugins list is empty." >&2
  exit 1
fi

echo "Comparing themes and plugins lists..."
comm -12 "$themes_list" "$plugins_list" \
  | jq -R -s 'split("\n") | map(select(length > 0))' > conflicts.json
echo "updated conflicts.json"

process_plugins() {
  echo "updating plugins.json..."

  local plugins=()

  while IFS= read -r plugin || [[ -n "$plugin" ]]; do
      if ! [[ $plugin =~ $PKG_NAME_REGEX ]]; then
          continue
      fi

      if ! check_slug_exists "plugin" "${plugin}"; then
          continue
      fi
      plugins+=("$plugin")
  done < "$plugins_list"

  printf '%s\n' "${plugins[@]}" | jq \
      --slurpfile conflicts conflicts.json \
      --slurpfile resolved resolved.json \
      -R -s '
          (split("\n") | map(select(length > 0))) as $initial_plugins |
          ($resolved[0].plugins | map(select(. as $p | $initial_plugins | index($p)))) as $concrete_resolved_plugins |
          ($conflicts[0] - $concrete_resolved_plugins) as $conflicts_to_remove |
          $initial_plugins - $conflicts_to_remove
      ' > plugins.json
  echo "updated plugins.json"
}

process_themes() {
  echo "updating themes.json..."

  local themes=()

  while IFS= read -r theme || [[ -n "$theme" ]]; do
      if ! [[ $theme =~ $PKG_NAME_REGEX ]]; then
          continue
      fi

      if ! check_slug_exists "theme" "${theme}"; then
          continue
      fi
      themes+=("$theme")
  done < "$themes_list"

  printf '%s\n' "${themes[@]}" | jq \
      --slurpfile conflicts conflicts.json \
      --slurpfile resolved resolved.json \
      -R -s '
          (split("\n") | map(select(length > 0))) as $initial_themes |
          ($resolved[0].themes | map(select(. as $t | $initial_themes | index($t)))) as $concrete_resolved_themes |
          ($conflicts[0] - $concrete_resolved_themes) as $conflicts_to_remove |
          $initial_themes - $conflicts_to_remove
      ' > themes.json
  echo "updated themes.json"
}

process_plugins &
pids+=($!)
process_themes &
pids+=($!)

failed=0
for pid in "${pids[@]}"; do
  if ! wait "$pid"; then
      echo "error: background process failed." >&2
      failed=1
  fi
done

if [[ "$failed" -ne 0 ]]; then
  exit 1
fi

echo "processing complete."
echo ""
echo "sorting JSON files..."

files_to_sort=(
  plugins.json
  themes.json
  conflicts.json
)

for file in "${files_to_sort[@]}"; do
  if [ -f "$file" ]; then
      jq sort "$file" > "sorted-$file" && mv "sorted-$file" "$file"
  else
      echo "file $file does not exist, skipping."
  fi
done

echo "sorted JSON files."
