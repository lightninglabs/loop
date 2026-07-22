# Commit Message Tooling

Use `scripts/commit_message.py` to lint, format, and safely reword commit
messages. The script enforces subject/body wrapping (`69`/`72`), keeps real
newlines, and preserves markdown-like body structure (lists, quotes, fenced
blocks, trailers).

## Common Workflows

```bash
# Lint the current commit message
make commitmsg-lint commit=HEAD

# Lint a commit range
make commitmsg-lint range=upstream/master..HEAD

# Format a message file in place
make commitmsg-fmt file=/tmp/msg inplace=1

# Reword a commit from formatted output
make commitmsg-reword commit=<sha>

# Preview a reword without rewriting history
make commitmsg-reword commit=<sha> dryrun=1
```

## Literal Newlines

If a message was created with literal `\n` sequences, use `decode=1` with
`commitmsg-fmt` or `commitmsg-reword` to convert them to real line breaks.
