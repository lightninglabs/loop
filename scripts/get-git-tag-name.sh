#!/bin/bash

# Derive a git tag name from the version fields defined in the given Go file.

set -e

get_git_tag_name() {
  local file_path="$1"

  if [ ! -f "$file_path" ]; then
      echo "Error: File not found at $file_path" >&2
      exit 1
  fi

  local app_major
  app_major=$(grep -oP 'appMajor\s*uint\s*=\s*\K\d+' "$file_path")

  local app_minor
  app_minor=$(grep -oP 'appMinor\s*uint\s*=\s*\K\d+' "$file_path")

  local app_patch
  app_patch=$(grep -oP 'appPatch\s*uint\s*=\s*\K\d+' "$file_path")

  local app_pre_release
  app_pre_release=$(grep -oP 'appPreRelease\s*=\s*"\K([A-Za-z0-9.-]*)' "$file_path")

  if [ -z "$app_major" ] || [ -z "$app_minor" ] || [ -z "$app_patch" ]; then
      echo "Error: Could not parse version constants from $file_path" >&2
      exit 1
  fi

  local tag_name="v${app_major}.${app_minor}.${app_patch}"

  if [ -n "$app_pre_release" ]; then
      tag_name+="-${app_pre_release}"
  fi

  echo "$tag_name"
}

file_path="$1"
echo "Reading version fields from file: $file_path" >&2
tag_name=$(get_git_tag_name "$file_path")
echo "Derived git tag name: $tag_name" >&2

if ! [[ "$tag_name" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    echo "Error: Derived tag \"$tag_name\" is not semver compliant" >&2
    exit 1
fi

echo "$tag_name"
