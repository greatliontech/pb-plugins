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
  platforms served, and the recipe's parameters; the registry, the
  `base` image, the `toolchains` the pipeline pins by kind (each one
  exact release) and the `sdks` it installs beside them on linux,
  pinned by checksum.
- `plugins/<owner>/<plugin>/versions` — the tags published, one per
  line ascending, in buf's spelling (`v1.36.12`; `v36.2` where
  upstream's releases carry two components). A recipe's own files
  lie under `plugins/<owner>/<plugin>/files`, copied into the source
  tree at the directory the recipe's `files` names
  (`plugins/protocolbuffers/csharp/files/` holds the `cc_binary`
  protobuf does not ship).
- `cmd/catalog` — the tool the workflows run: `check`, `plan`,
  `tree`, `publish`, `bump`; `toolchain`, `target` and `sdk` read
  what a runner installs for a kind.

## Membership

The catalog mirrors buf's registry by owner: every plugin under
`protocolbuffers`, `grpc`, `connectrpc`, `bufbuild`, `grpc-ecosystem`
and `pluginrpc`, and of the rest `apple/swift`,
`community/scalapb-scala`, `community/scalapb-zio-grpc`,
`community/planetscale-vtprotobuf` and the community generators the
`go` and `node` kinds build. A plugin enters when its kind exists,
the kinds below. A plugin that is a program for a runtime rather
than one executable (a jar, a Python package) ships the runtime in
its image beside the program, the image's process the runtime's
launcher over it, never compiled to a native executable here; ScalaPB's protoc-gen-scala
is such a program, served from the jar Maven publishes for every
version, as buf serves it.

## Recipe kinds

| kind | produces | platforms | discovery |
|---|---|---|---|
| `go` | a Go main package cross-compiled with CGO disabled, one host for every platform | all six | the module proxy; the repository's releases where the recipe names a tag |
| `node` | an npm package's executable compiled by bun into one standalone executable per platform, one host for every platform; a package needing its files on disk or a native addon runs under node's own runtime instead (`runtime`, with `script`, the executable's path within the package as its `bin` names it): node's binary bundled for every platform from nodejs.org at the checksums it publishes (`toolchains`), the package and its dependencies under `app/`, the image's process `/node app/node_modules/<package>/<script>`; the install runs on the host, so an addon serves where the package ships it for every platform itself, one fetched per platform at install serving the host alone | all six; a runtime recipe those its package's addons cover | the npm registry |
| `release` | the executable upstream ships prebuilt, one asset per platform: a GitHub release's, or at a URL wherever upstream publishes (Maven Central, a project's binary host), the asset an archive holding it or the executable itself (an executable of its platform and architecture, by its header), a digest upstream publishes beside it verified where it publishes one | the assets upstream ships | the repository's releases, Maven Central's metadata or the npm registry, as the recipe says |
| `bazel` | a C++ target built by bazel on a runner of the platform itself | linux and darwin on both architectures, windows/amd64 (no bazel C++ toolchain is established for windows/arm64) | the repository's releases |
| `swift` | a SwiftPM product built by swift at the repository's tag on a runner of the platform itself, with the toolchain the catalog pins (`toolchains`), with swift.org's static Linux SDK on linux so the executable is static (held to be before it is laid down, its symbols stripped), resolved to the lockfile the package commits where it commits one | linux and darwin on both architectures (no upstream builds its generator on windows) | the repository's releases |
| `dart` | a Dart package's script compiled by `dart compile exe` at the repository's tag on a runner of the platform itself, with the SDK the catalog pins (`toolchains`), its dependencies resolved by pub as the manifest allows | all six | the repository's releases |
| `jvm` | a jar from Maven Central at its coordinates (`maven`, a `classifier` and an `extension` where the file bears them), verified against the digest Maven publishes beside it (`checksum`: its sha256 where it has one, its sha1 for every artifact), bundled with a runtime jlink'd for every platform on one host from the pinned JDK's modules (`toolchains`, Temurin's release, its assets at Adoptium's checksums; `modules` where the kind's own list does not serve), the image's process `/jre/bin/java -jar <jar>` with the jar named relative to the working directory, the tree's root, processes forked rather than spawned through the runtime's helper | all six | Maven Central's metadata |
| `python` | a PyPI package's console script (`pypi`, the package; the entrypoint the script's name among the package's console scripts) run by the standalone CPython build bundled for every platform from python-build-standalone at the release the catalog pins (`toolchains`, CPython's version and the release tag, the assets at the checksums the release publishes), the package and its dependencies installed per platform by uv (`toolchains`, at its published checksum) from PyPI's index under no configuration of the host's, which picks that platform's wheels and reads the dependencies' markers for it, under `app/`; the interpreter laid down as `python/bin/python3` on every platform, no link in the tree; the image's process `/python/bin/python3 -I app/<entrypoint>.py`, the launcher written from the console script's entry | all six; a recipe those its package's and its dependencies' wheels cover | PyPI |
| `rust` | a crate's executable installed by cargo on a runner of the platform itself, with the toolchain the catalog pins (`toolchains`), for the musl target on linux so the executable is static (held to be before it is laid down; a crate whose C dependencies need a musl C toolchain beyond the runner's compiler fails its tree job), locked to the lockfile the crate publishes (one without is refused) | all six | crates.io |

