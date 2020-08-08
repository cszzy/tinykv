// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"math/rand"
	"sort"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// number of ticks since it reached last electionTimeout
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64

	//随机选举超时时间,避免选不出leader
	randomElectionTimeout int
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	r := &Raft{
		id: c.ID,
		RaftLog: newLog(c.Storage),
		Prs: make(map[uint64]*Progress),
		//first it's StateFollower
		votes: make(map[uint64]bool),
		msgs: make([]pb.Message, 0),
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout: c.ElectionTick,
		randomElectionTimeout: c.ElectionTick + rand.Intn(c.ElectionTick),
	}
	lastindex := r.RaftLog.LastIndex()
	for _, pr := range c.peers{
		if pr == r.id{
			r.Prs[pr] = &Progress{
				Next: lastindex + 1,
				Match: lastindex,
			}
		} else {
			r.Prs[pr] = &Progress{
				Next: lastindex + 1,
			}
		}
	}
	r.becomeFollower(0,None)
	state, _, _ := r.RaftLog.storage.InitialState()
	r.Term, r.Vote, r.RaftLog.committed = state.GetTerm(), state.GetVote(), state.GetCommit()
	return r
}

// sendAppend sends an append RPC with new entries (if any) and the
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	//leader[prevLogIndex].term == follower[prevLogIndex].term
	prevLogIndex := r.Prs[to].Next -1
	prevLogTerm,err := r.RaftLog.Term(prevLogIndex)
	if err!=nil{
		panic(err.Error())
	}
	entryLength := len(r.RaftLog.entries)
	entries := make([]*pb.Entry, 0)
	for i:= r.RaftLog.getStartIndex(r.Prs[to].Next); i<entryLength;i++{
		entries = append(entries, &r.RaftLog.entries[i])
	}
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		From: r.id,
		To: to,
		Term: r.Term,
		Commit: r.RaftLog.committed,
		LogTerm: prevLogTerm,
		Index: prevLogIndex,
		Entries: entries,
	}
	r.msgs = append(r.msgs, msg)
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		From: r.id,
		To: to,
		Term: r.Term,
		Commit: min(r.RaftLog.committed, r.Prs[to].Match),
	}
	r.msgs = append(r.msgs, msg)
}

//sendHeartbeatResponse sends a heartbeatResponse
func (r *Raft) sendHeartbeatResponse(to uint64, reject bool)  {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		From: r.id,
		To: to,
		Term: r.Term,
		Reject: reject,
	}
	r.msgs = append(r.msgs, msg)
}

//sendRequestVote a RequestVote RPC to the given peer
func (r *Raft) sendRequestVote(to uint64){
	lastindex := r.RaftLog.LastIndex()
	logterm, _ := r.RaftLog.Term(lastindex)
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		From: r.id,
		To: to,
		Term: r.Term,
		LogTerm: logterm,
		Index: lastindex,
	}
	r.msgs = append(r.msgs, msg)
}

//
func (r *Raft) sendRequestVoteResponse(to uint64, reject bool)  {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		From: r.id,
		To: to,
		Term: r.Term,
		Reject: reject,
	}
	r.msgs = append(r.msgs, msg)
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	switch r.State {
	case StateFollower, StateCandidate:
		r.electionElapsed++
		if r.electionElapsed >= r.randomElectionTimeout{
			r.electionElapsed = 0
			r.Step(pb.Message{
				MsgType: pb.MessageType_MsgHup,
				From: r.id,
				Term: r.Term,
			})
		}
	case StateLeader:
		r.heartbeatElapsed++
		if r.heartbeatElapsed >= r.heartbeatTimeout{
			r.heartbeatElapsed = 0
			r.Step(pb.Message{
				MsgType: pb.MessageType_MsgBeat,
				To: r.id,
				Term: r.Term,
			})
		}
	}
}

