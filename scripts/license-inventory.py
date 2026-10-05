#!/usr/bin/env python3
"""Collect installed dependency notices; metadata is not a legal clearance.

Run after `go mod download` and `npm ci` using the intended build toolchain.
Does not install/execute dependency code or modify source manifests. The Go tool
may resolve module metadata if it is missing from its configured module cache.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "LICENSES" / "dependencies"
OUT.parent.mkdir(parents=True, exist_ok=True)
# Collect into a fresh tree: dropped dependencies cannot retain stale notices.
# The previous generated tree is preserved outside the public notice directory
# rather than deleting any potentially hand-added material.
STAGE = Path(tempfile.mkdtemp(prefix=".dependencies-stage-", dir=OUT.parent))


def notices(folder, key):
    found = []
    for path in sorted(folder.iterdir()):
        if not path.is_file() or not re.match(r"^(license|licence|copying|notice|copyright|unlicense)([._-]|$)", path.name, re.I):
            continue
        body = path.read_bytes()
        if len(body) > 2_000_000:
            continue
        dest = STAGE / key / path.name
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(body)
        found.append({"path": (OUT / key / path.name).relative_to(ROOT).as_posix(), "sha256": hashlib.sha256(body).hexdigest()})
    return found


def key_for(ecosystem, name, version):
    return ecosystem + "/" + re.sub(r"[^a-zA-Z0-9._-]", "_", name + "@" + version)


items = []
raw = subprocess.check_output([os.environ.get("GO", "go"), "list", "-m", "-json", "all"], cwd=ROOT, text=True)
decoder = json.JSONDecoder()
while raw.strip():
    module, offset = decoder.raw_decode(raw.lstrip())
    raw = raw.lstrip()[offset:]
    if module.get("Main"):
        continue
    actual = module.get("Replace", module)
    name, version = module["Path"], module.get("Version", "")
    folder = Path(actual["Dir"]) if actual.get("Dir") else None
    items.append({"ecosystem": "go", "name": name, "version": version,
                  "replacement": module.get("Replace", {}).get("Path"),
                  "notices": notices(folder, key_for("go", name, version)) if folder and folder.is_dir() else [],
                  "license_metadata": None})

lock = ROOT / "web/package-lock.json"
if lock.exists():
    for relative, data in sorted(json.loads(lock.read_text()).get("packages", {}).items()):
        if not relative or data.get("link"):
            continue
        folder = ROOT / "web" / relative
        pkg = folder / "package.json"
        if not pkg.exists():
            continue  # Optional platform package not in this build.
        info = json.loads(pkg.read_text())
        name, version = info.get("name", relative), info.get("version", data.get("version", ""))
        items.append({"ecosystem": "npm", "name": name, "version": version,
                      "dev": data.get("dev", False), "license_metadata": info.get("license", data.get("license")),
                      "notices": notices(folder, key_for("npm", name, version))})

result = {"description": "Installed dependency metadata and verbatim top-level notices; not complete legal clearance. Vendored assets and embedded third-party code require separate review.",
          "packages": items,
          "missing_top_level_notices": [{"ecosystem": p["ecosystem"], "name": p["name"], "version": p["version"]} for p in items if not p["notices"]]}
previous = None
if OUT.exists():
    previous = Path(tempfile.mkdtemp(prefix=".dependencies-previous-", dir=OUT.parent))
    previous.rmdir()  # The freshly created empty placeholder contains no data.
    OUT.rename(previous)
try:
    STAGE.rename(OUT)
except Exception:
    if previous is not None:
        previous.rename(OUT)
    raise
(ROOT / "LICENSES/DEPENDENCIES.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
print(json.dumps({"packages": len(items), "missing_notices": len(result["missing_top_level_notices"]), "prior_generated_tree": str(previous) if previous else None}))
