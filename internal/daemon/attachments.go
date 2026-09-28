package daemon

// attachments.go handles binary attachments (images, media, archives/zipped folders) carried alongside
// chat and delegation messages. The CLI passes local FILE PATHS (the daemon runs on the same machine, so
// it does the disk I/O itself — keeping the control channel body small); the daemon reads them, pins each
// by content CID, stores the bytes in the interactions store, and relays them inline. On receipt it
// re-verifies CID == SumRaw(data) before storing. Bytes flow daemon→Hub→daemon; the receipt-bound
// transcript records only attachment metadata + CID, so verification stays cheap and Hub uploads small.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// maxAttachmentBytes bounds a single attachment. Larger payloads must be split or (future) uploaded to a
// chunked blob store; inline transport is bounded by the Hub relay body cap (see the Hub's request cap /
// nginx client_max_body_size, sized to accommodate this plus base64 overhead).
const maxAttachmentBytes = 64 << 20 // 64 MiB

// detectMime picks a media type from the filename extension, falling back to content sniffing.
func detectMime(name string, data []byte) string {
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

// attachmentFromPath reads a local file into a self-verified Attachment (bytes + content CID).
func attachmentFromPath(path string) (delegation.Attachment, error) {
	var zero delegation.Attachment
	info, err := os.Stat(path)
	if err != nil {
		return zero, fmt.Errorf("attachment %q: %w", path, err)
	}
	if info.IsDir() {
		return zero, fmt.Errorf("attachment %q is a directory — compress it first (e.g. `zip -r out.zip .`)", path)
	}
	if info.Size() > maxAttachmentBytes {
		return zero, fmt.Errorf("attachment %q is %d bytes, over the %d MiB limit — compress or split it",
			path, info.Size(), maxAttachmentBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, fmt.Errorf("attachment %q: %w", path, err)
	}
	cid, err := anetcid.SumRaw(data)
	if err != nil {
		return zero, err
	}
	name := filepath.Base(path)
	return delegation.Attachment{Name: name, Mime: detectMime(name, data), Size: int64(len(data)), CID: cid, Data: data}, nil
}

// attachmentFromBytes builds a self-verified Attachment from in-memory bytes. The web console uploads file
// BYTES (a browser cannot hand the daemon a path the way the CLI does), so this is the upload counterpart
// of attachmentFromPath: same size cap, MIME detection and content CID (anetcid.SumRaw).
func attachmentFromBytes(name string, data []byte) (delegation.Attachment, error) {
	var zero delegation.Attachment
	name = filepath.Base(name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "attachment"
	}
	if len(data) == 0 {
		return zero, fmt.Errorf("attachment %q is empty", name)
	}
	if int64(len(data)) > maxAttachmentBytes {
		return zero, fmt.Errorf("attachment %q is %d bytes, over the %d MiB limit — compress or split it",
			name, len(data), maxAttachmentBytes>>20)
	}
	cid, err := anetcid.SumRaw(data)
	if err != nil {
		return zero, err
	}
	return delegation.Attachment{Name: name, Mime: detectMime(name, data), Size: int64(len(data)), CID: cid, Data: data}, nil
}

// attachmentsFromPaths builds attachments for every path, failing fast on the first unreadable/oversized.
func attachmentsFromPaths(paths []string) ([]delegation.Attachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]delegation.Attachment, 0, len(paths))
	for _, p := range paths {
		a, err := attachmentFromPath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// verifyAttachment re-checks a received attachment's integrity (CID == SumRaw(data), size matches).
func verifyAttachment(a delegation.Attachment) error {
	if a.CID == "" {
		return fmt.Errorf("attachment %q missing cid", a.Name)
	}
	if int64(len(a.Data)) != a.Size {
		return fmt.Errorf("attachment %q size mismatch (declared %d, got %d)", a.Name, a.Size, len(a.Data))
	}
	if len(a.Data) > maxAttachmentBytes {
		return fmt.Errorf("attachment %q over size limit", a.Name)
	}
	got, err := anetcid.SumRaw(a.Data)
	if err != nil {
		return err
	}
	if got != a.CID {
		return fmt.Errorf("attachment %q content does not match cid", a.Name)
	}
	return nil
}

// inlineImageTypes are the only attachment types the console renders inline and /attachment serves
// with their own Content-Type (A2A-DESIGN §7.6). They are raster formats a browser decodes as an image
// and never executes. SVG is deliberately absent: it is a document that can carry script.
var inlineImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// sniffMime returns the media type (lower case, no parameters) that http.DetectContentType assigns to
// the leading bytes of data. It is the type every stored attachment carries, whatever the peer declared.
// DetectContentType never answers image/svg+xml; SVG sniffs as text/xml or text/plain.
func sniffMime(data []byte) string {
	if len(data) == 0 {
		return "application/octet-stream"
	}
	n := len(data)
	if n > 512 {
		n = 512
	}
	t := http.DetectContentType(data[:n])
	if mt, _, err := mime.ParseMediaType(t); err == nil {
		return mt
	}
	return "application/octet-stream"
}

// storeMsgAttachments verifies then persists attachments against a stored message. Bad attachments are
// skipped (logged by the caller via the returned error) rather than failing the whole message.
//
// The stored Mime is the sniffed type, not the declared one (A2A-DESIGN §7.6), so the console, /pull,
// the auto-reply image selection and /attachment all see one type, decided by the bytes. Both sides
// apply the same deterministic sniff, so the attachment fingerprints in a transcript agree.
func (d *Daemon) storeMsgAttachments(interactionID string, msgSeq int64, atts []delegation.Attachment) error {
	for _, a := range atts {
		if err := verifyAttachment(a); err != nil {
			return err
		}
		row := interactions.Attachment{Name: a.Name, Mime: sniffMime(a.Data), Size: a.Size, CID: a.CID, Data: a.Data}
		if err := d.ix.AddAttachment(interactionID, msgSeq, row); err != nil {
			return err
		}
	}
	return nil
}

// receivedAttachments verifies the attachments of a received message and
// returns the rows to store, in order, up to the first that does not
// verify, and that one's error: bad attachments are left out (logged by the
// caller), not the message. It writes nothing: the rows are stored with
// the message, in its step-10 transaction (addAttachmentsTx).
func receivedAttachments(atts []delegation.Attachment) ([]interactions.Attachment, error) {
	rows := make([]interactions.Attachment, 0, len(atts))
	for _, a := range atts {
		if err := verifyAttachment(a); err != nil {
			return rows, err
		}
		rows = append(rows, interactions.Attachment{Name: a.Name, Mime: sniffMime(a.Data), Size: a.Size, CID: a.CID, Data: a.Data})
	}
	return rows, nil
}

// addAttachmentsTx stores a received message's attachment rows inside its
// step-10 transaction ([redteam:F27]). A storage error there rolls back the
// message and its replay row with them, so the envelope is not acknowledged
// and its redelivery stores all of it; written after the commit, the error
// was only logged, the envelope acknowledged, every redelivery a duplicate,
// and the files lost for good.
func addAttachmentsTx(tx *interactions.Tx, interactionID string, msgSeq int64, rows []interactions.Attachment) error {
	for _, r := range rows {
		if err := tx.AddAttachment(interactionID, msgSeq, r); err != nil {
			return err
		}
	}
	return nil
}

// PullResult is one saved attachment file (returned by Pull).
type PullResult struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	CID  string `json:"cid"`
	// AlreadyPresent marks a file an earlier pull wrote: a file with the same content already sat at
	// Path, so nothing was written.
	AlreadyPresent bool `json:"already_present,omitempty"`
}

// errPullOutDir is the class of refusals for the out_dir argument; the control handler answers 400.
var errPullOutDir = errors.New("pull: out_dir")

// maxPullNameAttempts bounds the search for a free file name in the pull directory.
const maxPullNameAttempts = 1000

// Pull writes every attachment of an interaction to a fresh subdirectory <outDir>/anet-<ix first 12>/
// and returns what it wrote (A2A-DESIGN §7.7).
//
// The file names are chosen by the peer, so the writing is constrained:
//   - outDir must be absolute. The CLI sends its own working directory when --out is absent; the daemon
//     does not fall back to its own working directory, which the caller cannot see.
//   - outDir is resolved through symbolic links and refused when it lies inside the data dir or the exec
//     work dir, where a written file could replace keys, tokens, configuration or an agent's input.
//   - Files go into the per-interaction subdirectory, so a peer-chosen name never lands beside the
//     user's own files (a peer could otherwise create a CLAUDE.md, AGENTS.md or .envrc in a project
//     directory). The subdirectory is created with Mkdir and checked with Lstat; a symbolic link there
//     is refused.
//   - Each file is opened with O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW, so an existing file is never
//     overwritten and a symbolic link placed at the destination is never followed. When the name is
//     taken by a regular file with the same content CID, the attachment counts as already pulled, which
//     keeps a repeated pull idempotent; otherwise the next free name is used.
//   - safeName neutralizes a leading dot, strips control and bidirectional-override characters and
//     limits the length.
func (d *Daemon) Pull(interactionID, outDir string) ([]PullResult, error) {
	real, err := d.checkPullOutDir(outDir)
	if err != nil {
		return nil, err
	}
	metas, err := d.ix.Attachments(interactionID)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return []PullResult{}, nil
	}
	atts := make([]*interactions.Attachment, 0, len(metas))
	for _, m := range metas {
		full, err := d.ix.AttachmentData(interactionID, m.CID)
		if err != nil {
			return nil, err
		}
		atts = append(atts, full)
	}
	return pullInto(real, interactionID, atts)
}

