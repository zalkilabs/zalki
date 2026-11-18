package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"sync/atomic"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type pluginRetirementWitness struct {
	Event        string            `json:"event"`
	Before       pluginMemberGuard `json:"before"`
	After        pluginMemberGuard `json:"after"`
	Digest       string            `json:"digest"`
	BeforeDigest string            `json:"before_digest"`
}

type pluginRetirementRecord struct {
	root, session string
	witness       pluginRetirementWitness
	policy        sessionPluginPolicy
}

// Optional process-private proof, with a fixed bound across ALL roots. Hash
// collisions/eviction/restart simply lose the optimization. Native hot writes
// add no IO, probe, mutex or durable field: bounded hashes/one atomic slot on validated retirement.
var pluginRetirements [128]atomic.Pointer[pluginRetirementRecord]

func retirementSlot(root, session string) *atomic.Pointer[pluginRetirementRecord] {
	digest := sha256.Sum256([]byte(root + "\x00" + session))
	return &pluginRetirements[int(digest[0])%len(pluginRetirements)]
}

func discardPluginRetirement(root, session string) {
	slot := retirementSlot(root, session)
	if previous := slot.Load(); previous != nil && previous.root == root && previous.session == session {
		slot.CompareAndSwap(previous, nil)
	}
}

func rememberPluginRetirement(root string, prior []byte, next model.Session, event model.Event, put journal.Put) {
	if !next.Retired || event.Session.Cause == model.CauseOwnerExited || event.Type != model.EventSessionStateChanged || !event.Session.Retired || event.Session.PreviousRetired {
		return
	}
	var before model.Session
	if json.Unmarshal(prior, &before) != nil || before.Retired || before.ID != next.ID || before.Generation != next.Generation || !samePluginOwner(before.Owner, next.Owner) {
		return
	}
	old, oldValid := decodeSessionPluginPolicy(before)
	current, currentValid := decodeSessionPluginPolicy(next)
	if !oldValid || !currentValid || !directRetirementPolicyChange(old, current) {
		return
	}
	oldGuard := guardPluginMember(&pluginMemberEvidence{record: before, policy: old, policyValid: true})
	newGuard := guardPluginMember(&pluginMemberEvidence{record: next, policy: current, policyValid: true})
	beforeDigest := sha256.Sum256(prior)
	proof := pluginRetirementWitness{Event: event.ID, Before: oldGuard, After: newGuard, Digest: put.SHA256, BeforeDigest: hex.EncodeToString(beforeDigest[:])}
	retirementSlot(root, next.ID).Store(&pluginRetirementRecord{root: root, session: next.ID, witness: proof, policy: current})
}

func currentPluginRetirement(root, session, plugin string, entry *pluginMemberEvidence) *pluginRetirementWitness {
	proof := retirementSlot(root, session).Load()
	if proof == nil || proof.root != root || proof.session != session || !entry.record.Retired || !entry.policyValid || !sessionPolicyAllowsPlugin(proof.policy, true, plugin) || !sessionPolicyAllowsPlugin(entry.policy, true, plugin) {
		return nil
	}
	if guardPluginMember(entry) != proof.witness.After || hex.EncodeToString(entry.digest[:]) != proof.witness.Digest {
		return nil
	}
	proofCopy := proof.witness
	return &proofCopy
}

func directRetirementPolicyChange(before, after sessionPluginPolicy) bool {
	return before.LastChange.ID != after.LastChange.ID && after.LastChange.Plugin == "" && after.LastChange.Muted == nil && before.Mode == after.Mode && before.InvalidEnv == after.InvalidEnv && slices.Equal(before.Allow, after.Allow) && maps.Equal(before.Muted, after.Muted)
}
