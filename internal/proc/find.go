package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// FindTool is the command-line tool name as a terminal would run it: on
// PATH, else in one of the folders a user's tools are installed in
// (UserBinDirs), else, on Windows, on the PATH the registry has now, which
// a magpie started before the tool was installed doesn't have (#839: the
// Claude sign-in said Claude Code wasn't installed, its installer having
// put claude.exe in ~/.local/bin after magpie started). "" when none.
func FindTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	dirs := UserBinDirs()
	if runtime.GOOS == "windows" {
		dirs = append(dirs, LoginPath()...)
	}
	return toolIn(name, runtime.GOOS, dirs)
}

// SystemBinDirs are the machine's own folders a user's tools are in when
// PATH is a desktop app's: Homebrew's, and /usr/local's for a package
// manager that installs there. launchd's PATH has neither, so a magpie
// started from the Finder still finds a claude one of them holds.
// Windows has none (its registry PATH is read instead). A var so a test
// can point it away from the machine it runs on.
var SystemBinDirs = func() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	return []string{"/opt/homebrew/bin", "/usr/local/bin"}
}

// toolIn is name's program in the first of dirs that has one: on Windows
// name.exe, name.cmd or name, as npm's shim or an installer's binary.
func toolIn(name, goos string, dirs []string) string {
	names := []string{name}
	if goos == "windows" {
		names = []string{name + ".exe", name + ".cmd", name}
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		for _, n := range names {
			p := filepath.Join(d, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}
