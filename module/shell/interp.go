//go:build shell

package shell

// interp is the command interpreter that runs a command line: its absolute path (on Windows; on unix the
// configured value, as before) and the family that decides how a line is handed to it and how caller
// arguments are quoted for it (interp_unix.go, interp_windows.go).
type interp struct {
	path   string
	family int
}

// The interpreter families.
const (
	// familyPosix is a POSIX shell: `<path> -c <line>`, arguments in single quotes (quoteArgv).
	familyPosix = iota
	// familyPowerShell is Windows PowerShell or PowerShell 7: the line goes in -EncodedCommand.
	familyPowerShell
	// familyCmd is cmd.exe: `/d /s /c "<line>"`; caller arguments are refused.
	familyCmd
)
