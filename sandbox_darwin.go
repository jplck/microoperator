package main

import (
	"fmt"

	nono "github.com/nolabs-ai/nono-go"
)

// configurePlatformSandbox describes the macOS-specific part of the nono profile.
// Go selects this file by its _darwin.go suffix, so the shared launcher can use
// the same function name without mixing Linux and macOS enforcement code.
//
// The worker needs read access to Apple's runtime libraries to start. The extra
// Seatbelt rules below restrict Mach service lookup and POSIX shared-memory IPC,
// which are other ways processes can communicate outside our stdin/stdout pipes.
// These calls only add rules to caps; the later nono.Apply installs the profile.
//
// This preserves the existing sandbox profile, including its known limitation:
// the pinned native core adds a resolver allowance that these extra deny rules do
// not override. This function therefore does not establish strict macOS isolation.
func configurePlatformSandbox(caps *nono.CapabilitySet) error {
	for _, path := range []string{"/System/Library", "/usr/lib"} {
		if err := caps.AllowPath(path, nono.AccessRead); err != nil {
			return fmt.Errorf("grant runtime path %s: %w", path, err)
		}
	}
	// nono retains resolver IPC. These rules only tighten other unused IPC.
	for _, rule := range []string{
		"(deny mach-lookup)",
		"(deny mach-per-user-lookup)",
		"(deny ipc-posix-shm*)",
	} {
		if err := caps.AddPlatformRule(rule); err != nil {
			return fmt.Errorf("tighten sandbox: %w", err)
		}
	}
	return nil
}

// applyPlatformSandbox has no additional macOS mechanism to install: the rules
// above are enforced by the nono.Apply call that immediately follows this hook in
// sandboxExec. On Linux the matching function installs seccomp first.
//
// Returning nil here does not skip nono or provide an unsandboxed fallback.
// The unresolved macOS resolver limitation remains a separate qualification gate.
func applyPlatformSandbox() error { return nil }
