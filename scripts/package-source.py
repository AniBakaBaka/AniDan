#!/usr/bin/env python3
"""Create a deterministic, allowlisted AniDan source archive; no network access.

Run only after freezing the intended release tree. Build outputs and runtime files cannot
enter merely because they exist under the project root.
"""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import tarfile
import tempfile

ROOT_FILES = {"Dockerfile", ".dockerignore", "Makefile", "README.md", "LICENSE",
              "LICENSING.md", "SOURCE_DISTRIBUTION.md", "THIRD_PARTY_NOTICES.md",
              "go.mod", "go.sum", "compose.yaml"}
ROOT_DIRS = {"cmd", "internal", "web", "static", "scripts", "LICENSES"}
BLOCK_DIRS = {".git", ".svn", ".hg", "node_modules", "dist",
              "__pycache__", ".cache",
              "playwright-report", "test-results", ".next", "live-evidence"}
BLOCK_SUFFIXES = {".db", ".sqlite", ".sqlite3", ".log", ".pyc", ".pyo", ".so", ".pyd",
                  ".exe", ".dll", ".dylib", ".tar", ".gz", ".zip", ".pem", ".key", ".p12", ".pfx"}
DOT_FILES = {".dockerignore", ".npmrc", ".gitignore", ".editorconfig"}
REQUIRED = {"LICENSE", "THIRD_PARTY_NOTICES.md", "go.mod", "go.sum", "Dockerfile",
            "Makefile", "compose.yaml", "web/package.json", "web/package-lock.json",
            "LICENSING.md", "SOURCE_DISTRIBUTION.md",
            "scripts/package-source.py", "internal/server/server.go"}
MAX_FILE = 32 * 1024 * 1024
MAX_TOTAL = 256 * 1024 * 1024


def eligible(rel):
    parts = rel.parts
    if not parts or not (len(parts) == 1 and parts[0] in ROOT_FILES or parts[0] in ROOT_DIRS):
        return False
    if any(x in BLOCK_DIRS for x in parts):
        return False
    name = parts[-1]
    if any(x.startswith(".") and x not in DOT_FILES for x in parts):
        return False
    if name.startswith(".env") or name.lower() in {"credentials.json", "secrets.json", "id_rsa", "id_ed25519"}:
        return False
    if name.lower() in {"config.json", "config.yaml", "config.yml"}:
        return False
    if rel.suffix.lower() in BLOCK_SUFFIXES:
        return False
    return True


def collect(root):
    found = []
    total = 0
    for directory, dirs, names in os.walk(root, followlinks=False):
        base = Path(directory)
        dirs[:] = sorted(x for x in dirs if x not in BLOCK_DIRS and not x.startswith(".") and
                         (base != root or x in ROOT_DIRS))
        for name in sorted(names):
            path = base / name
            rel = path.relative_to(root)
            if not eligible(rel):
                continue
            meta = path.lstat()
            if not stat.S_ISREG(meta.st_mode):
                raise ValueError("non-regular allowlisted source entry: " + rel.as_posix())
            if meta.st_size > MAX_FILE:
                raise ValueError("source file exceeds size limit: " + rel.as_posix())
            # O_NOFOLLOW prevents a link replacement after lstat where supported.
            fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            with os.fdopen(fd, "rb") as stream:
                data = stream.read(MAX_FILE + 1)
            if len(data) > MAX_FILE:
                raise ValueError("source file grew beyond size limit")
            # Refuse recognizable credential material in normally public build files.
            if name == ".npmrc" and re.search(rb"(?im)(?:_authToken|_auth|_password)\s*=", data):
                raise ValueError("credential-bearing .npmrc must not be packaged")
            if re.search(rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----", data):
                raise ValueError("private key material in source entry: " + rel.as_posix())
            total += len(data)
            if total > MAX_TOTAL:
                raise ValueError("source archive input exceeds size limit")
            found.append((rel.as_posix(), data, 0o755 if meta.st_mode & 0o111 else 0o644))
    paths = {x[0] for x in found}
    missing = REQUIRED - paths
    if missing:
        raise ValueError("required source files missing: " + ", ".join(sorted(missing)))
    return sorted(found)


def build(root, output, version=None):
    files = collect(root)
    if version is None:
        source = next(data for name, data, _ in files if name == "internal/server/server.go")
        match = re.search(rb'(?m)^const Version = "([A-Za-z0-9.+_-]+)"', source)
        if not match:
            raise ValueError("cannot determine server version")
        version = match[1].decode()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,79}", version):
        raise ValueError("invalid archive version")
    entries = [{"path": name, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest(), "mode": oct(mode)}
               for name, data, mode in files]
    inventory = json.dumps(entries, sort_keys=True, separators=(",", ":")).encode()
    manifest = {"format": 1, "project": "AniDan", "version": version, "license": "AGPL-3.0-only",
                "sourceSha256": hashlib.sha256(inventory).hexdigest(), "files": entries,
                "dependencySources": "Pinned public dependencies are identified in go.mod/go.sum and web/package-lock.json; dependency source trees are not vendored.",
                "scope": "Allowlisted application source, build scripts, assets, documentation and notices; no runtime configuration or captured live comment pools."}
    manifest_data = (json.dumps(manifest, ensure_ascii=False, sort_keys=True, indent=2) + "\n").encode()
    output.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".anidan-source-", dir=output.parent)
    try:
        with os.fdopen(fd, "wb") as raw:
            # The archive is intentionally public application source. mkstemp's
            # 0600 default would make Docker's root-owned COPY unreadable by the
            # runtime UID and silently break the network source offer.
            if hasattr(os, "fchmod"):
                os.fchmod(raw.fileno(), 0o644)
            else:
                os.chmod(tmp, 0o644)
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0, compresslevel=9) as zipped:
                with tarfile.open(fileobj=zipped, mode="w|", format=tarfile.PAX_FORMAT) as tar:
                    for name, data, mode in files + [("SOURCE_MANIFEST.json", manifest_data, 0o644)]:
                        info = tarfile.TarInfo("anidan/" + name)
                        info.size, info.mode, info.mtime = len(data), mode, 0
                        info.uid = info.gid = 0
                        info.uname = info.gname = ""
                        tar.addfile(info, io.BytesIO(data))
            raw.flush()
            os.fsync(raw.fileno())
        os.replace(tmp, output)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    return {"archive": str(output), "archiveSha256": digest, "sourceSha256": manifest["sourceSha256"],
            "version": version, "license": manifest["license"], "files": len(files), "bytes": output.stat().st_size}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--output", type=Path)
    parser.add_argument("--version")
    args = parser.parse_args()
    root = args.root.resolve(strict=True)
    output = args.output or root / "source/anidan-source.tar.gz"
    print(json.dumps(build(root, output, args.version), ensure_ascii=False, indent=2))
