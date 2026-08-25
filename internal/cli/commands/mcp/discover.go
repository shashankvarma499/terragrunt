package mcp

import (
	"context"
	"io"

	"github.com/gruntwork-io/terragrunt/internal/component"
	"github.com/gruntwork-io/terragrunt/internal/discovery"
	"github.com/gruntwork-io/terragrunt/internal/tf"
	"github.com/gruntwork-io/terragrunt/internal/venv"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type discoverInput struct {
	WorkingDir   string   `json:"working_dir,omitempty"  jsonschema:"Directory to discover under, relative to the server root. Defaults to the server root."`
	Filter       []string `json:"filter,omitempty"       jsonschema:"Terragrunt filter queries (e.g. './stage/**', 'name=vpc') to narrow discovery."`
	Dependencies bool     `json:"dependencies,omitempty" jsonschema:"Parse configurations and include dependency edges and exclude-block results (slower)."`
	Hidden       bool     `json:"hidden,omitempty"       jsonschema:"Include units in hidden directories."`
}

type discoveredComponent struct {
	Path         string   `json:"path"`
	Dependencies []string `json:"dependencies,omitempty"`
	Excluded     bool     `json:"excluded,omitempty"`
}

type discoverOutput struct {
	WorkingDir string                `json:"working_dir"`
	Units      []discoveredComponent `json:"units"`
	Stacks     []discoveredComponent `json:"stacks,omitempty"`
	Degraded   []string              `json:"degraded,omitempty"`
	UnitCount  int                   `json:"unit_count"`
}

func registerDiscover(srv *mcp.Server, l log.Logger, d *serverDeps, rootVenv *venv.Venv) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "discover",
		Description: "List every Terragrunt unit and stack under a directory. Use this first to map an " +
			"unfamiliar Terragrunt tree, or to resolve a filter expression into concrete unit paths. Set " +
			"dependencies=true only when you need the dependency graph or exclude-block results; it parses " +
			"every configuration and is slower. 'excluded' reflects plan-command exclude blocks and is only " +
			"populated when dependencies=true. Results may carry a 'degraded' list when the server is " +
			"running without --allow=exec.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  new(d.reachesNetwork()),
		},
	}, parseToolHandler(l, d, "discover", rootVenv, runDiscover))
}

func runDiscover(
	ctx context.Context,
	l log.Logger,
	d *serverDeps,
	rootVenv *venv.Venv,
	input discoverInput,
) (discoverOutput, error) {
	dir, err := resolveWorkingDir(rootVenv.FS, d.launchDir, input.WorkingDir)
	if err != nil {
		return discoverOutput{}, err
	}

	opts, env, err := buildDirOptions(l, d, rootVenv, dir, input.Filter)
	if err != nil {
		return discoverOutput{}, err
	}

	// Exclude-block evaluation matches actions against TerraformCommand
	// (an empty command never matches), so evaluate as a plan run.
	opts.TerraformCommand = tf.CommandNamePlan

	cv, err := d.callVenv(rootVenv, env, io.Discard)
	if err != nil {
		return discoverOutput{}, err
	}

	ctx = freshCallContext(ctx)

	disc, err := discovery.NewForDiscoveryCommand(l, cv.FS, &discovery.DiscoveryCommandOptions{
		WorkingDir:        dir,
		DiscoveryBoundary: opts.DiscoveryBoundary,
		Filters:           opts.Filters,
		NoHidden:          !input.Hidden,
		WithRequiresParse: input.Dependencies,
		WithRelationships: input.Dependencies,
	})
	if err != nil {
		return discoverOutput{}, err
	}

	w, cleanup, err := setupGitFilterWorktrees(ctx, l, d, cv, opts)
	if err != nil {
		return discoverOutput{}, err
	}

	if w != nil {
		defer cleanup()

		disc = disc.WithWorktrees(w)
	}

	components, degraded := discoverDegraded(ctx, l, cv, opts, disc, "discover")

	out := discoverOutput{
		WorkingDir: dir,
		Units:      make([]discoveredComponent, 0, len(components)),
	}

	for _, c := range components {
		dc := discoveredComponent{
			Path: discovery.RelPathForComponent(l, c, dir, c.Path(), "component"),
		}

		if input.Dependencies {
			for _, dep := range c.Dependencies() {
				dc.Dependencies = append(
					dc.Dependencies,
					discovery.RelPathForComponent(l, dep, dir, dep.Path(), "dependency"),
				)
			}
		}

		switch c := c.(type) {
		case *component.Unit:
			dc.Excluded = c.Excluded()

			out.Units = append(out.Units, dc)
		case *component.Stack:
			out.Stacks = append(out.Stacks, dc)
		}
	}

	degraded = append(degraded, d.rec.notes()...)

	out.UnitCount = len(out.Units)
	out.Degraded = degraded

	return out, nil
}
