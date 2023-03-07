package run

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/vercel/turbo/cli/internal/env"
	"github.com/vercel/turbo/cli/internal/fs"
	"github.com/vercel/turbo/cli/internal/globby"
	"github.com/vercel/turbo/cli/internal/hashing"
	"github.com/vercel/turbo/cli/internal/lockfile"
	"github.com/vercel/turbo/cli/internal/packagemanager"
	"github.com/vercel/turbo/cli/internal/turbopath"
	"github.com/vercel/turbo/cli/internal/util"
)

const _globalCacheKey = "Buffalo buffalo Buffalo buffalo buffalo buffalo Buffalo buffalo"

// Variables that we always include
var _defaultEnvVars = []string{
	"VERCEL_ANALYTICS_ID",
}

var _emptyGlobalHashable = struct {
	globalFileHashMap    map[turbopath.AnchoredUnixPath]string
	rootExternalDepsHash string
	hashedSortedEnvPairs []string
	globalCacheKey       string
	pipeline             fs.PristinePipeline
}{}

func calculateGlobalHash(rootpath turbopath.AbsoluteSystemPath, rootPackageJSON *fs.PackageJSON, pipeline fs.Pipeline, envVarDependencies []string, globalFileDependencies []string, packageManager *packagemanager.PackageManager, lockFile lockfile.Lockfile, logger hclog.Logger) (struct {
	globalFileHashMap    map[turbopath.AnchoredUnixPath]string
	rootExternalDepsHash string
	hashedSortedEnvPairs []string
	globalCacheKey       string
	pipeline             fs.PristinePipeline
}, error) {
	// Calculate env var dependencies
	envVars := []string{}
	envVars = append(envVars, envVarDependencies...)
	envVars = append(envVars, _defaultEnvVars...)
	globalHashableEnvVars := env.GetHashableGlobalENVVars(envVars, []string{"THASH"})
	globalHashableEnvNames := globalHashableEnvVars.All.Names()
	globalHashableEnvPairs := globalHashableEnvVars.All.ToHashable()
	logger.Debug("global hash env vars", "vars", globalHashableEnvNames)

	// Calculate global file dependencies
	globalDeps := make(util.Set)
	if len(globalFileDependencies) > 0 {
		ignores, err := packageManager.GetWorkspaceIgnores(rootpath)
		if err != nil {
			return _emptyGlobalHashable, err
		}

		f, err := globby.GlobFiles(rootpath.ToStringDuringMigration(), globalFileDependencies, ignores)
		if err != nil {
			return _emptyGlobalHashable, err
		}

		for _, val := range f {
			globalDeps.Add(val)
		}
	}

	if lockFile == nil {
		// If we don't have lockfile information available, add the specfile and lockfile to global deps
		globalDeps.Add(filepath.Join(rootpath.ToStringDuringMigration(), packageManager.Specfile))
		globalDeps.Add(filepath.Join(rootpath.ToStringDuringMigration(), packageManager.Lockfile))
	}

	// No prefix, global deps already have full paths
	globalDepsArray := globalDeps.UnsafeListOfStrings()
	globalDepsPaths := make([]turbopath.AbsoluteSystemPath, len(globalDepsArray))
	for i, path := range globalDepsArray {
		globalDepsPaths[i] = turbopath.AbsoluteSystemPathFromUpstream(path)
	}

	globalFileHashMap, err := hashing.GetHashableDeps(rootpath, globalDepsPaths)
	if err != nil {
		return struct {
			globalFileHashMap    map[turbopath.AnchoredUnixPath]string
			rootExternalDepsHash string
			hashedSortedEnvPairs []string
			globalCacheKey       string
			pipeline             fs.PristinePipeline
		}{}, fmt.Errorf("error hashing files: %w", err)
	}

	globalHashable := struct {
		globalFileHashMap    map[turbopath.AnchoredUnixPath]string
		rootExternalDepsHash string
		hashedSortedEnvPairs []string
		globalCacheKey       string
		pipeline             fs.PristinePipeline
	}{
		globalFileHashMap:    globalFileHashMap,
		rootExternalDepsHash: rootPackageJSON.ExternalDepsHash,
		hashedSortedEnvPairs: globalHashableEnvPairs,
		globalCacheKey:       _globalCacheKey,
		pipeline:             pipeline.Pristine(),
	}

	return globalHashable, nil
}

// getHashableTurboEnvVarsFromOs returns a list of environment variables names and
// that are safe to include in the global hash
func getHashableTurboEnvVarsFromOs(env []string) ([]string, []string) {
	var justNames []string
	var pairs []string
	for _, e := range env {
		kv := strings.SplitN(e, "=", 2)
		if strings.Contains(kv[0], "THASH") {
			justNames = append(justNames, kv[0])
			pairs = append(pairs, e)
		}
	}
	return justNames, pairs
}
