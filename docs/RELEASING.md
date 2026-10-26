# Releasing

Releases are cut from a semver tag and produce both static binaries and a
multi-arch container image.

## Cut a release

```bash
git tag v1.2.3
git push origin v1.2.3
```

The `release` workflow (`.github/workflows/release.yml`) then:

1. **verify** — `go build`, `go vet`, `go test -race`. Nothing publishes if
   this fails.
2. **binaries** — builds `linux/amd64`, `linux/arm64`, `darwin/amd64`,
   `darwin/arm64`, `windows/amd64` with `CGO_ENABLED=0`, writes a `.sha256`
   next to each, and uploads them as artifacts.
3. **docker** — builds and pushes a multi-arch image
   (`linux/amd64,linux/arm64`) to GHCR with the tags `1.2.3`, `1.2`, `1`,
   `latest`, and the short SHA.
4. **release** — creates the GitHub Release, attaching every binary, its
   checksum, and a changelog generated from commits since the previous tag.

## Pull the image

```bash
docker run -p 3000:3000 \
  -e IL2API_API_KEY=your-key \
  ghcr.io/<owner>/inceptionlabs2api:1.2.3
```

The image is published to GHCR under the repository owner. The first publish
creates the package; set its visibility to public in the package settings if
you want anonymous pulls.

## Dry run

Trigger the workflow manually (`workflow_dispatch`) to run verify + build
without publishing anything. Both the binary and Docker jobs build, but only
tag pushes push or publish.

## Version metadata

`app/main.go` carries `version`, `commit`, and `date` variables that default
to `dev` / `none` / `unknown`. Releases inject them via
`-ldflags "-X main.version=..."`, both in the Go build steps and as Docker
build args. The startup log line reports all three, so you can confirm what a
running instance is:

```
inceptionlabs2api 1.2.3 (commit abc1234, built 2026-10-26T00:00:00Z) listening on :3000 ...
```

A local `go build` or `docker build .` without the flags keeps the defaults
and still works.
