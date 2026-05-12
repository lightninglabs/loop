#!/usr/bin/env bash

set -euo pipefail

usage() {
	echo "usage: $0 all|changed [base]" >&2
}

is_format_file() {
	local file="$1"

	[[ -f "$file" ]] || return 1
	[[ "$file" == *.go ]] || return 1

	case "$file" in
	*.pb.go | *.pb.gw.go | *.pb.json.go | *.pb.validate.go | \
		*.connect.go | *.gen.go | *_gen.go | *_generated.go | \
		*.sql.go | db.go | */db.go | models.go | */models.go | \
		querier.go | */querier.go | .git/* | */.git/* | \
		vendor/* | */vendor/* | third_party/* | */third_party/* | \
		testdata/* | */testdata/*)
		return 1
		;;
	esac

	return 0
}

print_file() {
	local file="$1"

	if is_format_file "$file"; then
		printf '%s\0' "$file"
	fi
}

list_all() {
	local file

	while IFS= read -r -d '' file; do
		print_file "${file#./}"
	done < <(find . -type f -name '*.go' -print0)
}

list_changed() {
	local base="$1"
	local file

	{
		git diff --name-only --diff-filter=ACMR "$base"...HEAD
		git diff --name-only --diff-filter=ACMR
		git diff --cached --name-only --diff-filter=ACMR
		git ls-files --others --exclude-standard
	} | sort -u | while IFS= read -r file; do
		print_file "$file"
	done
}

mode="${1:-}"

case "$mode" in
all)
	list_all
	;;
changed)
	list_changed "${2:-origin/master}"
	;;
*)
	usage
	exit 2
	;;
esac