Every kind's trees are held to the plugin protocol before they are
handed on: the tree of the platform the build runs on answers a probe
— one proto3 file with a message pair and a service, the request a
generator acts on — with a response holding a file; a tree that writes
nothing, or no response, or exits non-zero, or answers with an error
of its own, fails the build. A plugin generating only for options the
probe's file lacks — or for an import that carries them — is marked
`silent` in the catalog: its response holds no file, and bytes all
the same (its features, as every generator's framework writes them).
A plugin that refuses to run without a parameter — one naming where
its generated code's types live — names one as the recipe's
`parameter`, which the probe's request carries; it is the probe's
alone, no default a consumer's generation
sees. A go recipe's `tags` are its build tags.
A go recipe whose tags the module proxy does not
list — unprefixed tags, or a nested module the repository releases
under its root's tags — names its `repository`, a `tag` template and
the module's `dir` in the repository (`.` for the root) in place of
a `module`: its versions are the repository's releases, and the
module is fetched at the tag's commit, which the proxy serves as a
pseudo-version, its path read from the `go.mod` there — upstream's
own fact, a nested module's major suffix among it. Such a build
reads the repository through GitHub's API, a token in
`GITHUB_TOKEN` authenticating it where set. A release recipe's
`members` names a platform's member where upstream lays that
platform's archive out differently from the rest. A plugin marked
`frozen` takes no further version from the bump, the name's versions
complete (a generator that left its module, or a name upstream
released under before a move, the catalog serving the new name
beside it); one naming a `line`
(`v1`) takes versions of that major alone, upstream releasing
another line beside it that carries the executable no more
(grpc-swift's 2.x, whose generator moved to grpc-swift-protobuf).

The six platforms are pb's: `linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64`.
A kind lays out, per platform, the tree the image's process runs
from: for most kinds one file, the entrypoint at the tree's root,
which the image carries as `/<entrypoint>`; for the `jvm` kind the
runtime under `jre/` beside the jar, for a runtime `node` recipe
node's binary beside the package under `app/`, for a `python` recipe
the interpreter under `python/` beside the package and its launcher
under `app/`, the image's process the runtime's launcher over it. The Linux
executables of the `node`, `release`, `bazel`, `dart`, `jvm` and
`python` kinds may link the platform's C library (dart's, java's
and python's runtimes do), so their Linux
trees are layered over the catalog's `base` (distroless `cc`, pinned
by index digest and resolved to the platform's image at publish);
the `go`, `rust` and `swift` kinds are static and take no base. On an `OS` sandbox row pb
runs a Linux entrypoint only where it is static, so on such a host
those kinds run under the docker runner while the go, rust and swift
kinds' plugins run natively.
A rust, swift or dart recipe, like a bazel one, builds on a runner
of the platform itself: cargo installs the crate from crates.io,
swift builds the package at its tag, dart compiles the package's
script, for the host alone; a jvm recipe, a runtime node recipe and
a python recipe build every platform on one host: jlink links each
platform's runtime from that platform's modules, node's and
CPython's binaries are prebuilt for every platform, uv installs a
package for a platform other than the host's. The pipeline installs the kind's pinned
toolchain on the runner first (rustup, swiftly, the Dart SDK from
Google's archive at the checksum published beside it, the Temurin
JDK from Adoptium at the checksum it publishes; a python recipe's
interpreter and uv the build fetches for itself, at the checksums
their releases publish, into the user's cache) and, on linux,
what makes the executable static beside it
(cargo's musl target, swift.org's static Linux SDK at the checksum
the catalog pins, held equal to the one swift.org publishes). A
darwin tree is held to bind to the system's libraries or its own —
through a run path relative to the image, as a bundled runtime's
launcher binds its libraries — alone before it is laid down, so a
toolchain's library reached through a search path elsewhere is
refused rather than published to load on the runner alone.

## Pipeline

- `publish` (push to `main`, or by hand): `catalog plan` lists the
  versions the registry holds no signed list for; a cross-building kind
  builds its platforms and publishes in one job; a native kind builds
  one tree job per platform on that platform's runner, and a publish
  job downloads them once all are built. Publishing is `catalog
  publish`: `pb plugin build` composed from the catalog, then `cosign
  sign --recursive` keyless over the list's digest where no signature
  tag exists yet. A published tag is never rebuilt: the plan leaves
  a signed one out, a published but unsigned one (a run that died
  between the two) is signed as it stands, and pb refuses to publish
  over a tag in any case; a published tree is the pipeline's at its
  publish, so a rule the pipeline gains later reaches the versions
  published after it. pb itself is built from its repository at the
  commit the `PB_COMMIT` variable names.
- `bump` (weekly, or by hand): `catalog bump` appends the versions
  upstream has above each plugin's highest and opens a pull request;
  merging publishes them. Versions below the highest are never
  backfilled by the bump; a version enters by hand.
- `CI` (pull requests): format, vet, the catalog's validation and the
  tests, the live recipe tests included on every row, each building
  its own platform's trees.

Every fetch the tool makes (an upstream's archive, its metadata, a
published checksum) is repeated where upstream refuses it for the
moment — 429, or a 5xx — up to five times, after the wait the server
names where it names one within a minute (a longer ask is final),
else doubling from a second; any other refusal, and a request that
gets no answer at all, is final at once, so a throttled runner
builds and publishes what a later request answers and a missing
asset is reported as missing.

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
