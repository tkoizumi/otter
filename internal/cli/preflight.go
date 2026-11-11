package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// ConnectionCredential is one connection's system and the credential names it
// provides, as Cloud reports them.
type ConnectionCredential struct {
	System      string
	Name        string
	Credentials []string
}

// credentialPreflight reports which of a job's declared secrets a connection in
// the workspace provides, and which nothing provides.
//
// IT IS A NAME CHECK, NOT AN EXISTENCE CHECK. The platform's secret store is
// write-only, so Cloud cannot say whether a value was ever written — only which
// names the connections cover. A name that no connection provides will resolve
// to nothing in Cloud and fail the run with "missing", and this is the warning
// that turns that failure into a sentence before the deploy is sent.
//
// A job that declares no secrets returns two empty slices: there is nothing to
// warn about, and a warning for every job would train operators to ignore them.
func credentialPreflight(declared []string, connections []ConnectionCredential) (provided, unprovided []string) {
	byName := map[string]string{}
	for _, connection := range connections {
		for _, name := range connection.Credentials {
			label := connection.Name
			if strings.TrimSpace(label) == "" {
				label = connection.System
			}
			if _, exists := byName[name]; !exists {
				byName[name] = label
			}
		}
	}
	seen := map[string]bool{}
	for _, name := range declared {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := byName[name]; ok {
			provided = append(provided, name)
		} else {
			unprovided = append(unprovided, name)
		}
	}
	sort.Strings(provided)
	sort.Strings(unprovided)
	return provided, unprovided
}

// reportCredentialPreflight writes the warning, or nothing when every declared
// secret is covered.
func reportCredentialPreflight(w io.Writer, jobLabel string, provided, unprovided []string) {
	if len(provided) == 0 && len(unprovided) == 0 {
		return
	}
	fmt.Fprintf(w, "otter: credentials for %s\n", jobLabel)
	for _, name := range provided {
		fmt.Fprintf(w, "otter:   %s  provided by a connection in this workspace\n", name)
	}
	for _, name := range unprovided {
		fmt.Fprintf(w, "otter:   %s  NO connection provides this; a run will report it missing\n", name)
	}
	if len(unprovided) > 0 {
		fmt.Fprintf(w, "otter: add the connection in the dashboard, or rename the secret in otter.yaml to the name its connection provides\n")
	}
}
