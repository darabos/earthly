package earthfile2llb

import (
	"context"
	"fmt"

	"github.com/earthly/earthly/util/containerutil"
	"github.com/earthly/earthly/util/gatewaycrafter"
	"github.com/earthly/earthly/util/llbutil/secretprovider"
	"github.com/earthly/earthly/util/platutil"
	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/pkg/errors"

	"github.com/earthly/earthly/ast/spec"
	"github.com/earthly/earthly/buildcontext"
	"github.com/earthly/earthly/buildcontext/provider"
	"github.com/earthly/earthly/cleanup"
	"github.com/earthly/earthly/conslogging"
	"github.com/earthly/earthly/domain"
	"github.com/earthly/earthly/features"
	"github.com/earthly/earthly/states"
	"github.com/earthly/earthly/util/syncutil/semutil"
	"github.com/earthly/earthly/util/syncutil/serrgroup"
	"github.com/earthly/earthly/variables"
)

// ConvertOpt holds conversion parameters.
type ConvertOpt struct {
	// GwClient is the BuildKit gateway client.
	GwClient gwclient.Client
	// Resolver is the build context resolver.
	Resolver *buildcontext.Resolver
	// GlobalImports is a map of imports used to dereference import ref targets, commands, etc.
	GlobalImports map[string]domain.ImportTrackerVal
	// The resolve mode for referenced images (force pull or prefer local).
	ImageResolveMode llb.ResolveMode
	// DockerImageSolverTar is similar to the above solver but it uses a tar
	// file to transfer images. To be deprecated in favor of the local registry version.
	DockerImageSolverTar states.DockerTarImageSolver
	// MultiImageSolver can solve multiple images using a single build
	// request. Primarily used for WITH DOCKER commands.
	MultiImageSolver states.MultiImageSolver
	// CleanCollection is a collection of cleanup functions.
	CleanCollection *cleanup.Collection
	// Visited is a collection of target states which have been converted to LLB.
	// This is used for deduplication and infinite cycle detection.
	Visited *states.VisitedCollection
	// PlatformResolver is a platform resolver, which keeps track of
	// the current platform, the native platform, the user platform, and
	// the default platform.
	PlatformResolver *platutil.Resolver
	// OverridingVars is a collection of build args used for overriding args in the build.
	OverridingVars *variables.Scope
	// A cache for image solves. (maybe dockerTag +) depTargetInputHash -> context containing image.tar.
	SolveCache *states.SolveCache
	// BuildContextProvider is the provider used for local build context files.
	BuildContextProvider *provider.BuildContextProvider
	// MetaResolver is the image meta resolver to use for resolving image metadata.
	MetaResolver llb.ImageMetaResolver
	// CacheImports is a set of docker tags that can be used to import cache. Note that this
	// set is modified by the converter if InlineCache is enabled.
	CacheImports *states.CacheImports
	// UseInlineCache enables the inline caching feature (use any SAVE IMAGE --push declaration as
	// cache import).
	UseInlineCache bool
	// UseFakeDep is an internal feature flag for fake dep.
	UseFakeDep bool
	// AllowLocally is an internal feature flag for controlling if LOCALLY directives can be used.
	AllowLocally bool
	// AllowInteractive is an internal feature flag for controlling if interactive sessions can be initiated.
	AllowInteractive bool
	// EnableInteractiveDebugger is set to true when earthly is run with the --interactive cli flag
	InteractiveDebuggerEnabled bool
	// InteractiveDebuggerDebugLevelLogging controls if debug-level-logging is enabled within the interactive-debugger
	InteractiveDebuggerDebugLevelLogging bool
	// HasDangling represents whether the target has dangling instructions -
	// ie if there are any non-SAVE commands after the first SAVE command,
	// or if the target is invoked via BUILD command (not COPY nor FROM).
	HasDangling bool
	// Console is for logging
	Console conslogging.ConsoleLogger
	// AllowPrivileged is used to allow (or prevent) any "RUN --privileged" or RUNs under a LOCALLY target to be executed,
	// when set to false, it prevents other referenced remote targets from requesting elevated privileges
	AllowPrivileged bool
	// DoSaves controls when SAVE ARTIFACT AS LOCAL, and SAVE IMAGE (to the local docker instance) calls are executed
	// When a SAVE IMAGE --push is encountered, the image may still be pushed to the remote registry (as long as DoPushes=true),
	// but is not exported to the local docker instance.
	DoSaves bool
	// DoPushes controls when a SAVE IMAGE --push, and RUN --push commands are executed;
	// SAVE IMAGE --push ... will still export an image to the local docker instance (as long as DoSaves=true)
	DoPushes bool
	// IsCI determines whether it is running from a CI environment.
	IsCI bool
	// ForceSaveImage is used to force all SAVE IMAGE commands are executed regardless of if they are
	// for a local or remote target; this is to support the legacy behaviour that was first introduced in earthly (up to 0.5)
	// When this is set to false, SAVE IMAGE commands are only executed when DoSaves is true.
	ForceSaveImage bool
	// OnlyFinalTargetImages is used to ignore SAVE IMAGE commands in indirectly referenced targets
	OnlyFinalTargetImages bool
	// Gitlookup is used to attach credentials to GIT CLONE operations
	GitLookup *buildcontext.GitLookup
	// LocalStateCache provides a cache for local pllb.States
	LocalStateCache *LocalStateCache
	// UseLocalRegistry indicates whether the BuildKit-embedded registry can be used for exports.
	UseLocalRegistry bool
	// LocalRegistryAddr is the address of the BuildKit-embedded registry.
	LocalRegistryAddr string

	// Features is the set of enabled features
	Features *features.Features

	// ParallelConversion is a feature flag enabling the parallel conversion algorithm.
	ParallelConversion bool
	// Parallelism is a semaphore controlling the maximum parallelism.
	Parallelism semutil.Semaphore
	// ErrorGroup is a serrgroup used to submit parallel conversion jobs.
	ErrorGroup *serrgroup.Group

	// FeatureFlagOverrides is used to override feature flags that are defined in specific Earthfiles
	FeatureFlagOverrides string
	// Default set of ARGs to make available in Earthfile.
	BuiltinArgs variables.DefaultArgs
	// NoCache sets llb.IgnoreCache before calling StateToRef
	NoCache bool

	// parentDepSub is a channel informing of any new dependencies from the parent.
	parentDepSub chan string // chan of sts IDs.

	// ContainerFrontend is the currently used container frontend, as detected by Earthly at app start. It provides info
	// and access to commands to manipulate the current container frontend.
	ContainerFrontend containerutil.ContainerFrontend

	// waitBlock references the current WAIT/END scope
	waitBlock *waitBlock

	// GlobalWaitBlockFtr, when true, forces all Earthfiles to add entries into the WAIT/END block
	// this is to facilitate de-duplicating code from builder.go
	GlobalWaitBlockFtr bool

	// ExportCoordinator points to the per-connection map used by the builder's onPull callback
	ExportCoordinator *gatewaycrafter.ExportCoordinator

	// LocalArtifactWhiteList points to the per-connection list of seen SAVE ARTIFACT ... AS LOCAL entries
	LocalArtifactWhiteList *gatewaycrafter.LocalArtifactWhiteList

	// InternalSecretStore is a secret store used internally by Earthly.
	// It is mainly used to pass along parameters to buildkit processes without
	// invalidating the cache.
	InternalSecretStore *secretprovider.MutableMapStore

	// TempEarthlyOutDir is a path to a temp dir where artifacts are temporarily saved
	TempEarthlyOutDir func() (string, error)

	// LLBCaps indicates that builder's capabilities
	LLBCaps *apicaps.CapSet

	// MainTargetDetailsFuture is a channel that is used to signal the main target details, once known.
	MainTargetDetailsFuture chan TargetDetails

	// The runner used to execute the target on. This is used only for metadata reporting purposes.
	// May be one of the following:
	// * "local:<hostname>" - local builds
	// * "bk:<buildkit-address>" - remote builds via buildkit
	// * "sat:<org-name>/<sat-name>" - remote builds via satellite
	Runner string
}

