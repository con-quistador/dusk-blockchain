package reduction

import (
	"bytes"
	"encoding/hex"
	"time"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/agreement"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/sortition"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/user"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire/payload/consensusmsg"

	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire/payload"
)

// SignatureSet is the signature set reduction phase of the consensus.
func SignatureSet(ctx *user.Context) error {
	// Set up a fallback value
	fallback := make([]byte, 32)

	// Vote on collected signature set
	if err := sigSetVote(ctx); err != nil {
		return err
	}

	// Receive all other votes
	if err := countSigSetVotes(ctx); err != nil {
		return err
	}

	ctx.Step++

	// If we timed out, exit the loop and go back to signature set generation
	if ctx.SigSetHash == nil {
		ctx.SigSetHash = fallback
	}

	// Vote on collected signature set
	if err := sigSetVote(ctx); err != nil {
		return err
	}

	// Receive all other votes
	if err := countSigSetVotes(ctx); err != nil {
		return err
	}

	ctx.Step++

	// If we timed out, exit the loop and go back to signature set generation
	if ctx.SigSetHash == nil {
		return nil
	}

	// If SigSetHash is fallback, the committee has agreed to exit and restart
	// consensus.
	if bytes.Equal(ctx.SigSetHash, fallback) {
		ctx.SigSetHash = nil
		return nil
	}

	// If we got a result, populate certificate, send message to
	// set agreement and terminate
	if err := agreement.SendSet(ctx, ctx.SigSetVotes); err != nil {
		return err
	}

	return nil
}

func sigSetVote(ctx *user.Context) error {
	// Set committee first
	size := user.CommitteeSize
	if len(ctx.Committee) < int(user.CommitteeSize) {
		size = uint8(len(ctx.Committee))
	}

	currentCommittee, err := sortition.CreateCommittee(ctx.Round, ctx.W, ctx.Step, size,
		ctx.Committee, ctx.NodeWeights)
	if err != nil {
		return err
	}

	ctx.CurrentCommittee = currentCommittee

	// If we are not in the committee, then don't vote
	if votes := sortition.Verify(ctx.CurrentCommittee, []byte(*ctx.Keys.EdPubKey)); votes == 0 {
		return nil
	}

	// Hash our vote set
	sigSetHash, err := ctx.HashVotes(ctx.SigSetVotes)
	if err != nil {
		return err
	}

	ctx.SigSetHash = sigSetHash
	setStr := hex.EncodeToString(sigSetHash)
	ctx.AllVotes[setStr] = ctx.SigSetVotes

	// Sign signature set hash with BLS
	sigBLS, err := ctx.BLSSign(ctx.Keys.BLSSecretKey, ctx.Keys.BLSPubKey, ctx.SigSetHash)
	if err != nil {
		return err
	}

	// Create signature set vote message to gossip
	pl, err := consensusmsg.NewSigSetVote(ctx.BlockHash, ctx.SigSetHash, sigBLS,
		ctx.Keys.BLSPubKey.Marshal())
	if err != nil {
		return err
	}

	sigEd, err := ctx.CreateSignature(pl)
	if err != nil {
		return err
	}

	msg, err := payload.NewMsgConsensus(ctx.Version, ctx.Round, ctx.LastHeader.Hash, ctx.Step,
		sigEd, []byte(*ctx.Keys.EdPubKey), pl)
	if err != nil {
		return err
	}

	// Gossip message
	if err := ctx.SendMessage(ctx.Magic, msg); err != nil {
		return err
	}

	ctx.SigSetVoteChan <- msg
	return nil
}

func countSigSetVotes(ctx *user.Context) error {
	// Set vote limit
	voteLimit := uint8(len(ctx.CurrentCommittee))

	// Keep a counter of how many votes have been cast for a specific block
	counts := make(map[string]uint8)

	// Keep track of all nodes who have voted
	voters := make(map[string]bool)

	// Start the timer
	timer := time.NewTimer(user.StepTime * (time.Duration(ctx.Multiplier)))

	for {
		select {
		case <-timer.C:
			ctx.SigSetHash = nil
			return nil
		case m := <-ctx.SigSetVoteChan:
			pl := m.Payload.(*consensusmsg.SigSetVote)
			pkEd := hex.EncodeToString(m.PubKey)

			// Check if this node's vote is already recorded
			if voters[pkEd] {
				break
			}

			// Get amount of votes
			votes := sortition.Verify(ctx.CurrentCommittee, m.PubKey)

			// Log information
			voters[pkEd] = true
			setStr := hex.EncodeToString(pl.SigSetHash)
			counts[setStr] += votes

			// If a set exceeds vote threshold, we will end the loop.
			if counts[setStr] < voteLimit {
				break
			}

			timer.Stop()

			// Set signature set hash
			ctx.SigSetHash = pl.SigSetHash

			// Set vote set to winning hash
			ctx.SigSetVotes = ctx.AllVotes[setStr]
			return nil
		}
	}
}