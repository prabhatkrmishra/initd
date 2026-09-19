package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"initd/internal/logging"
)

// Output modes. Unknown names fall back to short in the caller so a typo
// degrades to readable output instead of an error.
var outputModes = map[string]bool{
	"short": true, "short-precise": true, "short-iso": true,
	"short-iso-precise": true, "short-full": true, "short-monotonic": true,
	"short-unix": true, "verbose": true, "export": true,
	"json": true, "json-pretty": true, "json-sse": true, "json-seq": true,
	"cat": true, "with-unit": true,
}

// formatEntries renders entries in the requested mode. utc shifts wall-clock
// modes to UTC; noHostname drops the host column; fields limits verbose/
// export/json to the named keys (comma-separated, journal field names).
func formatEntries(entries []logging.StoredEntry, mode string, utc, noHostname bool, fields string) []string {
	if !outputModes[mode] {
		mode = "short"
	}
	want := map[string]bool{}
	if fields != "" {
		for _, f := range strings.Split(fields, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				want[strings.ToUpper(f)] = true
			}
		}
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		switch mode {
		case "cat":
			out = append(out, e.Message)
		case "short-unix":
			out = append(out, fmt.Sprintf("%d %s[%d]: %s", e.RealtimeUsec/1e6, e.Identifier, e.PID, e.Message))
		case "short-monotonic":
			out = append(out, fmt.Sprintf("[%12.6f] %s[%d]: %s", float64(e.MonotonicUsec)/1e6, e.Identifier, e.PID, e.Message))
		case "short-precise":
			out = append(out, fmt.Sprintf("%s %s[%d]: %s", wallString(e, utc, true), e.Identifier, e.PID, e.Message))
		case "short-iso", "short-iso-precise":
			out = append(out, fmt.Sprintf("%s %s[%d]: %s", isoString(e, utc), e.Identifier, e.PID, e.Message))
		case "short-full":
			out = append(out, fmt.Sprintf("%s %s %s[%d]: %s", wallString(e, utc, false), hostString(e, noHostname), e.Identifier, e.PID, e.Message))
		case "with-unit":
			out = append(out, fmt.Sprintf("%s %s[%d]: %s (%s)", wallString(e, utc, false), e.Identifier, e.PID, e.Message, e.Unit))
		case "verbose":
			out = append(out, verboseBlock(e, want, utc))
		case "export":
			out = append(out, exportBlock(e, want))
		case "json", "json-sse", "json-seq":
			out = append(out, jsonLine(e, want, false))
		case "json-pretty":
			out = append(out, jsonLine(e, want, true))
		default: // short
			out = append(out, fmt.Sprintf("%s %s %s[%d]: %s", wallString(e, utc, false), hostString(e, noHostname), e.Identifier, e.PID, e.Message))
		}
	}
	if mode == "json-sse" && len(out) > 0 {
		wrapped := make([]string, 0, len(out)+1)
		for _, l := range out {
			wrapped = append(wrapped, "data: "+l)
		}
		wrapped = append(wrapped, "")
		return wrapped
	}
	if mode == "json-seq" && len(out) > 0 {
		wrapped := make([]string, 0, len(out))
		for _, l := range out {
			wrapped = append(wrapped, "\x1e"+l)
		}
		return wrapped
	}
	return out
}

func wallTime(e logging.StoredEntry, utc bool) time.Time {
	t := time.UnixMicro(e.RealtimeUsec)
	if utc {
		return t.UTC()
	}
	return t.Local()
}

func wallString(e logging.StoredEntry, utc, precise bool) string {
	t := wallTime(e, utc)
	if precise {
		return t.Format("Jan 02 15:04:05.000000")
	}
	return t.Format("Jan 02 15:04:05")
}

func isoString(e logging.StoredEntry, utc bool) string {
	return wallTime(e, utc).Format("2006-01-02T15:04:05-0700")
}

func hostString(e logging.StoredEntry, noHostname bool) string {
	if noHostname || e.Hostname == "" {
		return "-"
	}
	return e.Hostname
}

// entryMap exposes the journal field names for verbose/export/json.
func entryMap(e logging.StoredEntry) map[string]string {
	return map[string]string{
		"__CURSOR":            e.Cursor,
		"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", e.RealtimeUsec),
		"_MONOTONIC_USEC":     fmt.Sprintf("%d", e.MonotonicUsec),
		"_BOOT_ID":            e.BootID,
		"_SYSTEMD_UNIT":       e.Unit,
		"_PID":                fmt.Sprintf("%d", e.PID),
		"PRIORITY":            fmt.Sprintf("%d", e.Priority),
		"SYSLOG_IDENTIFIER":   e.Identifier,
		"_HOSTNAME":           e.Hostname,
		"MESSAGE":             e.Message,
	}
}

func verboseBlock(e logging.StoredEntry, want map[string]bool, utc bool) string {
	m := entryMap(e)
	keys := make([]string, 0, len(m))
	for k := range m {
		if len(want) > 0 && !want[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(wallString(e, utc, false))
	b.WriteString(" ")
	b.WriteString(e.Unit)
	for _, k := range keys {
		b.WriteString("\n    ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(m[k])
	}
	return b.String()
}

func exportBlock(e logging.StoredEntry, want map[string]bool) string {
	m := entryMap(e)
	keys := make([]string, 0, len(m))
	for k := range m {
		if len(want) > 0 && !want[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(m[k])
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return strings.TrimSuffix(b.String(), "\n")
}

func jsonLine(e logging.StoredEntry, want map[string]bool, pretty bool) string {
	m := entryMap(e)
	if len(want) > 0 {
		for k := range m {
			if !want[k] {
				delete(m, k)
			}
		}
	}
	var raw []byte
	var err error
	if pretty {
		raw, err = json.MarshalIndent(m, "", "  ")
	} else {
		raw, err = json.Marshal(m)
	}
	if err != nil {
		return "{}"
	}
	return string(raw)
}
