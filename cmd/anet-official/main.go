// Command anet-official is the backend of anet's official public agents
// (A2A-DESIGN §15): the process behind anet-echo-e, anet-echo-f,
// anet-tools, anet-docs and anet-paid-demo.
//
// It is an ordinary HTTP service mounted by a daemon's service module, and
// it is deliberately small in what it can do:
//
//   - Every capability is a deterministic computation over its arguments.
//     None executes a command, opens a network connection, reads a file at
//     run time or takes a URL. The documents docs.search and docs.get serve
//     are compiled into the binary.
//   - It listens on a Unix domain socket (or, for tests and old setups, a
//     loopback address), answers only a loopback Host, and only a caller
//     presenting the bearer token its daemon holds (module/service
//     token_file). The daemon is the only door: admission, the deny list,
//     quotas and payment happen there, in the kernel (A2A-DESIGN §5.4),
//     before a call reaches this process. On a socket the file system says
//     who may connect (mode 0660, -socket-group), and the daemon checks the
//     socket's path and, on Linux, this process's uid before it sends
//     anything (docs/notes/0030 N1); a loopback port could be taken by
//     another local user while this process is down.
//   - It keeps nothing. It logs who called what, how it ended and how many
//     bytes moved; never the arguments or the result.
//
// One binary serves every group; a deployment runs one instance per
// identity with -groups naming that identity's capabilities, so each
// daemon holds a token that opens only its own routes.
//
// Usage:
//
//	anet-official serve -listen unix:/run/anet-official/anet-echo-e/backend.sock -socket-group anet-official-ipc -token-file /run/credentials/…/token -groups echo
//	anet-official service-config -groups tools -url unix:///run/anet-official/anet-tools/backend.sock -token-file '${CREDENTIALS_DIRECTORY}/token'
//	anet-official capabilities
//	anet-official version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "service-config":
		err = runServiceConfig(os.Stdout, args)
	case "capabilities":
		err = runCapabilities(os.Stdout)
	case "version":
		c, cerr := loadCorpus()
		if cerr != nil {
			err = cerr
			break
		}
		fmt.Printf("anet-official %s (corpus %s, %d documents)\n", version, c.CID, len(c.docs))
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "anet-official:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  anet-official serve -listen unix:/PATH/backend.sock [-socket-group GROUP] -token-file PATH -groups echo,tools,docs,paid
  anet-official serve -listen 127.0.0.1:PORT -token-file PATH -groups …     (TCP: the daemon needs allow_tcp)
  anet-official service-config -groups GROUPS -url unix:///PATH/backend.sock [-token-file PATH] [-price N]
  anet-official capabilities
  anet-official version`

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "", "unix:/path/to/socket (recommended), or a loopback host:port (required)")
	socketGroup := fs.String("socket-group", "", "group (name or number) given the socket, which is made 0660: the daemon's user must be in it")
	tokenFile := fs.String("token-file", "", "file holding the bearer token the daemon presents (required)")
	groups := fs.String("groups", "", "comma-separated capability groups to serve: "+strings.Join(groupNames(), ","))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	caps, err := selectGroups(*groups)
	if err != nil {
		return err
	}
	if *tokenFile == "" {
		// There is no unauthenticated mode. A loopback port is reachable by
		// every process on the host, including a local agent in the
		// auto-reply sandbox (A2A-DESIGN §6).
		return errors.New("-token-file is required")
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	spec, err := parseListen(*listen)
	if err != nil {
		return err
	}
	if *socketGroup != "" && spec.network != "unix" {
		return errors.New("-socket-group applies to a unix: -listen only")
	}
	c, err := loadCorpus()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	e := &env{version: version, now: time.Now, corpus: c}
	srv := &http.Server{
		Handler:           newServer(e, caps, token, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          logger,
	}
	var ln net.Listener
	if spec.network == "unix" {
		ln, err = listenUnix(spec.addr, *socketGroup)
	} else {
		ln, err = net.Listen("tcp", spec.addr)
	}
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(caps))
	for _, cp := range caps {
		ids = append(ids, cp.ID)
	}
	logger.Printf("anet-official %s listening on %s; capabilities %s; corpus %s (%d documents)",
		version, ln.Addr(), strings.Join(ids, ","), c.CID, len(c.docs))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// listenSpec is where serve listens: a Unix socket, or a loopback address.
type listenSpec struct{ network, addr string }

// parseListen reads -listen: unix:/path or unix:///path (a socket), or a
// loopback host:port.
func parseListen(listen string) (listenSpec, error) {
	if listen == "" {
		return listenSpec{}, errors.New("-listen is required: unix:/path/to/backend.sock (recommended), or a loopback host:port")
	}
	if rest, ok := strings.CutPrefix(listen, "unix:"); ok {
		p := rest
		if strings.HasPrefix(rest, "///") {
			p = rest[2:]
		}
		if err := checkSocketPath(p); err != nil {
			return listenSpec{}, fmt.Errorf("-listen %q: %v", listen, err)
		}
		return listenSpec{"unix", p}, nil
	}
	addr, err := loopbackListenAddr(listen)
	if err != nil {
		return listenSpec{}, err
	}
	return listenSpec{"tcp", addr}, nil
}

// checkSocketPath holds a socket path to what the daemon's unix:// URL can
// name (internal/backendconn): absolute, clean, no colon (the colon starts
// the request path), no spaces or URL characters, and short enough for a
// socket address.
func checkSocketPath(p string) error {
	max := 103
	if runtime.GOOS == "linux" {
		max = 107
	}
	switch {
	case !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/":
		return errors.New("the socket path must be absolute and clean")
	case strings.ContainsAny(p, ":?#%\\") || strings.IndexFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0:
		return errors.New("the socket path must not contain spaces, control characters or :?#%\\")
	case len(p) > max:
		return fmt.Errorf("the socket path is %d bytes, more than the %d a socket address holds", len(p), max)
	}
	return nil
}

// listenUnix listens on a socket at path, made 0660 and given group when one
// is named: only this user and the group's members (the daemon) can connect.
//
// A socket left at path by a process that died is removed first. Anything
// else there — a live socket, a file that is not a socket — is left alone
// and is an error: this process does not take a path over from another.
func listenUnix(path, group string) (net.Listener, error) {
	gid := -1
	if group != "" {
		id, err := strconv.Atoi(group)
		if err != nil {
			g, lerr := user.LookupGroup(group)
			if lerr != nil {
				return nil, fmt.Errorf("-socket-group %q: %v", group, lerr)
			}
			if id, err = strconv.Atoi(g.Gid); err != nil {
				return nil, fmt.Errorf("-socket-group %q: gid %q is not a number", group, g.Gid)
			}
		}
		gid = id
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The group first, then the mode: until both are set the socket has the
	// umask's mode, which only narrows who can connect.
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			ln.Close()
			return nil, fmt.Errorf("-socket-group: %v (is this process a member of the group?)", err)
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// removeStaleSocket removes a socket nobody listens on.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; not removing it", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("%s is in use: another process listens on it", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%s: %v; not removing it", path, err)
	}
	return os.Remove(path)
}

// loopbackListenAddr accepts only an address on a loopback interface. The
// kernel's admission, quotas and payment all happen in the daemon; a
// listener reachable from elsewhere would be a door around all of them.
func loopbackListenAddr(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("-listen %q: %v", listen, err)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("-listen %q: only a loopback address (127.0.0.0/8 or ::1) is allowed", listen)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", fmt.Errorf("-listen %q: bad port", listen)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// minTokenBytes matches module/service: a short token is guessable by
// anything on the host that can open a socket and try.
const minTokenBytes = 16

// readToken reads the bearer token: the file's first line, trimmed. The
// rules are those of module/service, so a file one side accepts the other
// accepts too: variables must be set, the path absolute, the file regular
// and closed to other users.
func readToken(raw string) (string, error) {
	var unset []string
	path := os.Expand(raw, func(k string) string {
		v, ok := os.LookupEnv(k)
		if !ok || v == "" {
			unset = append(unset, k)
		}
		return v
	})
	if len(unset) > 0 {
		return "", fmt.Errorf("-token-file %q: %s is not set", raw, strings.Join(unset, ", "))
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("-token-file %q must be an absolute path", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		return "", fmt.Errorf("token file %s is accessible to other users (mode %04o); chmod o-rwx", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	tok := string(b)
	if i := strings.IndexAny(tok, "\r\n"); i >= 0 {
		tok = tok[:i]
	}
	tok = strings.TrimSpace(tok)
	if len(tok) < minTokenBytes {
		return "", fmt.Errorf("token file %s: token is %d bytes, at least %d required", path, len(tok), minTokenBytes)
	}
	for _, r := range tok {
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("token file %s: token contains whitespace or control characters", path)
		}
	}
	return tok, nil
}

func groupNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range allCapabilities() {
		if !seen[c.Group] {
			seen[c.Group] = true
			out = append(out, c.Group)
		}
	}
	sort.Strings(out)
	return out
}

// selectGroups returns the capabilities of the named groups, in table
// order. An empty or unknown group is an error rather than "all": a
// backend that serves more than its identity publishes is a token that
// opens more than it should.
func selectGroups(list string) ([]*capability, error) {
	if strings.TrimSpace(list) == "" {
		return nil, fmt.Errorf("-groups is required (one or more of %s)", strings.Join(groupNames(), ","))
	}
	want := map[string]bool{}
	for _, g := range strings.Split(list, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		known := false
		for _, n := range groupNames() {
			if n == g {
				known = true
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown group %q (have %s)", g, strings.Join(groupNames(), ","))
		}
		want[g] = true
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("-groups names no group (one or more of %s)", strings.Join(groupNames(), ","))
	}
	var out []*capability
	for _, c := range allCapabilities() {
		if want[c.Group] {
			out = append(out, c)
		}
	}
	return out, nil
}

// runCapabilities prints the capability table as JSON: what each one is,
// where it is routed, and its limits.
func runCapabilities(w *os.File) error {
	type row struct {
		ID           string   `json:"id"`
		Group        string   `json:"group"`
		Route        string   `json:"route"`
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		Tags         []string `json:"tags"`
		MaxArgsBytes int      `json:"max_args_bytes"`
		TimeoutMS    int64    `json:"timeout_ms"`
		Price        uint64   `json:"price,omitempty"`
	}
	var rows []row
	for _, c := range allCapabilities() {
		rows = append(rows, row{c.ID, c.Group, routeOf(c), c.Name, c.Description, c.Tags,
			c.MaxArgsBytes, c.Timeout.Milliseconds(), c.Price})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(rows)
}

// serviceConfig is the part of a daemon's config.json that mounts this
// backend: the public_capabilities entries and the service module block.
type serviceConfig struct {
	Inbound struct {
		Policy             string            `json:"policy"`
		PublicCapabilities []publicCapConfig `json:"public_capabilities"`
	} `json:"inbound"`
	Modules struct {
		Service svcModuleConfig `json:"service"`
	} `json:"modules"`
}

type publicCapConfig struct {
	ID              string `json:"id"`
	PerCallerPerMin int    `json:"per_caller_per_min,omitempty"`
	PerCallerPerDay int    `json:"per_caller_per_day,omitempty"`
	GlobalPerMin    int    `json:"global_per_min,omitempty"`
	MaxInflight     int    `json:"max_inflight,omitempty"`
	MaxArgsBytes    int    `json:"max_args_bytes"`
	// Evidence is the daemon's evidence mode for calls from strangers.
	// The official agents keep the result CID and the metrics only
	// (deploy/official/README.md §5).
	Evidence string `json:"evidence,omitempty"`
}

type svcModuleConfig struct {
	TokenFile    string          `json:"token_file,omitempty"`
	Capabilities []svcCapability `json:"capabilities"`
}

type svcCapability struct {
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Price       uint64   `json:"price,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"input_modes"`
	OutputModes []string `json:"output_modes"`
	TimeoutMS   int64    `json:"timeout_ms"`
}

