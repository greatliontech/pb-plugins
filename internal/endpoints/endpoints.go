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
)

// UserAgent names the catalog to a registry that asks every client
// to, crates.io among them.
const UserAgent = "pb-plugins (https://github.com/greatliontech/pb-plugins)"
