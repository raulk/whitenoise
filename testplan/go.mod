module github.com/raulk/whitenoise/testplan

go 1.15

require (
	github.com/filecoin-project/go-commp-utils v0.0.0-20201119054358-b88f7a96a434
	github.com/filecoin-project/go-data-transfer v1.6.1-0.20210608094723-dbb8833459fc
	github.com/filecoin-project/go-fil-markets v1.2.6-0.20210601213658-4449fd6940d8
	github.com/filecoin-project/go-state-types v0.1.0
	github.com/filecoin-project/lotus v1.5.2
	github.com/ipfs/go-blockservice v0.1.4
	github.com/ipfs/go-cid v0.0.7
	github.com/ipfs/go-datastore v0.4.5
	github.com/ipfs/go-graphsync v0.8.1-rc1
	github.com/ipfs/go-ipfs-blockstore v1.0.3
	github.com/ipfs/go-ipfs-chunker v0.0.5
	github.com/ipfs/go-merkledag v0.3.2
	github.com/ipfs/go-unixfs v0.2.4
	github.com/ipld/go-ipld-prime v0.9.1-0.20210324083106-dc342a9917db
	github.com/libp2p/go-libp2p v0.13.0
	github.com/libp2p/go-libp2p-connmgr v0.2.4
	github.com/libp2p/go-libp2p-core v0.8.5
	github.com/testground/sdk-go v0.2.8-0.20210319194830-4c62042da4c3
	go.uber.org/zap v1.16.0
)

replace github.com/filecoin-project/filecoin-ffi => ./ffi-stub
