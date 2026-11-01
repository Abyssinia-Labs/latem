package system

import (
	"bufio"
	"bytes"
	"os"
	"slices"
	"strings"
)

// Linux families: the package managers an install can be written for.
const (
	FamilyArch   = "arch"
	FamilyDebian = "debian"
	FamilyFedora = "fedora"
)

// Host is the operating system, as far as installing something on it goes.
type Host struct {
	OS   string `json:"os"` // "macOS" or "Linux"
	Arch string `json:"arch"`
	// Name is what the system calls itself: "Arch Linux". Empty off Linux.
	Name string `json:"name,omitempty"`
	// Family is the Linux family whose packages it takes, or "" when it is
	// none latem knows, or not Linux.
	Family string `json:"family,omitempty"`
}

// Linux reports whether h is a Linux host.
func (h Host) Linux() bool { return h.OS == "Linux" }

// Mac reports whether h is a Mac.
func (h Host) Mac() bool { return h.OS == "macOS" }

// Host reads this machine's. os-release's ID_LIKE is what makes a derivative
// work: EndeavourOS says arch, Pop!_OS says ubuntu debian, Rocky says rhel
// centos fedora.
func (s *System) Host() Host {
	h := Host{OS: s.goos, Arch: s.goarch}
	switch {
	case s.IsMacOS():
		h.OS = "macOS"
	case s.IsLinux():
		h.OS = "Linux"
		release := s.osRelease()
		h.Name = release["PRETTY_NAME"]
		h.Family = familyOf(release["ID"], release["ID_LIKE"])
	}
	return h
}

func familyOf(id, like string) string {
	names := append([]string{id}, strings.Fields(like)...)
	for _, f := range []struct {
		family string
		names  []string
	}{
		{FamilyArch, []string{"arch"}},
		{FamilyDebian, []string{"debian", "ubuntu"}},
		{FamilyFedora, []string{"fedora", "rhel", "centos"}},
	} {
		if slices.ContainsFunc(names, func(n string) bool { return slices.Contains(f.names, n) }) {
			return f.family
		}
	}
	return ""
}

// osRelease is /etc/os-release, or the file it is usually a link to.
func (s *System) osRelease() map[string]string {
	out := map[string]string{}
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		data, err := os.ReadFile(s.sysPath(p))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
			if ok && !strings.HasPrefix(k, "#") {
				out[k] = strings.Trim(v, `"'`)
			}
		}
		break
	}
	return out
}
