package pbft

import (
	"distributepki/util"

	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/rpc"
	"sync"
	"time"
)

type PBFTNode struct {
	//////
	// Immutable config data
	//////
	id         NodeId
	host       string
	port       int
	primary    bool
	peermap    map[NodeId]string // id => hostname
	hostToPeer map[string]NodeId // hostname => id
	cluster    ClusterConfig

	// MAIN MESSAGE CHANNELS.
	// Main execution loop selects from these.
	debugChannel          chan *DebugMessage
	requestChannel        chan *string
	preprepareChannel     chan *PrePrepareFull
	prepareChannel        chan *Prepare
	commitChannel         chan *Commit
	viewChangeChannel     chan *ViewChange
	newViewChannel        chan *NewView
	requestTimeoutChannel chan bool

	// CLIENT CHANNELS.
	// We write to these when we learn the
	// result of a client request.
	errorChannel     chan error
	committedChannel chan *string

	// Requests: did they finish yet?
	requests map[[sha256.Size]byte]requestInfo

	// local key grabbing hack
	KeyRequest chan *KeyRequest // hostname

	//////
	// The below are all mutable, but writes should ALWAYS
	// happen on the main routine unless otherwise indicated.
	//////

	// LEDGER (and where we are in it)
	// Sequence numbers have to start at 1 for a subtle reason.
	// a node with seqnum 0 hasn't yet "committed" to the
	// current view.
	log                  map[SlotId]*Slot
	viewNumber           int
	sequenceNumber       int
	issuedSequenceNumber int

	// VIEW CHANGE STATE. We are in the middle of a viewchange
	// if viewChange.inProgress.
	viewChange *viewChangeInfo

	// CHECKPOINT STATE.
	lastCheckpoint  SlotId
	checkpointProof map[NodeId]Checkpoint

	// TIMEOUTS. The heartbeat ticker allows the primary to
	// continually send timeouts; replicas use the timeout
	// timer to determine if the leader has been active.
	heartbeatTicker *time.Ticker
	timeoutTimer    *time.Timer

	// LEADER STATE (to catch up stragglers)
	// if some nodes aren't caught up, newView is broadcasted
	// to them instead of regular PrePrepare heartbeats.
	// note: caughtUp is written to in a goroutine when peers reply
	// to heartbeats, so we lock it.
	// are all my peers caught up? map peer => current seqnum
	caughtUp    map[NodeId]int
	caughtUpMux sync.RWMutex
	newView     *NewView // view message to continually broadcast

	// Debug states
	down bool
	slow bool
}

type requestInfo struct {
	committed bool
	// also info about the reply that we sent
}

// Information associated with current view change.
type viewChangeInfo struct {
	messages   map[NodeId]ViewChange
	inProgress bool
	viewNumber int
}

type KeyRequest struct {
	Hostname string
	Reply    chan *string
}

// Heartbeat ticker
const TIMEOUT time.Duration = time.Duration(time.Second)

// How many sequence numbers to wait before checkpointing
const CHECKPOINT uint = 5

