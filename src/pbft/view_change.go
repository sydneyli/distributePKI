package pbft

import (
	"crypto/sha256"
	"distributepki/util"
	"errors"
	"time"
)

// ** VIEW CHANGES ** //

func (n *PBFTNode) handleViewChange(message *ViewChange) {
	// Paper: section 4.5.2
	// if a replica receives a set of f+1 valid view change messages
	// from other replicas for views higher than its current view,
	// it sends a view-change message for the next smallest view in
	// the set, even if its timer has not expired.
	var currentView int
	if n.viewChange.inProgress {
		currentView = n.viewChange.viewNumber
	} else {
		currentView = n.viewNumber
	}
	// 0. Update most recently seen viewchange message from this node
	n.viewChange.messages[message.Node] = *message
	// 1. If a replica receives a set of f+1 valid view change messages
	// from other replicas for views higher than its current view...
	if message.ViewNumber > currentView {
		higherThanCurrent := 0
		lowestNewView := message.ViewNumber
		for _, msg := range n.viewChange.messages {
			if msg.ViewNumber > currentView {
				higherThanCurrent += 1
				if msg.ViewNumber < lowestNewView {
					lowestNewView = msg.ViewNumber
				}
			}
		}
		if higherThanCurrent >= (len(n.peermap)/3)+1 {
			// 2. Broadcast view-change messages for the next smallest
			//    view in that set.
			n.startViewChange(lowestNewView)
			return
		}
	}
	// Paper: 4.4
	// When the primary p of view v + 1 receives 2f valid view-change messages
	// for view v + 1, it multicasts NEW-VIEW.
	// < And then it does a bunch of stuff I haven't read through yet >
	// Then it /enters/ view v+1: at this point it is able to accept messages for
	// view v + 1.
	// 0. If no new view change was started, and I'm the leader of this view change
	if n.viewChange.inProgress && n.cluster.LeaderFor(message.ViewNumber) == n.id {
		// 1. See if we got 2f view-change messages for this view!
		votes := 0
		for _, msg := range n.viewChange.messages {
			if msg.ViewNumber == message.ViewNumber {
				votes += 1
			}
		}
		// 2. If so, multicast new-view (heartbeat)
		if votes >= (2 * len(n.peermap) / 3) {
			newview := NewView{
				ViewNumber:  message.ViewNumber,
				ViewChanges: n.viewChange.messages,
				PrePrepares: n.generatePrepreparesForNewView(message.ViewNumber),
				Node:        n.id,
			}
			n.newView = &newview
			n.caughtUpMux.Lock()
			for p, _ := range n.peermap {
				n.caughtUp[p] = 0
			}
			n.caughtUpMux.Unlock()
			n.enterNewView(message.ViewNumber)
			n.sendHeartbeat()
		}
	}
}

func (n *PBFTNode) handleNewView(message *NewView) {
	// Paper: section 4.4
	// A backup accepts a new-view message for view v+1 if it is signed
	// properly, if the view-change messages it contains are valid for view v+1,
	// and if the set O is correct. It multicasts prepares for each
	// message in O, and enters view + 1
	var currentView int
	if n.viewChange.inProgress {
		if message.ViewNumber == n.viewChange.viewNumber {
			n.enterNewView(message.ViewNumber)
			for _, preprepare := range message.PrePrepares {
				if preprepare.PrePrepareMessage.Number.SeqNumber > n.sequenceNumber {
					n.handlePrePrepare(&preprepare)
				}
			}
			return
		}
		currentView = n.viewChange.viewNumber
	} else {
		currentView = n.viewNumber
	}
	// TODO: validate everything correctly
	if message.ViewNumber > currentView {
		// Multicast prepares for each message in O
		// and enter view + 1
		n.enterNewView(message.ViewNumber)
		for _, preprepare := range message.PrePrepares {
			if preprepare.PrePrepareMessage.Number.SeqNumber > n.sequenceNumber {
				n.handlePrePrepare(&preprepare)
			}
		}
	}
}

func (n *PBFTNode) generateProofsSinceCheckpoint() map[SlotId]PreparedProof {
	proofs := make(map[SlotId]PreparedProof)
	for id, slot := range n.log {
		if n.lastCheckpoint.Number.Before(id) && n.isPrepared(slot) {
			proofs[id] = PreparedProof{
				Number:        id,
				Request:       *slot.request,
				RequestDigest: slot.requestDigest,
				Preprepare:    *slot.preprepare,
				Prepares:      slot.prepares,
			}
		}
	}
	return proofs
}

// Section 4.4
// If the timer of backup i expires in view v, it starts
// a view change to move the system to view v + 1.
// It stops accepting messages (other than checkpoint,
// view-change, and new-view) and multicasts a view-change
// to all other replicas.
// TODO (sydli): retransmit view change messages
func (n *PBFTNode) startViewChange(view int) {
	if n.down {
		return
	}

	if n.viewChange.viewNumber > view || n.viewChange.inProgress && n.viewChange.viewNumber == view {
		return
	}
	n.Log("START VIEW CHANGE FOR VIEW %d", view)
	n.viewChange.inProgress = true
	n.viewChange.viewNumber = view
	message := ViewChange{
		ViewNumber:      view,
		Checkpoint:      n.lastCheckpoint.Number,
		CheckpointProof: n.lastCheckpoint.Proof,
		Proofs:          n.generateProofsSinceCheckpoint(),
		Node:            n.id,
	}
	// TODO (sydli): instead of stopping this timer, use it for exponential backoff && to
	// re-transmit
	n.stopTimers()
	go broadcast(n.id, n.peermap, "PBFTNode.ViewChange", n.cluster.Endpoint, &message, time.Duration(100*time.Millisecond))
}

