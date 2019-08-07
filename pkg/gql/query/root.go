package query

import (
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire"
	"github.com/graphql-go/graphql"
)

type Root struct {
	Query *graphql.Object
}

func NewRoot(rpcBus *wire.RPCBus, db database.DB) *Root {

	b := blocks{db}
	t := transactions{db}
	m := mempool{rpcBus: rpcBus}

	root := Root{
		Query: graphql.NewObject(
			graphql.ObjectConfig{
				Name: "Query",
				Fields: graphql.Fields{
					"blocks":       b.getQuery(),
					"transactions": t.getQuery(),
					"mempool":      m.getQuery(),
				},
			},
		),
	}
	return &root
}
