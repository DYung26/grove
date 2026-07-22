package pool

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type packageManager struct {
	label   string
	binary  string
	install []string
	update  []string
}

var knownManagers = []packageManager{
	{label: "pacman (Arch)", binary: "pacman", install: []string{"pacman", "-S", "--needed", "--noconfirm"}},
	{label: "apt (Debian/Ubuntu)", binary: "apt-get", install: []string{"apt-get", "install", "-y"}, update: []string{"apt-get", "update"}},
	{label: "dnf (Fedora/RHEL)", binary: "dnf", install: []string{"dnf", "install", "-y"}},
	{label: "zypper (openSUSE)", binary: "zypper", install: []string{"zypper", "--non-interactive", "install"}},
	{label: "apk (Alpine)", binary: "apk", install: []string{"apk", "add"}},
}

var osReleaseIDToBinary = map[string]string{
	"arch":     "pacman",
	"debian":   "apt-get",
	"ubuntu":   "apt-get",
	"fedora":   "dnf",
	"rhel":     "dnf",
	"centos":   "dnf",
	"opensuse": "zypper",
	"alpine":   "apk",
}

func detectPackageManager() (packageManager, error) {
	if binary := binaryFromOSRelease(); binary != "" {
		if mgr, ok := managerByBinary(binary); ok {
			return mgr, nil
		}
	}

	for _, mgr := range knownManagers {
		if _, err := exec.LookPath(mgr.binary); err == nil {
			return mgr, nil
		}
	}

	return promptForManager()
}

func binaryFromOSRelease() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}

	fields := parseOSRelease(string(data))
	for _, key := range []string{"ID", "ID_LIKE"} {
		for _, id := range strings.Fields(fields[key]) {
			if binary, ok := osReleaseIDToBinary[strings.ToLower(id)]; ok {
				return binary
			}
		}
	}
	return ""
}

func parseOSRelease(content string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		fields[key] = strings.Trim(value, `"`)
	}
	return fields
}

func managerByBinary(binary string) (packageManager, bool) {
	for _, mgr := range knownManagers {
		if mgr.binary == binary {
			return mgr, true
		}
	}
	return packageManager{}, false
}

func promptForManager() (packageManager, error) {
	fmt.Println("Couldn't detect your distro's package manager. Pick one:")
	for i, mgr := range knownManagers {
		fmt.Printf("  %d) %s\n", i+1, mgr.label)
	}
	fmt.Print("> ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return packageManager{}, err
	}

	choice, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || choice < 1 || choice > len(knownManagers) {
		return packageManager{}, fmt.Errorf("invalid selection %q", strings.TrimSpace(line))
	}

	return knownManagers[choice-1], nil
}

// ensureCommand checks whether command is on PATH, and installs the
// package that provides it (via the detected package manager) if not.
// overrides lets a specific manager's binary use a different package
// name than genericPkgName, e.g. zypper's "btrfsprogs".
func ensureCommand(command, genericPkgName string, overrides map[string]string) error {
	if _, err := exec.LookPath(command); err == nil {
		return nil
	}

	mgr, err := detectPackageManager()
	if err != nil {
		return err
	}

	pkgName := genericPkgName
	if override, ok := overrides[mgr.binary]; ok {
		pkgName = override
	}

	fmt.Printf("%s not found, installing %s via %s...\n", command, pkgName, mgr.label)
	if len(mgr.update) > 0 {
		if err := runSudo(mgr.update...); err != nil {
			return fmt.Errorf("refresh package index: %w", err)
		}
	}
	args := append(append([]string{}, mgr.install...), pkgName)
	return runSudo(args...)
}
