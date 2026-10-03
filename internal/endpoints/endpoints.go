// Package endpoints names the registries the catalog's tools read —
// one place, so a test points every reader at a fake by one
// variable.
package endpoints

var (
	// Proxy is the Go module proxy.
	Proxy = "https://proxy.golang.org"
	// Maven is Maven Central.
	Maven = "https://repo1.maven.org/maven2"
	// Npm is the npm registry.
	Npm = "https://registry.npmjs.org"
	// Crates is crates.io's API, which answers any spelling of a
	// crate's name with the crate and its canonical name, and serves
	// its archive by redirect under the canonical spelling alone.
	Crates = "https://crates.io"
	// GitHub serves a repository's tag archives (the API is the
	// github package's own).
	GitHub = "https://github.com"
	// Adoptium answers for the Temurin JDK releases: a release's
	// assets per platform, each with its checksum.
	Adoptium = "https://api.adoptium.net"
	// SwiftOrg publishes the Swift releases and, per release, the
	// static Linux SDK's bundle revision and checksum; SwiftDownload
	// serves the bundles.
	SwiftOrg      = "https://www.swift.org"
	SwiftDownload = "https://download.swift.org"
)

// UserAgent names the catalog to a registry that asks every client
// to, crates.io among them.
const UserAgent = "pb-plugins (https://github.com/greatliontech/pb-plugins)"