// Entry point for each PBFT node.
// NodeConfig: configuration for this node
// ClusterConfig: configuration for entire cluster
// ready channel: send item when node is up & running
func StartNode(host NodeConfig, cluster ClusterConfig) *PBFTNode {
	// 1. Create id <=> peer hostname maps from list
	peermap := make(map[NodeId]string)
	hostToPeer := make(map[string]NodeId)
	for _, p := range cluster.Nodes {
		if p.Id != host.Id {
			hostname := util.GetHostname(p.Host, p.Port)
			peermap[p.Id] = hostname
			hostToPeer[hostname] = p.Id
		}
	}
	// 2. Create the node
	node := PBFTNode{
		id:                    host.Id,
		host:                  host.Host,
		port:                  host.Port,
		peermap:               peermap,
		hostToPeer:            hostToPeer,
		cluster:               cluster,
		debugChannel:          make(chan *DebugMessage),
		committedChannel:      make(chan *string),
		errorChannel:          make(chan error),
		requestChannel:        make(chan *string, 10), // some nice inherent rate limiting
		preprepareChannel:     make(chan *PrePrepareFull),
		prepareChannel:        make(chan *Prepare),
		commitChannel:         make(chan *Commit),
		viewChangeChannel:     make(chan *ViewChange),
		newViewChannel:        make(chan *NewView),
		requestTimeoutChannel: make(chan bool),
		requests:              make(map[[sha256.Size]byte]requestInfo),
		log:                   make(map[SlotId]*Slot),
		viewNumber:            0,
		sequenceNumber:        1,
		issuedSequenceNumber:  1,
		viewChange:            &viewChangeInfo{inProgress: false, viewNumber: 0, messages: make(map[NodeId]ViewChange)},
		lastCheckpoint:        SlotId{ViewNumber: 0, SeqNumber: 0},
		checkpointProof:       make(map[NodeId]Checkpoint),
		heartbeatTicker:       nil,
		timeoutTimer:          nil,
		caughtUp:              make(map[NodeId]int),
		newView:               &NewView{ViewNumber: 0, Node: host.Id},
		down:                  false,
		slow:                  false,
	}
	for p, _ := range node.peermap {
		node.caughtUp[p] = 1
	}

	// 3. Start RPC server
	server := rpc.NewServer()
	server.Register(&node)
	node.Log(cluster.Endpoint)
	server.HandleHTTP(cluster.Endpoint, "/debug/"+cluster.Endpoint)
	node.Log("Listening on %v", cluster.Endpoint)
	listener, e := net.Listen("tcp", util.GetHostname("", node.port))
	if e != nil {
		node.Error("Listen error: %v", e)
		return nil
	}
	if node.isPrimary() {
		node.heartbeatTicker = time.NewTicker(node.getTimeout())
	} else {
		node.timeoutTimer = time.NewTimer(node.getTimeout())
	}
	go http.Serve(listener, nil)
	// 4. Start exec loop
	go node.handleMessages()
	return &node
}

// ** HELPERS ** //

// Helper functions for logging! (prepends node id to logs) //
func (n PBFTNode) Log(format string, args ...interface{}) {
	args = append([]interface{}{n.id}, args...)
	plog.Infof("[Node %d] "+format, args...)
}

func (n PBFTNode) Error(format string, args ...interface{}) {
	args = append([]interface{}{n.id}, args...)
	plog.Fatalf("[Node %d] "+format, args...)
}

// ensure mapping from SlotId exists in PBFTNode
func (n *PBFTNode) ensureMapping(num SlotId) *Slot {
	slot, ok := n.log[num]
	if !ok {
		slot = Slot{
			request:       nil,
			requestDigest: [sha256.Size]byte{},
			preprepare:    nil,
			prepares:      make(map[NodeId]Prepare),
			commits:       make(map[NodeId]*Commit),
			prepared:      false,
			committed:     false,
		}
		n.log[num] = slot
	}
	return slot
}

func (n PBFTNode) keyFor(hostname string) (*string, error) {
	request := KeyRequest{
		Hostname: hostname,
		Reply:    make(chan *string),
	}
	n.KeyRequest <- &request
	if response := <-request.Reply; response != nil {
		return response, nil
	}
	return nil, errors.New("eft, no key")
}

func (n PBFTNode) isPrimary() bool {
	return n.cluster.LeaderFor(n.viewNumber) == n.id
}

func (n PBFTNode) getPrimary() (NodeId, string) {
	primaryId := n.cluster.LeaderFor(n.viewNumber)
	for i, p := range n.peermap {
		if i == primaryId {
			return i, p
		}
	}
	return NodeId(0), ""
}

func (n PBFTNode) isPrepared(slot *Slot) bool {
	// # Prepares received >= 2f = 2 * ((N - 1) / 3)
	return slot.preprepare != nil && len(slot.prepares) >= 2*(len(n.peermap)/3)
}

func (n PBFTNode) isCommitted(slot *Slot) bool {
	// # Commits received >= 2f = 2 * ((N - 1) / 3)
	return len(slot.commits) >= 2*(len(n.peermap)/3)
}