// buildServiceConfig renders the daemon configuration for the given
// groups, served at baseURL (unix:///path/to/socket, or http://host:port).
// price, when non-zero, replaces the suggested price of every priced
// capability.
func buildServiceConfig(caps []*capability, baseURL, tokenFile string, price uint64) serviceConfig {
	var sc serviceConfig
	sc.Inbound.Policy = "closed"
	sc.Modules.Service.TokenFile = tokenFile
	sep := ""
	if strings.HasPrefix(baseURL, "unix://") {
		// The request path follows the socket's after a colon.
		sep = ":"
	} else {
		baseURL = strings.TrimRight(baseURL, "/")
	}
	for _, c := range caps {
		sc.Inbound.PublicCapabilities = append(sc.Inbound.PublicCapabilities, publicCapConfig{
			ID: c.ID, PerCallerPerMin: c.Quota.PerCallerPerMin, PerCallerPerDay: c.Quota.PerCallerPerDay,
			GlobalPerMin: c.Quota.GlobalPerMin, MaxInflight: c.Quota.MaxInflight, MaxArgsBytes: c.MaxArgsBytes,
			Evidence: "cid",
		})
		p := c.Price
		if p > 0 && price > 0 {
			p = price
		}
		sc.Modules.Service.Capabilities = append(sc.Modules.Service.Capabilities, svcCapability{
			ID: c.ID, URL: baseURL + sep + routeOf(c), Price: p, Name: c.Name, Description: c.Description,
			Tags: c.Tags, Examples: c.Examples,
			InputModes: []string{"application/json"}, OutputModes: []string{"application/json"},
			TimeoutMS: c.Timeout.Milliseconds(),
		})
	}
	return sc
}

