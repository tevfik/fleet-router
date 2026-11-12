// Package stagemod is the public face of fleet's stage-runtime extension point: what an adapter module (an
// accelerator vendor's runtime, an exotic board farm...) imports to plug into `fleet deploy --engine chain
// --proto <name>` without living inside the fleet repository.
//
// A module is a Go package whose init() calls Register. The fleet binary links it in with one blank import
// (see docs/extending.md, `make ext`); nothing in the fleet core refers to any module by name.
//
// The types are aliases of the ones the planner uses, so a module sees exactly what the core does. What a
// module must honour is the Chain Stage Protocol (CSP1, fleet-engine docs): the planner only decides layer
// ranges, ports and environment; the runtime behind the module has to speak CSP1 between its stages.
package stagemod

import (
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/hostinfo"
)

type (
	// Module is one pluggable runtime: the strings of its binaries (Adapter) and, when its machines are not
	// reached over ssh, the Runner that starts and watches the stages.
	Module = deploy.StageModule
	// Adapter names the runtime's binaries, ports, paths and environment variables.
	Adapter = deploy.StagePipeAdapter
	// Launch is one stage of a pipeline: what it holds, how to start it, how to tell it is up.
	Launch = deploy.StageLaunch
	// Readiness says how a stage is probed (tcp or http).
	Readiness = deploy.Readiness
	// Runner does what a start plan asks of one machine: Cleanup, Launch, WaitReady, Exists.
	Runner = deploy.StageRunner
)

// Register adds a module (call it from init). A second module with the same name panics.
func Register(m Module) { deploy.RegisterStageModule(m) }

// Lookup finds a registered module.
func Lookup(name string) (Module, bool) { return deploy.LookupStageModule(name) }

// Names lists the registered modules, sorted.
func Names() []string { return deploy.StageModuleNames() }

// Accelerator is a device with a memory pool of its own (an NPU) that the module's hardware has and fleet does not
// detect by itself.
type Accelerator = hostinfo.Accelerator

// RegisterAccelerator adds a probe for such a device (call it from init, like Register). What it finds is counted
// by the benchmark (fast memory, read bandwidth) and named in the hardware fingerprint.
func RegisterAccelerator(probe func() (Accelerator, bool)) { hostinfo.RegisterAccelerator(probe) }