// ** ALL THE MESSAGE HANDLERS ** //
// MAIN EXECUTION LOOP
func (n *PBFTNode) handleMessages() {
	for {
		select {
		// come from RPCS
		case msg := <-n.debugChannel:
			n.handleDebug(msg)
		case msg := <-n.preprepareChannel:
			n.handlePrePrepare(msg)
		case msg := <-n.requestChannel:
			n.handleClientRequest(msg)
		case msg := <-n.prepareChannel:
			n.handlePrepare(msg)
		case msg := <-n.commitChannel:
			n.handleCommit(msg)
		case msg := <-n.viewChangeChannel:
			n.handleViewChange(msg)
		case msg := <-n.newViewChannel:
			n.handleNewView(msg)
		// Come from internal timers
		case <-n.requestTimeoutChannel: // one of my client requests timed out!
			n.startViewChange(n.viewNumber + 1)
		case <-n.getTimer(): // timer expired
			n.handleHeartbeatTimeout()
		}
	}
}

// does appropriate actions after receivin a client request
// i.e. send out preprepares and stuff
func (n *PBFTNode) handleClientRequest(request *string) {
	if n.viewChange.inProgress {
		return
	}
	requestDigest, err := util.GenerateDigest(*request)
	if _, ok := n.requests[requestDigest]; ok {
		// we've already processed this client request
		return
	}
	n.requests[requestDigest] = requestInfo{committed: false}

	if n.isPrimary() {
		if request == nil {
			return
		}
		if err != nil {
			n.Log(err.Error())
			return
		}
		n.issuedSequenceNumber = n.issuedSequenceNumber + 1
		n.Log("ISSUED SEQNUM %d for %d", n.issuedSequenceNumber, request.Id)
		id := SlotId{
			ViewNumber: n.viewNumber,
			SeqNumber:  n.issuedSequenceNumber,
		}
		n.Log("Received new request - View Number: %d, Sequence Number: %d", n.viewNumber, n.sequenceNumber)
		fullMessage := PrePrepareFull{
			PrePrepareMessage: PrePrepare{
				Number:        id,
				RequestDigest: requestDigest,
				Digest:        [sha256.Size]byte{},
			},
			Request: *request,
		}
		n.log[id] = Slot{
			request:       request,
			requestDigest: requestDigest,
			preprepare:    &fullMessage.PrePrepareMessage,
			prepares:      make(map[NodeId]Prepare),
			commits:       make(map[NodeId]*Commit),
			prepared:      false,
			committed:     false,
		}
		go n.broadcast("PBFTNode.PrePrepare", &fullMessage, 0)
	} else {
		// forward to all ma frandz if im not da leader
		// TODO: they should probably just forward it to the leader
		go n.broadcast("PBFTNode.ClientRequest", request, 0)
		// primaryId, primaryHostname := n.getPrimary()
		// n.Log("primary id: %d", primaryId)
		// go sendRpc(n.id, primaryId, primaryHostname, "PBFTNode.ClientRequest", n.cluster.Endpoint, request, nil, 5)

		// TODO: do some sort of timeout for a view change
		/*
			// start execution timer...
			go func(n *PBFTNode) {
				<-time.After(TIMEOUT)
				if !n.requests[request.Id].committed {
					n.requestTimeoutChannel <- true
				}
			}(n)
		*/
	}
}

