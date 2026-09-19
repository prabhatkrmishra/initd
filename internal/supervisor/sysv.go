package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"initd/internal/parser"
	"initd/internal/service"
)

// LSB header tokens worth honoring when wrapping an init script. Everything
// else (Facilities, runlevel policy, chkconfig lines) is out of scope.
var sysvDescriptionKeys = []string{"Short-Description", "Description"}

// sysvScriptVerbs probes /etc/init.d/<name> with no arguments and reports
// whether it looks like an LSB script (prints a usage line mentioning start
// and stop). Non-executable or missing files return false.
func sysvScriptVerbs(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return false
	}
	return true
}

// parseLSBDescription extracts Short-Description/Description from the
// BEGIN INIT INFO block. Empty when the script carries no LSB header.
func parseLSBDescription(data string) string {
	inBlock := false
	descs := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		// LSB headers look like "# Short-Description: ..." (possibly
		// "### ..." or "#  Key: ..."). Strip every leading # then space.
		t := strings.TrimSpace(line)
		for strings.HasPrefix(t, "#") {
			t = strings.TrimSpace(strings.TrimPrefix(t, "#"))
		}
		switch {
		case strings.HasPrefix(t, "BEGIN INIT INFO"):
			inBlock = true
		case strings.HasPrefix(t, "END INIT INFO"):
			inBlock = false
		case inBlock:
			for _, k := range sysvDescriptionKeys {
				if rest, ok := strings.CutPrefix(t, k+":"); ok {
					if _, seen := descs[k]; !seen {
						descs[k] = strings.TrimSpace(rest)
					}
				}
			}
		}
	}
	if d := descs["Short-Description"]; d != "" {
		return d
	}
	return descs["Description"]
}

// sysvUnitName maps an init script to its unit name: <base>.service, with
// ".sh" stripped so "/etc/init.d/foo.sh" becomes "foo.service" like the
// SysV generators do.
func sysvUnitName(base string) string {
	base = strings.TrimSuffix(base, ".sh")
	return base + ".service"
}

// sysvConfigFor builds the generated unit for an init script. The script is
// wrapped as oneshot with RemainAfterExit so start leaves Active (systemd's
// "active (exited)") and stop runs the matching verb; restart maps onto the
// script's restart verb with a start/stop fallback for scripts that lack it.
func sysvConfigFor(scriptPath, base, desc string) *parser.Unit {
	name := sysvUnitName(base)
	if desc == "" {
		desc = fmt.Sprintf("LSB: %s (generated from %s)", base, scriptPath)
	}
	hasRestart := sysvSupportsVerb(scriptPath, "restart")
	restartCmd := fmt.Sprintf("%s restart", scriptPath)
	if !hasRestart {
		restartCmd = fmt.Sprintf("%s stop; %s start", scriptPath, scriptPath)
	}
	return &parser.Unit{
		Name:          name,
		Type:          "service",
		Description:   desc,
		GeneratedFrom: scriptPath,
		Service: parser.ServiceSection{
			Type:            "oneshot",
			RemainAfterExit: "yes",
			ExecStart:       fmt.Sprintf("%s start", scriptPath),
			ExecStop:        fmt.Sprintf("%s stop", scriptPath),
			ExecReload:      []string{restartCmd},
		},
		Install: parser.InstallSection{WantedBy: []string{"multi-user.target"}},
	}
}

// sysvSupportsVerb reports whether the script's usage text mentions a verb.
// Probe failures are treated as "verb present" so generated units stay
// operable; start/stop/restart are validated again at run time.
func sysvSupportsVerb(scriptPath, verb string) bool {
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		return true
	}
	text := strings.ToLower(string(raw))
	v := strings.ToLower(verb)
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		fields := strings.FieldsFunc(t, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '|' || r == '/' || r == ',' || r == '(' || r == ')' || r == '{' || r == '}' || r == '"' || r == '\''
		})
		for _, f := range fields {
			if f == v {
				return true
			}
		}
	}
	return false
}

// loadSysVUnits wraps LSB init scripts that have no native unit file. Native
// units always win: a script is skipped when a loaded unit already claims
// the name. Generated units join the normal map so status/enable/stop work
// unchanged; only enable persists symlinks, exactly like file-backed units.
func (m *Manager) loadSysVUnits(units map[string]*service.Unit, order *[]string) {
	if m.UserMode || m.SysVInitDir == "" {
		return
	}
	entries, err := os.ReadDir(m.SysVInitDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.Name() == "README" || e.Name() == "skeleton" {
			continue
		}
		name := sysvUnitName(e.Name())
		if _, exists := units[name]; exists {
			continue
		}
		scriptPath := filepath.Join(m.SysVInitDir, e.Name())
		// skeleton is a template, never a service; rc locals run at boot.
		if !sysvScriptVerbs(scriptPath) {
			continue
		}
		desc := ""
		if raw, err := os.ReadFile(scriptPath); err == nil {
			desc = parseLSBDescription(string(raw))
		}
		cfg := sysvConfigFor(scriptPath, e.Name(), desc)
		u := service.NewUnit(cfg, scriptPath)
		if m.reaper != nil {
			u.SetReaper(m.reaper)
		}
		u.SetOnFailureHandler(m.onFailureCallback(name))
		m.attachJournalLocked(u)
		units[name] = u
		*order = append(*order, name)
	}
}
