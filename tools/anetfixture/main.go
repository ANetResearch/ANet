// anetfixture mints the signed objects a joint run needs.
//
// Two of the daemon's capabilities take an object the CALLER signed:
// blackboard.add takes a CogUnit signed by its author, org.verify takes a
// membership credential signed by its issuer. That is deliberate — a board
// that stamped contributions on arrival would destroy the authorship it
// exists to prove — but it means neither can be driven from the CLI alone,
// and "we tested it in a unit test where both halves are ours" is how a
// seam ships broken.
//
// So this is the agent side of those calls, using the daemon's own
// identity so the provider can resolve the signer's KEL through the hub
// exactly as it would for a real peer.
//
//	anetfixture cogunit       --home DIR [--task T] [--type claim] --body TEXT
//	anetfixture org-genesis   --home DIR [--nonce N]
//	anetfixture org-credential --home DIR --genesis B64 --subject AID [--role member]
//
// Each prints one base64 line, ready for `anet delegate … --args`.
package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/org"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "cogunit":
		err = cmdCogUnit(os.Args[2:])
	case "org-genesis":
		err = cmdOrgGenesis(os.Args[2:])
	case "org-credential":
		err = cmdOrgCredential(os.Args[2:])
	case "aid":
		err = cmdAID(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "anetfixture:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: anetfixture cogunit|org-genesis|org-credential|aid --home DIR [...]")
	os.Exit(2)
}

// load restores the controller a daemon runs as. The fixture signs AS that
// daemon rather than as a throwaway key: an object signed by a key nobody
// can resolve is rejected for the right reason and proves nothing.
func load(home string) (*identity.Controller, error) {
	if home == "" {
		return nil, fmt.Errorf("--home is required (the daemon data dir, e.g. ~/.anet)")
	}
	path := home
	if fi, err := os.Stat(filepath.Join(home, "identity.kel")); err == nil && !fi.IsDir() {
		path = filepath.Join(home, "identity.kel")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return identity.Restore(b)
}

func cmdAID(args []string) error {
	fs := flag.NewFlagSet("aid", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	fmt.Println(c.AID())
	return nil
}

func cmdCogUnit(args []string) error {
	fs := flag.NewFlagSet("cogunit", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir")
	task := fs.String("task", "", "task id this unit belongs to")
	typ := fs.String("type", "claim", "claim/evidence/conclusion/intent/retraction")
	body := fs.String("body", "", "inline payload")
	bodyCID := fs.String("body-cid", "", "CAS ref for a large payload (signed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	u := &blackboard.CogUnit{
		TaskID:  *task,
		Type:    *typ,
		Stamp:   blackboard.NewClock(c.AID()).Now(),
		Body:    []byte(*body),
		BodyCID: *bodyCID,
	}
	if err := u.Sign(c); err != nil {
		return err
	}
	b, err := u.Marshal()
	if err != nil {
		return err
	}
	id, err := u.ID()
	if err != nil {
		return err
	}
	// The id goes to stderr so stdout stays a single pipeable line, and a
	// caller can still assert the board stored the unit it was handed.
	fmt.Fprintln(os.Stderr, "unit id:", id)
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}

func cmdOrgGenesis(args []string) error {
	fs := flag.NewFlagSet("org-genesis", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir (its AID becomes the sole founder)")
	nonce := fs.String("nonce", "", "uniqueness nonce: same founders, distinct org")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	g := &org.Genesis{GovernanceRoot: []string{c.AID()}, M: 1, Nonce: *nonce}
	if err := g.Validate(); err != nil {
		return err
	}
	// The genesis travels as its canonical preimage: the org id IS the hash
	// of these bytes, so shipping any other encoding would ship an object
	// that does not hash to the org it claims to be.
	b, err := g.CanonicalPreimage()
	if err != nil {
		return err
	}
	id, err := g.OrgID()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "org id:", id)
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}

func cmdOrgCredential(args []string) error {
	fs := flag.NewFlagSet("org-credential", flag.ExitOnError)
	home := fs.String("home", "", "daemon data dir (its AID issues, so it must be a founder or admin)")
	genesis := fs.String("genesis", "", "base64 genesis from org-genesis")
	subject := fs.String("subject", "", "AID being granted membership")
	role := fs.String("role", org.RoleMember, "admin/member/guest")
	ttl := fs.Duration("ttl", time.Hour, "validity window")
	skew := fs.Duration("issued-ago", 0, "backdate issuance (for testing the window)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*home)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(*genesis)
	if err != nil {
		return fmt.Errorf("--genesis not base64: %w", err)
	}
	var g org.Genesis
	if err := coredet.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("--genesis malformed: %w", err)
	}
	orgID, err := g.OrgID()
	if err != nil {
		return err
	}
	if *subject == "" {
		*subject = c.AID()
	}
	now := time.Now().Add(-*skew).UnixMilli()
	cred := &org.Credential{
		OrgID:    orgID,
		Subject:  *subject,
		Role:     *role,
		IssuedAt: now,
		NotAfter: now + ttl.Milliseconds(),
	}
	if err := cred.Sign(c); err != nil {
		return err
	}
	b, err := org.MarshalCredential(cred)
	if err != nil {
		return err
	}
	cid, err := cred.CID()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "credential cid:", cid)
	fmt.Println(base64.StdEncoding.EncodeToString(b))
	return nil
}
