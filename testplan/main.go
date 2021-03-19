package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/filecoin-project/go-commp-utils/pieceio/cario"
	"github.com/filecoin-project/go-commp-utils/writer"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	dt "github.com/filecoin-project/go-data-transfer/impl"
	dtnet "github.com/filecoin-project/go-data-transfer/network"
	gst "github.com/filecoin-project/go-data-transfer/transport/graphsync"
	"github.com/filecoin-project/go-fil-markets/shared"
	"github.com/filecoin-project/go-fil-markets/storagemarket/impl/requestvalidation"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-storedcounter"
	"github.com/filecoin-project/lotus/blockstore"
	badgerbs "github.com/filecoin-project/lotus/blockstore/badger"
	"github.com/filecoin-project/lotus/node/repo"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	graphsync "github.com/ipfs/go-graphsync/impl"
	gsnet "github.com/ipfs/go-graphsync/network"
	"github.com/ipfs/go-graphsync/storeutil"
	chunker "github.com/ipfs/go-ipfs-chunker"
	"github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-unixfs/importer"
	"github.com/ipld/go-ipld-prime"
	"github.com/libp2p/go-libp2p"
	connmgr "github.com/libp2p/go-libp2p-connmgr"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/metrics"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/testground/sdk-go/network"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"
	"go.uber.org/zap"
)

var testcases = map[string]interface{}{
	"transfer": run.InitializedTestCaseFn(runTransfer),
}

var ctx = context.Background()

func main() {
	run.InvokeMap(testcases)
}

func runTransfer(runenv *runtime.RunEnv, initCtx *run.InitContext) error {
	var (
		size = runenv.SizeParam("size")

		interrupt = runenv.StringParam("interrupt")
		linkshape *network.LinkShape
	)

	if runenv.IsParamSet("net") {
		linkshape = new(network.LinkShape)
		runenv.JSONParam("linkshape", linkshape)
	}

	_, events, err := runenv.CreateStructuredAsset("events", zap.NewDevelopmentConfig())
	if err != nil {
		panic(err)
	}

	_ = initCtx.WaitAllInstancesInitialized(ctx)

	// create the libp2p host.
	host, _ := CreateHost(initCtx)

	// create a blockstore -- right now this is badger, but we should be able to parametrize.
	bs := CreateBlockstore(runenv)

	// create graphsync.
	gs := graphsync.New(context.Background(),
		gsnet.NewFromLibp2pHost(host),
		storeutil.LoaderForBlockstore(bs),
		storeutil.StorerForBlockstore(bs),
	)

	// create an ephemeral datastore.
	ds := dssync.MutexWrap(datastore.NewMapDatastore())

	dir := filepath.Join(runenv.TestTempPath, "cidlists")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// instantiate the data transfer module.
	// TODO make retry configurable.
	retry := dtnet.RetryParameters(time.Second, 5*time.Minute, 15, 5)
	mgr, err := dt.NewDataTransfer(
		ds,
		dir, // cid lists directory.
		dtnet.NewFromLibp2pHost(host, retry),
		gst.NewTransport(host.ID(), gs),
		storedcounter.New(ds, datastore.NewKey("datatransfer")),
		dt.PushChannelRestartConfig(time.Second*30, 10, 1024, 2*time.Minute, 3),
	)
	if err != nil {
		return err
	}

	// trace things as they happen for later review.
	mgr.SubscribeToEvents(func(event datatransfer.Event, state datatransfer.ChannelState) {
		m := map[string]interface{}{
			"name":            datatransfer.Events[event.Code],
			"status":          datatransfer.Statuses[state.Status()],
			"transfer ID":     state.TransferID(),
			"channel ID":      state.ChannelID(),
			"sent":            state.Sent(),
			"received":        state.Received(),
			"queued":          state.Queued(),
			"received count":  len(state.ReceivedCids()),
			"total size":      state.TotalSize(),
			"remote peer":     state.OtherPeer(),
			"event message":   event.Message,
			"channel message": state.Message(),
		}
		events.Infow("data transfer event", "event", m)
	})

	// some weirdness here.
	// this "validator" is supposed to associate the voucher with the deal.
	// since we're not doing a real deal, let's just return a dummy deal.
	err = mgr.RegisterVoucherType(&requestvalidation.StorageDataTransferVoucher{}, &OKValidator{})
	if err != nil {
		return err
	}

	// start the data transfer manager.
	ready := make(chan error, 1)
	mgr.OnReady(func(err error) {
		ready <- err
	})
	if err := mgr.Start(context.TODO()); err != nil {
		return err
	}
	<-ready

	runenv.RecordMessage("data transfer manager initialized")

	// find all participating peers.
	other := FindOther(runenv, initCtx, host)

	runenv.RecordMessage("one other peer found")

	defer initCtx.SyncClient.MustSignalAndWait(ctx, "done", runenv.TestInstanceCount)

	// start the interruptor loop.
	go interruptorLoop(interrupt, host, other)
	runenv.RecordMessage("interruptor loop started")

	// set the link shape if one was provided.
	if linkshape != nil {
		initCtx.NetClient.MustConfigureNetwork(ctx, &network.Config{
			Network: "default",
			Enable:  true,
			Default: *linkshape,
		})
	}

	switch runenv.TestGroupID {
	case "sender":
		// we are the sender: (1) create payload, (2) start the data transfer.
		runenv.RecordMessage("we are the sender")

		start := time.Now()

		// create a random payload of the supplied size.
		cid, err := CreatePayload(bs, size)
		if err != nil {
			return err
		}

		elapsed := time.Since(start)
		runenv.R().RecordPoint("sender_import_took_ms", float64(elapsed.Milliseconds()))
		runenv.RecordMessage("import took: %s", elapsed)

		// connect to the provider.
		provider := *other
		if err = host.Connect(context.Background(), provider); err != nil {
			return err
		}

		start = time.Now()

		// start the data transfer; the CID in the proposal doesn't matter (I think).
		runenv.RecordMessage("opening the push data channel")
		voucher := &requestvalidation.StorageDataTransferVoucher{Proposal: cid}
		chID, err := mgr.OpenPushDataChannel(ctx, provider.ID, voucher, cid, shared.AllSelector())
		if err != nil {
			return fmt.Errorf("failed to start channel: %w", err)
		}

		// monitor the channel, and restart if necessary.
		tick := time.Tick(1 * time.Second)
	Loop:
		for range tick {
			inprog, err := mgr.InProgressChannels(ctx)
			if err != nil {
				return fmt.Errorf("failed to query in progress channels: %w", err)
			}

			if l := len(inprog); l != 1 {
				return fmt.Errorf("expected 1 in progress channel, got: %d", l)
			}

			ch, ok := inprog[chID]
			if !ok {
				return fmt.Errorf("no in progress channel for ID %s", chID)
			}

			runenv.RecordMessage("channel state: %v", spew.Sdump(ch))
			switch ch.Status() {
			case datatransfer.Ongoing:
				continue // all ok, keep going!
			case datatransfer.TransferFinished, datatransfer.Finalizing, datatransfer.Completing, datatransfer.Completed:
				break Loop // finished!
			case datatransfer.Failed:
				runenv.RecordMessage("data transfer failed; restarting")
			case datatransfer.Cancelled:
				runenv.RecordMessage("data transfer cancelled; restarting")
			}

			// something failed, restart the data transfer.
			if err := mgr.RestartDataTransferChannel(context.Background(), chID); err != nil {
				runenv.RecordMessage("failed to restart data transfer: %s", err)
			}
		}

		elapsed = time.Since(start)
		runenv.R().RecordPoint("sender_transfer_took_ms", float64(elapsed.Milliseconds()))
		runenv.RecordMessage("transfer took (from sender's perspective): %s", elapsed)

		// clean shutdown.
		initCtx.SyncClient.MustSignalEntry(ctx, "sender-done")

	case "receiver":
		runenv.RecordMessage("we are the receiver")

		// wait until the client is done.
		initCtx.SyncClient.MustBarrier(ctx, "sender-done", 1)
	}

	return nil
}

