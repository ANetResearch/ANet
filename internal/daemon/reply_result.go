package daemon

// reply_result.go is the requester's half of a reply folded into its result
// (correctness-ux §4.3.3): the provider completes a text task with its last
// word in ResultResp.reply (completeWithReply), and the requester checks
// that reply against the transcript the receipt covers (CUX-R2), checks the
// transcript's account of what it said itself (CUX-R6 item 4), and stores
// the reply with the result in one transaction.

import (
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// reasonReceiptMismatch is the code of a result whose transcript changes
// what this node said (CUX-R6 item 4; ux §1.4): anet.receipt_verified is
// mismatch, and a review of it is refused with this code.
const reasonReceiptMismatch = "receipt_mismatch"

// errReplyMismatch is the detail of a reply-mismatch refusal.
var errReplyMismatch = errors.New("the reply is not the last provider entry of the transcript")

// replyMatches is CUX-R2: a result's reply must be the last entry of the
// transcript it carries that the provider said — the same body byte for
// byte and the same files in the same order (name, type, size, content id).
// A transcript that does not parse, or has no provider entry, matches no
// reply. The reply's message id is bound where the reply is stored
// (replyRow.writeTx): a transcript carries no message ids.
func replyMatches(deliverable []byte, r *delegation.ReplyMsg) error {
	tr, err := transcript.Parse(deliverable)
	if err != nil {
		return fmt.Errorf("%w: %v", errReplyMismatch, err)
	}
	for i := len(tr.Messages) - 1; i >= 0; i-- {
		e := tr.Messages[i]
		if e.From != transcriptProvider {
			continue
		}
		if e.Body != r.Body || !sameFiles(e.Attachments, wireFiles(r.Attachments)) {
			return errReplyMismatch
		}
		return nil
	}
	return fmt.Errorf("%w: the transcript has no provider entry", errReplyMismatch)
}

// wireFiles is the fingerprints of a message's files as they travel.
func wireFiles(atts []delegation.Attachment) []transcript.Attachment {
	out := make([]transcript.Attachment, 0, len(atts))
	for _, a := range atts {
		out = append(out, transcript.Attachment{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
	}
	return out
}

// sameFiles reports whether two lists of file fingerprints are equal, item
// by item and in order.
func sameFiles(a, b []transcript.Attachment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameEntry reports whether two transcript entries say the same thing: the
// same body and the same files in the same order.
func sameEntry(a, b transcript.Message) bool {
	return a.Body == b.Body && sameFiles(a.Attachments, b.Attachments)
}

// requesterEntriesMatch is CUX-R6 item 4: every entry of the transcript tr
// that the requester said must be found, in order, among the messages this
// node sent (sent: its own conversation turns, oldest first), each the same
// body and files as one of them — a greedy subsequence match. A message
// this node sent that the transcript leaves out is not a mismatch (the
// receipt does not prove what it omits, correctness-ux §6.3); one it adds
// or changes is.
func requesterEntriesMatch(sent, tr []transcript.Message) bool {
	j := 0
	for _, e := range tr {
		if e.From != transcriptRequester {
			continue
		}
		for j < len(sent) && !sameEntry(sent[j], e) {
			j++
		}
		if j == len(sent) {
			return false
		}
		j++
	}
	return true
}

// ownTurns is what this node said on ix, as a transcript would list it.
func ownTurns(ix *interactions.Interaction, self string, msgs []interactions.Message,
	atts []interactions.Attachment) []transcript.Message {
	conv := conversationOf(ix, self, msgs, atts)
	out := conv[:0]
	for _, e := range conv {
		if e.From == transcriptRequester {
			out = append(out, e)
		}
	}
	return out
}

// conversationMismatch reports, in tx, whether the transcript of a text
// task's result changes what this node said on it (CUX-R6 item 4). A
// transcript that does not parse cannot show it did not: it is a mismatch.
func (d *Daemon) conversationMismatch(tx *interactions.Tx, ix *interactions.Interaction, deliverable []byte) (bool, error) {
	if ix.IsCapability {
		return false, nil
	}
	tr, err := transcript.Parse(deliverable)
	if err != nil {
		return true, nil
	}
	msgs, err := tx.Messages(ix.ID)
	if err != nil {
		return false, err
	}
	atts, err := tx.Attachments(ix.ID)
	if err != nil {
		return false, err
	}
	return !requesterEntriesMatch(ownTurns(ix, d.AID(), msgs, atts), tr.Messages), nil
}

// replyRow is a result's reply as the requester stores it: a conversation
// message from the provider, with its files.
type replyRow struct {
	msgID string
	body  string
	files []interactions.Attachment
	// seq and stored are what writeTx did: the row's seq, and whether it
	// wrote it (false when it was already there).
	seq    int64
	stored bool
}

// newReplyRow is the row of r, a reply that passed CUX-R2. A file whose
// bytes are not what its fingerprint says is a reply-mismatch: the
// transcript names the fingerprint, and these bytes are not that file. Its
// type is stored as the bytes say, as for any message received.
func newReplyRow(r *delegation.ReplyMsg) (*replyRow, error) {
	if r == nil {
		return nil, nil
	}
	files, err := receivedAttachments(r.Attachments)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errReplyMismatch, err)
	}
	return &replyRow{msgID: r.MsgID, body: r.Body, files: files}, nil
}

// writeTx stores the reply on ix in tx, once: a message already stored
// under its message id is the reply only when it is the provider's text
// with the same body and files, and anything else stored under that id is a
// reply-mismatch (CUX-R2: the reply's key 9 is the id of the entry it is).
func (r *replyRow) writeTx(tx *interactions.Tx, ix, provider string) error {
	if r == nil {
		return nil
	}
	prior, err := tx.MessageByMsgID(ix, r.msgID)
	switch {
	case errors.Is(err, interactions.ErrNotFound):
	case err != nil:
		return err
	default:
		if prior.SenderAID != provider || prior.Kind != interactions.MsgText || prior.Body != r.body ||
			hasControlMeta(prior.Metadata) {
			return rxPermanent(dropReplyMismatch, fmt.Errorf("%w: another message has id %s", errReplyMismatch, r.msgID))
		}
		atts, err := tx.Attachments(ix)
		if err != nil {
			return err
		}
		var have []transcript.Attachment
		for _, a := range atts {
			if a.MsgSeq == prior.Seq {
				have = append(have, transcript.Attachment{Name: a.Name, Mime: a.Mime, Size: a.Size, CID: a.CID})
			}
		}
		var want []transcript.Attachment
		for _, f := range r.files {
			want = append(want, transcript.Attachment{Name: f.Name, Mime: f.Mime, Size: f.Size, CID: f.CID})
		}
		if !sameFiles(have, want) {
			return rxPermanent(dropReplyMismatch, fmt.Errorf("%w: message %s has other files", errReplyMismatch, r.msgID))
		}
		r.seq, r.stored = prior.Seq, false
		return nil
	}
	if r.seq, r.stored, err = tx.AddMessageRecord(interactions.MessageRecord{InteractionID: ix, SenderAID: provider,
		Kind: interactions.MsgText, Body: r.body, MsgID: r.msgID}); err != nil {
		return err
	}
	if !r.stored {
		return nil
	}
	return addAttachmentsTx(tx, ix, r.seq, r.files)
}

// announce publishes the stored reply, after the commit.
func (r *replyRow) announce(d *Daemon, ix string) {
	if r != nil && r.stored {
		d.publishMessage(ix, r.seq, interactions.MsgText)
	}
}
