"""Verify the release tag, its commit, and the source version before building."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys


SIGNER = "234856096+mio-qwq@users.noreply.github.com"
TAG_PATTERN = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")


def git(root, *arguments):
    result = subprocess.run(
        ["git", "--no-pager", "-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen",
         "-c", "gpg.ssh.allowedSignersFile=" + str(root / "signing/allowed_signers"),
         *arguments], cwd=root, text=True, capture_output=True, check=False)
    if result.returncode:
        raise ValueError("Git verification failed: " + " ".join(arguments) + "\n" + result.stderr.strip())
    return result.stdout.strip()


def verify(root, tag):
    if not TAG_PATTERN.fullmatch(tag):
        raise ValueError("Expected a stable release tag such as v2.1.0")
    if Path(git(root, "rev-parse", "--show-toplevel")).resolve() != root:
        raise ValueError("Run against the release repository root")
    trust = root / "signing/allowed_signers"
    if not trust.is_file() or trust.is_symlink() or trust.resolve().parent != root / "signing":
        raise ValueError("Missing release signing trust file")
    principals = [line.split()[0] for line in trust.read_text(encoding="utf-8").splitlines()
                  if line.strip() and not line.lstrip().startswith("#")]
    if not principals or any(principal != SIGNER for principal in principals):
        raise ValueError("Release trust file must name the designated signer")
    if git(root, "status", "--porcelain", "--untracked-files=normal"):
        raise ValueError("Release working tree must be clean")
    ref = "refs/tags/" + tag
    if git(root, "cat-file", "-t", ref) != "tag":
        raise ValueError("Release requires an annotated, signed tag")
    contents = git(root, "cat-file", "-p", ref)
    header = contents.split("\n\n", 1)[0]
    fields = dict(line.split(" ", 1) for line in header.splitlines())
    if fields.get("tag") != tag or fields.get("type") != "commit":
        raise ValueError("Tag name or target type does not match the release")
    if "-----BEGIN SSH SIGNATURE-----" not in contents:
        raise ValueError("Release tag must have an SSH signature")
    email = re.search(r"<([^<>]+)> [0-9]+ [+-][0-9]{4}$", fields.get("tagger", ""))
    if email is None or email.group(1) != SIGNER:
        raise ValueError("Unexpected release tag signer identity")
    commit = git(root, "rev-parse", ref + "^{commit}")
    if git(root, "rev-parse", "HEAD") != commit:
        raise ValueError("Checked-out commit does not match the release tag")
    if git(root, "show", "-s", "--format=%ce", commit) != SIGNER:
        raise ValueError("Unexpected release commit signer identity")
    if "gpgsig -----BEGIN SSH SIGNATURE-----" not in git(root, "cat-file", "-p", commit):
        raise ValueError("Release commit must have an SSH signature")
    git(root, "verify-tag", ref)
    git(root, "verify-commit", commit)
    versions = []
    for path in sorted(root.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        versions.extend(re.findall(r'(?m)^const\s+appVersion\s*=\s*"([^"]+)"', path.read_text(encoding="utf-8")))
    if versions != [tag[1:]]:
        raise ValueError("Source appVersion must match the release tag exactly")
    return {"verified": True, "tag": tag, "version": tag[1:], "commit": commit, "signer": SIGNER}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--repository", type=Path, default=Path(__file__).resolve().parents[1])
    arguments = parser.parse_args()
    try:
        result = verify(arguments.repository.resolve(), arguments.tag)
    except (ValueError, OSError) as error:
        print(str(error), file=sys.stderr)
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
