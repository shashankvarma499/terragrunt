package mcp

import (
	"errors"

	"github.com/gruntwork-io/terragrunt/internal/experiment"
)

// ErrApplyRequiresExec is returned when the apply and destroy tools are asked
// for without the capability to start the processes an apply is made of.
// Asking for both flags is deliberate. Granting execution as a side effect of
// a flag whose name says nothing about it would be a surprise on the one path
// where surprises are expensive.
var ErrApplyRequiresExec = errors.New(
	"--" + AllowApplyFlagName + " requires --" + AllowFlagName + "=" + string(
		CapabilityExec,
	) + ": applying is running tofu",
)

// ErrExecRequiresFSRW is returned when processes are allowed but the tools are
// still working against a copy of the tree, where a spawned command would
// miss a module downloaded into the copy or a file a generate block wrote
// there.
var ErrExecRequiresFSRW = errors.New(
	"--" + AllowFlagName + "=" + string(
		CapabilityExec,
	) + " requires --" + AllowFlagName + "=" + string(
		CapabilityFSRW,
	) +
		": a spawned command reads the real filesystem, not the copy the tools work against",
)

// ErrAllowCommandRequiresExec is returned when a program is allowed without
// the capability to start any program at all, which would leave the flag
// doing nothing rather than what its name promises.
var ErrAllowCommandRequiresExec = errors.New(
	"--" + AllowCommandFlagName + " requires --" + AllowFlagName + "=" + string(CapabilityExec) +
		": without it nothing runs, allowed or not",
)

// ErrExperimentRequired is returned when the mcp command is run without the
// mcp-command experiment enabled.
var ErrExperimentRequired = errors.New(
	"the '" + CommandName + "' command requires the '" + experiment.MCPCommand + "' experiment to be enabled" +
		" (set --experiment " + experiment.MCPCommand + " or TG_EXPERIMENT=" + experiment.MCPCommand + ")",
)
