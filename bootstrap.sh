#!/bin/bash

set -euo pipefail

cleanup() {
    [[ -n "${temp_file:-}" ]] && rm -f "$temp_file"
}
trap cleanup EXIT

die() {
    echo "Error: $*" >&2
    exit 1
}

check_dependencies() {
    local missing_deps=()
    for cmd in "$@"; do
        if ! command -v "$cmd" &>/dev/null; then
            missing_deps+=("$cmd")
        fi
    done
    
    if [[ ${#missing_deps[@]} -gt 0 ]]; then
        die "Missing required commands: ${missing_deps[*]}"
    fi
}

validate_revision() {
    local rev="$1"
    if ! [[ "$rev" =~ ^[0-9]+$ ]]; then
        die "Invalid revision number: $rev"
    fi
}

parse_args() {
    if [[ $# -ne 2 || "$1" != "--type" ]]; then
        die "Usage: $0 --type [plugin|theme]"
    fi
    
    case "$2" in
        plugin|theme) echo "$2" ;;
        *) die "Invalid type '$2'. Must be 'plugin' or 'theme'" ;;
    esac
}

get_config() {
    local type="$1"
    case "$type" in
        plugin)
            echo "https://plugins.svn.wordpress.org/ .plugin_last_rev"
            ;;
        theme)
            echo "https://themes.svn.wordpress.org/ .theme_last_rev"
            ;;
    esac
}

main() {
    check_dependencies svn xmlstarlet
    
    local type
    type=$(parse_args "$@")
    
    local config svn_url rev_file
    read -r svn_url rev_file <<< "$(get_config "$type")"
    
    [[ -f "$rev_file" ]] || die "State file '$rev_file' not found"
    
    local last_processed_rev start_rev
    last_processed_rev=$(cat "$rev_file")
    validate_revision "$last_processed_rev"
    start_rev=$((last_processed_rev + 1))
    
    temp_file=$(mktemp)
    local svn_xml_log
    svn_xml_log=$(svn log --xml "$svn_url" -q -v -r "$start_rev:HEAD" 2>"$temp_file") || true
    
    if [[ -z "$svn_xml_log" ]]; then
        if grep -q "has no history" "$temp_file"; then
            echo "last_head_rev=$last_processed_rev"
            echo "updated_items="
            exit 0
        else
            die "SVN command failed: $(cat "$temp_file")"
        fi
    fi
    
    local new_head_rev updated_items
    new_head_rev=$(echo "$svn_xml_log" | xmlstarlet sel -t -v "//logentry/@revision" -n | sort -nr | head -n1)
    updated_items=$(echo "$svn_xml_log" | xmlstarlet sel -t -v "//path" -n | cut -d'/' -f2 | sort -u | xargs)
    
    [[ -n "$new_head_rev" ]] || die "Failed to extract revision number"
    validate_revision "$new_head_rev"
    
    echo "last_head_rev=$new_head_rev"
    echo "updated_items=$updated_items"
}

main "$@"