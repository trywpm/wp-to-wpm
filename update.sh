#!/usr/bin/env bash
set -e

readonly pkg_name_regex='^[a-z0-9]+(-[a-z0-9]+)*$'
readonly themes_info_api='https://api.wordpress.org/themes/info/1.2/?action=theme_information&slug='
readonly plugins_info_api='https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&slug='

readonly max_retries=3
readonly base_backoff_seconds=5
readonly parallel_jobs=100

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

if ! command -v parallel &> /dev/null; then
  echo "error: gnu parallel is not installed." >&2
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
    url="${themes_info_api}${name}"
  else
    url="${plugins_info_api}${name}"
  fi

  while (( attempt <= max_retries )); do
    http_code=$(curl --silent --head --location --write-out "%{http_code}" --output /dev/null "$url")

    if [[ "$http_code" -eq 200 ]]; then
      return 0
    fi

    if (( http_code >= 500 && http_code < 600 )); then
      echo "server error ($http_code) for ${url}., retrying..." >&2
      local sleep_duration=$((attempt * base_backoff_seconds))
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

export -f check_slug_exists
export themes_info_api plugins_info_api max_retries base_backoff_seconds

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

echo "comparing themes and plugins lists..."
comm -12 "$themes_list" "$plugins_list" \
  | jq -R -s 'split("\n") | map(select(length > 0))' > conflicts.json
echo "updated conflicts.json"

process_plugins() {
  echo "updating plugins.json..."

  local valid_plugins
  valid_plugins=$(grep -E "$pkg_name_regex" "$plugins_list" | \
    parallel --bar --jobs "$parallel_jobs" 'check_slug_exists "plugin" "{}" && echo "{}"')

  echo "$valid_plugins" | jq \
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

  local valid_themes
  valid_themes=$(grep -E "$pkg_name_regex" "$themes_list" | \
    parallel --bar --jobs "$parallel_jobs" 'check_slug_exists "theme" "{}" && echo "{}"')

  echo "$valid_themes" | jq \
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

process_themes
process_plugins

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
