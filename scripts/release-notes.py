#!/usr/bin/env python3

"""Rotate and validate Loop release notes."""

import argparse
import datetime
import os
import re
import stat
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
NOTES = ROOT / "docs" / "release-notes"
NEXT = NOTES / "release-notes-next.md"
TEMPLATE = NOTES / "release-notes-template.md"
README = NOTES / "README.md"
NEXT_PATH = NEXT.relative_to(ROOT)
TEMPLATE_PATH = TEMPLATE.relative_to(ROOT)
VERSION_PATH = Path("version.go")

REPOSITORY_URL = "https://github.com/lightninglabs/loop"
NEXT_LINK = "[Next release](release-notes-next.md)"
NEXT_ROW = "| [Next release](release-notes-next.md) | Unreleased | No changes yet |"
FIELDS = (
    "Release date",
    "Release page",
    "Previous release",
    "Next release",
)
FIELD_RE = re.compile(
    r"^- \*\*(?P<name>[^*\n]+):\*\*(?P<value>[^\n]*)"
    r"(?P<continuation>(?:\n(?: {2,}|\t)[^\n]*)*)",
    re.MULTILINE,
)
RELEASE_RE = re.compile(
    r"^v?(?P<version>[0-9]+\.[0-9]+\.[0-9]+)"
    r"-beta(?P<suffix>(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?)$"
)
LINK_RE = re.compile(r"^\[(?P<label>[^]]+)\]\((?P<target>[^)]+)\)$")
VERSION_PATTERNS = (
    re.compile(r"^\s*appMajor\s+uint\s*=\s*([0-9]+)$", re.MULTILINE),
    re.compile(r"^\s*appMinor\s+uint\s*=\s*([0-9]+)$", re.MULTILINE),
    re.compile(r"^\s*appPatch\s+uint\s*=\s*([0-9]+)$", re.MULTILINE),
    re.compile(r'^\s*appPreRelease\s*=\s*"([^"]*)"$', re.MULTILINE),
)


class Error(Exception):
    pass


def one_field(document, name):
    matches = [
        match for match in FIELD_RE.finditer(document)
        if match.group("name") == name
    ]
    if len(matches) != 1:
        raise Error(f"expected exactly one {name!r} field, found {len(matches)}")
    return matches[0]


def field(document, name):
    match = one_field(document, name)
    return (match.group("value") + match.group("continuation")).strip()


def replace_field(document, name, value):
    match = one_field(document, name)
    space = "" if value.startswith("\n") else " "
    replacement = f"- **{name}:**{space}{value}"
    return document[:match.start()] + replacement + document[match.end():]


def add_template_fields(document, template):
    """Add template metadata to the current headerless next-notes file."""

    names = [match.group("name") for match in FIELD_RE.finditer(document)]
    counts = [names.count(name) for name in FIELDS]
    if counts == [1] * len(FIELDS):
        return document
    if any(counts):
        raise Error("release notes contain only some managed metadata fields")

    matches = sorted(
        (one_field(template, name) for name in FIELDS),
        key=lambda match: match.start(),
    )
    start, end = matches[0].start(), matches[-1].end()
    residue = template[start:end]
    for match in reversed(matches):
        left, right = match.start() - start, match.end() - start
        residue = residue[:left] + residue[right:]
    if residue.strip():
        raise Error("template metadata fields must form one block")

    prefix = template[:start]
    if not document.startswith(prefix):
        raise Error("cannot locate the template metadata block")
    whitespace = re.match(r"\s*", template[end:]).group(0)
    return prefix + template[start:end] + whitespace + document[len(prefix):]


def next_is_empty(document, template):
    try:
        for name in FIELDS:
            document = replace_field(document, name, f"<{name}>")
            template = replace_field(template, name, f"<{name}>")
        return document == template
    except Error:
        return False


def release_details(release_name):
    match = RELEASE_RE.fullmatch(release_name)
    if match is None:
        raise Error(
            f"unsupported release {release_name!r}; "
            "expected vX.Y.Z-beta[-suffix]"
        )
    tag = release_name if release_name.startswith("v") else f"v{release_name}"
    return tag, match.group("version") + match.group("suffix")


def git(*args):
    try:
        return subprocess.check_output(
            ["git", "-C", ROOT, *args],
            stderr=subprocess.STDOUT,
            text=True,
        )
    except subprocess.CalledProcessError as error:
        raise Error(error.output.strip()) from error


def at(revision, path):
    return git("show", f"{revision}:{path.as_posix()}")


def version(document, source):
    values = []
    for pattern in VERSION_PATTERNS:
        matches = pattern.findall(document)
        if len(matches) != 1:
            raise Error(f"cannot determine the version from {source}")
        values.append(matches[0])
    result = ".".join(values[:3])
    return f"{result}-{values[3]}" if values[3] else result


def check_release(release_name, revision=None):
    if os.environ.get("SKIP_RELEASE_NOTES_CHECK") == "1":
        print("Skipping release notes checks due to SKIP_RELEASE_NOTES_CHECK=1")
        return

    try:
        _, filename_version = release_details(release_name)
    except Error as error:
        raise Error(
            f"{error}; set SKIP_RELEASE_NOTES_CHECK=1 for a non-release build"
        ) from error

    release_path = Path(
        f"docs/release-notes/release-notes-{filename_version}.md"
    )
    if revision:
        try:
            at(revision, release_path)
        except Error as error:
            raise Error(
                f"release notes file not found at {revision}: {release_path}"
            ) from error
        next_notes = at(revision, NEXT_PATH)
        template = at(revision, TEMPLATE_PATH)
    else:
        if not (ROOT / release_path).is_file():
            raise Error(f"release notes file not found: {ROOT / release_path}")
        next_notes = NEXT.read_text(encoding="utf-8")
        template = TEMPLATE.read_text(encoding="utf-8")

    if not next_is_empty(next_notes, template):
        raise Error("next release notes must match the empty template")


