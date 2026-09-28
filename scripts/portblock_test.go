package scripts

// These tests pin lib.sh port_block, the port-block choice of joint.sh, joint-a2a.sh and
// joint-official.sh (docs/notes/0028 V1). On the test hosts the port blocks (47100-47499) lie inside
// the kernel's ephemeral range, so a run's own daemons take source ports in the block when they dial
// its hub; closed first by the daemon, such a connection stays in TIME_WAIT for 60 s on that port
// without SO_REUSEADDR, and no listener (Go's included) can open there until it is gone. The next run on
// the same JOINT_PORT_BASE then refused to start. port_block waits such a block out, and still refuses at
// once a block where something listens.

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// portBlock runs lib.sh port_block BASE N with PORT_BLOCK_WAIT=wait and returns its output and exit code.
func portBlock(t *testing.T, base, n int, wait string) (string, int, time.Duration) {
	t.Helper()
	lib, err := filepath.Abs("lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", ". "+lib+"\nport_block \"$1\" \"$2\"", "libsh", strconv.Itoa(base), strconv.Itoa(n))
	cmd.Env = append(os.Environ(), "ANET=/nonexistent/anet", "HOME="+t.TempDir(), "PORT_BLOCK_WAIT="+wait)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out), code, time.Since(start)
}

// freeLoopbackPort returns a loopback port nothing listens on at the moment.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// tcpStateOn reports whether /proc/net/tcp lists a socket with local port p in state st (hex, "06" is
// TIME_WAIT).
func tcpStateOn(p int, st string) bool {
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		col := strings.Fields(sc.Text())
		if len(col) < 4 {
			continue
		}
		i := strings.LastIndex(col[1], ":")
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseInt(col[1][i+1:], 16, 32); err == nil && int(v) == p && strings.EqualFold(col[3], st) {
			return true
		}
	}
	return false
}

// clientTimeWait leaves a closed client connection in TIME_WAIT on 127.0.0.1:p, as a daemon that
// dialled its hub from source port p and closed first does, and returns once /proc shows it.
func clientTimeWait(t *testing.T, p int) {
	t.Helper()
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := srv.Accept()
		if err == nil {
			accepted <- c
		}
		close(accepted)
	}()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p}, Timeout: 5 * time.Second}
	c, err := d.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Skipf("cannot dial from port %d: %v", p, err)
	}
	sc, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	c.Close() // the client closes first: TIME_WAIT is on its side
	_ = sc.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = sc.Read(make([]byte, 1))
	sc.Close()
	for i := 0; i < 50; i++ {
		if tcpStateOn(p, "06") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Skipf("no TIME_WAIT on port %d appeared in /proc/net/tcp", p)
}

func TestPortBlockTellsAClosedClientConnectionFromAListener(t *testing.T) {
	needLinuxShell(t)

	// The case of 0028 V1: a Go-style listener (SO_REUSEADDR) cannot open where a client's connection
	// sits in TIME_WAIT. Checked first, so that the test fails if the kernel ever stops refusing it
	// rather than passing for the wrong reason.
	tw := freeLoopbackPort(t)
	clientTimeWait(t, tw)
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", tw)); err == nil {
		l.Close()
		t.Skip("this kernel lets a listener open over a client's TIME_WAIT; nothing to wait for")
	}

	// Without waiting it says why, and it is not "in use".
	out, code, _ := portBlock(t, tw, 1, "0")
	if code == 0 || !strings.Contains(out, "still held by closed connections (TIME_WAIT)") || strings.Contains(out, "is in use") {
		t.Errorf("port_block %d 1 with a client TIME_WAIT on it, no wait: exit %d\n%s", tw, code, out)
	}
	// Given time, it waits (and says so) rather than refusing at once.
	out, code, took := portBlock(t, tw, 1, "3")
	if code == 0 || !strings.Contains(out, "waiting") || took < 2*time.Second {
		t.Errorf("port_block %d 1 with PORT_BLOCK_WAIT=3: exit %d after %s\n%s", tw, code, took, out)
	}

	// A listener in the block is refused at once, however long the wait allowed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	lp := l.Addr().(*net.TCPAddr).Port
	out, code, took = portBlock(t, lp, 1, "60")
	if code == 0 || !strings.Contains(out, fmt.Sprintf("is in use (%d)", lp)) || took > 10*time.Second {
		t.Errorf("port_block %d 1 with a listener on it: exit %d after %s\n%s", lp, code, took, out)
	}

	// A free port is given back as it is.
	fp := freeLoopbackPort(t)
	out, code, _ = portBlock(t, fp, 1, "0")
	if code != 0 || strings.TrimSpace(out) != strconv.Itoa(fp) {
		t.Errorf("port_block %d 1 on a free port: exit %d\n%s", fp, code, out)
	}
}

// The wait ends with the block: once the TIME_WAIT is gone (60 s on Linux) port_block prints the base.
// About a minute, so only with ANET_SCRIPTS_SLOW=1.
func TestPortBlockWaitsOutAClientTimeWait(t *testing.T) {
	if os.Getenv("ANET_SCRIPTS_SLOW") != "1" {
		t.Skip("takes a minute; ANET_SCRIPTS_SLOW=1 runs it")
	}
	needLinuxShell(t)
	tw := freeLoopbackPort(t)
	clientTimeWait(t, tw)
	out, code, took := portBlock(t, tw, 1, "75")
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), strconv.Itoa(tw)) {
		t.Errorf("port_block %d 1 after the TIME_WAIT: exit %d after %s\n%s", tw, code, took, out)
	}
	t.Logf("the block was free after %s", took.Round(time.Second))
}
