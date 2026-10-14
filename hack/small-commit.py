#!/usr/bin/env python3
"""Validate and commit an explicit, manually reviewed logical file allowlist."""
import argparse
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
from urllib.parse import urlsplit


def git(*args):
    return subprocess.check_output(["git", *args])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--message", required=True)
    parser.add_argument("--validate", required=True, help="Trusted maintainer validation command; no shell")
    parser.add_argument("files", nargs="+")
    args = parser.parse_args()
    if not re.fullmatch(r"(feat|fix|refactor|test|docs|perf|chore)(\([a-z0-9-]+\))?!?: .+", args.message):
        parser.error("use a meaningful Conventional Commit message")
    root = Path(git("rev-parse", "--show-toplevel").decode().strip()).resolve()
    os.chdir(root)
    if git("diff", "--cached", "--name-only", "-z"):
        parser.error("index is not empty; inspect existing staged changes first")
    paths = []
    validated = {}
    suspicious = re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----|(?:ghp_|github_pat_|sk_live_|AKIA)[A-Za-z0-9_]{16,}")
    credential_url = re.compile(rb"https?://[^\s/@]+:[^\s/@]+@[^\s\"'<>\\]+")
    for value in args.files:
        p = Path(value)
        if p.is_absolute() or ".." in p.parts or value.startswith("-"):
            parser.error("paths must be repository-relative without traversal/options")
        if not p.resolve().is_relative_to(root) or p.is_symlink() or not p.is_file():
            parser.error("only regular in-repository files are accepted")
        if any(part in {".git", "bin", "dist", "vendor", "__pycache__", ".coverage"} for part in p.parts) or p.suffix in {".log", ".out", ".pyc", ".tmp", ".exe", ".key", ".env"}:
            parser.error("generated, temporary, credential or vendored file rejected")
        content = p.read_bytes()
        if b"\0" in content or suspicious.search(content):
            parser.error("binary or likely credential detected in selected file: " + value)
        for match in credential_url.finditer(content):
            hostname = urlsplit(match.group().decode("utf-8", "replace")).hostname or ""
            if not (p.name.endswith("_test.go") and hostname.endswith(".invalid")):
                parser.error("credential-bearing URL detected in selected file: " + value)
        paths.append(value)
        validated[value] = content
    print(git("status", "--short", "--branch").decode(), end="")
    subprocess.run(["git", "diff", "--", *paths], check=True)
    subprocess.run(shlex.split(args.validate), check=True)
    # Another process may have changed the index during validation.
    if git("diff", "--cached", "--name-only", "-z"):
        parser.error("index changed during validation; nothing staged by this helper")
    if any(Path(path).is_symlink() or Path(path).read_bytes() != content for path, content in validated.items()):
        parser.error("selected content changed during validation; rerun after reviewing the change")
    subprocess.run(["git", "add", "--", *paths], check=True)
    staged = {p for p in git("diff", "--cached", "--name-only", "-z").decode().split("\0") if p}
    if not staged or not staged.issubset(set(paths)):
        parser.error("staged allowlist mismatch; inspect index")
    if any(git("show", ":" + path) != validated[path] for path in staged):
        parser.error("staged content differs from validated content; inspect index")
    subprocess.run(["git", "diff", "--cached", "--check"], check=True)
    subprocess.run(["git", "diff", "--cached", "--stat"], check=True)
    subprocess.run(["git", "commit", "--signoff", "-m", args.message], check=True)
    print(git("log", "-1", "--format=%H %s").decode(), end="")


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as exc:
        sys.exit(exc.returncode)
