# Corresponding source distribution

The application serves its matching source archive at `GET /source-code` without login. `/source` remains the frontend source-management page.

Run `make source` (or `python3 scripts/package-source.py`) to create `source/anidan-source.tar.gz`. The archive contains application source, frontend source and lockfiles, runtime static assets, build scripts, documentation and license notices. Tests, benchmarks, fixtures and historical validation reports are excluded from the source allowlist.

Entries use deterministic ordering, timestamps and permissions. `SOURCE_MANIFEST.json` records each file's size and SHA-256. Runtime configuration, secrets, `.env`, databases, logs, dependency caches and compiled output are excluded. Symlinks and recognizable private keys are rejected.

`make backend` and the Docker build bind the archive SHA-256 into the binary. The download endpoint rejects an archive that differs from that digest. A direct `go build` without the linker setting does not provide this binding.

The runtime image contains `/app/source/anidan-source.tar.gz`, the project license, third-party notices and the `LICENSES` directory. The archive includes `.github/workflows/docker.yml` so the automated build recipe is available with its source. The frontend source link defaults to `/source-code`. An external `ANIDAN_SOURCE_URL` must provide the complete corresponding source for the deployed version.

For standalone distribution, preserve `source/anidan-source.tar.gz` beside the application assets in its working directory. After dependency changes, regenerate the dependency license inventory using `scripts/license-inventory.py`. Pinned public dependencies are described in `go.mod`, `go.sum` and `web/package-lock.json`; source trees of dependencies are not vendored.

See [LICENSING.md](LICENSING.md) and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for attribution and upstream license differences.
