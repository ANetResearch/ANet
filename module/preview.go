package module

// preview.go is the seam of stream_preview (11-modularity §3.2, K-9, K-10;
// correctness-ux §2.3). The kernel keeps what only it can guarantee: the
// protocol (the frame and its validation), the facts a frame needs
// (CUX-P4, CUX-P14), the one-shot envelope a frame travels in, and the
// small table of the rounds a requester asked previews for. The preview
// implementation keeps the cadence and the caches: the coalescer of a
// provider's frames, the stream_seq high-water marks, and the requester's
// cache of the frame a task shows.
//
// Nothing here reaches a store. The implementation cannot read the
// interactions database, the outbox or the evidence chain: its Host is
// granted PreviewSeam alone (not StateDir, RecordEvidence, TaskSeam or
// Providers), so "previews are never stored" (SI-73) holds by what the
// module is handed (TestHostGrantsFollowNameTable,
// TestSeamClosureHasNoStore).

import "context"

// PreviewContent is what a frame shows: the tails of the text and of the
// reasoning, the bytes before each, and when each was taken (unix ms).
type PreviewContent struct {
	Text             string
	Omitted          uint64
	TextAt           uint64
	Reasoning        string
	ReasoningOmitted uint64
	ReasoningAt      uint64
}

// Same reports whether c and o show the same thing (their times aside).
func (c PreviewContent) Same(o PreviewContent) bool {
	return c.Text == o.Text && c.Omitted == o.Omitted && c.Reasoning == o.Reasoning && c.ReasoningOmitted == o.ReasoningOmitted
}

// Empty reports whether c shows nothing.
func (c PreviewContent) Empty() bool { return c.Text == "" && c.Reasoning == "" }

// PreviewSnapshot is a frame a requester took (CUX-P15): what the task's
// preview shows until the task moves on.
type PreviewSnapshot struct {
	// Answers is the id of the requester message the frame answers.
	Answers string
	// Seq is the frame's stream_seq.
	Seq uint64
	PreviewContent
	// At is when the frame was taken here (unix ms).
	At uint64
}

// PreviewIn is a frame as receive step 9 hands it to the implementation,
// after the kernel judged CUX-P14 items 1 to 3.
type PreviewIn struct {
	// IX is the task; From is the frame's sender.
	IX, From string
	PreviewSnapshot
	// Epoch is what Previewer.Epoch said before the facts were read.
	Epoch uint64
}

// PreviewOut is a frame the implementation hands to the kernel. The kernel
// takes the task's current round from its own table, never from here: Mode
// and the node's setting are not part of the request.
type PreviewOut struct {
	// Answers is the round the frame was made for.
	Answers string
	// Seq is the frame's stream_seq.
	Seq uint64
	PreviewContent
}

// PreviewSendResult is what SendFrame did with a frame. Sent is false when
// the kernel did not hand the frame on (a fact of CUX-P4 failed, the frame
// would not validate); Handed and HubReason are the one-shot result
// (ENG-O8b): the hub took the envelope, or the reason it did not.
type PreviewSendResult struct {
	Sent      bool
	Handed    bool
	HubReason string
}

// PreviewOffer is the answer to Previewer.Offer: whether the round took
// what was offered.
type PreviewOffer struct {
	Taken bool
}

// Previewer is the implementation of stream_preview (K-9). The kernel calls
// it from the places the protocol fixes and only after it judged the
// facts; the implementation answers from memory.
type Previewer interface {
	// Requested records the round a requester's message opened on task ix
	// (CUX-P0): answers is the message's id, mode its anet.preview_mode as
	// the kernel normalized it ("" when it asked for none). A copy of the
	// same message changes nothing.
	Requested(ix, answers, mode string)
	// Offer gives the implementation what the provider has written so far
	// in the round (the whole text and reasoning, not a delta).
	Offer(ix, text, reasoning string) PreviewOffer
	// CancelRound drops the frames of task ix not yet handed over: a real
	// answer or result of the round was queued in the same call (ENG-K20).
	CancelRound(ix string)
	// Epoch is read before a frame's facts are judged and passed in
	// PreviewIn; Arrived refuses a frame whose task was cleared since.
	Epoch() uint64
	// Arrived keeps a frame whose facts the kernel has judged when the
	// frame is within the task's and the peer's limits (CUX-P14 items 4
	// and 5): a stream_seq above the task's, one frame per task per 2
	// seconds, ten per peer per second.
	Arrived(in PreviewIn) bool
	// Clear forgets the preview of task ix: a message or a result was
	// stored on it, or it ended (CUX-P16).
	Clear(ix string)
	// Current is the preview task ix shows now.
	Current(ix string) (PreviewSnapshot, bool)
}

// PreviewHost is the optional Host interface of the preview module (K-9).
// It is granted to preview alone.
type PreviewHost interface {
	// PreviewSeam is the seam, or (nil, false) while this node cannot send
	// frames.
	PreviewSeam() (PreviewSeam, bool)
}

// PreviewSeam is the one thing a preview implementation may do as this
// node: send a stream_preview frame, and say a frame was taken. It cannot
// send anything else, on any task but the current round of an inbound text
// task that has not ended, and it reads nothing.
type PreviewSeam interface {
	// Mode is this node's stream_preview now: "off", "on" or
	// "with_reasoning".
	Mode() string
	// SendFrame makes the frame o of task ix a stream_preview envelope and
	// sends it once. The kernel checks the facts again (the task, the
	// round, the reasoning gate, ValidatePreview) and sends nothing when
	// one fails. The frame is not queued, stored or retried.
	SendFrame(ctx context.Context, ix string, o PreviewOut) PreviewSendResult
	// Published says task ix has a new preview to show. The kernel
	// publishes an event that carries no text: a subscriber reads the
	// preview with Previewer.Current.
	Published(ix string)
}
