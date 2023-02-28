package run

import (
	gocontext "context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/hashicorp/go-hclog"
	"github.com/mitchellh/cli"
	"github.com/pkg/errors"
	"github.com/pyr-sh/dag"
	"github.com/vercel/turbo/cli/internal/cache"
	"github.com/vercel/turbo/cli/internal/cmdutil"
	"github.com/vercel/turbo/cli/internal/colorcache"
	"github.com/vercel/turbo/cli/internal/core"
	"github.com/vercel/turbo/cli/internal/graph"
	"github.com/vercel/turbo/cli/internal/logstreamer"
	"github.com/vercel/turbo/cli/internal/nodes"
	"github.com/vercel/turbo/cli/internal/packagemanager"
	"github.com/vercel/turbo/cli/internal/process"
	"github.com/vercel/turbo/cli/internal/runcache"
	"github.com/vercel/turbo/cli/internal/spinner"
	"github.com/vercel/turbo/cli/internal/taskhash"
	"github.com/vercel/turbo/cli/internal/turbopath"
	"github.com/vercel/turbo/cli/internal/ui"
)

// RealRun executes a set of tasks
func RealRun(
	ctx gocontext.Context,
	g *graph.CompleteGraph,
	rs *runSpec,
	engine *core.Engine,
	taskHashTracker *taskhash.Tracker,
	turboCache cache.Cache,
	packagesInScope []string,
	base *cmdutil.CmdBase,
	summary *dryRunSummary,
	packageManager *packagemanager.PackageManager,
	processes *process.Manager,
	runState *RunState,
) error {
	singlePackage := rs.Opts.runOpts.singlePackage

	if singlePackage {
		base.UI.Output(fmt.Sprintf("%s %s", ui.Dim("• Running"), ui.Dim(ui.Bold(strings.Join(rs.Targets, ", ")))))
	} else {
		base.UI.Output(fmt.Sprintf(ui.Dim("• Packages in scope: %v"), strings.Join(packagesInScope, ", ")))
		base.UI.Output(fmt.Sprintf("%s %s %s", ui.Dim("• Running"), ui.Dim(ui.Bold(strings.Join(rs.Targets, ", "))), ui.Dim(fmt.Sprintf("in %v packages", rs.FilteredPkgs.Len()))))
	}

	// Log whether remote cache is enabled
	useHTTPCache := !rs.Opts.cacheOpts.SkipRemote
	if useHTTPCache {
		base.UI.Info(ui.Dim("• Remote caching enabled"))
	} else {
		base.UI.Info(ui.Dim("• Remote caching disabled"))
	}

	defer func() {
		_ = spinner.WaitFor(ctx, turboCache.Shutdown, base.UI, "...writing to cache...", 1500*time.Millisecond)
	}()
	colorCache := colorcache.New()

	runCache := runcache.New(turboCache, base.RepoRoot, rs.Opts.runcacheOpts, colorCache)

	ec := &execContext{
		colorCache:      colorCache,
		runState:        runState,
		rs:              rs,
		ui:              &cli.ConcurrentUi{Ui: base.UI},
		runCache:        runCache,
		logger:          base.Logger,
		packageManager:  packageManager,
		processes:       processes,
		taskHashTracker: taskHashTracker,
		repoRoot:        base.RepoRoot,
		isSinglePackage: singlePackage,
	}

	// run the thing
	execOpts := core.EngineExecutionOptions{
		Parallel:    rs.Opts.runOpts.parallel,
		Concurrency: rs.Opts.runOpts.concurrency,
	}

	taskSummaryMap := map[string]taskSummary{}

	execFunc := func(ctx gocontext.Context, packageTask *nodes.PackageTask) error {
		// COPY PASTE FROM DRY RUN!
		deps := engine.TaskGraph.DownEdges(packageTask.TaskID)

		passThroughArgs := rs.ArgsForTask(packageTask.Task)
		hash, err := taskHashTracker.CalculateTaskHash(packageTask, deps, base.Logger, passThroughArgs)
		expandedInputs := taskHashTracker.GetExpandedInputs(packageTask)
		envPairs := taskHashTracker.HashableEnvPairs[packageTask.TaskID]
		framework := taskHashTracker.PackageTaskFramework[packageTask.TaskID]
		if err != nil {
			fmt.Printf("Warning: error with collecting task summary: %s", err)
		}
		itemStatus, err := turboCache.Exists(hash)
		if err != nil {
			fmt.Printf("Warning: error with collecting task summary: %s", err)
		}
		command := "<NONEXISTENT>"
		if packageTask.Command != "" {
			command = packageTask.Command
		}
		ancestors, err := engine.GetTaskGraphAncestors(packageTask.TaskID)
		if err != nil {
			fmt.Printf("Warning: error with collecting task summary: %s", err)
		}
		descendents, err := engine.GetTaskGraphDescendants(packageTask.TaskID)
		if err != nil {
			fmt.Printf("Warning: error with collecting task summary: %s", err)
		}

		ts := taskSummary{
			TaskID:                 packageTask.TaskID,
			Task:                   packageTask.Task,
			Package:                packageTask.PackageName,
			Hash:                   hash,
			CacheState:             itemStatus,
			Command:                command,
			Dir:                    packageTask.Dir,
			Outputs:                packageTask.TaskDefinition.Outputs.Inclusions,
			ExcludedOutputs:        packageTask.TaskDefinition.Outputs.Exclusions,
			LogFile:                packageTask.LogFile,
			Dependencies:           ancestors,
			Dependents:             descendents,
			ResolvedTaskDefinition: packageTask.TaskDefinition,
			ExpandedInputs:         expandedInputs,
			Environment:            envPairs,
			Framework:              framework,
		}
		// End DRY RUN STOLEN

		// deps here are passed in to calculate the task hash
		expandedOutputs, err := ec.exec(ctx, packageTask, deps)
		if err != nil {
			return err
		}

		ts.ExpandedOutputs = expandedOutputs
		taskSummaryMap[packageTask.TaskID] = ts
		return nil
	}

	visitorFn := g.GetPackageTaskVisitor(ctx, execFunc)
	errs := engine.Execute(visitorFn, execOpts)

	// Track if we saw any child with a non-zero exit code
	exitCode := 0
	exitCodeErr := &process.ChildExit{}

	for _, err := range errs {
		if errors.As(err, &exitCodeErr) {
			if exitCodeErr.ExitCode > exitCode {
				exitCode = exitCodeErr.ExitCode
			}
		} else if exitCode == 0 {
			// We hit some error, it shouldn't be exit code 0
			exitCode = 1
		}
		base.UI.Error(err.Error())
	}

	// We gathered the info as a map, but we want to attach it as an array
	for _, s := range taskSummaryMap {
		summary.Tasks = append(summary.Tasks, s)
	}

	runState.mu.Lock()
	for taskID, state := range runState.state {
		t, ok := taskSummaryMap[taskID]
		if ok {
			t.RunSummary = state
		}
	}

	if err := runState.Close(base.UI); err != nil {
		return errors.Wrap(err, "error with profiler")
	}

	if exitCode != 0 {
		return &process.ChildExit{
			ExitCode: exitCode,
		}
	}

	summary.ExitCode = exitCode
	rendered, err := renderDryRunFullJSON(summary, singlePackage)
	if err != nil {
		return err
	}
	base.UI.Output(rendered)

	return nil
}

