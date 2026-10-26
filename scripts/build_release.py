"""Build reproducible archives and checksums for the current stable release."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import struct
import subprocess
import tarfile
import tempfile
import zipfile

from check_release import ROOT, validate

TARGETS = (("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64"))
COMMON = ("README.md", "LICENSE", "THIRD_PARTY_NOTICES", "SECURITY.md", "docs/USAGE.md", "docs/RELEASE.md", "deploy/miochat.service", "signing/allowed_signers", "signing/release-signing.pub")


def zip_item(archive, name, data, executable=False):
    info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
    info.create_system = 3
    info.compress_type = zipfile.ZIP_DEFLATED
    info.external_attr = (0o100755 if executable else 0o100644) << 16
    archive.writestr(info, data)


def tar_item(archive, name, data, executable=False):
    info = tarfile.TarInfo(name)
    info.size = len(data)
    info.mode = 0o755 if executable else 0o644
    info.mtime = 0
    info.uid = info.gid = 0
    archive.addfile(info, io.BytesIO(data))


def check_binary(data, system, architecture):
    if system == "linux":
        if data[:5] != b"\x7fELF\x02" or struct.unpack_from("<H", data, 18)[0] != {"amd64": 62, "arm64": 183}[architecture]:
            raise ValueError("Unexpected ELF target")
    else:
        offset = struct.unpack_from("<I", data, 0x3C)[0]
        if data[:2] != b"MZ" or data[offset:offset+4] != b"PE\0\0" or struct.unpack_from("<H", data, offset+4)[0] != 0x8664:
            raise ValueError("Unexpected PE target")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    actual, sources = validate(args.version)
    output = args.output.resolve()
    if output == ROOT or ".git" in output.parts or output.is_symlink():
        raise ValueError("Unsafe output directory")
    if output.exists() and any(output.iterdir()):
        raise ValueError("Output directory must be empty")
    output.mkdir(parents=True, exist_ok=True)
    files = []
    # Temporary compiler outputs stay below the selected output directory.
    with tempfile.TemporaryDirectory(prefix=".compile-", dir=output) as temporary:
        temporary = Path(temporary)
        if temporary.resolve().parent != output:
            raise ValueError("Unsafe compiler output directory")
        for system, architecture in TARGETS:
            name = "miochat.exe" if system == "windows" else "miochat"
            binary = temporary / (system + "-" + architecture + "-" + name)
            environment = dict(os.environ, CGO_ENABLED="0", GOOS=system, GOARCH=architecture)
            subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(binary), "."], cwd=ROOT, env=environment, check=True)
            data = binary.read_bytes()
            check_binary(data, system, architecture)
            extension = ".zip" if system == "windows" else ".tar.gz"
            asset = output / ("MioChat-v" + actual + "-" + system + "-" + architecture + extension)
            if system == "windows":
                with zipfile.ZipFile(asset, "w") as archive:
                    zip_item(archive, name, data, True)
                    for file in COMMON:
                        zip_item(archive, file, (ROOT / file).read_bytes())
            else:
                with asset.open("wb") as file:
                    with gzip.GzipFile(filename="", mode="wb", fileobj=file, mtime=0) as compressed:
                        with tarfile.open(fileobj=compressed, mode="w") as archive:
                            tar_item(archive, name, data, True)
                            for member in COMMON:
                                tar_item(archive, member, (ROOT / member).read_bytes())
            files.append(asset)
    source = output / ("MioChat-v" + actual + "-source.zip")
    with zipfile.ZipFile(source, "w") as archive:
        for path in sources:
            zip_item(archive, "MioChat-v" + actual + "/" + path.relative_to(ROOT).as_posix(), path.read_bytes())
    files.append(source)
    sums = "".join(hashlib.sha256(path.read_bytes()).hexdigest() + "  " + path.name + "\n" for path in sorted(files))
    (output / "SHA256SUMS").write_text(sums, encoding="ascii", newline="\n")
    print(json.dumps({"version": actual, "output": str(output), "assets": [path.name for path in sorted(files)] + ["SHA256SUMS"]}))


if __name__ == "__main__":
    main()
