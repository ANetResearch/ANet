package daemon

// files_admit.go is the files of a capability call: the declaration a
// capability makes (CapFilesConfig), the admission of the files a call
// carries (admitFiles, CUX-F10), and the files held in memory for the
// call's run (CUX-F13).
//
// A call carries its files as the attachments of its DelegateReq and names
// them in the signed args as "$files" (CUX §2.5), so the request CID binds
// each file's CID. The bytes are checked here and held in memory; whether
// they are ever written down is the capability's discard setting.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// CapFilesConfig is the files a public capability takes (config key
// public_capabilities[].files): exactly Count files, each at most
// MaxBytes, each of a sniffed type in Types. A zero value declares none.
type CapFilesConfig struct {
	Count    int      `json:"count"`
	MaxBytes int64    `json:"max_bytes"`
	Types    []string `json:"types"`
}

// declared reports whether the capability takes files.
func (c CapFilesConfig) declared() bool { return c.Count > 0 }

// Bounds of the declaration (CUX §2.10).
const (
	filesCountMax    = 8
	filesMaxBytesCap = 64 << 20
	fileNameMax      = 255
)

// Codes admitFiles answers with: note codes (CUX-Q1).
const (
	admitFilesCount    = "files_count"
	admitFilesTooLarge = "files_too_large"
	admitFilesType     = "file_type_not_accepted"
)

// sniffableTypes are the media types http.DetectContentType can answer
// (net/http sniff.go): a files.types entry outside the set could never
// match, so the configuration is refused (CUX §2.10).
var sniffableTypes = map[string]bool{
	"text/html": true, "text/xml": true, "text/plain": true,
	"application/pdf": true, "application/postscript": true,
	"image/x-icon": true, "image/vnd.microsoft.icon": true, "image/bmp": true, "image/gif": true,
	"image/webp": true, "image/png": true, "image/jpeg": true,
	"audio/basic": true, "audio/aiff": true, "audio/mpeg": true, "application/ogg": true,
	"audio/midi": true, "video/avi": true, "audio/wave": true, "video/mp4": true, "video/webm": true,
	"application/vnd.ms-fontobject": true, "font/ttf": true, "font/otf": true, "font/collection": true,
	"font/woff": true, "font/woff2": true,
	"application/x-gzip": true, "application/zip": true, "application/x-rar-compressed": true,
	"application/wasm": true, "application/octet-stream": true,
}

// checkFilesConfig is the validation of one declaration (CUX §2.10); the
// empty string is a valid one.
func checkFilesConfig(c CapFilesConfig) (zh, en string) {
	switch {
	case c.Count < 1 || c.Count > filesCountMax:
		return fmt.Sprintf("count 须是 1 到 %d 之间的整数", filesCountMax),
			fmt.Sprintf("count is a whole number from 1 to %d", filesCountMax)
	case c.MaxBytes < 1 || c.MaxBytes > filesMaxBytesCap:
		return "max_bytes 须在 1 字节到 64 MiB 之间", "max_bytes is from 1 byte to 64 MiB"
	case len(c.Types) == 0:
		return "types 至少写一项", "types lists at least one type"
	}
	for _, t := range c.Types {
		if !sniffableTypes[t] {
			return fmt.Sprintf("types 里的 %q 不是 anet 能从文件内容认出的类型", t),
				fmt.Sprintf("%q in types is not a type anet can tell from a file's content", t)
		}
	}
	return "", ""
}

// fileDecl is one entry of "$files" (CUX §2.5).
type fileDecl struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	CID  string `json:"cid"`
}