func interruptorLoop(rate string, host host.Host, other *peer.AddrInfo) {
	splt := strings.Split(rate, "/")
	probability, err := strconv.ParseFloat(splt[0], 64)
	if err != nil {
		panic(err)
	}

	every, err := time.ParseDuration(splt[1])
	if err != nil {
		panic(err)
	}

	// no interruption.
	if every == 0 {
		return
	}

	for range time.Tick(every) {
		if mrand.Float64() <= probability {
			conns := host.Network().ConnsToPeer(other.ID)
			if len(conns) == 0 {
				continue
			}
			_ = conns[1].Close()
		}
	}
}

func CreateBlockstore(runenv *runtime.RunEnv) blockstore.Blockstore {
	bsPath := filepath.Join(runenv.TestTempPath, "badger")
	opts, err := repo.BadgerBlockstoreOptions(repo.HotBlockstore, bsPath, false)
	if err != nil {
		panic(err)
	}

	bs, err := badgerbs.Open(opts)
	if err != nil {
		panic(err)
	}
	return bs
}

func CreateHost(initCtx *run.InitContext) (host.Host, *metrics.BandwidthCounter) {
	var (
		bwc        = metrics.NewBandwidthCounter()
		ip         = initCtx.NetClient.MustGetDataNetworkIP()
		listenAddr = fmt.Sprintf("/ip4/%s/tcp/0", ip)
	)

	host, err := libp2p.New(ctx,
		libp2p.ListenAddrStrings(listenAddr),
		libp2p.NATPortMap(),
		libp2p.ConnectionManager(connmgr.NewConnManager(500, 800, time.Minute)),
		libp2p.BandwidthReporter(bwc),
	)
	if err != nil {
		panic(err)
	}
	return host, bwc
}

