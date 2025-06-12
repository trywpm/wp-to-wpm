#!/usr/bin/bash

if ! command -v mc &> /dev/null; then
    echo "mc command not found"
    exit 1
fi

if [ "$#" -ne 2 ]; then
    echo "Usage: $0 <source_alias/bucket> <destination_alias/bucket>"
    echo "Example: $0 s3_src/source-data s3_dest/destination-data"
    exit 1
fi

# should be alias/bucket where alias can be set using `mc alias set`
export source_s3=$1
export dest_s3=$2

move_tar_zst() {
    local source_full_path=$1
    local object_key="${source_full_path#$source_s3/}"
    local dest_full_path="$dest_s3/$object_key"

    echo "[$$] Moving: $source_full_path -> $dest_full_path"
    mc mv -H "x-amz-acl:public-read" "$source_full_path" "$dest_full_path"
}

export -f move_tar_zst

echo "Starting S3 migration script with $workers concurrent jobs. Press Ctrl+C to exit."

# this loop will keep running indefinitely
# once all files are processed, it will sleep for a while
while true; do
    echo "=================================================="
    echo "$(date) - Starting new migration cycle for .tar.zst files."

    source_find_path="$source_s3"

    first_file=$(mc find "$source_find_path" --name "*.tar.zst" --ignore "_t*" | head -n 1)

    if [ -z "$first_file" ]; then
        echo "No .tar.zst files found to move. Sleeping for 30 seconds..."
        sleep 30
        continue
    fi

    echo "Found files to process. Starting parallel move with 5 workers."

    mc find "$source_find_path" --name "*.tar.zst" --ignore "_t*" | xargs -n 1 -P 5 -I {} bash -c 'move_tar_zst "{}"'
done