// TODO: verification should include:
//     1. only the leader of the specified view can send preprepares
// TODO: handle high, low water marks (Paper 4.2)
func (n *PBFTNode) handlePrePrepare(preprepare *PrePrepareFull) {
	if n.viewChange.inProgress || n.isPrimary() {
		return
	}
	// A backup accepts a pre-prepare message provided:
	// -  the signatures in the request and the pre-prepare message are
	//    correct and d is the digest for message m
	// -  it is in view v
	// -  it has not accepted a pre-prepare message for view v and seq
	//    num n containing a different digest
	// -  the sequence number in the preprepare message is between the
	//    low water-mark h and high water-mark H
	// TODO: Drop invalid Preprepares...
	// verification should include:
	//     1. only the leader of the specified view can send preprepares
	n.timeoutTimer.Reset(n.getTimeout())

	preprepareMessage := preprepare.PrePrepareMessage
	if preprepareMessage == (PrePrepare{}) {
		//NO-OP heartbeat... don't bother processing
		return
	}
	if !preprepareMessage.DigestValid() {
		n.Log("Error: PrePrepare digest does not match data")
		return
	}

	requestDigest, err := util.GenerateDigest(preprepare.Request)
	if err != nil {
		n.Log(err.Error())
		return
	}

	if preprepareMessage.RequestDigest != requestDigest {
		n.Log("Error: PrePrepare request digest does not match the request data")
		return
	}

	slot := n.ensureMapping(preprepareMessage.Number)

	// XXX: assumes that it's correct to NOT resend Prepares if we've already seen or sent the PrePrepare (i.e. request != nil)
	if slot.request != nil {
		if slot.requestDigest != preprepareMessage.RequestDigest {
			plog.Errorf("Received pre-prepare for slot id %+v with mismatched digest.", preprepareMessage.Number)
		}
		return
	}

	newSeqNum := preprepare.PrePrepareMessage.Number.SeqNumber
	if newSeqNum > n.issuedSequenceNumber {
		n.issuedSequenceNumber = newSeqNum
	}

	slot.request = &preprepare.Request
	slot.requestDigest = preprepareMessage.RequestDigest
	slot.preprepare = &preprepareMessage

	// TODO: iterate through potentially existing Prepare and Commit entries and check hashes, throw away non-matching with warning

	prepare := Prepare{
		Number:        preprepareMessage.Number,
		RequestDigest: preprepareMessage.RequestDigest,
		Node:          n.id,
		Digest:        [sha256.Size]byte{},
	}
	prepare.SetDigest()

	slot.prepares[n.id] = &prepare
	log[preprepareMessage.Number].preprepare = preprepare.PrePrepareMessage
	go n.broadcast("PBFTNode.Prepare", &prepare, 0)
}

func (n *PBFTNode) handlePrepare(message *Prepare) {
	if n.viewChange.inProgress {
		return
	}

	if !message.DigestValid() {
		n.Log("Error: Prepare digest does not match data")
		return
	}

	slot := n.ensureMapping(message.Number)
	if slot.request != nil && slot.requestDigest != message.RequestDigest {
		plog.Errorf("Received prepare for slot id %+v with mismatched digest.", message.Number)
	}
	slot.prepares[message.Node] = message

	n.log[message.Number] = slot
	if !slot.prepared && n.isPrepared(slot) {
		n.Log("PREPARED %+v", message.Number)
		slot.prepared = true

		commit := Commit{
			Number:        message.Number,
			RequestDigest: message.RequestDigest,
			Node:          n.id,
			Digest:        [sha256.Size]byte{},
		}
		commit.SetDigest()
		slot.commits[n.id] = &commit
		go n.broadcast("PBFTNode.Commit", &commit, 0)
	}
}

func (n *PBFTNode) handleCommit(message *Commit) {
	if n.viewChange.inProgress {
		return
	}

	if !message.DigestValid() {
		n.Log("Error: Commit digest does not match data")
		return
	}

	slot := n.ensureMapping(message.Number)
	if slot.request != nil && slot.requestDigest != message.RequestDigest {
		plog.Errorf("Received commit for slot id %+v with mismatched digest.", message.Number)
	}
	slot.commits[message.Node] = message

	if !slot.committed && n.isCommitted(slot) {
		n.Log("COMMITTED %+v", message.Number)
		slot.committed = true
		// info := n.requests[message.Message.Id] //.committed = true
		// n.requests[message.Message.Id] = requestInfo{
		// 	id:        info.id,
		// 	committed: true,
		// 	request:   info.request,
		// }
		if message.Number.SeqNumber > n.sequenceNumber {
			n.sequenceNumber = message.Number.SeqNumber
		}
	}
	n.log[message.Number] = slot
	if nowCommitted {
		// TODO: try to execute as many sequential queries as possible and
		// then reply to the clients via committedChannel. Figure out what
		// the highest committed operation sequence number was and apply
		// up to that
		// TODO: fix this
		n.Committed() <- slot.request
	}
}

type requestView struct {
	request ClientRequest
	view    int
}