def check_pr(base, head):
    base_version = version(at(base, VERSION_PATH), f"version.go at {base}")
    head_version = version(at(head, VERSION_PATH), f"version.go at {head}")
    if base_version != head_version:
        check_release(head_version, head)
        return
    if os.environ.get("NO_CHANGELOG") == "true":
        print("Skipping release note entry due to the no-changelog label")
        return

    diff = git("diff", "--unified=0", f"{base}...{head}", "--", NEXT_PATH)
    for line in diff.splitlines():
        if line.startswith("+") and not line.startswith("+++"):
            added = line[1:].strip()
            if added and not added.startswith("#"):
                return
    raise Error("add a release note or apply the no-changelog label")


def install(contents):
    """Stage every output before replacing any destination."""

    staged = {}
    try:
        for path, content in contents.items():
            descriptor, name = tempfile.mkstemp(dir=path.parent)
            temporary = Path(name)
            staged[path] = temporary
            with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                output.write(content)
            mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o644
            temporary.chmod(mode)
        for path, temporary in staged.items():
            os.replace(temporary, path)
    finally:
        for temporary in staged.values():
            temporary.unlink(missing_ok=True)


def rotate(tag, highlights, release_date):
    tag, filename_version = release_details(tag)
    try:
        datetime.date.fromisoformat(release_date)
    except ValueError as error:
        raise Error("release date must use YYYY-MM-DD") from error
    if not highlights.strip() or "\n" in highlights or "|" in highlights:
        raise Error("release highlights must be one line and cannot contain '|'")

    release_notes = NOTES / f"release-notes-{filename_version}.md"
    if release_notes.exists():
        raise Error(f"release notes already exist: {release_notes}")

    next_notes = NEXT.read_text(encoding="utf-8")
    template = TEMPLATE.read_text(encoding="utf-8")
    readme = README.read_text(encoding="utf-8")

    previous = []
    for path in NOTES.glob("release-notes-*.md"):
        if path in (NEXT, TEMPLATE):
            continue
        document = path.read_text(encoding="utf-8")
        try:
            if field(document, "Next release") == NEXT_LINK:
                previous.append((path, document))
        except Error:
            pass
    if len(previous) != 1:
        raise Error("expected one versioned file to link to the next release")

    previous_path, previous_notes = previous[0]
    page = LINK_RE.fullmatch(field(previous_notes, "Release page"))
    if page is None:
        raise Error("previous release page must contain one Markdown link")
    previous_tag = page.group("label")
    expected_url = f"{REPOSITORY_URL}/releases/tag/{previous_tag}"
    if page.group("target") != expected_url:
        raise Error(f"previous release page must link to {expected_url}")

    versioned = add_template_fields(next_notes, template)
    versioned = replace_field(versioned, "Release date", release_date)
    versioned = replace_field(
        versioned,
        "Release page",
        f"\n  [{tag}]({REPOSITORY_URL}/releases/tag/{tag})",
    )
    versioned = replace_field(
        versioned,
        "Previous release",
        f"[{previous_tag}]({previous_path.name})",
    )
    versioned = replace_field(versioned, "Next release", NEXT_LINK)

    previous_notes = replace_field(
        previous_notes,
        "Next release",
        f"[{tag}]({release_notes.name})",
    )
    new_next = replace_field(
        template,
        "Previous release",
        f"[{tag}]({release_notes.name})",
    )
    new_next = replace_field(new_next, "Next release", "None")

    if readme.count(NEXT_ROW) != 1:
        raise Error("expected one next-release row in the release-notes README")
    release_row = (
        f"| [{tag}]({release_notes.name}) | {release_date} | {highlights} |"
    )
    readme = readme.replace(NEXT_ROW, f"{release_row}\n{NEXT_ROW}")

    install({
        release_notes: versioned,
        previous_path: previous_notes,
        README: readme,
        NEXT: new_next,
    })
    print(
        f"Created {release_notes.relative_to(ROOT)} and reset "
        f"{NEXT.relative_to(ROOT)}."
    )


def arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    rotate_parser = commands.add_parser("rotate")
    rotate_parser.add_argument("release_tag", help="vX.Y.Z-beta[-suffix]")
    rotate_parser.add_argument("release_highlights")
    rotate_parser.add_argument(
        "--date",
        default=datetime.date.today().isoformat(),
        help="release date (default: today)",
    )

    pr = commands.add_parser("pr")
    pr.add_argument("base")
    pr.add_argument("head")

    build = commands.add_parser("release")
    build.add_argument("release_name")
    return parser.parse_args()


def main():
    args = arguments()
    try:
        if args.command == "rotate":
            rotate(args.release_tag, args.release_highlights, args.date)
        elif args.command == "pr":
            check_pr(args.base, args.head)
        else:
            check_release(args.release_name)
    except (Error, OSError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