// declaredFiles reads "$files" of args. present says the key is there;
// ok says it is a well-formed list (every entry exactly the four members).
func declaredFiles(args map[string]any) (list []fileDecl, present, ok bool) {
	raw, present := args[argsFilesKey]
	if !present {
		return nil, false, true
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, true, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil || list == nil {
		return nil, true, false
	}
	return list, true, true
}

// declaresFiles reports whether args name at least one file.
func declaresFiles(args map[string]any) bool {
	l, present, ok := declaredFiles(args)
	return present && (!ok || len(l) > 0)
}

// sniffedType is the type of data by its content, without parameters.
func sniffedType(data []byte) string {
	n := min(len(data), 512)
	t := http.DetectContentType(data[:n])
	if mt, _, err := mime.ParseMediaType(t); err == nil {
		return mt
	}
	return "application/octet-stream"
}

// validDeclaredMime is the form of a declared mime: lower case, a media
// type, no parameters (CUX §2.5).
func validDeclaredMime(s string) bool {
	mt, params, err := mime.ParseMediaType(s)
	return err == nil && len(params) == 0 && mt == s && s == strings.ToLower(s)
}

// admitFiles is the files' admission (CUX-F10, step 4 of CUX-Q1a), a pure
// function of the capability's declaration, the call's args and the
// attachments it carries. It returns "" or the code to refuse with; the
// bytes are read, not kept. In order:
//
//  1. the capability declares no files but the call carries some: files_count;
//  2. the attachments are not the "$files" list (their number, or any
//     name, size or cid): bad_request;
//  3. their number is not files.count: files_count;
//  4. one is over files.max_bytes: files_too_large;
//  5. one's sniffed type is not in files.types: file_type_not_accepted.
func admitFiles(c CapFilesConfig, args map[string]any, atts []delegation.Attachment) string {
	list, present, ok := declaredFiles(args)
	if !c.declared() {
		if present || len(atts) > 0 {
			return admitFilesCount
		}
		return ""
	}
	if !ok || len(list) != len(atts) {
		return reasonBadRequest
	}
	for i, a := range atts {
		d := list[i]
		if d.Name != a.Name || len(d.Name) > fileNameMax || !utf8.ValidString(d.Name) ||
			d.Size != a.Size || d.Size != int64(len(a.Data)) || !validDeclaredMime(d.Mime) {
			return reasonBadRequest
		}
		if cid, err := anetcid.SumRaw(a.Data); err != nil || cid != d.CID || cid != a.CID {
			return reasonBadRequest
		}
	}
	if len(atts) != c.Count {
		return admitFilesCount
	}
	for _, a := range atts {
		if int64(len(a.Data)) > c.MaxBytes {
			return admitFilesTooLarge
		}
	}
	for _, a := range atts {
		t := sniffedType(a.Data)
		found := false
		for _, want := range c.Types {
			if want == t {
				found = true
				break
			}
		}
		if !found {
			return admitFilesType
		}
	}
	return ""
}

// providerFiles are the files of an admitted call as the provider gets them
// (CUX-F20): the sniffed type, the name as declared.
func providerFiles(atts []delegation.Attachment) []provider.File {
	out := make([]provider.File, len(atts))
	for i, a := range atts {
		out[i] = provider.File{Name: a.Name, Mime: sniffedType(a.Data), Data: a.Data}
	}
	return out
}

// capAcceptsFiles reports whether the provider serving capID takes files
// (CUX-F15): it implements provider.FileReceiver and says yes for the
// capability. Nothing else about the provider is looked at.
func (d *Daemon) capAcceptsFiles(capID string) bool {
	if d.providers == nil {
		return false
	}
	p, ok := d.providers.Resolve(capID)
	if !ok {
		return false
	}
	fr, ok := p.(provider.FileReceiver)
	return ok && fr.AcceptsFiles(capID)
}

// checkFilesReceivers is CUX-F15: every public capability that declares
// files is served by a provider that takes them. It runs after the modules
// start and before the first card is published, and on each write of the
// capability list; nothing serving the capability counts as not taking.
func (d *Daemon) checkFilesReceivers(c Config) error {
	for i, p := range c.inbound().PublicCapabilities {
		if p.Files == nil || !p.Files.declared() {
			continue
		}
		if !d.capAcceptsFiles(strings.TrimSpace(p.ID)) {
			return configInvalid(fmt.Sprintf("inbound.public_capabilities[%d].files", i),
				ruleText(fmt.Sprintf("能力 %s 的提供方不能接收文件；请改由能接收文件的服务提供，或去掉 files", p.ID),
					fmt.Sprintf("nothing serving the capability %s can receive files; serve it with a service that takes files, or remove files", p.ID)))
		}
	}
	return nil
}

// --- the files of a running call ---

// callFiles holds the files of the calls that are running, by interaction
// id. They are in memory only (CUX-F13): not in the store, the queue or the
// log. A call's entry goes with its answer (deliverCapabilityResult), and
// sweepCallFiles drops the ones of calls that ended otherwise.
func (d *Daemon) putCallFiles(ix string, files []provider.File) {
	if len(files) > 0 {
		d.callFiles.Store(ix, files)
	}
}

func (d *Daemon) callFilesOf(ix string) []provider.File {
	if v, ok := d.callFiles.Load(ix); ok {
		fs, _ := v.([]provider.File)
		return fs
	}
	return nil
}

func (d *Daemon) dropCallFiles(ix string) { d.callFiles.Delete(ix) }

// sweepCallFiles drops the files of calls whose task has ended (a cancel
// reaches a task without an answer from here).
func (d *Daemon) sweepCallFiles() {
	d.callFiles.Range(func(k, _ any) bool {
		id, _ := k.(string)
		ix, err := d.ix.Get(id)
		if (err == nil && ix.IsTerminal()) || errors.Is(err, interactions.ErrNotFound) {
			if _, running := d.running.Load(id); !running {
				d.callFiles.Delete(k)
			}
		}
		return true
	})
}
