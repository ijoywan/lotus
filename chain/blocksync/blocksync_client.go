package blocksync

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"time"

	blocks "github.com/ipfs/go-block-format"
	bserv "github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	graphsync "github.com/ipfs/go-graphsync"
	gsnet "github.com/ipfs/go-graphsync/network"
	host "github.com/libp2p/go-libp2p-core/host"
	inet "github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"go.opencensus.io/trace"
	"golang.org/x/xerrors"

	cborutil "github.com/filecoin-project/go-cbor-util"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/store"
	"github.com/filecoin-project/lotus/chain/types"
	incrt "github.com/filecoin-project/lotus/lib/increadtimeout"
	"github.com/filecoin-project/lotus/lib/peermgr"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
)

// Block synchronization client.
type BlockSync struct {
	bserv bserv.BlockService
	gsync graphsync.GraphExchange
	host  host.Host

	peerTracker *bsPeerTracker
	peerMgr     *peermgr.PeerMgr
}

func NewBlockSyncClient(
	bserv dtypes.ChainBlockService,
	h host.Host,
	pmgr peermgr.MaybePeerMgr,
	gs dtypes.Graphsync,
) *BlockSync {
	return &BlockSync{
		bserv:       bserv,
		host:        h,
		peerTracker: newPeerTracker(pmgr.Mgr),
		peerMgr:     pmgr.Mgr,
		gsync:       gs,
	}
}

// FIXME: Check request.
func (bs *BlockSync) processStatus(req *BlockSyncRequest, res *BlockSyncResponse) error {
	switch res.Status {
	case StatusPartial: // Partial Response
		return xerrors.Errorf("not handling partial blocksync responses yet")
	case StatusNotFound: // req.Start not found
		return xerrors.Errorf("not found")
	case StatusGoAway: // Go Away
		return xerrors.Errorf("not handling 'go away' blocksync responses yet")
	case StatusInternalError: // Internal Error
		return xerrors.Errorf("block sync peer errored: %s", res.Message)
	case StatusBadRequest:
		return xerrors.Errorf("block sync request invalid: %s", res.Message)
	default:
		return xerrors.Errorf("unrecognized response code: %d", res.Status)
	}
}

// GetBlocks fetches count blocks from the network, from the provided tipset
// *backwards*, returning as many tipsets as count.
//
// {hint/usage}: This is used by the Syncer during normal chain syncing and when
// resolving forks.
func (bs *BlockSync) GetBlocks(ctx context.Context, tsk types.TipSetKey, count int) ([]*types.TipSet, error) {
	ctx, span := trace.StartSpan(ctx, "bsync.GetBlocks")
	defer span.End()
	if span.IsRecordingEvents() {
		span.AddAttributes(
			trace.StringAttribute("tipset", fmt.Sprint(tsk.Cids())),
			trace.Int64Attribute("count", int64(count)),
		)
	}

	req := &BlockSyncRequest{
		Start:         tsk.Cids(),
		RequestLength: uint64(count),
		Options:       BSOptBlocks,
	}

	// this peerset is sorted by latency and failure counting.
	peers := bs.getPeers()

	// randomize the first few peers so we don't always pick the same peer
	shufflePrefix(peers)

	start := build.Clock.Now()
	var oerr error

	for _, p := range peers {
		// TODO: doing this synchronously isnt great, but fetching in parallel
		// may not be a good idea either. think about this more
		select {
		case <-ctx.Done():
			return nil, xerrors.Errorf("blocksync getblocks failed: %w", ctx.Err())
		default:
		}

		res, err := bs.sendRequestToPeer(ctx, p, req)
		if err != nil {
			oerr = err
			if !xerrors.Is(err, inet.ErrNoConn) {
				log.Warnf("BlockSync request failed for peer %s: %s", p.String(), err)
			}
			continue
		}

		if res.Status == StatusOK || res.Status == StatusPartial {
			resp, err := bs.processBlocksResponse(req, res)
			if err != nil {
				return nil, xerrors.Errorf("success response from peer failed to process: %w", err)
			}
			bs.peerTracker.logGlobalSuccess(build.Clock.Since(start))
			bs.host.ConnManager().TagPeer(p, "bsync", 25)
			return resp, nil
		}

		oerr = bs.processStatus(req, res)
		if oerr != nil {
			log.Warnf("BlockSync peer %s response was an error: %s", p.String(), oerr)
		}
	}
	return nil, xerrors.Errorf("GetBlocks failed with all peers: %w", oerr)
}

