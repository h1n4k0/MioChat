"""Check the public source tree and stable release metadata."""
import argparse
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
ROOT_FILES = {"go.mod", "go.sum", ".gitignore", ".gitattributes", "README.md", "LICENSE", "SECURITY.md", "THIRD_PARTY_NOTICES"}
DIRECTORIES = {".github", "deploy", "docs", "internal", "scripts", "signing", "testdata"}
GENERATED = {".git", ".build", "dist", ".venv", "__pycache__"}
PRIVATE_HEADER = re.compile(rb"(?m)^-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----\r?$")


def version():
    match = re.search(r'^const appVersion = "(\d+\.\d+\.\d+)"$', (ROOT / "migrations.go").read_text(encoding="utf8"), re.M)
    if not match:
        raise ValueError("Application version must be a stable semantic version")
    return match[1]


def source_paths():
    paths = []
    for path in sorted(ROOT.rglob("*")):
        relative = path.relative_to(ROOT)
        if any(part in GENERATED for part in relative.parts):
            continue
        if path.is_symlink():
            raise ValueError("Source must not contain symlinks: " + relative.as_posix())
        if not path.is_file():
            continue
        allowed = relative.parts[0] in DIRECTORIES or (len(relative.parts) == 1 and (path.suffix == ".go" or path.name in ROOT_FILES))
        if not allowed:
            raise ValueError("Unexpected source file: " + relative.as_posix())
        if path.suffix.lower() in {".db", ".sqlite", ".sqlite3", ".pem", ".key", ".ppk", ".bak", ".exe", ".zip", ".gz", ".pyc"} or path.name.endswith(("-wal", "-shm")):
            raise ValueError("Generated or private file in source: " + relative.as_posix())
        if PRIVATE_HEADER.search(path.read_bytes()):
            raise ValueError("Private key material in source: " + relative.as_posix())
        paths.append(path)
    return paths


def validate(expected=None):
    actual = version()
    if expected is not None and expected != actual:
        raise ValueError("Requested version differs from the application version")
    if not (ROOT / "go.mod").read_text(encoding="utf8").startswith("module github.com/mio-qwq/MioChat\n"):
        raise ValueError("Unexpected Go module")
    for name in ("LICENSE", "THIRD_PARTY_NOTICES", "README.md", "docs/USAGE.md", "docs/RELEASE.md", "docs/RELEASE-NOTES.md", "signing/allowed_signers", "signing/release-signing.pub"):
        if not (ROOT / name).is_file() or not (ROOT / name).read_bytes().strip():
            raise ValueError("Missing release file: " + name)
    paths = source_paths()
    for path in paths:
        if path.suffix != ".md":
            continue
        for destination in re.findall(r"\]\(([^)]+)\)", path.read_text(encoding="utf8")):
            if re.match(r"(?:[a-z]+:|#)", destination):
                continue
            destination = destination.split("#", 1)[0]
            if destination and not (path.parent / destination).exists():
                raise ValueError("Broken document link in " + path.relative_to(ROOT).as_posix())
    return actual, paths


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version")
    args = parser.parse_args()
    actual, paths = validate(args.version)
    print(json.dumps({"version": actual, "source_files": len(paths), "valid": True}))


if __name__ == "__main__":
    main()
