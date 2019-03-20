package committee

import (
	"bytes"
	"time"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/msg"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire/encoding"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/util/nativeutils/prerror"
)

type (
	// Committee is the interface for operations depending on the set of Provisioners extracted for a fiven step
	Committee interface {
		// isMember can accept a BLS Public Key or an Ed25519
		IsMember([]byte) bool
		GetVotingCommittee(uint64, uint8) (map[string]uint8, error)
		VerifyVoteSet(voteSet []*msg.Vote, hash []byte, round uint64, step uint8) *prerror.PrError
		Quorum() int
		// Priority is a way to categorize members of a committee. Suitable implementation details are stake, scores, etc. Returns true if the second argument has priority over the first, false otherwise
		Priority([]byte, []byte) bool
	}

	//Event is the message that encapsulates data relevant for components relying on committee information
	Event struct {
		*consensus.EventHeader
		VoteSet       []*msg.Vote
		SignedVoteSet []byte
		BlockHash     []byte
	}

	// EventUnMarshaller implements both Marshaller and Unmarshaller interface
	EventUnMarshaller struct {
		*consensus.EventHeaderMarshaller
		*consensus.EventHeaderUnmarshaller
	}

	// Collector is a helper that groups common operations performed on Events related to a committee
	Collector struct {
		wire.StepEventCollector
		Committee    Committee
		CurrentRound uint64
	}

	// Selector is basically a picker of Events based on the priority of their sender
	Selector struct {
		EventChan     chan wire.Event
		BestEventChan chan wire.Event
		StopChan      chan bool
		committee     Committee
		timerLength   time.Duration
	}
)

// NewEvent creates an empty Event
func NewEvent() *Event {
	return &Event{
		EventHeader: &consensus.EventHeader{},
	}
}

// Equal as specified in the Event interface
func (ceh *Event) Equal(e wire.Event) bool {
	other, ok := e.(*Event)
	return ok && ceh.EventHeader.Equal(other) && bytes.Equal(other.SignedVoteSet, ceh.SignedVoteSet)
}

// NewEventUnMarshaller creates a new EventUnMarshaller. Internally it creates an EventHeaderUnMarshaller which takes care of Decoding and Encoding operations
func NewEventUnMarshaller(validate func(*bytes.Buffer) error) *EventUnMarshaller {
	return &EventUnMarshaller{
		EventHeaderMarshaller:   new(consensus.EventHeaderMarshaller),
		EventHeaderUnmarshaller: consensus.NewEventHeaderUnmarshaller(validate),
	}
}

// Unmarshal unmarshals the buffer into a CommitteeEventHeader
// Field order is the following:
// * Consensus Header [BLS Public Key; Round; Step]
// * Committee Header [Signed Vote Set; Vote Set; BlockHash]
func (ceu *EventUnMarshaller) Unmarshal(r *bytes.Buffer, ev wire.Event) error {
	cev := ev.(*Event)
	if err := ceu.EventHeaderUnmarshaller.Unmarshal(r, cev.EventHeader); err != nil {
		return err
	}

	if err := encoding.ReadBLS(r, &cev.SignedVoteSet); err != nil {
		return err
	}

	voteSet, err := msg.DecodeVoteSet(r)
	if err != nil {
		return err
	}
	cev.VoteSet = voteSet

	if err := encoding.Read256(r, &cev.BlockHash); err != nil {
		return err
	}

	return nil
}

// Marshal the buffer into a committee Event
// Field order is the following:
// * Consensus Header [BLS Public Key; Round; Step]
// * Committee Header [Signed Vote Set; Vote Set; BlockHash]
func (ceu *EventUnMarshaller) Marshal(r *bytes.Buffer, ev wire.Event) error {
	cev := ev.(*Event)
	if err := ceu.EventHeaderMarshaller.Marshal(r, cev.EventHeader); err != nil {
		return err
	}

	// Marshal BLS Signature of VoteSet
	if err := encoding.WriteBLS(r, cev.SignedVoteSet); err != nil {
		return err
	}

	// Marshal VoteSet
	bvotes, err := msg.EncodeVoteSet(cev.VoteSet)
	if err != nil {
		return err
	}

	if _, err := r.Write(bvotes); err != nil {
		return err
	}

	if err := encoding.Write256(r, cev.BlockHash); err != nil {
		return err
	}
	// TODO: write the vote set to the buffer
	return nil
}

//ShouldBeSkipped is a shortcut for validating if an Event is relevant
// NOTE: currentRound is handled by some other process, so it is not this component's responsibility to handle corner cases (for example being on an obsolete round because of a disconnect, etc)
// Deprecated: Collectors should use Collector.ShouldSkip instead, considering that verification of Events should be decoupled from syntactic validation and the decision flow should likely be handled differently by different components
func (cc *Collector) ShouldBeSkipped(m *Event) bool {
	shouldSkip := cc.ShouldSkip(m, m.Round, m.Step)
	//TODO: the round element needs to be reassessed
	err := cc.Committee.VerifyVoteSet(m.VoteSet, m.BlockHash, m.Round, m.Step)
	failedVerification := err != nil
	return shouldSkip || failedVerification
}

// ShouldSkip checks if the message is not propagated by a committee member, that is not a duplicate (and in this case should probably check if the Provisioner is malicious) and that is relevant to the current round
func (cc *Collector) ShouldSkip(ev wire.Event, round uint64, step uint8) bool {
	isDupe := cc.Contains(ev, step)
	isPleb := !cc.Committee.IsMember(ev.Sender())
	return isDupe || isPleb
}

// UpdateRound is a utility function that can be overridden by the embedding collector in case of custom behaviour when updating the current round
func (cc *Collector) UpdateRound(round uint64) {
	cc.CurrentRound = round
}

//NewSelector creates the Selector
func NewSelector(c Committee, timeout time.Duration) *Selector {
	return &Selector{
		EventChan:     make(chan wire.Event),
		BestEventChan: make(chan wire.Event),
		StopChan:      make(chan bool),
		committee:     c,
		timerLength:   timeout,
	}
}

// PickBest picks the best event depending on the priority of the sender
func (s *Selector) PickBest() {
	var bestEvent wire.Event
	timer := time.NewTimer(s.timerLength)

	for {
		select {
		case ev := <-s.EventChan:
			if s.committee.Priority(bestEvent.Sender(), ev.Sender()) {
				bestEvent = ev
			}
		case <-timer.C:
			s.pick(bestEvent)
			return
		case <-s.StopChan:
			s.pick(bestEvent)
			return
		}
	}
}

func (s *Selector) pick(ev wire.Event) {
	s.BestEventChan <- ev
}
