# pb-plugins

**wip** — work in progress: nothing here is released, tags and
recipes may change without notice.

The plugin catalog behind `ghcr.io/greatliontech/pb-plugins`: buf's
plugin names, each built from a recipe of this repository's own and
published as a plugin image pb runs (`pb`'s `plugin-execution.md` and
`plugin-publish.md`), one manifest list per version over every
platform the plugin serves, signed keyless. No Dockerfile and no
container engine take part: `pb plugin build` packages the
executables the recipes produce.

## Layout

- `catalog.yaml` — the plugins: upstream, recipe kind, entrypoint,
  platforms served, and the recipe's parameters.
- `plugins/<owner>/<plugin>/versions` — the tags published, one per
  line ascending, in buf's spelling (`v1.36.12`; `v36.2` where
  upstream's releases carry two components). A recipe's own files
  lie under `plugins/<owner>/<plugin>/files`, copied into the source
  tree at the directory the recipe's `files` names
  (`plugins/protocolbuffers/csharp/files/` holds the `cc_binary`
  protobuf does not ship).
- `cmd/catalog` — the tool the workflows run: `check`, `plan`,
  `tree`, `publish`, `bump`.

## Recipe kinds

| kind | produces | platforms | discovery |
|---|---|---|---|
| `go` | a Go main package cross-compiled with CGO disabled, one host for every platform | all six | the module proxy |
| `node` | an npm package's executable compiled by bun into one standalone executable per platform, one host for every platform | all six | the npm registry |
| `release` | the executable an upstream GitHub release ships prebuilt, one asset per platform | the assets upstream ships | the repository's releases |
| `bazel` | a C++ target built by bazel on a runner of the platform itself | linux and darwin on both architectures, windows/amd64 (no bazel C++ toolchain is established for windows/arm64) | the repository's releases |

The six platforms are pb's: `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64`.
Every kind lays out one file per platform, the entrypoint at the
tree's root, which the image carries as `/<entrypoint>`. The Linux
executables of the `node`, `release` and `bazel` kinds link the
platform's C library, so their Linux trees are layered over the
catalog's `base` (distroless `cc`, pinned by index digest and
resolved to the platform's image at publish); the `go` kind is
static and takes no base. On an `OS` sandbox row pb runs a Linux
entrypoint only where it is static, so on such a host these three
kinds run under the docker runner while the Go plugins run natively.

## Pipeline

- `publish` (push to `main`, or by hand): `catalog plan` lists the
  versions whose tag the registry lacks; a cross-building kind
  builds its platforms and publishes in one job; a native kind builds
  one tree job per platform on that platform's runner, and a publish
  job downloads them once all are built. Publishing is `catalog
  publish`: `pb plugin build` composed from the catalog, then `cosign
  sign --recursive` keyless over the list's digest where no signature
  tag exists yet.
  A published tag is never rebuilt: the plan leaves it out and pb
  refuses to publish over it. pb itself is built from its repository
  at the commit the `PB_COMMIT` variable names.
- `bump` (weekly, or by hand): `catalog bump` appends the versions
  upstream has above each plugin's highest and opens a pull request;
  merging publishes them. Versions below the highest are never
  backfilled by the bump; a version enters by hand.
- `CI` (pull requests): format, vet, the catalog's validation and the
  tests, the live recipe test included.

## Trust

Every list and image is signed by the `publish` workflow's identity.
A pb trust policy accepts the catalog with

```yaml
plugins:
  - prefix: ghcr.io/greatliontech/pb-plugins/
    require: true
    identity:
      san: https://github.com/greatliontech/pb-plugins/.github/workflows/publish.yaml@refs/heads/main
      issuer: https://token.actions.githubusercontent.com
```

## Running the tool by hand

```
go run ./cmd/catalog check
go run ./cmd/catalog plan --all
go run ./cmd/catalog tree protocolbuffers/go v1.36.12 --platform linux/amd64,darwin/arm64 --out trees
go run ./cmd/catalog publish protocolbuffers/go v1.36.12 --trees trees
```

`tree` for a `bazel` recipe builds the host's platform alone;
`publish` needs a tree for every platform the plugin serves, `pb` and
`cosign` on the path, and the registry's credentials in the ambient
credential store.