func (bs *BlockSync) GetFullTipSet(ctx context.Context, p peer.ID, tsk types.TipSetKey) (*store.FullTipSet, error) {
	// TODO: round robin through these peers on error

	req := &BlockSyncRequest{
		Start:         tsk.Cids(),
		RequestLength: 1,
		Options:       BSOptBlocks | BSOptMessages,
	}

	res, err := bs.sendRequestToPeer(ctx, p, req)
	if err != nil {
		return nil, err
	}

	switch res.Status {
	case 0: // Success
		if len(res.Chain) == 0 {
			return nil, fmt.Errorf("got zero length chain response")
		}
		bts := res.Chain[0]

		return bstsToFullTipSet(bts)
	case 101: // Partial Response
		return nil, xerrors.Errorf("partial responses are not handled for single tipset fetching")
	case 201: // req.Start not found
		return nil, fmt.Errorf("not found")
	case 202: // Go Away
		return nil, xerrors.Errorf("received 'go away' response peer")
	case 203: // Internal Error
		return nil, fmt.Errorf("block sync peer errored: %q", res.Message)
	case 204: // Invalid Request
		return nil, fmt.Errorf("block sync request invalid: %q", res.Message)
	default:
		return nil, fmt.Errorf("unrecognized response code")
	}
}

func shufflePrefix(peers []peer.ID) {
	// FIXME: Extract.
	pref := 5
	if len(peers) < pref {
		pref = len(peers)
	}

	buf := make([]peer.ID, pref)
	perm := rand.Perm(pref)
	for i, v := range perm {
		buf[i] = peers[v]
	}

	copy(peers, buf)
}

func (bs *BlockSync) GetChainMessages(ctx context.Context, h *types.TipSet, count uint64) ([]*BSTipSet, error) {
	ctx, span := trace.StartSpan(ctx, "GetChainMessages")
	defer span.End()

	peers := bs.getPeers()
	// randomize the first few peers so we don't always pick the same peer
	shufflePrefix(peers)

	req := &BlockSyncRequest{
		Start:         h.Cids(),
		RequestLength: count,
		Options:       BSOptMessages,
	}

	var err error
	start := build.Clock.Now()

	for _, p := range peers {
		res, rerr := bs.sendRequestToPeer(ctx, p, req)
		if rerr != nil {
			err = rerr
			log.Warnf("BlockSync request failed for peer %s: %s", p.String(), err)
			continue
		}

		if res.Status == StatusOK {
			bs.peerTracker.logGlobalSuccess(build.Clock.Since(start))
			return res.Chain, nil
		}

		if res.Status == StatusPartial {
			// TODO: track partial response sizes to ensure we don't overrequest too often
			return res.Chain, nil
		}

		err = bs.processStatus(req, res)
		if err != nil {
			log.Warnf("BlockSync peer %s response was an error: %s", p.String(), err)
		}
	}

	if err == nil {
		return nil, xerrors.Errorf("GetChainMessages failed, no peers connected")
	}

	// TODO: What if we have no peers (and err is nil)?
	return nil, xerrors.Errorf("GetChainMessages failed with all peers(%d): %w", len(peers), err)
}

func (bs *BlockSync) sendRequestToPeer(
	ctx context.Context,
	peer peer.ID,
	req *BlockSyncRequest,
) (_ *BlockSyncResponse, err error) {
	// Trace code.
	ctx, span := trace.StartSpan(ctx, "sendRequestToPeer")
	defer span.End()
	if span.IsRecordingEvents() {
		span.AddAttributes(
			trace.StringAttribute("peer", peer.Pretty()),
		)
	}
	defer func() {
		if err != nil {
			if span.IsRecordingEvents() {
				span.SetStatus(trace.Status{
					Code:    5,
					Message: err.Error(),
				})
			}
		}
	}()
	// -- TRACE --

	gsproto := string(gsnet.ProtocolGraphsync)
	supp, err := bs.host.Peerstore().SupportsProtocols(peer, BlockSyncProtocolID, gsproto)
	if err != nil {
		return nil, xerrors.Errorf("failed to get protocols for peer: %w", err)
	}

	if len(supp) == 0 {
		return nil, xerrors.Errorf("peer %s supports no known sync protocols", peer)
	}

	switch supp[0] {
	case BlockSyncProtocolID:
		res, err := bs.fetchBlocksBlockSync(ctx, peer, req)
		if err != nil {
			return nil, xerrors.Errorf("blocksync req failed: %w", err)
		}
		return res, nil
	case gsproto:
		res, err := bs.fetchBlocksGraphSync(ctx, peer, req)
		if err != nil {
			return nil, xerrors.Errorf("graphsync req failed: %w", err)
		}
		return res, nil
	default:
		return nil, xerrors.Errorf("peerstore somehow returned unexpected protocols: %v", supp)
	}

}