// TargetDetails contains details about the target being built.
type TargetDetails struct {
	// ID is the sts ID of the target.
	ID string
	// EarthlyOrgName is the name of the Earthly org.
	EarthlyOrgName string
	// EarthlyProjectName is the name of the Earthly project.
	EarthlyProjectName string
}

// Earthfile2LLB parses a earthfile and executes the statements for a given target.
func Earthfile2LLB(ctx context.Context, target domain.Target, opt ConvertOpt, initialCall bool) (mts *states.MultiTarget, retErr error) {
	if opt.SolveCache == nil {
		opt.SolveCache = states.NewSolveCache()
	}
	if opt.Visited == nil {
		opt.Visited = states.NewVisitedCollection()
	}
	if opt.MetaResolver == nil {
		opt.MetaResolver = NewCachedMetaResolver(opt.GwClient)
	}
	egWait := false
	if opt.ErrorGroup == nil {
		opt.ErrorGroup, ctx = serrgroup.WithContext(ctx)
		egWait = true
		defer func() {
			if retErr == nil {
				return
			}
			if egWait {
				// We haven't waited for the ErrorGroup yet. The ErrorGroup will
				// return the very first error encountered, which may be
				// different than what our error is (our error could be
				// context.Canceled resulted from the cancellation of the
				// ErrorGroup, but not the root cause).
				err2 := opt.ErrorGroup.Err()
				opt.Console.VerbosePrintf("earthfile2llb immediate error: %v", retErr)
				opt.Console.VerbosePrintf("earthfile2llb group error: %v", err2)
				if err2 != nil {
					retErr = err2
					return
				}
			}
		}()
	}
	// Resolve build context.
	bc, err := opt.Resolver.Resolve(ctx, opt.GwClient, opt.PlatformResolver, target)
	if err != nil {
		return nil, errors.Wrapf(err, "resolve build context for target %s", target.String())
	}

	opt.Features = bc.Features
	if initialCall && !bc.Features.ReferencedSaveOnly {
		opt.DoSaves = !target.IsRemote() // legacy mode only saves artifacts that are locally referenced
		opt.ForceSaveImage = true        // legacy mode always saves images regardless of locally or remotely referenced
	}
	opt.PlatformResolver.AllowNativeAndUser = opt.Features.NewPlatform

	wbWait := false
	if opt.waitBlock == nil {
		opt.waitBlock = newWaitBlock()

		// we must call opt.waitBlock.wait(), since we are the creator.
		// unfortunately this must be done before opt.ErrorGroup.Wait() is called (rather than here via a defer),
		// as the ctx would otherwise be canceled.
		wbWait = true
	}

	targetWithMetadata := bc.Ref.(domain.Target)
	sts, found, err := opt.Visited.Add(ctx, targetWithMetadata, opt.PlatformResolver, opt.AllowPrivileged, opt.OverridingVars, opt.parentDepSub)
	if err != nil {
		return nil, err
	}
	if opt.MainTargetDetailsFuture != nil {
		opt.MainTargetDetailsFuture <- TargetDetails{
			ID:                 sts.ID,
			EarthlyOrgName:     bc.EarthlyOrgName,
			EarthlyProjectName: bc.EarthlyProjectName,
		}
		opt.MainTargetDetailsFuture = nil
	}
	if found {
		if opt.DoSaves {
			// Set the do saves flag, in case it was not set before.
			sts.SetDoSaves()
		}
		// This target has already been done.
		return &states.MultiTarget{
			Final:   sts,
			Visited: opt.Visited,
		}, nil
	}
	converter, err := NewConverter(ctx, targetWithMetadata, bc, sts, opt)
	if err != nil {
		return nil, err
	}
	interpreter := newInterpreter(converter, targetWithMetadata, opt.AllowPrivileged, opt.ParallelConversion, opt.Console, opt.GitLookup)
	err = interpreter.Run(ctx, bc.Earthfile)
	if err != nil {
		return nil, err
	}

	mts, err = converter.FinalizeStates(ctx)
	if err != nil {
		return nil, err
	}

	if wbWait {
		err = opt.waitBlock.wait(ctx)
		if err != nil {
			return nil, err
		}
	}

	if egWait {
		egWait = false
		err := opt.ErrorGroup.Wait()
		if err != nil {
			return nil, err
		}
	}
	return mts, nil
}

