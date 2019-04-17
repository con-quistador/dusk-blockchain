package generation

import (
	"sync"

	log "github.com/sirupsen/logrus"

	ristretto "github.com/bwesterb/go-ristretto"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/consensus/user"
	"gitlab.dusk.network/dusk-core/zkproof"
)

var empty struct{}

type generator interface {
	generateProof([]byte) zkproof.ZkProof
	updateBidList(user.BidList)
}

type proofGenerator struct {
	d, k ristretto.Scalar
	sync.RWMutex
	bidList user.BidList
}

func newProofGenerator(d, k ristretto.Scalar) *proofGenerator {
	return &proofGenerator{
		d: d,
		k: k,
	}
}

func (g *proofGenerator) updateBidList(bidList user.BidList) {
	g.Lock()
	defer g.Unlock()
	g.bidList = bidList
}

// GenerateProof will generate the proof of blind bid, needed to successfully
// propose a block to the voting committee.
func (g *proofGenerator) generateProof(seed []byte) zkproof.ZkProof {
	log.WithField("process", "generation").Traceln("generating proof")
	// Turn seed into scalar
	seedScalar := ristretto.Scalar{}
	seedScalar.Derive(seed)

	// Create a slice of scalars with a number of random bids (up to 10)
	g.Lock()
	bidListSubset := createBidListSubset(g.bidList)
	g.Unlock()
	bidListScalars := convertBidListToScalars(bidListSubset)

	return zkproof.Prove(g.d, g.k, seedScalar, bidListScalars)
}

// bidsToScalars will take a global public list, take a subset from it, and then
// return it as a slice of scalars.
func createBidListSubset(bidList user.BidList) user.BidList {
	numBids := getNumBids(bidList)
	return bidList.Subset(numBids)
}

// getNumBids will return how many bids to include in the bid list subset
// for the proof.
func getNumBids(bidList user.BidList) int {
	numBids := len(bidList)
	if numBids > 10 {
		numBids = 10
	}

	return numBids
}

// convertBidListToScalars will take a BidList, and create a slice of scalars from it.
func convertBidListToScalars(bidList user.BidList) []ristretto.Scalar {
	scalarList := make([]ristretto.Scalar, len(bidList))
	for i, bid := range bidList {
		bidScalar := ristretto.Scalar{}
		bidScalar.UnmarshalBinary(bid[:])
		scalarList[i] = bidScalar
	}

	return scalarList
}