func (bs *BlockSync) fetchBlocksBlockSync(
	ctx context.Context,
	peer peer.ID,
	req *BlockSyncRequest,
) (*BlockSyncResponse, error) {
	ctx, span := trace.StartSpan(ctx, "blockSyncFetch")
	defer span.End()

	start := build.Clock.Now()
	stream, err := bs.host.NewStream(
		inet.WithNoDial(ctx, "should already have connection"),
		peer,
		BlockSyncProtocolID)
	if err != nil {
		bs.RemovePeer(peer)
		return nil, xerrors.Errorf("failed to open stream to peer: %w", err)
	}
	// FIXME: Extract deadline constant.
	_ = stream.SetWriteDeadline(time.Now().Add(5 * time.Second)) // always use real time for socket/stream deadlines.

	if err := cborutil.WriteCborRPC(stream, req); err != nil {
		// FIXME: What's the point of setting a blank deadline that won't time out?
		_ = stream.SetWriteDeadline(time.Time{})
		bs.peerTracker.logFailure(peer, build.Clock.Since(start))
		return nil, err
	}
	// FIXME: Same. Why are we doing this?
	_ = stream.SetWriteDeadline(time.Time{})

	var res BlockSyncResponse
	err = cborutil.ReadCborRPC(
		// FIXME: Extract constants.
		bufio.NewReader(incrt.New(stream, 50<<10, 5*time.Second)),
		&res)
	if err != nil {
		bs.peerTracker.logFailure(peer, build.Clock.Since(start))
		return nil, err
	}
	bs.peerTracker.logSuccess(peer, build.Clock.Since(start))

	if span.IsRecordingEvents() {
		span.AddAttributes(
			trace.Int64Attribute("resp_status", int64(res.Status)),
			trace.StringAttribute("msg", res.Message),
			trace.Int64Attribute("chain_len", int64(len(res.Chain))),
		)
	}

	return &res, nil
}

// FIXME: Check request.
func (bs *BlockSync) processBlocksResponse(
	req *BlockSyncRequest,
	res *BlockSyncResponse,
) ([]*types.TipSet, error) {
	if len(res.Chain) == 0 {
		return nil, xerrors.Errorf("got no blocks in successful blocksync response")
	}

	// FIXME: Comment on current/next.
	cur, err := types.NewTipSet(res.Chain[0].Blocks)
	if err != nil {
		return nil, err
	}

	out := []*types.TipSet{cur}
	for bi := 1; bi < len(res.Chain); bi++ {
		next := res.Chain[bi].Blocks
		nts, err := types.NewTipSet(next)
		if err != nil {
			return nil, err
		}

		if !types.CidArrsEqual(cur.Parents().Cids(), nts.Cids()) {
			return nil, fmt.Errorf("parents of tipset[%d] were not tipset[%d]", bi-1, bi)
		}

		out = append(out, nts)
		cur = nts
	}
	return out, nil
}

// FIXME: Who uses this? Remove otherwise.
func (bs *BlockSync) GetBlock(ctx context.Context, c cid.Cid) (*types.BlockHeader, error) {
	sb, err := bs.bserv.GetBlock(ctx, c)
	if err != nil {
		return nil, err
	}

	return types.DecodeBlock(sb.RawData())
}

func (bs *BlockSync) AddPeer(p peer.ID) {
	bs.peerTracker.addPeer(p)
}

func (bs *BlockSync) RemovePeer(p peer.ID) {
	bs.peerTracker.removePeer(p)
}

// getPeers returns a preference-sorted set of peers to query.
func (bs *BlockSync) getPeers() []peer.ID {
	return bs.peerTracker.prefSortedPeers()
}

func (bs *BlockSync) FetchMessagesByCids(ctx context.Context, cids []cid.Cid) ([]*types.Message, error) {
	out := make([]*types.Message, len(cids))

	err := bs.fetchCids(ctx, cids, func(i int, b blocks.Block) error {
		msg, err := types.DecodeMessage(b.RawData())
		if err != nil {
			return err
		}

		// FIXME: We already sort in `fetchCids`, we are duplicating too much work,
		//  we don't need to pass the index.
		if out[i] != nil {
			return fmt.Errorf("received duplicate message")
		}

		out[i] = msg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FIXME: Duplicate of above.
func (bs *BlockSync) FetchSignedMessagesByCids(ctx context.Context, cids []cid.Cid) ([]*types.SignedMessage, error) {
	out := make([]*types.SignedMessage, len(cids))

	err := bs.fetchCids(ctx, cids, func(i int, b blocks.Block) error {
		smsg, err := types.DecodeSignedMessage(b.RawData())
		if err != nil {
			return err
		}

		if out[i] != nil {
			return fmt.Errorf("received duplicate message")
		}

		out[i] = smsg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Fetch `cids` from the block service, apply `cb` on each of them. Used
//  by the fetch message functions above.
// We check that each block is received only once and we do not received
//  blocks we did not request.
// FIXME: We should probably extract this logic to the `BlockService` and
//  make it public.
func (bs *BlockSync) fetchCids(
	ctx context.Context,
	cids []cid.Cid,
	cb func(int, blocks.Block) error,
) error {
	// FIXME: Why don't we use the context here?
	fetchedBlocks := bs.bserv.GetBlocks(context.TODO(), cids)

	cidIndex := make(map[cid.Cid]int)
	for i, c := range cids {
		cidIndex[c] = i
	}

	for i := 0; i < len(cids); i++ {
		select {
		case block, ok := <-fetchedBlocks:
			if !ok {
				// Closed channel, no more blocks fetched, check if we have all
				// of the CIDs requested.
				// FIXME: Review this check. We don't call the callback on the
				//  last index?
				if i == len(cids)-1 {
					break
				}

				return fmt.Errorf("failed to fetch all messages")
			}

			ix, ok := cidIndex[block.Cid()]
			if !ok {
				return fmt.Errorf("received message we didnt ask for")
			}

			if err := cb(ix, block); err != nil {
				return err
			}
		}
	}

	return nil
}
