#!/usr/bin/env python3

"""
commit_message lints, formats, and rewrites Git commit messages.

Goals:
- Enforce a commit subject in "<package>: <summary>" form.
- Wrap subjects and bodies using repository defaults (69/72 columns).
- Preserve markdown-like body structure:
  - Paragraph breaks.
  - Bullet/numbered list item indentation.
  - Fenced and indented code blocks.
  - Git trailers.
- Support linting existing commit ranges and rewording commits in-place.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
import tempfile
import textwrap
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable


DEFAULT_SUBJECT_WIDTH = 69
DEFAULT_BODY_WIDTH = 72

SUBJECT_PATTERN = re.compile(
    r"^(?P<pkg>[A-Za-z0-9_][A-Za-z0-9+_.\-/]*): (?P<summary>.+)$"
)
LIST_ITEM_PATTERN = re.compile(
    r"^(?P<indent>\s*)(?P<marker>[-+*]|\d+[.)])(?P<space>\s+)(?P<text>.*)$"
)
TRAILER_PATTERN = re.compile(r"^[A-Za-z0-9-]+:\s+\S.*$")
FENCE_PATTERN = re.compile(r"^\s*(```|~~~)")
QUOTE_PATTERN = re.compile(r"^(?P<indent>\s*)>\s?(?P<text>.*)$")
NEWLINE_ESCAPE_PATTERN = re.compile(r"\\n")


@dataclass
class LintIssue:
    """LintIssue captures one formatting violation."""

    line: int
    message: str


@dataclass
class LintResult:
    """LintResult stores issues and whether a message passed lint."""

    issues: list[LintIssue]

    def ok(self) -> bool:
        return len(self.issues) == 0


def run_git(args: list[str]) -> str:
    """run_git executes git and returns trimmed stdout."""

    return subprocess.check_output(
        ["git", *args],
        stderr=subprocess.STDOUT,
        text=True,
    ).rstrip("\n")


def get_commit_message(rev: str) -> str:
    """get_commit_message returns the full commit message for a revision."""

    return run_git(["show", "-s", "--format=%B", rev])


def commit_subject(rev: str) -> str:
    """commit_subject returns only the subject line for a revision."""

    return run_git(["show", "-s", "--format=%s", rev])


def commit_is_merge(rev: str) -> bool:
    """commit_is_merge reports whether rev has more than one parent."""

    parents = run_git(["rev-list", "--parents", "-n", "1", rev]).split()
    return len(parents) > 2


def split_subject_body(message: str) -> tuple[str, list[str]]:
    """
    split_subject_body returns (subject, body_lines) from a commit message.

    The first line is always treated as subject. Any following lines are body.
    """

    lines = message.splitlines()
    if not lines:
        return "", []
    return lines[0], lines[1:]


def _count_columns(text: str) -> int:
    """_count_columns counts characters for width checks."""

    return len(text)


def _is_list_item(line: str) -> bool:
    """_is_list_item checks whether a line starts a markdown-like list item."""

    return LIST_ITEM_PATTERN.match(line) is not None


def _is_trailer(line: str) -> bool:
    """_is_trailer checks whether a line is a git trailer."""

    return TRAILER_PATTERN.match(line) is not None


def _is_indented_code(line: str) -> bool:
    """_is_indented_code checks whether a line looks like indented code."""

    if "\t" in line[:1]:
        return True

    spaces = len(line) - len(line.lstrip(" "))
    return spaces >= 4 and line.strip() != ""


def _is_quote_line(line: str) -> bool:
    """_is_quote_line checks whether a line is markdown quote text."""

    return QUOTE_PATTERN.match(line) is not None


def _normalize_body_leading_blank(body_lines: list[str]) -> list[str]:
    """
    _normalize_body_leading_blank removes body-leading blank lines.

    The formatter emits exactly one blank separator between subject and body.
    """

    idx = 0
    while idx < len(body_lines) and body_lines[idx].strip() == "":
        idx += 1
    return body_lines[idx:]


def _normalize_body_trailing_blank(body_lines: list[str]) -> list[str]:
    """_normalize_body_trailing_blank trims trailing blank lines."""

    idx = len(body_lines)
    while idx > 0 and body_lines[idx - 1].strip() == "":
        idx -= 1
    return body_lines[:idx]


def _collapse_redundant_blank_lines(lines: list[str]) -> list[str]:
    """_collapse_redundant_blank_lines keeps at most one consecutive blank."""

    out: list[str] = []
    blank = False

    for line in lines:
        is_blank = line.strip() == ""
        if is_blank and blank:
            continue
        out.append(line)
        blank = is_blank

    return out


def _decode_escaped_newlines(lines: list[str]) -> list[str]:
    """
    _decode_escaped_newlines expands literal "\\n" sequences into real lines.

    This targets the common shell-escaping mistake from `git commit -m`.
    """

    out: list[str] = []
    split_re = re.compile(r"(?<!\\)\\n")

    for line in lines:
        if "\\n" not in line:
            out.append(line)
            continue

        out.extend(split_re.split(line))

    return out


def wrap_text(
    text: str,
    width: int,
    initial_indent: str = "",
    subsequent_indent: str = "",
) -> list[str]:
    """wrap_text wraps a paragraph without splitting long tokens."""

    wrapper = textwrap.TextWrapper(
        width=width,
        initial_indent=initial_indent,
        subsequent_indent=subsequent_indent,
        break_long_words=False,
        break_on_hyphens=False,
        replace_whitespace=True,
        drop_whitespace=True,
    )

    wrapped = wrapper.wrap(text)
    return wrapped if wrapped else [initial_indent.rstrip()]


def _consume_paragraph(body: list[str], start: int) -> tuple[int, list[str]]:
    """
    _consume_paragraph consumes plain paragraph lines from body[start:].

    It stops before blank lines and before other structured blocks.
    """

    parts: list[str] = []
    i = start

    while i < len(body):
        line = body[i]
        if line.strip() == "":
            break
        if FENCE_PATTERN.match(line):
            break
        if _is_list_item(line):
            break
        if _is_indented_code(line):
            break
        if _is_trailer(line):
            break
        if _is_quote_line(line):
            break
        parts.append(line.strip())
        i += 1

    return i, parts


def _format_paragraph(parts: list[str], width: int) -> list[str]:
    """_format_paragraph normalizes spacing and wraps plain text paragraphs."""

    text = " ".join(part for part in parts if part)
    if not text:
        return []
    return wrap_text(text, width)


def _consume_quote_block(
    body: list[str],
    start: int,
    width: int,
) -> tuple[int, list[str]]:
    """_consume_quote_block consumes consecutive markdown quote lines."""

    i = start
    quoted: list[str] = []
    indent = ""

    while i < len(body):
        line = body[i]
        if line.strip() == "":
            break
        match = QUOTE_PATTERN.match(line)
        if not match:
            break
        indent = match.group("indent")
        quoted.append(match.group("text").strip())
        i += 1

    text = " ".join(fragment for fragment in quoted if fragment)
    if not text:
        return i, [f"{indent}>"]

    prefix = f"{indent}> "
    return i, wrap_text(
        text,
        width,
        initial_indent=prefix,
        subsequent_indent=prefix,
    )


def _consume_list_item(
    body: list[str],
    start: int,
    width: int,
) -> tuple[int, list[str]]:
    """
    _consume_list_item consumes a single list item with continuation lines.

    Continuation lines are treated as part of the item until a new peer list
    item starts or a blank line is reached.
    """

    first = body[start]
    match = LIST_ITEM_PATTERN.match(first)
    if match is None:
        return start + 1, [first.rstrip()]

    indent = match.group("indent")
    marker = match.group("marker")
    content = [match.group("text").strip()]
    i = start + 1

    while i < len(body):
        line = body[i]
        if line.strip() == "":
            break
        next_match = LIST_ITEM_PATTERN.match(line)
        if next_match is not None and len(next_match.group("indent")) <= len(indent):
            break
        if FENCE_PATTERN.match(line):
            break
        if _is_trailer(line):
            break
        content.append(line.strip())
        i += 1

    text = " ".join(part for part in content if part)
    prefix = f"{indent}{marker} "
    continuation = " " * len(prefix)

    if not text:
        return i, [prefix.rstrip()]

    return i, wrap_text(
        text,
        width,
        initial_indent=prefix,
        subsequent_indent=continuation,
    )


def _consume_fence_block(body: list[str], start: int) -> tuple[int, list[str]]:
    """_consume_fence_block preserves fenced code blocks verbatim."""

    lines: list[str] = []
    i = start
    open_line = body[i].rstrip()
    lines.append(open_line)
    i += 1

    while i < len(body):
        line = body[i].rstrip()
        lines.append(line)
        if FENCE_PATTERN.match(line):
            i += 1
            break
        i += 1

    return i, lines


def _consume_indented_block(body: list[str], start: int) -> tuple[int, list[str]]:
    """_consume_indented_block preserves indented code blocks verbatim."""

    lines: list[str] = []
    i = start

    while i < len(body):
        line = body[i]
        if line.strip() == "":
            lines.append("")
            i += 1
            continue
        if _is_indented_code(line):
            lines.append(line.rstrip())
            i += 1
            continue
        break

    while lines and lines[-1] == "":
        lines.pop()

    return i, lines


def format_body(body_lines: list[str], body_width: int) -> list[str]:
    """format_body returns a wrapped body while preserving markdown structure."""

    body = _normalize_body_leading_blank(body_lines)
    body = _normalize_body_trailing_blank(body)

    if not body:
        return []

    out: list[str] = []
    i = 0
    while i < len(body):
        line = body[i]

        if line.strip() == "":
            out.append("")
            i += 1
            continue

        if FENCE_PATTERN.match(line):
            i, block = _consume_fence_block(body, i)
            out.extend(block)
            continue

        if _is_indented_code(line):
            i, block = _consume_indented_block(body, i)
            out.extend(block)
            continue

        if _is_list_item(line):
            while i < len(body):
                current = body[i]
                if current.strip() == "":
                    break
                if not _is_list_item(current):
                    break
                i, item = _consume_list_item(body, i, body_width)
                out.extend(item)
            continue

        if _is_quote_line(line):
            i, quoted = _consume_quote_block(body, i, body_width)
            out.extend(quoted)
            continue

        if _is_trailer(line):
            out.append(line.rstrip())
            i += 1
            continue

        i, para = _consume_paragraph(body, i)
        out.extend(_format_paragraph(para, body_width))

    return _collapse_redundant_blank_lines(out)


def format_message(
    message: str,
    subject_width: int = DEFAULT_SUBJECT_WIDTH,
    body_width: int = DEFAULT_BODY_WIDTH,
    decode_escaped_newlines: bool = False,
) -> str:
    """format_message normalizes spacing and wraps subject/body."""

    subject, body_lines = split_subject_body(message)
    subject = subject.strip()
    if decode_escaped_newlines:
        body_lines = _decode_escaped_newlines(body_lines)

    match = SUBJECT_PATTERN.match(subject)
    if match is None:
        wrapped_subject = wrap_text(subject, subject_width)
        subject_out = " ".join(wrapped_subject).strip()
    else:
        pkg = match.group("pkg")
        summary = match.group("summary").strip()
        prefix = f"{pkg}: "
        available = max(10, subject_width - len(prefix))
        summary_wrapped = wrap_text(summary, available)
        subject_out = prefix + " ".join(
            line.strip() for line in summary_wrapped if line.strip()
        )

    body_lines = format_body(body_lines, body_width)

    if not body_lines:
        return f"{subject_out}\n"

    return f"{subject_out}\n\n" + "\n".join(body_lines) + "\n"


def lint_message(
    message: str,
    subject_width: int = DEFAULT_SUBJECT_WIDTH,
    body_width: int = DEFAULT_BODY_WIDTH,
) -> LintResult:
    """lint_message validates message structure and line widths."""

    issues: list[LintIssue] = []
    subject, body_lines = split_subject_body(message)

    if not subject:
        issues.append(LintIssue(1, "empty commit message subject"))
        return LintResult(issues)

    if _count_columns(subject) > subject_width:
        issues.append(
            LintIssue(
                1,
                f"subject length {_count_columns(subject)} exceeds "
                f"{subject_width} chars",
            )
        )

    match = SUBJECT_PATTERN.match(subject)
    if match is None:
        issues.append(
            LintIssue(
                1,
                'subject must match "<package>: <summary>"',
            )
        )
    else:
        summary = match.group("summary")
        if summary.endswith("."):
            issues.append(
                LintIssue(1, "subject summary should not end with period")
            )

    if body_lines:
        if body_lines[0].strip() != "":
            issues.append(
                LintIssue(
                    2,
                    "body must be separated from subject by one blank line",
                )
            )
        else:
            leading_blanks = 0
            while (
                leading_blanks < len(body_lines)
                and body_lines[leading_blanks].strip() == ""
            ):
                leading_blanks += 1
            if leading_blanks > 1 and leading_blanks < len(body_lines):
                issues.append(
                    LintIssue(
                        2,
                        "use exactly one blank line between subject and body",
                    )
                )

    in_fence = False
    for idx, line in enumerate(body_lines, start=2):
        if NEWLINE_ESCAPE_PATTERN.search(line):
            issues.append(
                LintIssue(
                    idx,
                    r'found literal "\n"; use real newlines in commit body',
                )
            )

        if FENCE_PATTERN.match(line):
            in_fence = not in_fence
            continue

        if in_fence:
            continue

        if line.strip() == "":
            continue

        if _is_indented_code(line):
            continue
        if _is_trailer(line):
            continue

        width = _count_columns(line)
        if width > body_width:
            issues.append(
                LintIssue(
                    idx,
                    f"body line length {width} exceeds {body_width} chars",
                )
            )

    return LintResult(issues)


def read_input_message(args: argparse.Namespace) -> str:
    """read_input_message reads commit message input from file/stdin/commit."""

    if args.file:
        return Path(args.file).read_text(encoding="utf-8")

    if args.commit:
        return get_commit_message(args.commit)

    if not sys.stdin.isatty():
        return sys.stdin.read()

    raise ValueError("no input provided: use --file, --commit, or stdin")


def write_output_message(args: argparse.Namespace, message: str) -> None:
    """write_output_message writes formatted output to stdout or file."""

    if args.in_place and args.file:
        Path(args.file).write_text(message, encoding="utf-8")
        return

    sys.stdout.write(message)


def _lint_one(
    label: str,
    message: str,
    subject_width: int,
    body_width: int,
) -> bool:
    """_lint_one lints one message and prints issues. Returns success."""

    result = lint_message(
        message,
        subject_width=subject_width,
        body_width=body_width,
    )
    if result.ok():
        print(f"{label}: OK")
        return True

    print(f"{label}: FAIL")
    for issue in result.issues:
        print(f"  L{issue.line}: {issue.message}")
    return False


def _collect_revs(range_expr: str, include_merges: bool) -> list[str]:
    """_collect_revs returns commits in a revision range."""

    args = ["rev-list", "--reverse", range_expr]
    if not include_merges:
        args.insert(1, "--no-merges")
    out = run_git(args)
    return [line for line in out.splitlines() if line.strip()]


def lint_command(args: argparse.Namespace) -> int:
    """lint_command runs linting for a message source or commit range."""

    all_ok = True

    if args.range:
        revs = _collect_revs(args.range, include_merges=args.include_merges)
        if not revs:
            print(f"no commits found in range: {args.range}")
            return 0

        for rev in revs:
            if not args.include_merges and commit_is_merge(rev):
                continue
            subject = commit_subject(rev)
            label = f"{rev[:7]} {subject}"
            all_ok &= _lint_one(
                label,
                get_commit_message(rev),
                args.subject_width,
                args.body_width,
            )

        return 0 if all_ok else 1

    message = read_input_message(args)
    label = args.file or args.commit or "<stdin>"
    ok = _lint_one(label, message, args.subject_width, args.body_width)
    return 0 if ok else 1


def fmt_command(args: argparse.Namespace) -> int:
    """fmt_command formats a message from file/stdin/commit."""

    message = read_input_message(args)
    formatted = format_message(
        message,
        subject_width=args.subject_width,
        body_width=args.body_width,
        decode_escaped_newlines=args.decode_escaped_newlines,
    )

    if args.check:
        if message == formatted:
            return 0
        print("message is not properly formatted")
        return 1

    write_output_message(args, formatted)
    return 0


def _assert_clean_worktree() -> None:
    """_assert_clean_worktree ensures there are no staged or unstaged changes."""

    status = run_git(["status", "--porcelain"])
    if status.strip():
        raise ValueError(
            "worktree is not clean; commit rewriting requires a clean state"
        )


def _reword_head(new_message: str, no_verify: bool) -> None:
    """_reword_head amends HEAD with new_message."""

    with tempfile.NamedTemporaryFile("w", delete=False) as tmp:
        tmp.write(new_message)
        tmp_path = tmp.name

    try:
        cmd = ["git", "commit", "--amend", "-F", tmp_path]
        if no_verify:
            cmd.append("--no-verify")
        subprocess.check_call(cmd)
    finally:
        Path(tmp_path).unlink(missing_ok=True)


def _reword_non_head(target_rev: str, new_message: str, no_verify: bool) -> None:
    """_reword_non_head rewrites a non-HEAD commit via non-interactive rebase."""

    full = run_git(["rev-parse", target_rev])
    head = run_git(["rev-parse", "HEAD"])
    if full == head:
        _reword_head(new_message, no_verify=no_verify)
        return

    merge_count = run_git(["rev-list", "--count", "--merges", f"{full}..HEAD"])
    if int(merge_count) > 0:
        raise ValueError(
            "range from target commit to HEAD contains merge commits; "
            "automatic reword is disabled for this case"
        )

    is_ancestor = subprocess.call(
        ["git", "merge-base", "--is-ancestor", full, "HEAD"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    if is_ancestor != 0:
        raise ValueError(f"target commit {target_rev} is not an ancestor of HEAD")

    parent = run_git(["rev-parse", f"{full}^"])

    with tempfile.TemporaryDirectory() as tmpdir:
        tmp = Path(tmpdir)
        msg_file = tmp / "new-message.txt"
        seq_editor = tmp / "sequence-editor.py"
        msg_editor = tmp / "message-editor.py"

        msg_file.write_text(new_message, encoding="utf-8")
        seq_editor.write_text(
            textwrap.dedent(
                """\
                #!/usr/bin/env python3
                import os
                import pathlib
                import re
                import sys

                target = os.environ["COMMIT_REWORD_TARGET"]
                todo_path = pathlib.Path(sys.argv[1])
                lines = todo_path.read_text(encoding="utf-8").splitlines()

                out = []
                replaced = False
                for line in lines:
                    match = re.match(r"^(pick|p)\\s+([0-9a-f]+)(\\s+.*)$", line)
                    if match and not replaced:
                        commit = match.group(2)
                        if target.startswith(commit) or commit.startswith(target):
                            out.append(f"reword {commit}{match.group(3)}")
                            replaced = True
                            continue
                    out.append(line)

                if not replaced:
                    raise SystemExit(
                        f"could not find commit {target} in rebase todo"
                    )

                todo_path.write_text(
                    "\\n".join(out) + "\\n",
                    encoding="utf-8",
                )
                """
            ),
            encoding="utf-8",
        )
        msg_editor.write_text(
            textwrap.dedent(
                """\
                #!/usr/bin/env python3
                import pathlib
                import shutil
                import sys

                src = pathlib.Path(__import__("os").environ["COMMIT_REWORD_FILE"])
                dst = pathlib.Path(sys.argv[1])
                shutil.copyfile(src, dst)
                """
            ),
            encoding="utf-8",
        )
        seq_editor.chmod(0o755)
        msg_editor.chmod(0o755)

        env = os.environ.copy()
        env["COMMIT_REWORD_TARGET"] = full
        env["COMMIT_REWORD_FILE"] = str(msg_file)
        env["GIT_SEQUENCE_EDITOR"] = str(seq_editor)
        env["GIT_EDITOR"] = str(msg_editor)

        cmd = ["git", "rebase", "-i", parent]
        if no_verify:
            cmd.append("--no-verify")

        subprocess.check_call(cmd, env=env)


def reword_command(args: argparse.Namespace) -> int:
    """reword_command rewrites a commit message using formatted content."""

    original = get_commit_message(args.commit)
    formatted = format_message(
        original,
        subject_width=args.subject_width,
        body_width=args.body_width,
        decode_escaped_newlines=args.decode_escaped_newlines,
    )

    if original == formatted and not args.force:
        print("commit message already formatted; nothing to do")
        return 0

    if args.dry_run:
        print(formatted, end="")
        return 0

    _assert_clean_worktree()

    try:
        _reword_non_head(
            args.commit,
            formatted,
            no_verify=args.no_verify,
        )
    except subprocess.CalledProcessError as err:
        print(f"git command failed with exit status {err.returncode}")
        return err.returncode or 1
    except ValueError as err:
        print(f"error: {err}")
        return 1

    print(f"reworded commit {args.commit} with formatted message")
    return 0


def build_parser() -> argparse.ArgumentParser:
    """build_parser creates the CLI parser."""

    parser = argparse.ArgumentParser(
        description=(
            "Lint, format, and reword commit messages with markdown-aware "
            "body wrapping."
        )
    )
    parser.add_argument(
        "--subject-width",
        type=int,
        default=DEFAULT_SUBJECT_WIDTH,
        help=f"max subject width (default: {DEFAULT_SUBJECT_WIDTH})",
    )
    parser.add_argument(
        "--body-width",
        type=int,
        default=DEFAULT_BODY_WIDTH,
        help=f"max body width (default: {DEFAULT_BODY_WIDTH})",
    )

    sub = parser.add_subparsers(dest="cmd", required=True)

    lint = sub.add_parser("lint", help="lint a message or commit range")
    lint.add_argument("--file", help="path to commit message file")
    lint.add_argument("--commit", help="commit revision to lint")
    lint.add_argument(
        "--range",
        help="lint all commits in revision range (example: origin/main..HEAD)",
    )
    lint.add_argument(
        "--include-merges",
        action="store_true",
        help="include merge commits when linting --range",
    )
    lint.set_defaults(func=lint_command)

    fmt = sub.add_parser("fmt", help="format a message from file/stdin/commit")
    fmt.add_argument("--file", help="path to commit message file")
    fmt.add_argument("--commit", help="commit revision to read as input")
    fmt.add_argument(
        "--in-place",
        action="store_true",
        help="write formatted output back to --file",
    )
    fmt.add_argument(
        "--check",
        action="store_true",
        help="exit non-zero if formatting changes would be applied",
    )
    fmt.add_argument(
        "--decode-escaped-newlines",
        action="store_true",
        help='decode literal "\\n" body sequences before formatting',
    )
    fmt.set_defaults(func=fmt_command)

    reword = sub.add_parser(
        "reword",
        help="reword an existing commit with its formatted message",
    )
    reword.add_argument(
        "--commit",
        default="HEAD",
        help="commit revision to reword (default: HEAD)",
    )
    reword.add_argument(
        "--force",
        action="store_true",
        help="rewrite even if message is already formatted",
    )
    reword.add_argument(
        "--no-verify",
        action="store_true",
        help="pass --no-verify to git amend/rebase reword operations",
    )
    reword.add_argument(
        "--decode-escaped-newlines",
        action="store_true",
        help='decode literal "\\n" body sequences before rewording',
    )
    reword.add_argument(
        "--dry-run",
        action="store_true",
        help="print the formatted message instead of rewriting commits",
    )
    reword.set_defaults(func=reword_command)

    return parser


def main(argv: Iterable[str] | None = None) -> int:
    """main parses arguments and dispatches subcommands."""

    parser = build_parser()
    args = parser.parse_args(argv)

    try:
        return int(args.func(args))
    except ValueError as err:
        print(f"error: {err}")
        return 1


if __name__ == "__main__":
    sys.exit(main())
