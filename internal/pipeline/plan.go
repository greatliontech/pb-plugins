// Package pipeline is the publishing pipeline's logic: the plan of
// what to build, the publish of one plugin version through `pb plugin
// build` and cosign, and the bump discovering upstream versions. The
// workflows call each through the catalog command and carry no logic
// of their own.
package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
)

// runners are the GitHub-hosted runners a native recipe builds on,
// per platform; a cross-building recipe builds every platform on
// CrossRunner.
var runners = map[string]string{
	"linux/amd64":   "ubuntu-24.04",
	"linux/arm64":   "ubuntu-24.04-arm",
	"darwin/amd64":  "macos-15-intel",
	"darwin/arm64":  "macos-15",
	"windows/amd64": "windows-2025",
	"windows/arm64": "windows-11-arm",
}

// CrossRunner builds every platform of a cross-building recipe.
const CrossRunner = "ubuntu-24.04"

// Tree is one tree job: the platforms one runner builds for one
// plugin version, and the artifact name its trees upload under.
type Tree struct {
	Plugin    string `json:"plugin"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Platforms string `json:"platforms"`
	Runner    string `json:"runner"`
	Build     string `json:"build"`
	Tree      string `json:"tree"`
	// Toolchain is the pinned toolchain the kind's build needs on the
	// runner, empty where the runner's own serves (the target the
	// runner adds beside it is the host's, read there).
	Toolchain string `json:"toolchain"`
}

// Build is one publish job: a plugin version whose trees are all
// built, packaged into one list.
type Build struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Build   string `json:"build"`
}

// Matrix is a GitHub Actions matrix over its entries.
type Matrix[T any] struct {
	Include []T `json:"include"`
}

// Plan is what a run builds and publishes: Cross the cross-building
// kinds' jobs, each building every platform and publishing at once;
// Trees the native kinds' one-platform jobs and Builds their publish
// jobs.
type Plan struct {
	Cross  Matrix[Tree]  `json:"cross"`
	Trees  Matrix[Tree]  `json:"trees"`
	Builds Matrix[Build] `json:"builds"`
	// Any reports whether there is anything to do, for the workflow's
	// job conditions.
	Any bool `json:"any"`
}

// Exists reports whether a reference's tag is already published and
// signed; one published but unsigned is not done.
type Exists func(ctx context.Context, reference string) (bool, error)

// Compute plans every plugin version the registry lacks a signed
// list for, or every version where all is set: a native kind one tree job per platform
// on its runner and one build, another kind one cross job for all
// platforms on CrossRunner. A registry that cannot answer fails the
// plan, so nothing is silently skipped.
func Compute(ctx context.Context, c *catalog.Catalog, exists Exists, all bool) (*Plan, error) {
	plan := &Plan{}
	for _, name := range c.Names() {
		p := c.Plugins[name]
		for _, v := range p.Versions {
			ref := c.Reference(name, v)
			if !all {
				ok, err := exists(ctx, ref)
				if err != nil {
					return nil, err
				}
				if ok {
					continue
				}
			}
			build := BuildID(name, v)
			if p.Kind.Native() {
				plan.Builds.Include = append(plan.Builds.Include, Build{Plugin: name, Version: v, Build: build})
				for _, pl := range p.PlatformsOf() {
					os, arch := catalog.SplitPlatform(pl)
					plan.Trees.Include = append(plan.Trees.Include, Tree{
						Plugin: name, Version: v, Kind: string(p.Kind), Platforms: pl,
						Runner: runners[pl], Build: build, Tree: build + "-" + os + "-" + arch,
						Toolchain: c.Installed(p.Kind),
					})
				}
			} else {
				plan.Cross.Include = append(plan.Cross.Include, Tree{
					Plugin: name, Version: v, Kind: string(p.Kind), Platforms: strings.Join(p.PlatformsOf(), ","),
					Runner: CrossRunner, Build: build, Tree: build + "-cross",
					Toolchain: c.Installed(p.Kind),
				})
			}
		}
	}
	plan.Any = len(plan.Builds.Include)+len(plan.Cross.Include) > 0
	return plan, nil
}

// BuildID names a plugin version in artifact names: the name's slash
// a hyphen, so `protocolbuffers/go` at `v1.36.12` is
// `protocolbuffers-go-v1.36.12`.
func BuildID(name, version string) string {
	return strings.ReplaceAll(name, "/", "-") + "-" + version
}

// String renders the plan for a reader.
func (p *Plan) String() string {
	var b strings.Builder
	for _, t := range p.Cross.Include {
		fmt.Fprintf(&b, "cross %s on %s: %s\n", t.Build, t.Runner, t.Platforms)
	}
	for _, t := range p.Trees.Include {
		fmt.Fprintf(&b, "tree %s on %s: %s\n", t.Tree, t.Runner, t.Platforms)
	}
	for _, bd := range p.Builds.Include {
		fmt.Fprintf(&b, "build %s\n", bd.Build)
	}
	if !p.Any {
		b.WriteString("nothing to build: every version is published\n")
	}
	return b.String()
}
