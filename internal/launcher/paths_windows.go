//go:build windows

package launcher

import (
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
)

// taskFolder is the Task Scheduler folder under which the worker
// task lives. Folder paths use backslashes per Task Scheduler's
// addressing convention.
const taskFolder = `\Semantica\`

// UnitPath returns the path to the Task Scheduler XML definition.
// Task Scheduler does not require the XML file to live in any
// specific filesystem location - schtasks /Create /XML reads it as
// a one-time import, and the registered task is stored internally
// by the OS. The launcher keeps the XML alongside other semantica
// state so reinstalls and uninstalls have a deterministic file to
// rewrite or remove.
func UnitPath() (string, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, LabelWorker+".xml"), nil
}

// UserDomain returns the empty string on Windows. Task Scheduler
// has no analog to launchctl's gui/<uid> tuple; the task name
// (UnitTarget) is the access point for schtasks.exe.
func UserDomain() string {
	return ""
}

// UnitTarget returns the Task Scheduler task name. schtasks.exe
// accepts this string as the /TN argument. Folder + label form;
// addressing convention is backslash-separated.
func UnitTarget() string {
	return taskFolder + LabelWorker
}
