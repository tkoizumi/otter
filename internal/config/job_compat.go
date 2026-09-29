package config

import "os"

// DefaultJobsDirectory preserves an existing default directory in place. Moving
// it would change canonical paths and could retire existing job identities.
//
// This is deliberately the only compatibility retained for the old terminology:
// the historical --integrations flag and the OTTER_INTEGRATIONS_DIR variable are
// no longer read, because no installed unit or project file is known to depend
// on them.
func DefaultJobsDirectory() string {
	if _, err := os.Stat(DefaultJobs); os.IsNotExist(err) {
		if info, err := os.Stat("./integrations"); err == nil && info.IsDir() {
			return "./integrations"
		}
	}
	return DefaultJobs
}