//reset before election
func (r *Raft) reset(term uint64){
	if r.Term != term{
		r.Term = term
		r.Vote = None
	}
	r.Lead = None
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.votes = make(map[uint64]bool)
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.State = StateFollower
	r.Term = term
	r.Lead = lead
	r.Vote = None
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.State = StateCandidate
	r.Lead = None
	r.Term++
	r.Vote = r.id
	//at a new round reset
	r.votes = make(map[uint64]bool)
	r.votes[r.id] = true
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	r.State = StateLeader
	r.Lead = r.id
	lastindex := r.RaftLog.LastIndex()
	r.heartbeatElapsed = 0
	for pr := range r.Prs{
		if pr == r.id{
			r.Prs[pr] = &Progress{
				Next: lastindex + 2 ,
				Match: lastindex + 1,
			}
		} else {
			r.Prs[pr] = &Progress{
				Next: lastindex +1 ,
			}
		}
	}
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{
		Term: r.Term,
		Index: r.RaftLog.LastIndex()+1,
	})
	for pr := range r.Prs{
		if pr==r.id{
			continue
		}
		r.sendAppend(pr)
	}
	if len(r.Prs) == 1{
		r.RaftLog.committed = r.Prs[r.id].Match
	}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	if m.Term > r.Term {
		if m.MsgType == pb.MessageType_MsgAppend ||
			m.MsgType == pb.MessageType_MsgHeartbeat ||
			m.MsgType == pb.MessageType_MsgSnapshot {
			r.becomeFollower(m.Term, m.From)
		} else {
			r.becomeFollower(m.Term, None)
		}
	}
	switch r.State {
	case StateFollower:
		switch m.MsgType {
		case pb.MessageType_MsgHup:
			//election timeout,start a new election
			r.goElection()
		case pb.MessageType_MsgPropose, pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgAppendResponse, pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse, pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		default:
		}
	case StateCandidate:
		switch m.MsgType {
		case pb.MessageType_MsgHup:
			//election timeout,start a new election
			r.goElection()
		case pb.MessageType_MsgPropose, pb.MessageType_MsgAppend:
			if m.Term == r.Term{
				r.becomeFollower(m.Term, m.From)
			}
			r.handleAppendEntries(m)
		case pb.MessageType_MsgAppendResponse, pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse:
			r.hadleRequestVoteResponse(m)
		case pb.MessageType_MsgSnapshot:
			if m.Term == r.Term{
				r.becomeFollower(m.Term, m.From)
			}
			r.handleSnapshot(m)
		case pb.MessageType_MsgHeartbeat:
			if m.Term == r.Term{
				r.becomeFollower(m.Term, m.From)
			}
			r.handleHeartbeat(m)
		default:
		}
	case StateLeader:
		switch m.MsgType {
		case pb.MessageType_MsgHup, pb.MessageType_MsgBeat:
			//election timeout,start a new election
			for pr := range r.Prs {
				if pr == r.id{
					continue
				}
				r.sendHeartbeat(pr)
			}
		case pb.MessageType_MsgPropose:
			r.appendEntries(m)
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgAppendResponse:
			r.handleAppendResponse(m)
		case pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse, pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgHeartbeatResponse:
			r.sendAppend(m.From)
		default:
		}
	}
	return nil
}

//goElection
func (r *Raft) goElection(){
	r.becomeCandidate()
	r.heartbeatElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	if len(r.Prs) == 1{
		r.becomeLeader()
		return
	}
	for pr := range r.Prs{
		if pr == r.id{
			continue
		}
		r.sendRequestVote(pr)
	}
}

//handleRequestVote
func (r *Raft) handleRequestVote(m pb.Message){
	if m.Term < r.Term{
		r.sendRequestVoteResponse(m.From, true)
		return
	}
	if r.Vote != None && r.Vote != m.From{
		r.sendRequestVoteResponse(m.From, true)
		return
	}
	lastindex := r.RaftLog.LastIndex()
	logterm, _ := r.RaftLog.Term(lastindex)
	if logterm > m.LogTerm ||
		(logterm == m.LogTerm && lastindex > m.Index){
		r.sendRequestVoteResponse(m.From, true)
		return
	}
	r.Vote = m.From
	r.electionElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.sendRequestVoteResponse(m.From, false)
}

//hadleRequestVoteResponse
func (r *Raft) hadleRequestVoteResponse(m pb.Message){
	if m.Term < r.Term{
		return
	}
	r.votes[m.From] = !m.Reject
	vote := 0
	for _, vs := range r.votes{
		if vs {
			vote++
		}
	}
	if vote > len(r.Prs)/2{
		r.becomeLeader()
	} else if len(r.votes)-vote > len(r.Prs)/2{
		r.becomeFollower(r.Term, None)
	}
}

