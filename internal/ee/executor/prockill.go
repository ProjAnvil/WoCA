package executor

// The kill ladder's pure half. Docker has no "kill one exec" API and closing
// the attach connection does not stop the exec'd process — so every process
// the docker driver starts is wrapped in a self-recording shell that writes
// its pid to /tmp before exec'ing the real command, and Kill targets that
// pidfile with a second exec. Both argv constructors live here, docker-free,
// so plain tests can pin the exact command lines (shell-injection safety
// included).

// pidfilePath is the container-side pidfile for a session id.
func pidfilePath(sid string) string {
	return "/tmp/.woca-ses-" + sid + ".pid"
}

// WrapArgv wraps argv in a self-recording shell:
//
//	sh -c 'echo $$ > /tmp/.woca-ses-<sid>.pid; exec "$@"' sh <argv…>
//
// The sh writes its pid (which "exec $@" inherits, since exec replaces the
// process image without forking), then becomes the real command — exit codes
// and signals pass through unchanged. A sid outside [0-9A-Za-z._-] (it is
// interpolated into a shell script) returns nil.
func WrapArgv(sid string, argv []string) []string {
	if !safeSessionID(sid) {
		return nil
	}
	script := `echo $$ > ` + pidfilePath(sid) + `; exec "$@"`
	return append([]string{"sh", "-c", script, "sh"}, argv...)
}

// KillArgv returns the command that signals the pid recorded by WrapArgv:
//
//	sh -c 'kill -<sig> $(cat /tmp/.woca-ses-<sid>.pid)'
//
// sig must be a bare signal name or number (TERM, KILL, 9); anything else —
// like an unsafe sid — returns nil rather than reaching a shell.
func KillArgv(sid string, sig string) []string {
	if !safeSessionID(sid) || !safeSignal(sig) {
		return nil
	}
	script := `kill -` + sig + ` $(cat ` + pidfilePath(sid) + `)`
	return []string{"sh", "-c", script}
}

// safeSessionID reports whether sid can be interpolated into a shell script
// and a /tmp path without escaping: non-empty, at most 64 bytes from
// [0-9A-Za-z._-], and not a dotfile-style relative path.
func safeSessionID(sid string) bool {
	if sid == "" || len(sid) > 64 || sid == "." || sid == ".." {
		return false
	}
	for i := 0; i < len(sid); i++ {
		c := sid[i]
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// safeSignal reports whether sig is a bare signal name or number: one
// optional alphabetic prefix ("SIG" is not accepted; kill wants bare names)
// and at most eight alphanumeric characters total.
func safeSignal(sig string) bool {
	if sig == "" || len(sig) > 8 {
		return false
	}
	for i := 0; i < len(sig); i++ {
		c := sig[i]
		alnum := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if !alnum {
			return false
		}
	}
	return true
}
