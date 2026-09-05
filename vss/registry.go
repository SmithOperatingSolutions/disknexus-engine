// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package vss

import "sync"

// Active-snapshot registry (#322). A machine that sleeps mid-capture can
// wake with its shadow copies gone (diff-area pressure during the
// suspend/resume writes releases them). The wake handler needs to know WHICH
// snapshot devices this process currently depends on so it can probe them —
// so every Snapshot/Set registers its device paths here for its lifetime.
//
// The registry is deliberately platform-neutral: real entries only ever come
// from Windows (the constructors refuse elsewhere), but the shape runs
// everywhere so the wake handler's probe logic is testable off Windows via
// registerActiveDevices — the same door the constructors use.
var (
	activeMu   sync.Mutex
	activeSets = map[int][]string{}
	activeNext int
)

// registerActiveDevices records a live snapshot's device paths; the returned
// func removes them (idempotent). Exported to tests through the package's
// own constructors on Windows and RegisterActiveDevicesForProbe elsewhere.
func registerActiveDevices(devices []string) func() {
	activeMu.Lock()
	id := activeNext
	activeNext++
	activeSets[id] = append([]string(nil), devices...)
	activeMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			activeMu.Lock()
			delete(activeSets, id)
			activeMu.Unlock()
		})
	}
}

// RegisterActiveDevicesForProbe is registerActiveDevices for callers outside
// the package: the wake-tolerance tests (no Windows, no real VSS) stand in
// for a live capture with it. Production registration happens inside the
// constructors, so a real capture cannot forget to register.
func RegisterActiveDevicesForProbe(devices []string) func() {
	return registerActiveDevices(devices)
}

// ActiveDevicePaths returns every snapshot device this process currently
// holds open, across all live snapshots and sets.
func ActiveDevicePaths() []string {
	activeMu.Lock()
	defer activeMu.Unlock()
	var out []string
	for _, devs := range activeSets {
		out = append(out, devs...)
	}
	return out
}
