// Command unit-caps-gen renders the systemd unit that `otter deploy` would
// write, so scripts/drill/unit-caps.sh installs exactly the generated unit on a
// real systemd host instead of a hand-written copy that can drift from the
// renderer.
//
// It is a drill helper, not part of the shipped runtime: `otter deploy` renders
// the same unit through internal/deploy. Every path it takes is a flag so the
// drill can target its own workspace.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tkoizumi/otter/internal/deploy"
)

func main() {
	remoteDir := flag.String("remote-dir", "/opt/otter", "install root on the host")
	workspace := flag.String("workspace", "drill", "workspace directory name under the install root")
	service := flag.String("service", "otterd-drill", "systemd unit name, without .service")
	user := flag.String("user", "otter", "service account")
	dataDir := flag.String("data-dir", "", "data directory (default <workspace>/.otter/data)")
	listen := flag.String("listen", "127.0.0.1:7337", "API listen address")
	memoryMax := flag.String("memory-max", deploy.DefaultMemoryMax, "MemoryMax= value, or off")
	memoryHigh := flag.String("memory-high", deploy.DefaultMemoryHigh, "MemoryHigh= value, or off")
	cpuQuota := flag.String("cpu-quota", deploy.DefaultCPUQuota, "CPUQuota= value, or off")
	tasksMax := flag.String("tasks-max", deploy.DefaultTasksMax, "TasksMax= value, or off")
	flag.Parse()

	target := deploy.Target{
		RemoteDir:             *remoteDir,
		WorkspaceSlug:         *workspace,
		WorkspaceNameOverride: *workspace,
		ServiceName:           *service,
		RunAsUser:             *user,
		DataDir:               *dataDir,
		Listen:                *listen,
	}
	target.ApplyDefaults()

	opts := deploy.UnitOptions{
		MemoryMax:  *memoryMax,
		MemoryHigh: *memoryHigh,
		CPUQuota:   *cpuQuota,
		TasksMax:   *tasksMax,
	}
	if err := deploy.ValidateUnitPaths(target, nil); err != nil {
		fmt.Fprintln(os.Stderr, "unit-caps-gen:", err)
		os.Exit(1)
	}
	fmt.Print(deploy.UnitFile(target, opts))
}