// Section 4.4 in paper
// O is computed as follows:
// 1. The primary determines the sequence number min-s of the
//    last stable checkpoint in V and the highest sequence
//    number max-s in a prepare message in V.
// 2. The primary creates a new pre-prepare message for view
//    v+1 for each sequence number n between min-s and max-s.
//    There are two cases:
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
	minS := n.lastCheckpoint.SeqNumber
	maxS := n.lastCheckpoint.SeqNumber
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
				view:    num.ViewNumber,
				request: prepareproof.Preprepare.Request,
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
			var request ClientRequest
			//    There are two cases:
			if reqInfo, ok := seqNums[s]; ok {
				//    1. There is at least one set in the P component of some
				//       view-change message in V with sequence number n.
				//       Primary creates Pre-prepare with the request digest
				//       in the pre-prepare message for n with the highest
				//       view number in V.
				request = reqInfo.request
			} else {
				//    2. There is no such set.
				//       Primary creates Pre-prepare with a no-op message.
				request = ClientRequest{}
			}
			preprepare := PrePrepareFull{
				PrePrepareMessage: PrePrepare{Number: slotId},
				Request:           request,
			}
			preprepares[slotId] = preprepare
			n.log[slotId] = Slot{
				request:    &request,
				preprepare: &preprepare,
				prepares:   make(map[NodeId]Prepare),
				commits:    make(map[NodeId]*Commit),
				prepared:   false,
				committed:  false,
			}
		}
	}
	n.issuedSequenceNumber = maxS
	return preprepares
}

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
	// and *if the set O is correct* (TODO). It multicasts prepares for each
	// message in O, and enters view + 1
	var currentView int
	if n.viewChange.inProgress {
		if message.ViewNumber == n.viewChange.viewNumber {
			n.enterNewView(message.ViewNumber)
			for _, preprepare := range message.PrePrepares {
				n.handlePrePrepare(&preprepare)
			}
			return
		}
		currentView = n.viewChange.viewNumber
	} else {
		currentView = n.viewNumber
	}
	// TODO: validate everything
	if message.ViewNumber > currentView {
		// Multicast prepares for each message in O
		// and enter view + 1
		n.enterNewView(message.ViewNumber)
		for _, preprepare := range message.PrePrepares {
			n.handlePrePrepare(&preprepare)
		}
	}
}

// ** TIMER/HEARTBEAT STUFF ** //

func (n *PBFTNode) getTimeout() time.Duration {
	// When primary times out, it send a heartbeat
	if n.isPrimary() {
		return TIMEOUT
	} else {
		// When non-primaries timeout with no heartbeat,
		// they start view change
		return TIMEOUT * time.Duration(len(n.peermap))
	}
}

func (n *PBFTNode) handleHeartbeatTimeout() {
	if n.viewChange.inProgress {
		return
	}
	if n.down {
		return
	}
	if !n.isPrimary() {
		n.startViewChange(n.viewNumber + 1)
	} else {
		n.sendHeartbeat()
	}
}

func (n *PBFTNode) heartbeatMessage(peerSequence int) PrePrepareFull {
	if n.sequenceNumber == peerSequence {
		return PrePrepareFull{
			PrePrepareMessage: PrePrepare{},
			Request:           "",
		}
	}
	slot := SlotId{
		ViewNumber: n.viewNumber,
		SeqNumber:  peerSequence + 1,
	}
	return *(n.log[slot].preprepare)
}

// TODO (sydli): Clean up the below. It's gross.
func (n *PBFTNode) sendHeartbeat() {
	if !n.isPrimary() {
		return
	}
	for id, hostname := range n.peermap {
		var rpcType string
		var msg interface{}
		var after func(NodeId, PPResponse, error)
		// TOCTOU isn't too dangerous here since
		// it just means we send an unnecessary preprepare
		// that the peer has already received before
		n.caughtUpMux.RLock()
		caughtUp := n.caughtUp[id]
		n.caughtUpMux.RUnlock()
		if caughtUp == 0 {
			rpcType = "PBFTNode.NewView"
			msg = n.newView
			after = func(id NodeId, response PPResponse, err error) {
				if err == nil {
					n.caughtUpMux.Lock()
					n.caughtUp[id] = 1
					n.caughtUpMux.Unlock()
				}
			}
		} else {
			rpcType = "PBFTNode.PrePrepare"
			msg = n.heartbeatMessage(caughtUp)
			after = func(id NodeId, response PPResponse, err error) {
				if err == nil {
					n.caughtUpMux.Lock()
					n.caughtUp[id] = response.SeqNumber
					n.caughtUpMux.Unlock()
				}
			}
		}
		go func(id NodeId, hostname string, rpcType string, msg interface{}, after func(NodeId, PPResponse, error)) {
			resp := PPResponse{}
			err := sendRpc(n.id, id, hostname, rpcType, n.cluster.Endpoint, msg, &resp, 1, time.Duration(100*time.Millisecond))
			after(id, resp, err)
		}(id, hostname, rpcType, msg, after)
	}
}

