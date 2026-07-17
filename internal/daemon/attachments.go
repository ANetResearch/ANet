package daemon

// attachments.go handles binary attachments (images, media, archives/zipped folders) carried alongside
// chat and delegation messages. The CLI passes local FILE PATHS (the daemon runs on the same machine, so
// it does the disk I/O itself — keeping the control channel body small); the daemon reads them, pins each
// by content CID, stores the bytes in the interactions store, and relays them inline. On receipt it
// re-verifies CID == SumRaw(data) before storing. Bytes flow daemon→Hub→daemon; the receipt-bound
// transcript records only attachment metadata + CID, so verification stays cheap and Hub uploads small.

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANet/internal/protocol/anetcid"
	"github.com/ANetResearch/ANet/internal/protocol/delegation"
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

// storeMsgAttachments verifies then persists attachments against a stored message. Bad attachments are
// skipped (logged by the caller via the returned error) rather than failing the whole message.
func (d *Daemon) storeMsgAttachments(interactionID string, msgSeq int64, atts []delegation.Attachment) error {
	for _, a := range atts {
		if err := verifyAttachment(a); err != nil {
			return err
		}
		row := interactions.Attachment{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID, Data: a.Data}
		if err := d.ix.AddAttachment(interactionID, msgSeq, row); err != nil {
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
}

// Pull writes every attachment of an interaction to outDir (filenames de-duplicated on collision) and
// returns what it wrote. This is how a receiving agent gets the delivered files onto disk.
func (d *Daemon) Pull(interactionID, outDir string) ([]PullResult, error) {
	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	metas, err := d.ix.Attachments(interactionID)
	if err != nil {
		return nil, err
	}
	out := make([]PullResult, 0, len(metas))
	used := map[string]bool{}
	for _, m := range metas {
		full, err := d.ix.AttachmentData(interactionID, m.CID)
		if err != nil {
			return nil, err
		}
		name := uniqueName(filepath.Base(safeName(full.Name)), used)
		used[name] = true
		dest := filepath.Join(outDir, name)
		if err := os.WriteFile(dest, full.Data, 0o644); err != nil {
			return nil, err
		}
		abs, _ := filepath.Abs(dest)
		out = append(out, PullResult{Name: name, Path: abs, Mime: full.Mime, Size: full.Size, CID: full.CID})
	}
	return out, nil
}

// AttachmentBytes returns one attachment's name/mime/bytes for streaming to the web console.
func (d *Daemon) AttachmentBytes(interactionID, cid string) (name, mimeType string, data []byte, err error) {
	a, err := d.ix.AttachmentData(interactionID, cid)
	if err != nil {
		return "", "", nil, err
	}
	return a.Name, a.Mime, a.Data, nil
}

// safeName strips path separators so a peer-supplied filename cannot escape the output directory.
func safeName(name string) string {
	name = filepath.Base(name)
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return "attachment"
	}
	return name
}

// uniqueName appends -1, -2, … before the extension if name is already taken.
func uniqueName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !used[cand] {
			return cand
		}
	}
}
