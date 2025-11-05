#!/usr/bin/env bash

set -e

readonly pkg_name_regex='^[a-z0-9]+(-[a-z0-9]+)*$'
readonly themes_info_api='https://api.wordpress.org/themes/info/1.2/?action=theme_information&slug='
readonly plugins_info_api='https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&slug='

readonly MAX_RETRIES=3
readonly BASE_BACKOFF_SECONDS=5

themes_list=$(mktemp)
plugins_list=$(mktemp)
trap 'rm -f "$themes_list" "$plugins_list"' EXIT

if ! command -v svn &> /dev/null; then
	echo "error: svn command not found." >&2
	exit 1
fi

check_slug_exists() {
	local type=$1
	local name=$2
	local http_code
	local attempt=1

	if [[ "$type" != "theme" && "$type" != "plugin" ]]; then
		echo "error: invalid type '$type' provided to check_slug_exists" >&2
		exit 1
	fi

	local url
	if [[ "$type" == "theme" ]]; then
		url="${themes_info_api}${name}"
	else
		url="${plugins_info_api}${name}"
	fi

	while [[ "$attempt" -le $((MAX_RETRIES + 1)) ]]; do
		http_code=$(curl --silent --head --location \
			--write-out "%{http_code}" --output /dev/null "$url")

		if [[ "$http_code" -eq 200 ]]; then
			return 0
		fi

		if [[ "$http_code" -ge 500 && "$http_code" -lt 600 ]]; then
			echo "server error ($http_code) for ${url}. retrying..." >&2
		else
			return 1
		fi

		if [[ "$attempt" -le "$MAX_RETRIES" ]]; then
			local sleep_duration=$((attempt * BASE_BACKOFF_SECONDS))
			sleep "$sleep_duration"
		fi

		((attempt++))
	done

	echo "error: exceeded maximum retries for ${url}, falling back to previous data" >&2

	if [[ "$type" == "theme" ]]; then
		if jq -e --arg name "$name" 'index($name)' themes.json > /dev/null; then
			return 0
		fi
	else
		if jq -e --arg name "$name" 'index($name)' plugins.json > /dev/null; then
			return 0
		fi
	fi

	return 1
}

if [[ ! -f "resolved.json" ]]; then
	echo "error: resolved.json not found." >&2
	exit 1
fi

echo "fetching themes and plugins lists..."
svn list https://themes.svn.wordpress.org | sed 's:/$::' | sort > "$themes_list" &
svn list https://plugins.svn.wordpress.org | sed 's:/$::' | sort > "$plugins_list" &
wait

echo "comparing themes and plugins lists..."
comm -12 "$themes_list" "$plugins_list" \
	| jq -R -s 'split("\n") | map(select(length > 0))' > conflicts.json
echo "updated conflicts.json"

{
	echo "updating plugins.json..."

	plugins=()
	while IFS=read -r plugin; do
		if ! [[ $plugin =~ $pkg_name_regex ]]; then
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
} &

{
	echo "updating themes.json..."

	themes=()
	while IFS=read -r theme; do
		if ! [[ $theme =~ $pkg_name_regex ]]; then
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
} &

wait

echo "update complete."
echo ""
echo "sorting JSON files..."

files_to_sort=(
	plugins.json
	themes.json
	conflicts.json
)

for file in "${files_to_sort[@]}"; do
	if [ -f "$file" ]; then
		jq sort $file > "sorted-$file" && mv "sorted-$file" "$file"
	else
		echo "file $file does not exist, skipping."
	fi
done

echo "sorting complete."