func FindOther(runenv *runtime.RunEnv, initCtx *run.InitContext, host host.Host) *peer.AddrInfo {
	var (
		id = host.ID()
		ai = &peer.AddrInfo{ID: id, Addrs: host.Addrs()}

		// the peers topic where all instances will advertise their AddrInfo.
		peersTopic = sync.NewTopic("peers", new(peer.AddrInfo))

		// initialize a slice to store the AddrInfos of all other peers in the run.
		peers = make([]*peer.AddrInfo, 0, runenv.TestInstanceCount-1)
	)

	// Publish our own.
	initCtx.SyncClient.MustPublish(ctx, peersTopic, ai)

	// Now subscribe to the peers topic and consume all addresses, storing them
	// in the peers slice.
	peersCh := make(chan *peer.AddrInfo)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel() // cancels the subscription.

	sub := initCtx.SyncClient.MustSubscribe(sctx, peersTopic, peersCh)

	// Receive the expected number of AddrInfos.
	for len(peers) < cap(peers) {
		select {
		case ai := <-peersCh:
			if ai.ID == id {
				continue // skip over ourselves.
			}
			peers = append(peers, ai)
		case err := <-sub.Done():
			panic(err)
		}
	}

	if l := len(peers); l != 1 {
		panic(fmt.Sprintf("expected one more peer; got: %d", l))
	}

	return peers[0]
}

func CreatePayload(bs blockstore.Blockstore, size uint64) (cid.Cid, error) {
	var (
		bserv = blockservice.New(bs, nil)
		dserv = merkledag.NewDAGService(bserv)
		r     = io.LimitReader(rand.Reader, int64(size))
		spl   = chunker.DefaultSplitter(r)
	)

	nd, err := importer.BuildDagFromReader(dserv, spl)
	if err != nil {
		return cid.Undef, err
	}

	return nd.Cid(), nil
}

type OKValidator struct{}

func (v *OKValidator) ValidatePush(peer.ID, datatransfer.Voucher, cid.Cid, ipld.Node) (datatransfer.VoucherResult, error) {
	return nil, nil
}

func (v *OKValidator) ValidatePull(peer.ID, datatransfer.Voucher, cid.Cid, ipld.Node) (datatransfer.VoucherResult, error) {
	return nil, nil
}

var _ datatransfer.RequestValidator = (*OKValidator)(nil)

func GeneratePieceCommitment(rt abi.RegisteredSealProof, payloadCid cid.Cid, bstore blockstore.Blockstore) (cid.Cid, abi.UnpaddedPieceSize, error) {
	cario := cario.NewCarIO()
	preparedCar, err := cario.PrepareCar(context.Background(), bstore, payloadCid, shared.AllSelector())
	if err != nil {
		return cid.Undef, 0, err
	}

	commpWriter := new(writer.Writer)
	err = preparedCar.Dump(commpWriter)
	if err != nil {
		return cid.Undef, 0, err
	}

	dataCIDSize, err := commpWriter.Sum()
	if err != nil {
		return cid.Undef, 0, err
	}
	return dataCIDSize.PieceCID, dataCIDSize.PieceSize.Unpadded(), nil
}

//
// func StorageProvider(runenv *runtime.RunEnv,
// 	h host.Host,
// 	ds datastore.Batching,
// 	mds *multistore.MultiStore,
// 	dt datatransfer.Manager,
// 	spn storagemarket.StorageProviderNode,
// ) storagemarket.StorageProvider {
// 	var (
// 		net        = smnet.NewFromLibp2pHost(h)
// 		dir        = filepath.Join(runenv.TestOutputsPath, "piecestore")
// 		store, err = piecefilestore.NewLocalFileStore(piecefilestore.OsPath(dir))
// 	)
// 	if err != nil {
// 		panic(err)
// 	}
//
// 	ps, err := piecestoreimpl.NewPieceStore(namespace.Wrap(ds, datastore.NewKey("/storagemarket")))
// 	if err != nil {
// 		panic(err)
// 	}
// 	ps.OnReady(marketevents.ReadyLogger("piecestore"))
// 	if err = ps.Start(ctx); err != nil {
// 		panic(err)
// 	}
//
// 	var (
// 		addr, _ = address.NewIDAddress(2001)
// 		ss, _   = abi.RegisteredPoStProof_StackedDrgWindow32GiBV1.SectorSize()
// 		maxSize = storagemarket.MaxPieceSize(abi.PaddedPieceSize(ss))
// 	)
//
// 	ask, err := storedask.NewStoredAsk(
// 		namespace.Wrap(ds, datastore.NewKey("/storage-ask")),
// 		datastore.NewKey("latest"),
// 		spn, addr, maxSize)
// 	if err != nil {
// 		panic(err)
// 	}
//
// 	// set an initial ask.
// 	if err = ask.SetAsk(abi.NewTokenAmount(100000), abi.NewTokenAmount(100000), abi.ChainEpoch(600)); err != nil {
// 		panic(fmt.Sprintf("failed to set ask: %s", err))
// 	}
//
// 	prov, err := storageimpl.NewProvider(net,
// 		namespace.Wrap(ds, datastore.NewKey("/deals/provider")),
// 		store, mds, ps, dt, spn, addr, ask)
// 	if err != nil {
// 		panic(err)
// 	}
//
// 	return prov
// }
