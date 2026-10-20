// Package pathsafe holds the path and file-name primitives that the kernel's
// attachment handling and the auto-reply module's work directories share: a
// path lies within a root, symbolic links resolved through the part of a path
// that exists, a peer's file name made safe to create, a media type picked from
// a name and its bytes, and the size bound of one attachment. Each is a pure
// function of its arguments; the package imports nothing of this module.
package pathsafe

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxAttachmentBytes bounds a single attachment. Larger payloads must be split or (future) uploaded to a
// chunked blob store; inline transport is bounded by the Hub relay body cap (see the Hub's request cap /
// nginx client_max_body_size, sized to accommodate this plus base64 overhead).
const MaxAttachmentBytes = 64 << 20 // 64 MiB

// DetectMime picks a media type from the filename extension, falling back to content sniffing.
func DetectMime(name string, data []byte) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	n := len(data)
	if n > 512 {
		n = 512
	}
	if n == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(data[:n])
}

// ResolveThroughExisting resolves symbolic links in the longest existing prefix of the absolute path p
// and appends the components that do not exist yet (which cannot be links). The result is what p would
// name once created.
func ResolveThroughExisting(p string) (string, error) {
	cur := p
	var rest []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// PathWithin reports whether p equals root or lies below it. Both must be cleaned absolute paths.
//
// On Windows, where file names are case-insensitive, the two paths must be on the same volume and
// their components are compared without regard to case (and without the trailing dots and spaces
// Windows ignores); every caller uses the answer to refuse an overlap, so the comparison errs
// toward "within".
func PathWithin(p, root string) bool {
	return pathWithin(p, root)
}

// MaxSafeNameBytes caps a file name derived from a peer-supplied name.
const MaxSafeNameBytes = 128

// SafeName turns a peer-supplied file name into one that is safe to create in a directory and to show
// in a download prompt:
//   - only the last path element survives (both / and \ count as separators);
//   - control characters and bidirectional formatting characters are removed, so a name cannot hide
//     its real extension (for example "invoice\u202Efdp.exe");
//   - a leading dot is replaced by "_", so the file is not hidden and cannot be a tool's dotfile
//     (.envrc, .npmrc);
//   - trailing dots and spaces are removed and Windows device names (CON, NUL, COM1, …) get a "_"
//     prefix, for clients that save the name on Windows;
//   - the result is limited to MaxSafeNameBytes, keeping a short extension;
//   - on Windows only, the characters < > : " | ? * become "_" (so a name cannot address an
//     alternate data stream) and COM0, LPT0 and the superscript forms COM¹ COM² COM³ LPT¹ LPT²
//     LPT³ are treated as device names too.
//
// An empty result becomes "attachment".
func SafeName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || IsBidiFormat(r) {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	if strings.HasPrefix(name, ".") {
		name = "_" + strings.TrimLeft(name, ".")
	}
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if windowsReservedName(stem) {
		name = "_" + name
	}
	name = limitName(name)
	if name == "" || name == "_" {
		return "attachment"
	}
	return osSafeName(name)
}

// limitName limits name to MaxSafeNameBytes, keeping an extension of up to 16 bytes.
func limitName(name string) string {
	if len(name) > MaxSafeNameBytes {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = TruncateUTF8(name[:len(name)-len(ext)], MaxSafeNameBytes-len(ext)) + ext
	}
	return name
}

// IsBidiFormat reports the Unicode bidirectional formatting characters (embeddings, overrides,
// isolates and marks).
func IsBidiFormat(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200E, r == 0x200F, r == 0x061C:
		return true
	}
	return false
}

func windowsReservedName(stem string) bool {
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// TruncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func TruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
