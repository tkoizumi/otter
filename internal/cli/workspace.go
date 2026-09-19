package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/tkoizumi/otter/internal/config"
)

// This file answers the three questions every local command has to answer
// before it can do anything: where is the workspace, what should be scanned for
// manifests, and where does state live.
//
// They used to be answered separately inside each command, which is how
// `otter start` and `otter release` came to disagree about the same workspace:
// start scanned the project root and wrote to .otter/data, release scanned
// ./integrations relative to the working directory and wrote to .otter/data
// only by accident of which directory it happened to be run from. One resolver,
// used by every local command, is the fix.

// workspaceRoot reports the nearest directory carrying a project marker, if
// there is one.
func workspaceRoot() (root string, inProject bool, err error) {
	wd, err := workingDirForTest()
	if err != nil {
		return "", false, err
	}
	root, ok := detectProjectRoot(wd)
	return root, ok, nil
}

// resolveIntegrationsRoot decides which directory a local command scans for
// manifests.
//
// Precedence is the flag, then the workspace root, then the conventional
// ./integrations. The workspace root is what makes a command work wherever an
// integration sits under it -- directly at the root, under integrations/, or
// under any grouping directory -- and the flag is what lets `otter deploy` name
// a path on a host that has no workspace marker of its own.
func resolveIntegrationsRoot(stderr io.Writer, requested string, explicit bool) (string, int) {
	if explicit {
		return requested, 0
	}
	root, inProject, err := workspaceRoot()
	if err != nil {
		fmt.Fprintf(stderr, "otter: cannot determine the working directory: %v\n", err)
		return "", 1
	}
	if inProject {
		return root, 0
	}
	fmt.Fprintf(stderr, "otter: no workspace here (no .otter in this directory or above)\n")
	fmt.Fprintf(stderr, "otter: run this from a workspace, or pass --integrations <dir>\n")
	return "", 2
}

// manifestByName finds the manifest of an integration by the name the runtime
// knows it by, searching the workspace the working directory belongs to.
//
// It is what lets a bare word work in commands that are otherwise path-based,
// such as `otter validate <name>` -- the spelling `otter init` prints.
func manifestByName(name string) (string, bool) {
	if name == "" || looksLikePath(name) {
		return "", false
	}
	root, inProject, err := workspaceRoot()
	if err != nil || !inProject {
		return "", false
	}
	items, err := config.Discover(root)
	if err != nil {
		return "", false
	}
	for _, item := range items {
		if item.ID == name {
			return item.ManifestPath, true
		}
	}
	return "", false
}

// The workspace's live runtime is authoritative when there is one: state is
// written where that daemon reads it, or it is not written at all. Without a
// live runtime the project convention decides, so a workspace that has not been
// started yet still has exactly one place its state will appear. An explicit
// flag keeps working outside a workspace, which is what lets `otter deploy`
// prepare a directory on a host that has no checkout.
func resolveWorkspaceData(stderr io.Writer, requested string, explicit bool) (string, int) {
	root, inProject, err := workspaceRoot()
	if err != nil {
		fmt.Fprintf(stderr, "otter: cannot determine the working directory: %v\n", err)
		return "", 1
	}
	if !inProject {
		if explicit {
			return requested, 0
		}
		fmt.Fprintf(stderr, "otter: no workspace here (no .otter in this directory or above)\n")
		fmt.Fprintf(stderr, "otter: run this from a workspace, or pass --data <dir>\n")
		return "", 2
	}

	convention := filepath.Join(root, stateDirName, "data")
	record := serveDir(root, convention)
	if base, ok := runningURL(record); ok {
		// The data directory the live daemon reads, which is what a release has
		// to match. The record falls back to the convention for a runtime
		// started before the directory was written down.
		served, hasServed := readServeData(record)
		if !hasServed {
			served = convention
		}
		if explicit && !sameDir(requested, served) {
			fmt.Fprintf(stderr, "otter: refusing to use %s\n", requested)
			fmt.Fprintf(stderr, "otter: the runtime serving this workspace reads %s (at %s)\n", served, base)
			fmt.Fprintf(stderr, "otter: state the daemon cannot see is never used; drop --data or fix the runtime\n")
			return "", 1
		}
		return served, 0
	}
	if explicit {
		return requested, 0
	}
	return convention, 0
}
