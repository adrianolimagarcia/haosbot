package config

// Timezone detection and validation.
//
// Port of upstream/nanobot/nanobot/config/timezone.py (19 lines), which wraps
// `tzlocal.get_localzone_name()` + `zoneinfo.ZoneInfo`.

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	// Embed the IANA timezone database so LoadLocation behaves identically on
	// hosts with no /usr/share/zoneinfo. Standard library only (Go >= 1.15).
	_ "time/tzdata"
)

// utcAliases mirrors _UTC_ALIASES in config/timezone.py:6-8. tzlocal reports
// several spellings for UTC; nanobot normalizes them all to "UTC".
var utcAliases = map[string]bool{
	"Etc/GMT": true, "Etc/UTC": true, "GMT": true, "GMT0": true,
	"Greenwich": true, "UCT": true, "Universal": true, "Zulu": true,
}

// detectSystemTimezone ports detect_system_timezone (config/timezone.py:11-18):
//
//	try:
//	    timezone = get_localzone_name()
//	    ZoneInfo(timezone)
//	except Exception:
//	    return "UTC"
//	return "UTC" if timezone in _UTC_ALIASES else timezone
//
// get_localzone_name is reimplemented over the same inputs tzlocal reads on
// Linux (tzlocal/unix.py:_get_localzone_name): $TZ, then /etc/timezone and
// /var/db/zoneinfo, then the /etc/localtime symlink. UNVERIFIED: the Windows and
// Termux branches of tzlocal are not reproduced; on those platforms this
// degrades to "UTC" instead of the native zone name.
func detectSystemTimezone() string {
	name := localZoneName()
	if name == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(name); err != nil {
		return "UTC"
	}
	if utcAliases[name] {
		return "UTC"
	}
	return name
}

func localZoneName() string {
	// 1. $TZ, matching tzlocal.utils._tz_name_from_env.
	if tz := os.Getenv("TZ"); tz != "" {
		if strings.HasPrefix(tz, ":") {
			tz = tz[1:]
		}
		if tz != "" && !filepath.IsAbs(tz) {
			if _, err := time.LoadLocation(tz); err == nil {
				return tz
			}
		}
	}

	// 2. /etc/timezone and /var/db/zoneinfo: first usable line.
	for _, p := range []string{"/etc/timezone", "/var/db/zoneinfo"} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.Trim(line, "/ \t\r\n")
			if line == "" {
				continue
			}
			if i := strings.IndexByte(line, ' '); i >= 0 {
				line = line[:i]
			}
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			if line == "" {
				continue
			}
			line = strings.ReplaceAll(line, " ", "_")
			if _, err := time.LoadLocation(line); err == nil {
				return line
			}
		}
	}

	// 3. /etc/localtime symlink: find the first trailing suffix that names a zone.
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		rest := target
		for {
			i := strings.IndexByte(rest, '/')
			if i < 0 {
				break
			}
			rest = rest[i+1:]
			if rest == "" {
				break
			}
			if _, err := time.LoadLocation(rest); err == nil {
				return rest
			}
		}
	}
	return ""
}

// validateTimezone ports the `validate_timezone` field_validator
// (schema.py:175-184): an unknown zone raises ValueError.
func validateTimezone(c *collector, o *jmap, f fieldDef, value string) string {
	if _, err := time.LoadLocation(value); err != nil {
		v, key, ok := f.get(o)
		if !ok {
			return value
		}
		_ = v
		c.add(c.at(o, key), "value_error", "unknown timezone '"+value+"'")
		return value
	}
	return value
}

// ValidTimezone reports whether name is a loadable IANA timezone, matching the
// reference's `ZoneInfo(name)` check. Exported for callers that need the same
// predicate the schema uses.
func ValidTimezone(name string) bool {
	_, err := time.LoadLocation(name)
	return err == nil
}