func runServiceConfig(w *os.File, args []string) error {
	fs := flag.NewFlagSet("service-config", flag.ContinueOnError)
	groups := fs.String("groups", "", "capability groups this identity serves")
	base := fs.String("url", "", "where the backend instance listens: unix:///path/to/backend.sock (recommended) or http://127.0.0.1:PORT")
	tokenFile := fs.String("token-file", "${CREDENTIALS_DIRECTORY}/token", "token_file for the service module")
	price := fs.Uint64("price", 0, "price in credit for priced capabilities (0 keeps the suggested price)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	caps, err := selectGroups(*groups)
	if err != nil {
		return err
	}
	if *base == "" {
		return errors.New("-url is required")
	}
	if u, ok := strings.CutPrefix(*base, "unix://"); ok {
		if err := checkSocketPath(u); err != nil {
			return fmt.Errorf("-url %q: %v (write unix:///absolute/path)", *base, err)
		}
	} else if !isLoopbackURL(*base) {
		return fmt.Errorf("-url %q: the backend listens on a Unix socket (unix:///path) or on loopback only", *base)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(buildServiceConfig(caps, *base, *tokenFile, *price))
}

// daemonURL is the base URL a daemon reaches a -listen value at.
func daemonURL(listen string) (string, error) {
	spec, err := parseListen(listen)
	if err != nil {
		return "", err
	}
	if spec.network == "unix" {
		return "unix://" + spec.addr, nil
	}
	return "http://" + spec.addr, nil
}

func isLoopbackURL(raw string) bool {
	if !strings.HasPrefix(raw, "http://") {
		return false
	}
	hostport := strings.TrimPrefix(raw, "http://")
	if i := strings.IndexByte(hostport, '/'); i >= 0 {
		hostport = hostport[:i]
	}
	return loopbackHost(hostport)
}
