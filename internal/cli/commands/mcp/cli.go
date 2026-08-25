// Package mcp provides the `terragrunt mcp` command, which serves Terragrunt
// operations to AI agents over the Model Context Protocol.
//
// The server denies every capability in [allCapabilities] and grants each
// on its own. Without --allow=exec it spawns nothing at all, because every
// tool call runs against an exec handle that starts no process. With it, the
// handle allows the commands Terragrunt runs itself, listed in
// [execAllowedBinaries], plus whatever --allow-command named. A program a
// configuration names beyond those is refused, and the parse-only tools offer
// it for approval through [runWithCommandApproval] rather than answering
// without it. Without --allow=http every request Terragrunt itself makes
// fails, without --allow=sops nothing is decrypted, and without
// --dangerously-allow-apply there is no apply or destroy tool to offer. A
// result that a denial affected carries a "degraded" list naming what was
// substituted.
//
// The filesystem is bounded for writes and not for reads. Without
// --allow=fs-rw each call works against a copy of the tree, so what it writes
// never reaches the working directory, but an HCL function such as file()
// reads the real disk either way and can reach anything the invoking user can.
// What bounds a tool call is [resolveWorkingDir], and that governs the
// working_dir argument rather than what the parse then reaches for.
package mcp

import (
	"context"

	"github.com/gruntwork-io/terragrunt/internal/cli/flags"
	"github.com/gruntwork-io/terragrunt/internal/clihelper"
	"github.com/gruntwork-io/terragrunt/internal/experiment"
	"github.com/gruntwork-io/terragrunt/internal/venv"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/gruntwork-io/terragrunt/pkg/options"
)

const (
	// CommandName is the name of the mcp command.
	CommandName = "mcp"

	// AllowFlagName is the name of the flag that grants a capability the
	// server otherwise denies.
	AllowFlagName = "allow"

	// AllowCommandFlagName is the name of the flag that adds a program a
	// configuration may run, on top of Terragrunt's own commands.
	AllowCommandFlagName = "allow-command"

	// AllowApplyFlagName is the name of the flag that adds the apply and
	// destroy tools, which change real infrastructure.
	AllowApplyFlagName = "dangerously-allow-apply"
)

// NewFlags returns the flags for the mcp command.
func NewFlags(opts *Options, prefix flags.Prefix) clihelper.Flags {
	tgPrefix := prefix.Prepend(flags.TgPrefix)

	return clihelper.Flags{
		flags.NewFlag(&clihelper.SliceFlag[string]{
			Name:        AllowFlagName,
			EnvVars:     tgPrefix.EnvVars(AllowFlagName),
			Destination: &opts.Allow,
			Usage:       "Grant a capability the server otherwise denies. Repeatable.",
		}),
		flags.NewFlag(&clihelper.SliceFlag[string]{
			Name:        AllowCommandFlagName,
			EnvVars:     tgPrefix.EnvVars(AllowCommandFlagName),
			Destination: &opts.AllowCommands,
			Usage:       "Allow MCP tools to run a program a configuration names, such as one a run_cmd or a hook calls. Repeatable. Requires --allow=exec.",
		}),
		flags.NewFlag(&clihelper.BoolFlag{
			Name:        AllowApplyFlagName,
			EnvVars:     tgPrefix.EnvVars(AllowApplyFlagName),
			Destination: &opts.AllowApply,
			Usage:       "Serve apply and destroy tools, which change and tear down real infrastructure. Each run is gated on a person accepting it in your MCP client. Requires --allow=exec.",
		}),
	}
}

// NewCommand returns the mcp command, which serves Terragrunt operations over
// the Model Context Protocol on stdio.
func NewCommand(l log.Logger, opts *options.TerragruntOptions, v *venv.Venv) *clihelper.Command {
	cmdOpts := NewOptions(opts)

	return &clihelper.Command{
		Name:  CommandName,
		Usage: "Serve Terragrunt operations to AI agents over the Model Context Protocol (stdio).",
		Flags: NewFlags(cmdOpts, nil),
		Before: func(_ context.Context, _ *clihelper.Context) error {
			if !opts.Experiments.Evaluate(experiment.MCPCommand) {
				return clihelper.NewExitError(ErrExperimentRequired, clihelper.ExitCodeGeneralError)
			}

			granted, err := ParseCapabilities(cmdOpts.Allow)
			if err != nil {
				return clihelper.NewExitError(err, clihelper.ExitCodeGeneralError)
			}

			if cmdOpts.AllowApply && !granted.Has(CapabilityExec) {
				return clihelper.NewExitError(ErrApplyRequiresExec, clihelper.ExitCodeGeneralError)
			}

			if granted.Has(CapabilityExec) && !granted.Has(CapabilityFSRW) {
				return clihelper.NewExitError(ErrExecRequiresFSRW, clihelper.ExitCodeGeneralError)
			}

			if len(cmdOpts.AllowCommands) > 0 && !granted.Has(CapabilityExec) {
				return clihelper.NewExitError(
					ErrAllowCommandRequiresExec,
					clihelper.ExitCodeGeneralError,
				)
			}

			// RunAction's auto-provider-cache setup probes a real
			// `tofu version` before the Action runs, so it spawns with the
			// unmodified venv and would hand that subprocess the client's
			// request stream as its stdin.
			opts.NoAutoProviderCacheDir = true

			return nil
		},
		Action: func(ctx context.Context, _ *clihelper.Context) error {
			return Run(ctx, l, v, cmdOpts)
		},
	}
}
