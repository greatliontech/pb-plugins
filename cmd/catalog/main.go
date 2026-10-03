// Command catalog drives the plugin catalog's pipeline:
//
//	catalog check                       validate the catalog
//	catalog plan [--all] [--json]       what the registry lacks
//	catalog tree <plugin> <version> --platform <os/arch>[,...] --out <dir>
//	catalog publish <plugin> <version> --trees <dir>
//	catalog bump                        append upstream's new versions
//
// Every command reads the catalog at --catalog, the working directory
// by default.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/pipeline"
	"github.com/greatliontech/pb-plugins/internal/recipe"
	"github.com/greatliontech/pb-plugins/internal/registry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "catalog:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("a command is required: check, plan, tree, publish, bump")
	}
	fs := flag.NewFlagSet("catalog "+args[0], flag.ContinueOnError)
	dir := fs.String("catalog", ".", "the catalog's directory")
	var (
		all       = fs.Bool("all", false, "plan: every version, published or not")
		asJSON    = fs.Bool("json", false, "plan: the matrix as JSON")
		platforms = fs.String("platform", "", "tree: the platforms, comma-separated")
		out       = fs.String("out", "trees", "tree: the directory the trees are written under")
		trees     = fs.String("trees", "trees", "publish: the directory the trees lie under")
		pb        = fs.String("pb", "pb", "publish: the pb executable")
		cosign    = fs.String("cosign", "cosign", "publish: the cosign executable")
	)
	rest, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	c, err := catalog.Load(*dir)
	if err != nil {
		return err
	}
	switch args[0] {
	case "check":
		fmt.Printf("%d plugins, %d versions\n", len(c.Plugins), countVersions(c))
		return nil
	case "plan":
		plan, err := pipeline.Compute(ctx, c, registry.New().Signed, *all)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(plan)
		}
		fmt.Print(plan)
		return nil
	case "tree":
		if len(rest) != 2 || *platforms == "" {
			return fmt.Errorf("tree <plugin> <version> --platform <os/arch>[,...] [--out <dir>]")
		}
		return recipe.Build(ctx, c, rest[0], rest[1], strings.Split(*platforms, ","), *out)
	case "publish":
		if len(rest) != 2 {
			return fmt.Errorf("publish <plugin> <version> [--trees <dir>]")
		}
		p := &pipeline.Publisher{Catalog: c, Registry: registry.New(), PB: *pb, Cosign: *cosign, Trees: *trees, Out: os.Stdout}
		return p.Publish(ctx, rest[0], rest[1])
	case "toolchain":
		if len(rest) != 1 {
			return fmt.Errorf("toolchain <kind>")
		}
		v, ok := c.Toolchains[rest[0]]
		if !ok {
			return fmt.Errorf("no toolchain pinned for %q", rest[0])
		}
		fmt.Println(v)
		return nil
	case "bump":
		added, err := pipeline.Bump(ctx, c, pipeline.Upstream)
		if err != nil {
			return err
		}
		for _, name := range c.Names() {
			if vs := added[name]; len(vs) > 0 {
				fmt.Printf("%s: %s\n", name, strings.Join(vs, " "))
			}
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// parseInterspersed parses flags wherever they stand among the
// positional arguments, which are returned in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func countVersions(c *catalog.Catalog) int {
	n := 0
	for _, p := range c.Plugins {
		n += len(p.Versions)
	}
	return n
}
