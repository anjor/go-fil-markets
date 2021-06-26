package storageimpl

import (
	"context"
	"io"

	"github.com/filecoin-project/go-commp-utils/writer"
	"github.com/ipfs/go-cid"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/ipld/go-car/v2/blockstore"
	"github.com/libp2p/go-libp2p-core/peer"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-fil-markets/filestore"
	"github.com/filecoin-project/go-fil-markets/piecestore"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	"github.com/filecoin-project/go-fil-markets/storagemarket/impl/providerstates"
	"github.com/filecoin-project/go-fil-markets/storagemarket/network"
)

// -------
// providerDealEnvironment
// -------

type providerDealEnvironment struct {
	p *Provider
}

// TODO Uncomment code when DAG Store compiles
func (p *providerDealEnvironment) ActivateShard(pieceCid cid.Cid) error {
	/*
		key := shard.KeyFromCID(pieceCid)

		mt, err := marketdagstore.NewLotusMount(pieceCid, p.p.mountApi)
		if err != nil {
			return err
		}

		return p.p.dagStore.RegisterShard(key, mt)*/

	return nil
}

func (p *providerDealEnvironment) ReadWriteBlockstoreFor(proposalCid cid.Cid) (*blockstore.ReadWrite, error) {
	return p.p.readWriteBlockStores.Get(proposalCid.String())
}

func (p *providerDealEnvironment) Address() address.Address {
	return p.p.actor
}

func (p *providerDealEnvironment) Node() storagemarket.StorageProviderNode {
	return p.p.spn
}

func (p *providerDealEnvironment) Ask() storagemarket.StorageAsk {
	sask := p.p.storedAsk.GetAsk()
	if sask == nil {
		return storagemarket.StorageAskUndefined
	}
	return *sask.Ask
}

func (p *providerDealEnvironment) CleanupBlockstore(proposalCid cid.Cid, carV2FilePath string) error {
	if err := p.p.readWriteBlockStores.Clean(proposalCid.String()); err != nil {
		log.Errorf("failed to clean read write blockstore, proposalCid=%s, err=%s", proposalCid, err)
	}

	// clean up the backing CAR file.
	return p.p.fs.Delete(filestore.Path(carV2FilePath))
}

func (p *providerDealEnvironment) GeneratePieceCommitment(carV2FilePath string) (cid.Cid, filestore.Path, error) {
	rd, err := carv2.NewReaderFromCARv2File(carV2FilePath)
	if err != nil {
		return cid.Undef, "", xerrors.Errorf("failed to get CARv2 reader, err=%s", err)
	}
	defer func() {
		if err := rd.Close(); err != nil {
			log.Errorf("failed to close CARv2 reader, err=%s", err)
		}
	}()

	if p.p.universalRetrievalEnabled {
		// TODO Get this work later = punt on it for now as this is anyways NOT enabled.
		//return providerutils.GeneratePieceCommitmentWithMetadata(p.p.fs, p.p.pio.GeneratePieceCommitment, proofType, payloadCid, selector, storeID)
	}

	// dump the CARv1 payload of the CARv2 file to the Commp Writer and get back the CommP.
	w := &writer.Writer{}
	written, err := io.Copy(w, rd.CarV1Reader())
	if err != nil {
		return cid.Undef, "", xerrors.Errorf("failed to write to CommP writer, err=%w", err)
	}
	if written != int64(rd.Header.CarV1Size) {
		return cid.Undef, "", xerrors.Errorf("number of bytes written not equal to CARv1 payload size")
	}

	cidAndSize, err := w.Sum()
	if err != nil {
		return cid.Undef, "", xerrors.Errorf("failed to get CommP, err=%w", err)
	}

	return cidAndSize.PieceCID, filestore.Path(""), err
}

func (p *providerDealEnvironment) FileStore() filestore.FileStore {
	return p.p.fs
}

func (p *providerDealEnvironment) PieceStore() piecestore.PieceStore {
	return p.p.pieceStore
}

func (p *providerDealEnvironment) SendSignedResponse(ctx context.Context, resp *network.Response) error {
	s, err := p.p.conns.DealStream(resp.Proposal)
	if err != nil {
		return xerrors.Errorf("couldn't send response: %w", err)
	}

	sig, err := p.p.sign(ctx, resp)
	if err != nil {
		return xerrors.Errorf("failed to sign response message: %w", err)
	}

	signedResponse := network.SignedResponse{
		Response:  *resp,
		Signature: sig,
	}

	err = s.WriteDealResponse(signedResponse, p.p.sign)
	if err != nil {
		// Assume client disconnected
		_ = p.p.conns.Disconnect(resp.Proposal)
	}
	return err
}

func (p *providerDealEnvironment) Disconnect(proposalCid cid.Cid) error {
	return p.p.conns.Disconnect(proposalCid)
}

func (p *providerDealEnvironment) RunCustomDecisionLogic(ctx context.Context, deal storagemarket.MinerDeal) (bool, string, error) {
	if p.p.customDealDeciderFunc == nil {
		return true, "", nil
	}
	return p.p.customDealDeciderFunc(ctx, deal)
}

func (p *providerDealEnvironment) TagPeer(id peer.ID, s string) {
	p.p.net.TagPeer(id, s)
}

func (p *providerDealEnvironment) UntagPeer(id peer.ID, s string) {
	p.p.net.UntagPeer(id, s)
}

var _ providerstates.ProviderDealEnvironment = &providerDealEnvironment{}

type providerStoreGetter struct {
	p *Provider
}

func (psg *providerStoreGetter) Get(proposalCid cid.Cid) (bstore.Blockstore, error) {
	// Wait for the provider to be ready
	err := awaitProviderReady(psg.p)
	if err != nil {
		return nil, err
	}

	var deal storagemarket.MinerDeal
	err = psg.p.deals.Get(proposalCid).Get(&deal)
	if err != nil {
		return nil, xerrors.Errorf("failed to get deal state, err=%w", err)
	}

	return psg.p.readWriteBlockStores.GetOrCreate(proposalCid.String(), deal.CARv2FilePath, deal.Ref.Root)
}

type providerPushDeals struct {
	p *Provider
}

func (ppd *providerPushDeals) Get(proposalCid cid.Cid) (storagemarket.MinerDeal, error) {
	// Wait for the provider to be ready
	var deal storagemarket.MinerDeal
	err := awaitProviderReady(ppd.p)
	if err != nil {
		return deal, err
	}

	err = ppd.p.deals.GetSync(context.TODO(), proposalCid, &deal)
	return deal, err
}

// awaitProviderReady waits for the provider to startup
func awaitProviderReady(p *Provider) error {
	err := p.AwaitReady()
	if err != nil {
		return xerrors.Errorf("could not get deal with proposal CID %s: error waiting for provider startup: %w")
	}

	return nil
}