type execContext struct {
	colorCache      *colorcache.ColorCache
	runState        *RunState
	rs              *runSpec
	ui              cli.Ui
	runCache        *runcache.RunCache
	logger          hclog.Logger
	packageManager  *packagemanager.PackageManager
	processes       *process.Manager
	taskHashTracker *taskhash.Tracker
	repoRoot        turbopath.AbsoluteSystemPath
	isSinglePackage bool
}

func (ec *execContext) logError(log hclog.Logger, prefix string, err error) {
	ec.logger.Error(prefix, "error", err)

	if prefix != "" {
		prefix += ": "
	}

	ec.ui.Error(fmt.Sprintf("%s%s%s", ui.ERROR_PREFIX, prefix, color.RedString(" %v", err)))
}

func (ec *execContext) exec(ctx gocontext.Context, packageTask *nodes.PackageTask, deps dag.Set) (*runcache.ExpandedOutputs, error) {
	cmdTime := time.Now()

	progressLogger := ec.logger.Named("")
	progressLogger.Debug("start")

	// Setup tracer
	tracer := ec.runState.Run(packageTask.TaskID)

	passThroughArgs := ec.rs.ArgsForTask(packageTask.Task)
	hash, err := ec.taskHashTracker.CalculateTaskHash(packageTask, deps, ec.logger, passThroughArgs)
	ec.logger.Debug("task hash", "value", hash)
	if err != nil {
		ec.ui.Error(fmt.Sprintf("Hashing error: %v", err))
		// @TODO probably should abort fatally???
	}
	// TODO(gsoltis): if/when we fix https://github.com/vercel/turbo/issues/937
	// the following block should never get hit. In the meantime, keep it after hashing
	// so that downstream tasks can count on the hash existing
	//
	// bail if the script doesn't exist
	if packageTask.Command == "" {
		progressLogger.Debug("no task in package, skipping")
		progressLogger.Debug("done", "status", "skipped", "duration", time.Since(cmdTime))
		return nil, nil
	}

	var prefix string
	var prettyPrefix string
	if ec.rs.Opts.runOpts.logPrefix == "none" {
		prefix = ""
	} else {
		prefix = packageTask.OutputPrefix(ec.isSinglePackage)
	}

	prettyPrefix = ec.colorCache.PrefixWithColor(packageTask.PackageName, prefix)

	// Cache ---------------------------------------------
	taskCache := ec.runCache.TaskCache(packageTask, hash)
	// Create a logger for replaying
	prefixedUI := &cli.PrefixedUi{
		Ui:           ec.ui,
		OutputPrefix: prettyPrefix,
		InfoPrefix:   prettyPrefix,
		ErrorPrefix:  prettyPrefix,
		WarnPrefix:   prettyPrefix,
	}
	hit, err := taskCache.RestoreOutputs(ctx, prefixedUI, progressLogger)
	if err != nil {
		prefixedUI.Error(fmt.Sprintf("error fetching from cache: %s", err))
	} else if hit {
		tracer(TargetCached, nil)
		return nil, nil
	}

	// Setup command execution
	argsactual := append([]string{"run"}, packageTask.Task)
	if len(passThroughArgs) > 0 {
		// This will be either '--' or a typed nil
		argsactual = append(argsactual, ec.packageManager.ArgSeparator...)
		argsactual = append(argsactual, passThroughArgs...)
	}

	cmd := exec.Command(ec.packageManager.Command, argsactual...)
	cmd.Dir = packageTask.Pkg.Dir.ToSystemPath().RestoreAnchor(ec.repoRoot).ToString()
	envs := fmt.Sprintf("TURBO_HASH=%v", hash)
	cmd.Env = append(os.Environ(), envs)

	// Setup stdout/stderr
	// If we are not caching anything, then we don't need to write logs to disk
	// be careful about this conditional given the default of cache = true
	writer, err := taskCache.OutputWriter(prettyPrefix)
	if err != nil {
		tracer(TargetBuildFailed, err)
		ec.logError(progressLogger, prettyPrefix, err)
		if !ec.rs.Opts.runOpts.continueOnError {
			os.Exit(1)
		}
	}

	// Create a logger
	logger := log.New(writer, "", 0)
	// Setup a streamer that we'll pipe cmd.Stdout to
	logStreamerOut := logstreamer.NewLogstreamer(logger, prettyPrefix, false)
	// Setup a streamer that we'll pipe cmd.Stderr to.
	logStreamerErr := logstreamer.NewLogstreamer(logger, prettyPrefix, false)
	cmd.Stderr = logStreamerErr
	cmd.Stdout = logStreamerOut
	// Flush/Reset any error we recorded
	logStreamerErr.FlushRecord()
	logStreamerOut.FlushRecord()

	closeOutputs := func() error {
		var closeErrors []error

		if err := logStreamerOut.Close(); err != nil {
			closeErrors = append(closeErrors, errors.Wrap(err, "log stdout"))
		}
		if err := logStreamerErr.Close(); err != nil {
			closeErrors = append(closeErrors, errors.Wrap(err, "log stderr"))
		}

		if err := writer.Close(); err != nil {
			closeErrors = append(closeErrors, errors.Wrap(err, "log file"))
		}
		if len(closeErrors) > 0 {
			msgs := make([]string, len(closeErrors))
			for i, err := range closeErrors {
				msgs[i] = err.Error()
			}
			return fmt.Errorf("could not flush log output: %v", strings.Join(msgs, ", "))
		}
		return nil
	}

	// Run the command
	if err := ec.processes.Exec(cmd); err != nil {
		// close off our outputs. We errored, so we mostly don't care if we fail to close
		_ = closeOutputs()
		// if we already know we're in the process of exiting,
		// we don't need to record an error to that effect.
		if errors.Is(err, process.ErrClosing) {
			return nil, nil
		}
		tracer(TargetBuildFailed, err)
		progressLogger.Error(fmt.Sprintf("Error: command finished with error: %v", err))
		if !ec.rs.Opts.runOpts.continueOnError {
			prefixedUI.Error(fmt.Sprintf("ERROR: command finished with error: %s", err))
			ec.processes.Close()
		} else {
			prefixedUI.Warn("command finished with error, but continuing...")
		}

		// If there was an error, flush the buffered output
		taskCache.OnError(prefixedUI, progressLogger)

		return nil, err
	}

	var expandedOutputs runcache.ExpandedOutputs

	duration := time.Since(cmdTime)
	// Close off our outputs and cache them
	if err := closeOutputs(); err != nil {
		ec.logError(progressLogger, "", err)
	} else {
		if expandedOutputs, err = taskCache.SaveOutputs(ctx, progressLogger, prefixedUI, int(duration.Milliseconds())); err != nil {
			ec.logError(progressLogger, "", fmt.Errorf("error caching output: %w", err))
		}
	}

	// Clean up tracing
	tracer(TargetBuilt, nil)
	progressLogger.Debug("done", "status", "complete", "duration", duration)
	return &expandedOutputs, nil
}