// GetTargets returns a list of targets from an Earthfile.
// Note that the passed in domain.Target's target name is ignored (only the reference to the Earthfile is used)
func GetTargets(ctx context.Context, resolver *buildcontext.Resolver, gwClient gwclient.Client, target domain.Target) ([]string, error) {
	platr := platutil.NewResolver(platutil.GetUserPlatform())
	bc, err := resolver.Resolve(ctx, gwClient, platr, target)
	if err != nil {
		return nil, errors.Wrapf(err, "resolve build context for target %s", target.String())
	}
	targets := make([]string, 0, len(bc.Earthfile.Targets))
	for _, target := range bc.Earthfile.Targets {
		targets = append(targets, target.Name)
	}
	return targets, nil
}

// GetTargetArgs returns a list of build arguments for a specified target
func GetTargetArgs(ctx context.Context, resolver *buildcontext.Resolver, gwClient gwclient.Client, target domain.Target) ([]string, error) {
	platr := platutil.NewResolver(platutil.GetUserPlatform())
	bc, err := resolver.Resolve(ctx, gwClient, platr, target)
	if err != nil {
		return nil, errors.Wrapf(err, "resolve build context for target %s", target.String())
	}
	var t *spec.Target
	for _, tt := range bc.Earthfile.Targets {
		if tt.Name == target.Target {
			t = &tt
			break
		}
	}
	if t == nil {
		return nil, fmt.Errorf("failed to find %s", target.String())
	}
	var args []string
	for _, stmt := range t.Recipe {
		if stmt.Command != nil && stmt.Command.Name == "ARG" {
			isBase := t.Name == "base"
			// since Arg opts are ignored (and feature flags are not available) we set explicitGlobalArgFlag as false
			explicitGlobal := false
			_, argName, _, err := parseArgArgs(ctx, *stmt.Command, isBase, explicitGlobal)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse ARG arguments %v", stmt.Command.Args)
			}
			args = append(args, argName)
		}
	}
	return args, nil

}