func (n *PBFTNode) enterNewView(view int) {
	n.Log("ENTER NEW VIEW FOR VIEW %d", view)
	n.viewChange.inProgress = false
	n.viewNumber = view
	n.sequenceNumber = 1
	if n.lastCheckpoint.Number.SeqNumber > n.sequenceNumber {
		n.sequenceNumber = n.lastCheckpoint.Number.SeqNumber
	}
	n.startTimers()
}

func (n PBFTNode) ViewChange(req *ViewChange, res *Ack) error {
	if n.down {
		return errors.New("I'm down")
	}
	n.viewChangeChannel <- req
	return nil
}

func (n PBFTNode) NewView(req *NewView, res *Ack) error {
	if n.down {
		return errors.New("I'm down")
	}
	n.newViewChannel <- req
	return nil
}

// Section 4.4 in paper
// O is computed as follows:
// 1. The primary determines the sequence number min-s of the
//    last stable checkpoint in V and the highest sequence
//    number max-s in a prepare message in V.
// 2. The primary creates a new pre-prepare message for view
//    v+1 for each sequence number n between min-s and max-s.  //    There are two cases:
//    1. There is at least one set in the P component of some
//       view-change message in V with sequence number n.
//       Primary creates Pre-prepare with the request digest
//       in the pre-prepare message for n with the highest
//       view number in V.
//    2. There is no set in P of any view-change message in V
//       with sequence number n.
//       Primary creates Pre-prepare with a no-op message.
// Then the primary appends the messages in O to its log.
// Then enters new view.
func (n *PBFTNode) generatePrepreparesForNewView(view int) map[SlotId]PrePrepareFull {
	// 1. The primary determines the sequence number min-s of the
	//    latest stable checkpoint in V and the highest sequence
	//    number max-s in a prepare message in V.
	minS := n.lastCheckpoint.Number.SeqNumber
	maxS := n.lastCheckpoint.Number.SeqNumber
	seqNums := make(map[int]requestView)
	for _, viewChange := range n.viewChange.messages {
		// 1a. Get the latest stable checkpoint in V
		if viewChange.Checkpoint.SeqNumber > minS {
			minS = viewChange.Checkpoint.SeqNumber
		}
		for num, prepareproof := range viewChange.Proofs {
			// 1b. Get the highest sequence number in any prepare message.
			//     We also want to record the ClientRequest messages
			//     for all the sequence numbers (we choose the ones with
			//     the highest view numbers), for part (2).
			reqInfo := requestView{
				view:          num.ViewNumber,
				requestDigest: prepareproof.RequestDigest,
				request:       prepareproof.Request,
			}
			if prevReqInfo, ok := seqNums[num.SeqNumber]; ok {
				// if it exists, only replace if this one has a higher viewnum
				if reqInfo.view > prevReqInfo.view {
					seqNums[num.SeqNumber] = reqInfo
				}
			} else {
				// if it doesn't exist already
				seqNums[num.SeqNumber] = reqInfo
			}
			if maxS < num.SeqNumber {
				maxS = num.SeqNumber
			}
		}
	}
	n.Log("sending prepares for messages between %d and %d", minS, maxS)
	// 2. The primary creates a new pre-prepare message for view
	//    v+1 for each sequence number n between min-s and max-s.
	preprepares := make(map[SlotId]PrePrepareFull)
	if minS < maxS {
		for s := minS; s <= maxS; s++ {
			slotId := SlotId{
				ViewNumber: view,
				SeqNumber:  s,
			}
			var request string
			var requestDigest [sha256.Size]byte
			emptyRequestDigest, err := util.GenerateDigest("")
			if err != nil {
				n.Error(" OH SHIT DIGEST EMPTY??? ")
			}
			//    There are two cases:
			if reqInfo, ok := seqNums[s]; ok {
				//    1. There is at least one set in the P component of some
				//       view-change message in V with sequence number n.
				//       Primary creates Pre-prepare with the request digest
				//       in the pre-prepare message for n with the highest
				//       view number in V.
				request = reqInfo.request
				requestDigest = reqInfo.requestDigest
			} else {
				//    2. There is no such set.
				//       Primary creates Pre-prepare with a no-op message.
				request = ""
				requestDigest = emptyRequestDigest

			}
			preprepare := PrePrepareFull{
				PrePrepareMessage: PrePrepare{
					Number:        slotId,
					RequestDigest: requestDigest,
					Digest:        [sha256.Size]byte{},
				},
				Request: request,
			}
			preprepare.PrePrepareMessage.SetDigest()
			preprepares[slotId] = preprepare
			n.log[slotId] = &Slot{
				request:       &request,
				requestDigest: requestDigest,
				preprepare:    &preprepare.PrePrepareMessage,
				prepares:      make(map[NodeId]Prepare),
				commits:       make(map[NodeId]*Commit),
				prepared:      false,
				committed:     false,
			}
		}
	}
	if maxS > n.issuedSequenceNumber {
		n.issuedSequenceNumber = maxS
	}
	return preprepares
}