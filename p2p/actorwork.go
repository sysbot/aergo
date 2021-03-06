package p2p

import (
	"github.com/aergoio/aergo/internal/enc"
	"github.com/aergoio/aergo/message"
	"github.com/aergoio/aergo/types"
	peer "github.com/libp2p/go-libp2p-peer"
)

// GetAddresses send getAddress request to other peer
func (p *P2P) GetAddresses(peerID peer.ID, size uint32) bool {
	remotePeer, ok := p.pm.GetPeer(peerID)
	if !ok {
		p.Warn().Str(LogPeerID, peerID.Pretty()).Msg("Message addressRequest to Unknown peer, check if a bug")

		return false
	}
	senderAddr := p.pm.SelfMeta().ToPeerAddress()
	// create message data
	req := &types.AddressesRequest{MessageData: &types.MessageData{},
		Sender: &senderAddr, MaxSize: 50}
	remotePeer.sendMessage(newPbMsgRequestOrder(true, false, addressesRequest, req))
	return true
}

// GetBlockHeaders send request message to peer and
func (p *P2P) GetBlockHeaders(msg *message.GetBlockHeaders) bool {
	remotePeer, exists := p.pm.GetPeer(msg.ToWhom)
	if !exists {
		p.Warn().Str(LogPeerID, msg.ToWhom.Pretty()).Msg("Request to invalid peer")
		return false
	}
	peerID := remotePeer.meta.ID

	p.Debug().Str(LogPeerID, peerID.Pretty()).Interface("msg", msg).Msg("Sending Get block Header request")
	// create message data
	reqMsg := &types.GetBlockHeadersRequest{MessageData: &types.MessageData{}, Hash: msg.Hash,
		Height: msg.Height, Offset: msg.Offset, Size: msg.MaxSize, Asc: msg.Asc,
	}
	remotePeer.sendMessage(newPbMsgRequestOrder(true, true, getBlockHeadersRequest, reqMsg))
	return true
}

// GetBlocks send request message to peer and
func (p *P2P) GetBlocks(peerID peer.ID, blockHashes []message.BlockHash) bool {
	remotePeer, exists := p.pm.GetPeer(peerID)
	if !exists {
		p.Warn().Str(LogPeerID, peerID.Pretty()).Str(LogProtoID, string(getBlocksRequest)).Msg("Message to Unknown peer, check if a bug")
		return false
	}
	p.Debug().Str(LogPeerID, peerID.Pretty()).Int("block_cnt", len(blockHashes)).Msg("Sending Get block request")

	hashes := make([][]byte, len(blockHashes))
	for i, hash := range blockHashes {
		hashes[i] = ([]byte)(hash)
	}
	// create message data
	req := &types.GetBlockRequest{MessageData: &types.MessageData{},
		Hashes: hashes}

	remotePeer.sendMessage(newPbMsgRequestOrder(true, true, getBlocksRequest, req))
	return true
}

// NotifyNewBlock send notice message of new block to a peer
func (p *P2P) NotifyNewBlock(newBlock message.NotifyNewBlock) bool {
	// create message data
	for _, neighbor := range p.pm.GetPeers() {
		if neighbor == nil {
			continue
		}
		req := &types.NewBlockNotice{MessageData: &types.MessageData{},
			BlockHash: newBlock.Block.Hash,
			BlockNo:   newBlock.BlockNo}
		msg := newPbMsgBroadcastOrder(false, newBlockNotice, req)
		if neighbor.State() == types.RUNNING {
			p.Debug().Str(LogPeerID, neighbor.meta.ID.Pretty()).Str("hash", enc.ToString(newBlock.Block.Hash)).Msg("Notifying new block")
			// FIXME need to check if remote peer knows this hash already.
			// but can't do that in peer's write goroutine, since the context is gone in
			// protobuf serialization.
			neighbor.sendMessage(msg)
		}
	}
	return true
}

// GetMissingBlocks send request message to peer about blocks which my local peer doesn't have
func (p *P2P) GetMissingBlocks(peerID peer.ID, hashes []message.BlockHash) bool {
	remotePeer, exists := p.pm.GetPeer(peerID)
	if !exists {
		p.Warn().Str(LogPeerID, peerID.Pretty()).Msg("invalid peer id")
		return false
	}
	p.Debug().Str(LogPeerID, peerID.Pretty()).Msg("Send Get Missing Blocks")

	bhashes := make([][]byte, 0)
	for _, a := range hashes {
		bhashes = append(bhashes, a)
	}
	// create message data
	req := &types.GetMissingRequest{
		MessageData: &types.MessageData{},
		Hashes:      bhashes[1:],
		Stophash:    bhashes[0]}

	remotePeer.sendMessage(newPbMsgRequestOrder(false, true, getMissingRequest, req))
	return true
}

// GetTXs send request message to peer and
func (p *P2P) GetTXs(peerID peer.ID, txHashes []message.TXHash) bool {
	remotePeer, ok := p.pm.GetPeer(peerID)
	if !ok {
		p.Warn().Str(LogPeerID, peerID.Pretty()).Msg("Invalid peer. check for bug")
		return false
	}
	p.Debug().Str(LogPeerID, peerID.Pretty()).Int("tx_cnt", len(txHashes)).Msg("Sending GetTransactions request")
	if len(txHashes) == 0 {
		p.Warn().Msg("empty hash list")
		return false
	}

	hashes := make([][]byte, len(txHashes))
	for i, hash := range txHashes {
		if len(hash) == 0 {
			p.Warn().Msg("empty hash value requested.")
			return false
		}
		hashes[i] = ([]byte)(hash)
	}
	// create message data
	req := &types.GetTransactionsRequest{MessageData: &types.MessageData{},
		Hashes: hashes}

	remotePeer.sendMessage(newPbMsgRequestOrder(true, true, getTXsRequest, req))
	return true
}

// NotifyNewTX notice tx(s) id created
func (p *P2P) NotifyNewTX(newTXs message.NotifyNewTransactions) bool {
	hashes := make([][]byte, len(newTXs.Txs))
	for i, tx := range newTXs.Txs {
		hashes[i] = tx.Hash
	}
	p.Debug().Int("peer_cnt", len(p.pm.GetPeers())).Str("hashes", bytesArrToString(hashes)).Msg("Notifying newTXs to peers")
	// send to peers
	for _, peer := range p.pm.GetPeers() {
		// create message data
		req := &types.NewTransactionsNotice{MessageData: &types.MessageData{},
			TxHashes: hashes,
		}
		peer.sendMessage(newPbMsgBroadcastOrder(false, newBlockNotice, req))
	}

	return true
}
