#!/usr/bin/env bash

set -euo pipefail

# Color codes
readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly BLUE='\033[0;34m'
readonly CYAN='\033[0;36m'
readonly GRAY='\033[0;90m'
readonly NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARNING]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

log_processing() {
    echo -e "${CYAN}[PROCESSING]${NC} $*"
}

# Separator functions
separator_item() {
    echo -e "${GRAY}$(printf '=%.0s' {1..80})${NC}"
}

separator_tag() {
    echo -e "${GRAY}$(printf '.%.0s' {1..60})${NC}"
}

# validate arguments
if [[ $# -lt 2 || "$1" != "--type" ]]; then
    log_error "usage: $0 --type [plugin|theme] [--registry <registry_url>]"
    exit 1
fi

case "$2" in
    plugin|theme) type="$2" ;;
    *) log_error "invalid type '$2'. must be 'plugin' or 'theme'"; exit 1 ;;
esac

# parse optional registry flag
registry="registry.wpm.so"  # default
shift 2
while [[ $# -gt 0 ]]; do
    case $1 in
        --registry)
            [[ $# -lt 2 ]] && { log_error "--registry requires a value"; exit 1; }
            registry="$2"
            shift 2
            ;;
        *)
            log_error "unknown option '$1'"
            exit 1
            ;;
    esac
done

log_info "Starting WordPress $type processor"
log_info "Registry: $registry"

# check dependencies
log_info "Checking dependencies..."
for cmd in svn xmlstarlet wpm jq curl; do
    command -v "$cmd" >/dev/null 2>&1 || { log_error "missing dependency: $cmd"; exit 1; }
done
log_success "All dependencies found"

# validate configuration
config_file="${type}s.json"
[[ -f "$config_file" ]] || { log_error "configuration file '$config_file' not found"; exit 1; }

# set svn url and revision file
case "$type" in
    plugin) svn_url="https://plugins.svn.wordpress.org/"; rev_file=".plugin_last_rev"; api_url="https://api.wordpress.org/plugins/info/1.2/?action=plugin_information&slug=" ;;
    theme) svn_url="https://themes.svn.wordpress.org/"; rev_file=".theme_last_rev"; api_url="https://api.wordpress.org/themes/info/1.2/?action=theme_information&slug=" ;;
esac

[[ -f "$rev_file" ]] || { log_error "state file '$rev_file' not found"; exit 1; }

# read and validate last processed revision
last_processed_rev=$(cat "$rev_file")
[[ "$last_processed_rev" =~ ^[0-9]+$ ]] || { log_error "invalid revision number: $last_processed_rev"; exit 1; }
start_rev=$((last_processed_rev + 1))

log_info "Last processed revision: $last_processed_rev"
log_info "Starting from revision: $start_rev"

# initialize temporary files
temp_file=$(mktemp)

# fetch svn log
log_info "Fetching SVN log from $svn_url..."
svn_xml_log=$(svn log --xml "$svn_url" -q -v -r "$start_rev:HEAD" 2>"$temp_file") || {
    if grep -q "has no history" "$temp_file"; then
        log_info "No new revisions found"
        echo "last_head_rev=$last_processed_rev"
        echo "updated_items="
        exit 0
    fi
    log_error "svn command failed: $(cat "$temp_file")"
    exit 1
}

# extract latest revision
new_head_rev=$(echo "$svn_xml_log" | xmlstarlet sel -t -v "//logentry/@revision" -n | sort -nr | head -n1)
[[ -n "$new_head_rev" ]] || { log_error "failed to extract revision number"; exit 1; }
[[ "$new_head_rev" =~ ^[0-9]+$ ]] || { log_error "invalid revision number: $new_head_rev"; exit 1; }

log_info "Latest revision: $new_head_rev"

# check for new updates
if [[ "$new_head_rev" -le "$last_processed_rev" ]]; then
    log_info "No new updates found. last_head_rev=$new_head_rev, last_processed_rev=$last_processed_rev"
    exit 0
fi

# get updated items
updated_items=$(echo "$svn_xml_log" | xmlstarlet sel -t -v "//path" -n | cut -d'/' -f2 | sort -u | xargs)
[[ -n "$updated_items" ]] || {
    log_info "No new updates found for type '$type'"
    exit 0
}

log_success "Found $(echo $updated_items | wc -w) updated items: $updated_items"

# validate allowed items
allowed_items=$(jq -r . "$config_file") || { log_error "failed to read config file '$config_file'"; exit 1; }
[[ -n "$allowed_items" ]] || { log_error "no items found in '$config_file' or file is invalid"; exit 1; }

echo
separator_item
log_info "Starting item processing..."
separator_item

# process each item
for item in $updated_items; do
    echo
    if ! echo "$allowed_items" | jq -e --arg item "$item" '. | index($item)' >/dev/null 2>&1; then
        log_warn "Skipping $type '$item' as it is not in the allowed list"
        continue
    fi

    log_processing "Processing $type: $item"
    checkout_path="/tmp/$item"

    # checkout svn repository
    if [[ "$type" == "plugin" ]]; then
        if ! svn checkout "$svn_url/$item/tags" "$checkout_path" -q; then
            log_error "Failed to checkout $type '$item' from svn. skipping"
            continue
        fi
    else
        if ! svn checkout "$svn_url/$item" "$checkout_path" -q; then
            log_error "Failed to checkout $type '$item' from svn. skipping"
            continue
        fi
    fi

    # process version tags
    version_tags=$(ls "$checkout_path")
    if [[ -z "$version_tags" ]]; then
        log_warn "No version tags found for $type '$item', skipping"
        continue
    fi

    # get latest version from API
    log_info "Fetching latest version from WordPress API..."
    latest_version=$(curl -s "$api_url$item" | jq -r '.version')
    if [[ -z "$latest_version" ]]; then
        log_error "Failed to fetch version information for $type '$item' from API"
        continue
    fi
    log_info "Latest version: $latest_version"

    separator_tag
    log_info "Processing $(echo $version_tags | wc -w) version tags for $item"
    separator_tag

    for tag in $version_tags; do
        if [[ ! -d "$checkout_path/$tag" ]]; then
            log_warn "Skipping non-directory tag '$tag' for $type '$item'"
            continue
        fi

        echo
        log_processing "Processing $item@$tag"

        if ! wpm --cwd "$checkout_path/$tag" init --migrate --name "$item" --version "$tag" --type "$type"; then
            log_error "Failed to initialize wpm for $type '$item' at version '$tag'. skipping"
            continue
        fi

        dist_tag="untagged"
        if [[ "$tag" == "$latest_version" ]]; then
            dist_tag="latest"
        fi

        log_info "Publishing $type '$item' at version '$tag' to registry '$registry' with dist tag '$dist_tag'"

        if wpm --cwd "$checkout_path/$tag" --registry "$registry" publish --access public --tag "$dist_tag"; then
            log_success "Published $item@$tag successfully"
        else
            log_error "Failed to publish $type '$item' at version '$tag'. skipping"
            continue
        fi
    done

    rm -rf "$checkout_path"
    echo
    log_success "Finished processing $type: $item"
    separator_item
done

# update revision file
echo "$new_head_rev" > "$rev_file"
echo
log_success "Updated revision file to $new_head_rev"
log_success "WordPress $type processor completed successfully"
