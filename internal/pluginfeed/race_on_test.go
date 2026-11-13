//go:build race

package pluginfeed

// raceSlowdown widens wall-clock limits under the race detector, which slows
// every memory access; the plain build keeps the real limit.
const raceSlowdown = 10