func (n *PBFTNode) getTimer() <-chan time.Time {
	if n.isPrimary() {
		return n.heartbeatTicker.C
	} else {
		return n.timeoutTimer.C
	}
}

func (n *PBFTNode) stopTimers() {
	// TODO: properly close these guys (flush channels):
	// https://github.com/golang/go/issues/14383
	if n.timeoutTimer != nil {
		n.timeoutTimer.Stop()
	}
	if n.heartbeatTicker != nil {
		n.heartbeatTicker.Stop()
	}
}

func (n *PBFTNode) startTimers() {
	if n.isPrimary() {
		n.heartbeatTicker = time.NewTicker(n.getTimeout())
	} else {
		n.timeoutTimer = time.NewTimer(n.getTimeout())
	}
}

// ** VIEW CHANGES ** //

func (n *PBFTNode) generateProofsSinceCheckpoint() map[SlotId]PreparedProof {
	proofs := make(map[SlotId]PreparedProof)
	for id, slot := range n.log {
		if n.lastCheckpoint.Before(id) && n.isPrepared(slot) {
			proofs[id] = PreparedProof{
				Number:     id,
				Preprepare: *slot.preprepare,
				Prepares:   slot.prepares,
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
		Checkpoint:      n.lastCheckpoint,
		CheckpointProof: n.checkpointProof,
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
	n.startTimers()
}

func (n PBFTNode) Failure() chan error {
	return n.errorChannel
}

func (n PBFTNode) Committed() chan *string {
	return n.committedChannel
}

func (n *PBFTNode) Propose(operation *string) {
	n.requestChannel <- operation
}

func (n PBFTNode) GetCheckpoint() (interface{}, error) {
	// TODO: implement
	return nil, nil
}

func (n PBFTNode) ClientRequest(req *string, res *Ack) error {
	if n.down {
		return errors.New("I'm down")
	}
	n.requestChannel <- req
	return nil
}

func (n PBFTNode) PrePrepare(req *PrePrepareFull, res *PPResponse) error {
	if n.down {
		return errors.New("I'm down")
	}
	n.preprepareChannel <- req
	res.SeqNumber = n.sequenceNumber
	return nil
}

func (n PBFTNode) Prepare(req *Prepare, res *Ack) error {
	if n.down {
		return errors.New("I'm down")
	}
	n.prepareChannel <- req
	return nil
}

func (n PBFTNode) Commit(req *Commit, res *Ack) error {
	if n.down {
		return errors.New("I'm down")
	}
	n.commitChannel <- req
	return nil
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

func (n PBFTNode) broadcast(rpcName string, message interface{}, timeout time.Duration) {
	broadcast(n.id, n.peermap, rpcName, n.cluster.Endpoint, message, timeout)
}

// ** RPC helpers ** //
func broadcast(fromId NodeId, peers map[NodeId]string, rpcName string, endpoint string, message interface{}, timeout time.Duration) {
	for i, p := range peers {
		go func(i NodeId, hostname string) {
			err := sendRpc(fromId, i, hostname, rpcName, endpoint, message, nil, 10, 0)
			if err != nil {
				plog.Info(err)
			}
		}(i, p)
	}
}

func sendRpc(fromId NodeId, peerId NodeId, hostName string, rpcName string, endpoint string, message interface{}, response interface{}, retries int, timeout time.Duration) error {
	plog.Infof("[Node %d] Sending RPC (%s) to Node %d", fromId, rpcName, peerId)
	return util.SendRpc(hostName, endpoint, rpcName, message, response, retries, 0)
}
