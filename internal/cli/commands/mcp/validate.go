package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/gruntwork-io/terragrunt/internal/component"
	"github.com/gruntwork-io/terragrunt/internal/configbridge"
	"github.com/gruntwork-io/terragrunt/internal/discovery"
	"github.com/gruntwork-io/terragrunt/internal/venv"
	"github.com/gruntwork-io/terragrunt/internal/vfs"
	"github.com/gruntwork-io/terragrunt/internal/vhttp"
	"github.com/gruntwork-io/terragrunt/internal/view/diagnostic"
	"github.com/gruntwork-io/terragrunt/pkg/config"
	"github.com/gruntwork-io/terragrunt/pkg/config/hclparse"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/gruntwork-io/terragrunt/pkg/options"
	"github.com/hashicorp/hcl/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type validateInput struct {
	WorkingDir string   `json:"working_dir,omitempty" jsonschema:"Directory whose Terragrunt configurations to validate. Defaults to the server root."`
	Unit       string   `json:"unit,omitempty"        jsonschema:"Path of a single unit or stack to validate, relative to working_dir. When set, discovery is skipped and filter must be empty."`
	Filter     []string `json:"filter,omitempty"      jsonschema:"Terragrunt filter queries to narrow which units are validated."`
}

type validateDiagnostic struct {
	File     string `json:"file,omitempty"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
	Line     int    `json:"line,omitempty"`
}

type validateOutput struct {
	Diagnostics  []validateDiagnostic `json:"diagnostics,omitempty"`
	ParseErrors  []string             `json:"parse_errors,omitempty"`
	Degraded     []string             `json:"degraded,omitempty"`
	ErrorCount   int                  `json:"error_count"`
	WarningCount int                  `json:"warning_count"`
	Valid        bool                 `json:"valid"`
}

func registerValidate(srv *mcp.Server, l log.Logger, d *serverDeps, rootVenv *venv.Venv) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "validate",
		Description: "Check every Terragrunt configuration under a directory for HCL syntax and evaluation " +
			"errors, or a single unit via the optional 'unit' argument. Use after editing terragrunt.hcl " +
			"files, before running plan. Returns structured diagnostics (file/line/severity/summary) plus " +
			"parse_errors for failures that have no source location, and error_count/warning_count totals. " +
			"A configuration with findings is still a successful call: read valid and the arrays, valid=true " +
			"means no findings. Failures caused by the server running without --allow=exec are reported in " +
			"the degraded list, not as configuration errors.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  new(d.reachesNetwork()),
		},
	}, parseToolHandler(l, d, "validate", rootVenv, runValidate))
}

func runValidate(
	ctx context.Context,
	l log.Logger,
	d *serverDeps,
	rootVenv *venv.Venv,
	input validateInput,
) (validateOutput, error) {
	if input.Unit != "" && len(input.Filter) > 0 {
		return validateOutput{}, errors.New(
			"the 'unit' and 'filter' arguments are mutually exclusive",
		)
	}

	dir, err := resolveWorkingDir(rootVenv.FS, d.launchDir, input.WorkingDir)
	if err != nil {
		return validateOutput{}, err
	}

	opts, env, err := buildDirOptions(l, d, rootVenv, dir, input.Filter)
	if err != nil {
		return validateOutput{}, err
	}

	// Mirrors the hcl validate command: nothing should stream while
	// diagnostics are being collected.
	opts.SkipOutput = true

	if d.allowExec {
		// The full parse's dependency-output fetches drive real runs into
		// the shared per-unit cache dirs.
		release, err := d.acquireRunSlot(ctx)
		if err != nil {
			return validateOutput{}, err
		}

		defer release()
	}

	cv, err := d.callVenv(rootVenv, env, io.Discard)
	if err != nil {
		return validateOutput{}, err
	}

	ctx = freshCallContext(ctx)

	var (
		diags    diagnostic.Diagnostics
		degraded []string
	)

	diagHandler := hclparse.WithDiagnosticsHandler(
		func(file *hcl.File, hclDiags hcl.Diagnostics) (hcl.Diagnostics, error) {
			for _, hclDiag := range hclDiags {
				// Only report diagnostics located in the file being parsed, not
				// errors bubbled up from dependencies or other files.
				if hclDiag.Subject != nil && file != nil &&
					hclDiag.Subject.Filename != file.Body.MissingItemRange().Filename {
					continue
				}

				newDiag := diagnostic.NewDiagnostic(file, hclDiag)
				if !validateDiagsContain(diags, newDiag) {
					diags = append(diags, newDiag)
				}
			}

			return nil, nil
		},
	)

	var parseErrs []error

	if input.Unit == "" {
		parseErrs, degraded, err = validateDiscoveredTree(ctx, l, d, cv, opts, dir, diagHandler)
		if err != nil {
			return validateOutput{}, err
		}
	}

	if input.Unit != "" {
		parseErrs, err = validateSingleTarget(
			ctx,
			l,
			d,
			cv,
			rootVenv,
			opts,
			dir,
			input.Unit,
			diagHandler,
		)
		if err != nil {
			return validateOutput{}, err
		}
	}

	out := validateOutput{}

	for _, diag := range diags {
		vd := validateDiagnostic{
			Severity: diag.Severity.String(),
			Summary:  diag.Summary,
			Detail:   diag.Detail,
		}

		if diag.Range != nil {
			vd.File = discovery.RelPathOrAbs(l, dir, diag.Range.Filename, "diagnostic file")
			vd.Line = diag.Range.Start.Line
		}

		switch vd.Severity {
		case diagnostic.DiagnosticSeverityError:
			out.ErrorCount++
		case diagnostic.DiagnosticSeverityWarning:
			out.WarningCount++
		}

		out.Diagnostics = append(out.Diagnostics, vd)
	}

	slices.SortFunc(out.Diagnostics, func(a, b validateDiagnostic) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line))
	})

	for _, parseErr := range parseErrs {
		// Exec-denied failures are sandbox artifacts, not configuration
		// bugs: surface them as degradation so agents don't chase them.
		if errors.Is(parseErr, ErrExecDenied) {
			degraded = append(
				degraded,
				fmt.Sprintf("parse degraded by disabled subprocess execution: %v", parseErr),
			)

			continue
		}

		if errors.Is(parseErr, ErrSopsDenied) {
			degraded = append(
				degraded,
				fmt.Sprintf("parse degraded by disabled SOPS decryption: %v", parseErr),
			)

			continue
		}

		if errors.Is(parseErr, vhttp.ErrNoNetwork) {
			degraded = append(
				degraded,
				fmt.Sprintf("parse degraded by disabled outbound HTTP: %v", parseErr),
			)

			continue
		}

		out.ParseErrors = append(out.ParseErrors, parseErr.Error())
	}

	degraded = append(degraded, d.rec.notes()...)

	out.ErrorCount += len(out.ParseErrors)
	out.Degraded = degraded
	out.Valid = len(out.Diagnostics) == 0 && len(out.ParseErrors) == 0

	return out, nil
}

// validateDiscoveredTree discovers every unit and stack under dir and parses
// each one with the collecting diagnostics handler, returning per-component
// parse errors and degradation notes. A non-nil error means nothing was
// validated at all.
func validateDiscoveredTree(
	ctx context.Context,
	l log.Logger,
	d *serverDeps,
	v *venv.Venv,
	opts *options.TerragruntOptions,
	dir string,
	diagHandler hclparse.Option,
) ([]error, []string, error) {
	disc, err := discovery.NewForHCLCommand(l, v.FS, discovery.HCLCommandOptions{
		WorkingDir:        dir,
		DiscoveryBoundary: opts.DiscoveryBoundary,
		Filters:           opts.Filters,
	})
	if err != nil {
		return nil, nil, err
	}

	w, cleanup, err := setupGitFilterWorktrees(ctx, l, d, v, opts)
	if err != nil {
		return nil, nil, err
	}

	if w != nil {
		defer cleanup()

		disc = disc.WithWorktrees(w)
	}

	components, discErr := disc.Discover(ctx, l, v, opts)
	if discErr != nil && len(components) == 0 {
		return nil, nil, discErr
	}

	var degraded []string

	if discErr != nil {
		l.Debugf("validate tool: suppressed discovery errors: %v", discErr)

		degraded = append(
			degraded,
			fmt.Sprintf("discovery reported suppressed errors: %v", discErr),
		)
	}

	var parseErrs []error

	for _, c := range components {
		if _, ok := c.(*component.Stack); ok {
			parseErrs = append(
				parseErrs,
				validateStackDir(ctx, l, v, opts, c.Path(), diagHandler)...)

			continue
		}

		if unitErr := validateUnitDir(ctx, l, v, opts, c.Path(), diagHandler); unitErr != nil {
			parseErrs = append(parseErrs, unitErr)
		}
	}

	return parseErrs, degraded, nil
}

// validateSingleTarget validates exactly one directory named by the unit
// argument (relative to the tool call's working dir), bypassing discovery.
// The directory may hold a unit config, a stack config, or both.
func validateSingleTarget(
	ctx context.Context,
	l log.Logger,
	d *serverDeps,
	v *venv.Venv,
	rootVenv *venv.Venv,
	opts *options.TerragruntOptions,
	dir, unit string,
	diagHandler hclparse.Option,
) ([]error, error) {
	unitPath := unit
	if !filepath.IsAbs(unitPath) {
		unitPath = filepath.Join(dir, unitPath)
	}

	targetDir, err := resolveWorkingDir(rootVenv.FS, d.launchDir, unitPath)
	if err != nil {
		return nil, err
	}

	hasUnit, err := vfs.FileExists(
		rootVenv.FS,
		filepath.Join(targetDir, config.DefaultTerragruntConfigPath),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"checking for %s in %s: %w",
			config.DefaultTerragruntConfigPath,
			targetDir,
			err,
		)
	}

	hasStack, err := vfs.FileExists(rootVenv.FS, filepath.Join(targetDir, config.DefaultStackFile))
	if err != nil {
		return nil, fmt.Errorf("checking for %s in %s: %w", config.DefaultStackFile, targetDir, err)
	}

	if !hasUnit && !hasStack {
		return nil, fmt.Errorf(
			"unit %q: no %s or %s found in %s",
			unit,
			config.DefaultTerragruntConfigPath,
			config.DefaultStackFile,
			targetDir,
		)
	}

	var parseErrs []error

	if hasUnit {
		if unitErr := validateUnitDir(ctx, l, v, opts, targetDir, diagHandler); unitErr != nil {
			parseErrs = append(parseErrs, unitErr)
		}
	}

	if hasStack {
		parseErrs = append(parseErrs, validateStackDir(ctx, l, v, opts, targetDir, diagHandler)...)
	}

	return parseErrs, nil
}

// validateUnitDir fully parses one unit's terragrunt.hcl with the collecting
// diagnostics handler attached.
func validateUnitDir(
	ctx context.Context,
	l log.Logger,
	v *venv.Venv,
	opts *options.TerragruntOptions,
	unitDir string,
	diagHandler hclparse.Option,
) error {
	parseOpts := opts.Clone()
	parseOpts.WorkingDir = unitDir
	parseOpts.TerragruntConfigPath = filepath.Join(unitDir, config.DefaultTerragruntConfigPath)

	_, pctx := configbridge.NewParsingContext(ctx, l, v, parseOpts)

	// Unlike the hcl validate CLI (which replaces the parser options with
	// just its handler), retain the defaults so strict-control diagnostics
	// handling stays active, and stack the collector on top. Do not "fix"
	// this back to the replace form.
	_, err := config.ReadTerragruntConfig(ctx, l, pctx, append(pctx.ParserOptions, diagHandler))

	return err
}

// validateStackDir parses one stack's terragrunt.stack.hcl (values file,
// stack config, and autoinclude validation) with the collecting diagnostics
// handler attached. Later steps are skipped once the stack file itself fails
// to parse.
func validateStackDir(
	ctx context.Context,
	l log.Logger,
	v *venv.Venv,
	opts *options.TerragruntOptions,
	stackDir string,
	diagHandler hclparse.Option,
) []error {
	stackFilePath := filepath.Join(stackDir, config.DefaultStackFile)

	parseOpts := opts.Clone()
	parseOpts.WorkingDir = stackDir
	parseOpts.TerragruntConfigPath = stackFilePath

	sctx, parser := configbridge.NewParsingContext(ctx, l, v, parseOpts)

	var parseErrs []error

	values, err := config.ReadValues(sctx, parser, l, stackDir)
	if err != nil {
		parseErrs = append(parseErrs, err)
	}

	// Retain the default parser options on top of the collector; see the
	// matching comment in validateUnitDir.
	parser = parser.WithParseOption(append(parser.ParserOptions, diagHandler))
	if values != nil {
		parser = parser.WithValues(values)
	}

	file, err := hclparse.NewParser(parser.ParserOptions...).ParseFromFile(v.FS, stackFilePath)
	if err != nil {
		return append(parseErrs, err)
	}

	stackCfg, err := config.ParseStackConfig(sctx, l, parser, file, values)
	if err != nil {
		return append(parseErrs, err)
	}

	// The lenient stack decode above leaves autoinclude blocks unvalidated,
	// so run the strict autoinclude parse `stack generate` uses.
	if err := config.ValidateStackAutoIncludes(
		sctx,
		l,
		parser,
		stackFilePath,
		stackCfg,
		values,
	); err != nil {
		parseErrs = append(parseErrs, err)
	}

	return parseErrs
}

// validateDiagsContain reports whether diags already holds an equivalent
// diagnostic. It replaces [diagnostic.Diagnostics.Contains], which
// dereferences a nil Range on already-collected entries; diagnostics without
// a source range are deduplicated by summary and detail instead.
func validateDiagsContain(diags diagnostic.Diagnostics, find *diagnostic.Diagnostic) bool {
	for _, diag := range diags {
		if find.Range == nil || diag.Range == nil {
			if find.Range == nil && diag.Range == nil && find.Summary == diag.Summary &&
				find.Detail == diag.Detail {
				return true
			}

			continue
		}

		if find.Range.String() == diag.Range.String() {
			return true
		}
	}

	return false
}