//appendEntries
func (r *Raft) appendEntries (m pb.Message){
	lastindex := r.RaftLog.LastIndex()
	for i, entry := range m.Entries{
		entry.Index = lastindex + uint64(i) +1
		entry.Term = r.Term
		r.RaftLog.entries = append(r.RaftLog.entries, *entry)
	}
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.Prs[r.id].Match +1
	for pr := range r.Prs{
		if pr == r.id{
			continue
		}
		r.sendAppend(pr)
	}
	// only itself
	if len(r.Prs) == 1{
		r.RaftLog.committed = r.Prs[r.id].Match
	}
}

//handleAppendResponse
func (r *Raft) handleAppendResponse (m pb.Message){
	if m.Term < r.Term{
		return
	}
	if m.Reject{
		index := m.Index
		logterm := m.LogTerm
		startindex := sort.Search(len(r.RaftLog.entries),
			func(i int) bool {
				return r.RaftLog.entries[i].Term > logterm
			})
		if startindex>0 && r.RaftLog.entries[startindex-1].Term == logterm{
			firstindex, _ := r.RaftLog.storage.FirstIndex()
			index = firstindex + uint64(startindex)
		}
		r.Prs[m.From].Next = index
		r.sendAppend(m.From)
		return
	}
	r.Prs[m.From].Match = m.Index
	r.Prs[m.From].Next = m.Index + 1
	//commit
	match := make(uint64Slice, len(r.Prs))
	i := 0
	for _, prs := range r.Prs {
		match[i] = prs.Match
		i++
	}
	sort.Sort(match)
	n := match[(len(r.Prs)-1)/2]
	logTerm, err := r.RaftLog.Term(n)
	if err != nil {
		panic(err)
	}
	if n > r.RaftLog.committed && logTerm == r.Term {
		r.RaftLog.committed = n
		for pr := range r.Prs{
			if pr == r.id{
				continue
			}
			r.sendAppend(pr)
		}
	}
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if m.Term < r.Term{
		r.sendAppendResponse(m.From, true, None, None)
		return
	}
	r.electionElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.Lead = m.From
	lastindex := r.RaftLog.LastIndex()
	if m.Index > lastindex{
		r.sendAppendResponse(m.From, true, None, lastindex+1)
		return
	}
	logterm, err := r.RaftLog.Term(m.Index)
	if err != nil{
		panic(err.Error())
	}
	//not consist
	if logterm != m.LogTerm{
		firstindex, _ := r.RaftLog.storage.FirstIndex()
		offset := sort.Search(r.RaftLog.getStartIndex(m.Index+1),
			func (i int) bool {
				return r.RaftLog.entries[i].Term == logterm
			})
		index := firstindex + uint64(offset)
		r.sendAppendResponse(m.From, true, logterm, index)
		return
	}
	for i, entry := range m.Entries{
		if entry.Index <= r.RaftLog.LastIndex(){
			logterm, err := r.RaftLog.Term(entry.Index)
			if err != nil{
				panic(err.Error())
			}
			if logterm != entry.Term{
				startindex := r.RaftLog.getStartIndex(entry.Index)
				r.RaftLog.entries[startindex] = *entry
				r.RaftLog.entries = r.RaftLog.entries[:startindex+1]
				if r.RaftLog.stabled >= entry.Index{
					r.RaftLog.stabled = entry.Index - 1
				}
			}
		}else{
			length := len(m.Entries)
			for j:=i;j<length;j++{
				r.RaftLog.entries = append(r.RaftLog.entries, *m.Entries[j])
			}
			break
		}
	}
	if m.Commit > r.RaftLog.committed{
		r.RaftLog.committed = min(m.Commit, m.Index+uint64(len(m.Entries)))
	}
	r.sendAppendResponse(m.From, false, None, r.RaftLog.LastIndex())
}

//sendAppendResponse
func (r *Raft) sendAppendResponse(to uint64, reject bool, term, index uint64){
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		From: r.id,
		To: to,
		Reject: reject,
		Term: r.Term,
		LogTerm: term,
		Index: index,
	}
	r.msgs = append(r.msgs, msg)
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if m.Term < r.Term{
		r.sendHeartbeatResponse(m.From, true)
		return
	}
	r.Lead = m.From
	r.heartbeatElapsed = 0
	r.electionElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.RaftLog.committed = m.Commit
	r.sendHeartbeatResponse(m.From, false)
}

func (r *Raft) softState() *SoftState{
	return &SoftState{
		Lead: r.Lead,
		RaftState: r.State,
	}
}

func (r *Raft) hardState() pb.HardState{
	return pb.HardState{
		Term: r.Term,
		Vote: r.Vote,
		Commit: r.RaftLog.committed,
	}
}


// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}