// pullInto writes atts into the per-interaction subdirectory of the resolved directory real.
func pullInto(real, interactionID string, atts []*interactions.Attachment) ([]PullResult, error) {
	out := make([]PullResult, 0, len(atts))
	if err := os.MkdirAll(real, 0o755); err != nil {
		return nil, err
	}
	sub := filepath.Join(real, "anet-"+safeName(prefix(interactionID, 12)))
	if err := os.Mkdir(sub, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if fi, err := os.Lstat(sub); err != nil {
		return nil, err
	} else if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, fmt.Errorf("pull: %s exists and is not a directory (or is a symbolic link); refusing to write into it", sub)
	}
	used := map[string]bool{}
	for _, a := range atts {
		res, err := pullOne(sub, safeName(a.Name), a, used)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// checkPullOutDir validates the out_dir of a pull and returns it resolved through symbolic links.
func (d *Daemon) checkPullOutDir(outDir string) (string, error) {
	if strings.TrimSpace(outDir) == "" {
		return "", fmt.Errorf("%w is required (an absolute directory)", errPullOutDir)
	}
	if !filepath.IsAbs(outDir) {
		return "", fmt.Errorf("%w %q is not an absolute path", errPullOutDir, outDir)
	}
	real, err := resolveThroughExisting(filepath.Clean(outDir))
	if err != nil {
		return "", fmt.Errorf("%w %q: %v", errPullOutDir, outDir, err)
	}
	for _, root := range d.pullForbiddenRoots() {
		if pathWithin(real, root) {
			return "", fmt.Errorf("%w %q lies inside %s, which anet does not write peer files into", errPullOutDir, outDir, root)
		}
	}
	return real, nil
}

// pullOne writes one attachment into dir under name or the first free numbered variant of it.
func pullOne(dir, name string, a *interactions.Attachment, used map[string]bool) (PullResult, error) {
	for i := 0; i < maxPullNameAttempts; i++ {
		cand := numberedName(name, i)
		if used[cand] {
			continue
		}
		p := filepath.Join(dir, cand)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if err == nil {
			_, werr := f.Write(a.Data)
			cerr := f.Close()
			if werr != nil {
				return PullResult{}, werr
			}
			if cerr != nil {
				return PullResult{}, cerr
			}
			used[cand] = true
			return PullResult{Name: cand, Path: p, Mime: a.Mime, Size: a.Size, CID: a.CID}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return PullResult{}, err
		}
		if regularFileHasCID(p, a.CID) {
			used[cand] = true
			return PullResult{Name: cand, Path: p, Mime: a.Mime, Size: a.Size, CID: a.CID, AlreadyPresent: true}, nil
		}
	}
	return PullResult{}, fmt.Errorf("pull: no free file name for %q in %s", name, dir)
}

// regularFileHasCID reports whether p is a regular file (not a symbolic link) whose content has the
// given CID.
func regularFileHasCID(p, cid string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxAttachmentBytes {
		return false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentBytes+1))
	if err != nil {
		return false
	}
	got, err := anetcid.SumRaw(data)
	return err == nil && got == cid
}

// numberedName returns name for i == 0 and stem-i.ext after that.
func numberedName(name string, i int) string {
	if i == 0 {
		return name
	}
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s-%d%s", name[:len(name)-len(ext)], i, ext)
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// pullForbiddenRoots are the directories /pull never writes into, resolved through symbolic links: the
// data dir and the exec work dir.
func (d *Daemon) pullForbiddenRoots() []string {
	var out []string
	for _, p := range []string{d.layout.Root, execWorkRoot()} {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if r, err := resolveThroughExisting(abs); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// resolveThroughExisting resolves symbolic links in the longest existing prefix of the absolute path p
// and appends the components that do not exist yet (which cannot be links). The result is what p would
// name once created.
func resolveThroughExisting(p string) (string, error) {
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

// pathWithin reports whether p equals root or lies below it. Both must be cleaned absolute paths.
func pathWithin(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// AttachmentBytes returns one attachment's name/mime/bytes for streaming to the web console.
func (d *Daemon) AttachmentBytes(interactionID, cid string) (name, mimeType string, data []byte, err error) {
	a, err := d.ix.AttachmentData(interactionID, cid)
	if err != nil {
		return "", "", nil, err
	}
	return a.Name, a.Mime, a.Data, nil
}

// maxSafeNameBytes caps a file name derived from a peer-supplied name.
const maxSafeNameBytes = 128

// safeName turns a peer-supplied file name into one that is safe to create in a directory and to show
// in a download prompt:
//   - only the last path element survives (both / and \ count as separators);
//   - control characters and bidirectional formatting characters are removed, so a name cannot hide
//     its real extension (for example "invoice\u202Efdp.exe");
//   - a leading dot is replaced by "_", so the file is not hidden and cannot be a tool's dotfile
//     (.envrc, .npmrc);
//   - trailing dots and spaces are removed and Windows device names (CON, NUL, COM1, …) get a "_"
//     prefix, for clients that save the name on Windows;
//   - the result is limited to maxSafeNameBytes, keeping a short extension.
//
// An empty result becomes "attachment".
func safeName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || isBidiFormat(r) {
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
	if len(name) > maxSafeNameBytes {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = truncateUTF8(name[:len(name)-len(ext)], maxSafeNameBytes-len(ext)) + ext
	}
	if name == "" || name == "_" {
		return "attachment"
	}
	return name
}

// isBidiFormat reports the Unicode bidirectional formatting characters (embeddings, overrides,
// isolates and marks).
func isBidiFormat(r rune) bool {
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

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
