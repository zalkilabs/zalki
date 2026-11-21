// Package agent defines contracts for trusted, compiled-in adapters.
// It is not a plugin API or a sandbox. Core's HookHost rejects ids other than
// its nonempty bound actor; this is not a general isolation guarantee.
//
// An adapter must implement Adapter. Every other interface is optional: core
// checks for it with a type assertion and uses the default below when it is
// missing. The descriptor's Lifecycle states two client facts that core reads
// instead of switching on a client name.
//
//	Interface            Core calls it                         Default when missing
//	HookAdapter          every lifecycle hook                  no hooks: the hook command fails
//	HookPayloadAdapter   hook decoding of large payloads       all events use the 8 MiB limit
//	SessionStateAdapter  load and save of adapter state        state is not stored
//	ConfigurationAdapter install and config files              the adapter adds no files
//	LivenessAdapter      session liveness checks               owner process walk
//	NameAdapter          native session names and config dir   no native name
//	HealthAdapter        diagnostics                           no transport or listener health
//	RecoveryHintAdapter  diagnostics hints                     no client-specific hint
//	Listener             cursor-style wake-up listener         sessions never listen
//
// Package agenttest runs a shared conformance check against any adapter.
package agent
