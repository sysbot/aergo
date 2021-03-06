/**
 *  @file
 *  @copyright defined in aergo/LICENSE.txt
 */

package p2p

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	lru "github.com/hashicorp/golang-lru"
	"github.com/libp2p/go-libp2p-host"

	"github.com/aergoio/aergo-lib/log"
	"github.com/aergoio/aergo/message"
	"github.com/aergoio/aergo/types"

	cfg "github.com/aergoio/aergo/config"
	"github.com/aergoio/aergo/pkg/component"
	libp2p "github.com/libp2p/go-libp2p"
	crypto "github.com/libp2p/go-libp2p-crypto"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	ma "github.com/multiformats/go-multiaddr"
)

var myPeerInfo peerInfo

type peerInfo struct {
	sync.RWMutex
	id      *peer.ID
	privKey *crypto.PrivKey
}

// TODO this value better related to max peer and block produce interval, not constant
const (
	DefaultGlobalInvCacheSize = 100
	DefaultPeerInvCacheSize   = 30
)

// PeerManager is internal service that provide peer management
type PeerManager interface {
	host.Host
	Start() error
	Stop() error
	GetStatus() component.Status

	PrivateKey() crypto.PrivKey
	PublicKey() crypto.PubKey
	SelfMeta() PeerMeta
	SelfNodeID() peer.ID

	AddNewPeer(peer PeerMeta)
	RemovePeer(peerID peer.ID)
	NotifyPeerHandshake(peerID peer.ID)
	NotifyPeerAddressReceived([]PeerMeta)

	HandleNewBlockNotice(peerID peer.ID, b64hash string, data *types.NewBlockNotice)

	// GetPeer return registered(handshaked) remote peer object
	GetPeer(ID peer.ID) (*RemotePeer, bool)
	GetPeers() []*RemotePeer
	GetPeerAddresses() ([]*types.PeerAddress, []types.PeerState)

	// deprecated methods... use sendmessage helper functions instead
	SignProtoMessage(message proto.Message) ([]byte, error)
	AuthenticateMessage(message proto.Message, data *types.MessageData) bool
}

/**
 * peerManager connect to and listen from other nodes.
 * It implements  Component interface
 */
type peerManager struct {
	host.Host
	privateKey crypto.PrivKey
	publicKey  crypto.PubKey
	selfMeta   PeerMeta
	iServ      ActorService
	rm         ReconnectManager

	designatedPeers map[peer.ID]PeerMeta

	subProtocols []subProtocol
	remotePeers  map[peer.ID]*RemotePeer
	peerPool     map[peer.ID]PeerMeta
	conf         *cfg.P2PConfig
	log          *log.Logger
	mutex        *sync.Mutex
	peerCache    []*RemotePeer

	status component.Status

	addPeerChannel    chan PeerMeta
	removePeerChannel chan peer.ID
	hsPeerChannel     chan peer.ID
	fillPoolChannel   chan []PeerMeta
	finishChannel     chan struct{}
	eventListeners    []PeerEventListener

	invCache *lru.Cache
}

var _ PeerManager = (*peerManager)(nil)

// PeerEventListener listen peer manage event
type PeerEventListener interface {
	// OnAddPeer is called just after the peer is added.
	OnAddPeer(peerID peer.ID)

	// OnRemovePeer is called just before the peer is removed
	OnRemovePeer(peerID peer.ID)
}

// subProtocol is sub protocol of p2p protocol
type subProtocol interface {
	setPeerManager(PeerManager)
	// initWith init subprotocol implementation with PeerManager.
	startHandling()
}

func init() {
}

// NewPeerManager creates a peer manager object.
func NewPeerManager(iServ ActorService, cfg *cfg.Config, rm ReconnectManager, logger *log.Logger) PeerManager {
	p2pConf := cfg.P2P
	//logger.SetLevel("debug")
	hl := &peerManager{
		iServ: iServ,
		conf:  p2pConf,
		rm:    rm,
		log:   logger,
		mutex: &sync.Mutex{},

		designatedPeers: make(map[peer.ID]PeerMeta, len(cfg.P2P.NPAddPeers)),

		remotePeers: make(map[peer.ID]*RemotePeer, p2pConf.NPMaxPeers),
		peerPool:    make(map[peer.ID]PeerMeta, p2pConf.NPPeerPool),
		peerCache:   make([]*RemotePeer, 0, p2pConf.NPMaxPeers),

		subProtocols:      make([]subProtocol, 0, 4),
		status:            component.StoppedStatus,
		addPeerChannel:    make(chan PeerMeta, 2),
		removePeerChannel: make(chan peer.ID),
		hsPeerChannel:     make(chan peer.ID),
		fillPoolChannel:   make(chan []PeerMeta),
		eventListeners:    make([]PeerEventListener, 0, 4),
		finishChannel:     make(chan struct{}),
	}

	var err error
	hl.invCache, err = lru.New(DefaultGlobalInvCacheSize)
	if err != nil {
		panic("Failed to create peermanager " + err.Error())
	}
	// additional initializations
	hl.init()

	return hl
}

func (ps *peerManager) PrivateKey() crypto.PrivKey {
	return ps.privateKey
}
func (ps *peerManager) PublicKey() crypto.PubKey {
	return ps.publicKey
}
func (ps *peerManager) SelfMeta() PeerMeta {
	return ps.selfMeta
}
func (ps *peerManager) SelfNodeID() peer.ID {
	return ps.selfMeta.ID
}

func (ps *peerManager) AddSubProtocol(p subProtocol) {
	ps.subProtocols = append(ps.subProtocols, p)
	p.setPeerManager(ps)
}
func (ps *peerManager) RegisterEventListener(listener PeerEventListener) {
	ps.mutex.Lock()
	defer ps.mutex.Unlock()
	ps.eventListeners = append(ps.eventListeners, listener)
}

func (ps *peerManager) init() {
	// check Key and address
	var priv crypto.PrivKey
	var pub crypto.PubKey
	if ps.conf.NPKey != "" {
		dat, err := ioutil.ReadFile(ps.conf.NPKey)
		if err == nil {
			priv, err = crypto.UnmarshalPrivateKey(dat)
			if err != nil {
				ps.log.Warn().Str("npkey", ps.conf.NPKey).Msg("invalid keyfile. It's not private key file")
			}
			pub = priv.GetPublic()
		} else {
			ps.log.Warn().Str("npkey", ps.conf.NPKey).Msg("invalid keyfile path")
		}
	}
	if nil == priv {
		ps.log.Info().Msg("No valid private key file is found. use temporary pk instead")
		priv, pub, _ = crypto.GenerateKeyPair(crypto.Secp256k1, 256)
	}
	pid, _ := peer.IDFromPublicKey(pub)
	myPeerInfo.set(&pid, &priv)

	listenAddr := net.ParseIP(ps.conf.NetProtocolAddr)
	listenPort := ps.conf.NetProtocolPort
	var err error
	if nil == listenAddr {
		panic("invalid NetProtocolAddr " + ps.conf.NetProtocolAddr)
	} else if !listenAddr.IsUnspecified() {
		ps.log.Info().Str("ps.conf.NetProtocolAddr", ps.conf.NetProtocolAddr).Int("listenPort", listenPort).Msg("Using NetProtocolAddr in configfile")
	} else {
		listenAddr, err = externalIP()
		ps.log.Info().Str("addr", listenAddr.To4().String()).Int("port", listenPort).Msg("No NetProtocolAddr is specified")
		if err != nil {
			panic("Couldn't find listening ip address: " + err.Error())
		}
	}
	ps.privateKey = priv
	ps.publicKey = pub
	ps.selfMeta.IPAddress = listenAddr.String()
	ps.selfMeta.Port = uint32(listenPort)
	ps.selfMeta.ID = pid

	// set designated peers
	ps.addDesignatedPeers()
}

func (ps *peerManager) run() {

	go ps.runManagePeers()
	// need to start listen after chainservice is read to init
	// FIXME: adhoc code
	go func() {
		time.Sleep(time.Second * 3)
		ps.startListener()

		// addition should start after all modules are started
		go func() {
			time.Sleep(time.Second * 2)
			for _, meta := range ps.designatedPeers {
				ps.addPeerChannel <- meta
			}
		}()
	}()
}

func (ps *peerManager) addDesignatedPeers() {
	// add remote node from config
	for _, target := range ps.conf.NPAddPeers {
		// go-multiaddr implementation does not support recent p2p protocol yet, but deprecated name ipfs.
		// This adhoc will be removed when go-multiaddr is patched.
		target = strings.Replace(target, "/p2p/", "/ipfs/", 1)
		targetAddr, err := ma.NewMultiaddr(target)
		if err != nil {
			ps.log.Warn().Err(err).Str("target", target).Msg("invalid NPAddPeer address")
			continue
		}
		splitted := strings.Split(targetAddr.String(), "/")
		if len(splitted) != 7 {
			ps.log.Warn().Str("target", target).Msg("invalid NPAddPeer address")
			continue
		}
		peerAddrString := splitted[2]
		peerPortString := splitted[4]
		peerPort, err := strconv.Atoi(peerPortString)
		if err != nil {
			ps.log.Warn().Str("port", peerPortString).Msg("invalid Peer port")
			continue
		}
		peerIDString := splitted[6]
		peerID, err := peer.IDB58Decode(peerIDString)
		if err != nil {
			ps.log.Warn().Str(LogPeerID, peerIDString).Msg("invalid PeerID")
			continue
		}
		peerMeta := PeerMeta{
			ID:         peerID,
			Port:       uint32(peerPort),
			IPAddress:  peerAddrString,
			Designated: true,
			Outbound:   true,
		}
		ps.log.Info().Str(LogPeerID, peerID.Pretty()).Str("addr", peerAddrString).Int("port", peerPort).Msg("Adding Designated peer")
		ps.designatedPeers[peerID] = peerMeta
	}
}

func (ps *peerManager) runManagePeers() {
	addrDuration := time.Minute * 3
	addrTicker := time.NewTicker(addrDuration)
	// reconnectRunners := make(map[peer.ID]*reconnectRunner)
MANLOOP:
	for {
		select {
		case meta := <-ps.addPeerChannel:
			if ps.addOutboundPeer(meta) {
				if _, found := ps.designatedPeers[meta.ID]; found {
					ps.rm.CancelJob(meta.ID)
				}
			}
		case id := <-ps.removePeerChannel:
			if ps.removePeer(id) {
				if meta, found := ps.designatedPeers[id]; found {
					ps.rm.AddJob(meta)
				}
			}
		case <-addrTicker.C:
			ps.checkAndCollectPeerListFromAll()
		case peerID := <-ps.hsPeerChannel:
			ps.checkAndCollectPeerList(peerID)
		case peerMetas := <-ps.fillPoolChannel:
			ps.tryFillPool(&peerMetas)
		case <-ps.finishChannel:
			break MANLOOP
		}
	}
	addrTicker.Stop()

	// cleanup peers
	for peerID := range ps.remotePeers {
		ps.removePeer(peerID)
	}
}

// addOutboundPeer try to connect and handshake to remote peer. it can be called after peermanager is inited.
// It return true if peer is added or already exist, or return false if failed to add peer.
func (ps *peerManager) addOutboundPeer(meta PeerMeta) bool {
	addrString := fmt.Sprintf("/ip4/%s/tcp/%d", meta.IPAddress, meta.Port)
	var peerAddr, err = ma.NewMultiaddr(addrString)
	if err != nil {
		ps.log.Warn().Err(err).Str("addr", addrString).Msg("invalid NPAddPeer address")
		return false
	}
	var peerID = meta.ID
	ps.mutex.Lock()
	newPeer, ok := ps.remotePeers[peerID]
	if ok {
		// peer is already exist
		ps.log.Info().Str(LogPeerID, newPeer.meta.ID.Pretty()).Msg("Peer is already managed by peerService")
		if meta.Designated {
			// If remote peer was connected first. designated flag is not set yet.
			newPeer.meta.Designated = true
		}
		ps.mutex.Unlock()
		return true
	}
	ps.mutex.Unlock()

	// if peer exists in peerstore already, reuse that peer again.
	if !ps.checkInPeerstore(peerID) {
		ps.Peerstore().AddAddr(peerID, peerAddr, meta.TTL())
	}

	ctx := context.Background()
	s, err := ps.NewStream(ctx, meta.ID, aergoP2PSub)
	if err != nil {
		ps.log.Warn().Err(err).Str(LogPeerID, meta.ID.Pretty()).Str(LogProtoID, string(aergoP2PSub)).Msg("Error while get stream")
		return false
	}
	rw := &bufio.ReadWriter{Reader: bufio.NewReader(s), Writer: bufio.NewWriter(s)}

	success := doHandshake(ps, peerID, rw)
	if !success {
		ps.sendGoAway(rw, "Failed to handshake")
		s.Close()
		return false
	}

	ps.mutex.Lock()
	defer ps.mutex.Unlock()
	newPeer, ok = ps.remotePeers[peerID]
	if ok {
		if ComparePeerID(ps.selfMeta.ID, meta.ID) <= 0 {
			ps.log.Info().Str(LogPeerID, newPeer.meta.ID.Pretty()).Msg("Peer is added while handshaking")
			s.Close()
			return true
		}
	}

	newPeer = newRemotePeer(meta, ps, ps.iServ, ps.log)
	newPeer.rw = &bufio.ReadWriter{Reader: bufio.NewReader(s), Writer: bufio.NewWriter(s)}
	// insert Handlers
	ps.insertHandlers(newPeer)
	go newPeer.runPeer()
	newPeer.setState(types.RUNNING)

	ps.insertPeer(peerID, newPeer)
	ps.log.Info().Str(LogPeerID, peerID.Pretty()).Str("addr", peerAddr.String()).Msg("Outbound peer is  added to peerService")
	return true
}

func (ps *peerManager) insertHandlers(peer *RemotePeer) {
	// PingHandler
	ph := NewPingHandler(ps, peer, ps.log)
	peer.handlers[pingRequest] = ph.handlePing
	peer.handlers[pingResponse] = ph.handlePingResponse
	peer.handlers[goAway] = ph.handleGoAway
	peer.handlers[addressesRequest] = ph.handleAddressesRequest
	peer.handlers[addressesResponse] = ph.handleAddressesResponse

	// BlockHandler
	bh := NewBlockHandler(ps, peer, ps.log)
	peer.handlers[getBlocksRequest] = bh.handleBlockRequest
	peer.handlers[getBlocksResponse] = bh.handleGetBlockResponse
	peer.handlers[getBlockHeadersRequest] = bh.handleGetBlockHeadersRequest
	peer.handlers[getBlockHeadersResponse] = bh.handleGetBlockHeadersResponse
	peer.handlers[getMissingRequest] = bh.handleGetMissingRequest
	// peer.handlers[getMissingResponse] = // no function yet
	peer.handlers[newBlockNotice] = bh.handleNewBlockNotice

	th := NewTxHandler(ps, peer, ps.log)
	peer.handlers[getTXsRequest] = th.handleGetTXsRequest
	peer.handlers[getTxsResponse] = th.handleGetTXsResponse
	peer.handlers[newTxNotice] = th.handleNewTXsNotice
}
func (ps *peerManager) tryAddInboundPeer(meta PeerMeta, rw *bufio.ReadWriter) bool {
	ps.mutex.Lock()
	defer ps.mutex.Unlock()
	peerID := meta.ID
	peer, found := ps.remotePeers[peerID]

	if found {
		// already found. drop this connection
		if ComparePeerID(ps.selfMeta.ID, peerID) <= 0 {
			return false
		}
	}
	peer = newRemotePeer(meta, ps, ps.iServ, ps.log)
	peer.rw = rw
	ps.insertHandlers(peer)
	go peer.runPeer()
	peer.setState(types.RUNNING)
	ps.insertPeer(peerID, peer)
	peerAddr := meta.ToPeerAddress()
	ps.log.Info().Str(LogPeerID, peerID.Pretty()).Str("addr", peerAddr.String()).Msg("Inbound peer is  added to peerService")
	return true
}

func (ps *peerManager) checkInPeerstore(peerID peer.ID) bool {
	found := false
	for _, existingPeerID := range ps.Peerstore().Peers() {
		if existingPeerID == peerID {
			found = true
			break
		}
	}
	return found
}

func (ps *peerManager) AddNewPeer(peer PeerMeta) {
	ps.addPeerChannel <- peer
}

func (ps *peerManager) RemovePeer(peerID peer.ID) {
	ps.removePeerChannel <- peerID
}

func (ps *peerManager) NotifyPeerHandshake(peerID peer.ID) {
	ps.hsPeerChannel <- peerID
}

func (ps *peerManager) NotifyPeerAddressReceived(metas []PeerMeta) {
	ps.fillPoolChannel <- metas
}

// removePeer remove and disconnect managed remote peer connection
// It return true if peer is exist and managed by peermanager
func (ps *peerManager) removePeer(peerID peer.ID) bool {
	ps.mutex.Lock()
	target, ok := ps.remotePeers[peerID]
	if !ok {
		ps.mutex.Unlock()
		return false
	}
	ps.deletePeer(peerID)
	// No internal module access this peer anymore, but remote message can be received.
	target.stop()
	ps.mutex.Unlock()

	// also disconnect connection
	for _, existingPeerID := range ps.Peerstore().Peers() {
		if existingPeerID == peerID {
			for _, listener := range ps.eventListeners {
				listener.OnRemovePeer(peerID)
			}
			ps.Network().ClosePeer(peerID)
			return true
		}
	}
	return true
}

func (ps *peerManager) Peerstore() pstore.Peerstore {
	return ps.Host.Peerstore()
}

func (ps *peerManager) startListener() {
	var err error
	listens := make([]ma.Multiaddr, 0, 2)
	// FIXME: should also support ip6 later
	listen, err := ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d",
		ps.selfMeta.IPAddress, ps.selfMeta.Port))
	if err != nil {
		panic("Can't estabilish listening address: " + err.Error())
	}
	listens = append(listens, listen)
	listen, _ = ma.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", ps.selfMeta.Port))
	listens = append(listens, listen)

	peerStore := pstore.NewPeerstore()

	newHost, err := libp2p.New(context.Background(), libp2p.Identity(ps.privateKey), libp2p.Peerstore(peerStore), libp2p.ListenAddrs(listens...))
	if err != nil {
		ps.log.Fatal().Err(err).Str("addr", listen.String()).Msg("Couldn't listen from")
		panic(err.Error())
	}

	ps.log.Info().Str("pid", ps.SelfNodeID().Pretty()).Str("addr[0]", listens[0].String()).Str("addr[1]", listens[1].String()).
		Msg("Set self node's pid, and listening for connections")
	ps.Host = newHost

	ps.SetStreamHandler(aergoP2PSub, ps.onHandshake)
	// // listen subprotocols also
	// for _, sub := range ps.subProtocols {
	// 	sub.startHandling()
	// }
}

func (pi *peerInfo) set(id *peer.ID, privKey *crypto.PrivKey) {
	pi.Lock()
	pi.id = id
	pi.privKey = privKey
	pi.Unlock()
}

// GetMyID returns the peer id of the current node
func GetMyID() (peer.ID, crypto.PrivKey) {
	var id *peer.ID
	var pk *crypto.PrivKey

	for pk == nil || id == nil {
		myPeerInfo.RLock()
		id = myPeerInfo.id
		pk = myPeerInfo.privKey
		myPeerInfo.RUnlock()

		// To prevent high cpu usage
		time.Sleep(100 * time.Millisecond)
	}

	return *id, *pk
}

func (ps *peerManager) Start() error {
	ps.run()
	ps.status = component.StartedStatus
	//ps.conf.NPAddPeers
	return nil
}
func (ps *peerManager) Stop() error {
	// TODO stop service
	ps.status = component.StoppingStatus
	close(ps.addPeerChannel)
	close(ps.removePeerChannel)
	ps.status = component.StoppedStatus
	ps.finishChannel <- struct{}{}
	return nil
}

func (ps *peerManager) GetStatus() component.Status {
	return ps.status
}

func (ps *peerManager) Started() bool {
	return ps.status == component.StartedStatus
}

func (ps *peerManager) Ended() bool {
	return ps.status == component.StoppedStatus
}

func (ps *peerManager) GetName() string {
	return "p2p service"
}

func (ps *peerManager) checkAndCollectPeerListFromAll() {
	if ps.hasEnoughPeers() {
		return
	}
	for _, remotePeer := range ps.remotePeers {
		ps.iServ.SendRequest(message.P2PSvc, &message.GetAddressesMsg{ToWhom: remotePeer.meta.ID, Size: 20, Offset: 0})
	}
}

func (ps *peerManager) checkAndCollectPeerList(ID peer.ID) {
	if ps.hasEnoughPeers() {
		return
	}
	peer, ok := ps.GetPeer(ID)
	if !ok {
		//ps.log.Warnf("invalid peer id %s", ID.Pretty())
		ps.log.Warn().Str(LogPeerID, ID.Pretty()).Msg("invalid peer id")
		return
	}
	ps.iServ.SendRequest(message.P2PSvc, &message.GetAddressesMsg{ToWhom: peer.meta.ID, Size: 20, Offset: 0})
}

func (ps *peerManager) hasEnoughPeers() bool {
	return len(ps.peerPool) >= ps.conf.NPPeerPool
}

// tryConnectPeers should be called in runManagePeers() only
func (ps *peerManager) tryFillPool(metas *[]PeerMeta) {
	added := make([]PeerMeta, 0, len(*metas))
	for _, meta := range *metas {
		_, found := ps.peerPool[meta.ID]
		if !found {
			// change some properties
			meta.Outbound = true
			meta.Designated = false
			ps.peerPool[meta.ID] = meta
			added = append(added, meta)
		}
	}
	ps.log.Debug().Int("added_cnt", len(added)).Msg("Filled unknown peer addresses to peerpool")
	ps.tryConnectPeers()
}

// tryConnectPeers should be called in runManagePeers() only
func (ps *peerManager) tryConnectPeers() {
	remained := ps.conf.NPMaxPeers - len(ps.remotePeers)
	for ID, meta := range ps.peerPool {
		if _, found := ps.GetPeer(ID); found {
			delete(ps.peerPool, ID)
			continue
		}
		if meta.IPAddress == "" || meta.Port == 0 {
			ps.log.Warn().Str(LogPeerID, meta.ID.Pretty()).Str("addr", meta.IPAddress).
				Uint32("port", meta.Port).Msg("Invalid peer meta informations")
			continue
		}
		// in same go rountine.
		ps.addOutboundPeer(meta)
		remained--
		if remained <= 0 {
			break
		}
	}
}

// Authenticate incoming p2p message
// message: a protobufs go data object
// data: common p2p message data
func (ps *peerManager) AuthenticateMessage(message proto.Message, data *types.MessageData) bool {
	// for Test only
	return true

	// store a temp ref to signature and remove it from message data
	// sign is a string to allow easy reset to zero-value (empty string)
	sign := data.Sign
	data.Sign = []byte{}

	// marshall data without the signature to protobufs3 binary format
	bin, err := proto.Marshal(message)
	if err != nil {
		ps.log.Warn().Msg("failed to marshal pb message")
		return false
	}

	// restore sig in message data (for possible future use)
	data.Sign = sign

	// restore peer peer.ID binary format from base58 encoded node peer.ID data
	peerID, err := peer.IDB58Decode(data.PeerID)
	if err != nil {
		ps.log.Warn().Err(err).Msg("Failed to decode node peer.ID from base58")
		return false
	}

	// verify the data was authored by the signing peer identified by the public key
	// and signature included in the message
	return ps.VerifyData(bin, []byte(sign), peerID, data.NodePubKey)
}

// sign an outgoing p2p message payload
func (ps *peerManager) SignProtoMessage(message proto.Message) ([]byte, error) {
	data, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	return ps.SignData(data)
}

// sign binary data using the local node's private key
func (ps *peerManager) SignData(data []byte) ([]byte, error) {
	key := ps.privateKey
	res, err := key.Sign(data)
	return res, err
}

// VerifyData Verifies incoming p2p message data integrity
// data: data to verify
// signature: author signature provided in the message payload
// peerID: author peer peer.ID from the message payload
// pubKeyData: author public key from the message payload
func (ps *peerManager) VerifyData(data []byte, signature []byte, peerID peer.ID, pubKeyData []byte) bool {
	key, err := crypto.UnmarshalPublicKey(pubKeyData)
	if err != nil {
		ps.log.Warn().Msg("Failed to extract key from message key data")
		return false
	}

	// extract node peer.ID from the provided public key
	idFromKey, err := peer.IDFromPublicKey(key)

	if err != nil {
		ps.log.Warn().Msg("Failed to extract peer peer.ID from public key")
		return false
	}

	// verify that message author node peer.ID matches the provided node public key
	if idFromKey != peerID {
		ps.log.Warn().Msg("Node peer.ID and provided public key mismatch")
		return false
	}

	res, err := key.Verify(data, signature)
	if err != nil {
		ps.log.Warn().Msg("Error authenticating data")
		return false
	}

	return res
}

func (ps *peerManager) GetPeer(ID peer.ID) (*RemotePeer, bool) {
	ps.mutex.Lock()
	defer ps.mutex.Unlock()

	// vs code's lint does not allow direct return of map operation
	ptr, ok := ps.remotePeers[ID]
	if !ok {
		return nil, false
	}
	return ptr, ok
}

func (ps *peerManager) GetPeers() []*RemotePeer {
	ps.mutex.Lock()
	defer ps.mutex.Unlock()
	return ps.peerCache
}

func (ps *peerManager) GetPeerAddresses() ([]*types.PeerAddress, []types.PeerState) {
	peers := make([]*types.PeerAddress, 0, len(ps.remotePeers))
	states := make([]types.PeerState, 0, len(ps.remotePeers))
	for _, aPeer := range ps.remotePeers {
		addr := aPeer.meta.ToPeerAddress()
		peers = append(peers, &addr)
		states = append(states, aPeer.state)
	}
	return peers, states
}

func (ps *peerManager) HandleNewBlockNotice(peerID peer.ID, b64hash string, data *types.NewBlockNotice) {
	// TODO check if evicted return value is needed.
	ok, _ := ps.invCache.ContainsOrAdd(b64hash, data.BlockHash)
	if ok {
		ps.log.Debug().Str(LogBlkHash, b64hash).Str(LogPeerID, peerID.Pretty()).Msg("Got NewBlock notice, but sent already from other peer")
		// this notice is already sent to chainservice
		return
	}

	// request block info if selfnode does not have block already
	rawResp, err := ps.iServ.CallRequest(message.ChainSvc, &message.GetBlock{BlockHash: message.BlockHash(data.BlockHash)})
	if err != nil {
		ps.log.Warn().Err(err).Msg("actor return error on getblock")
		return
	}
	resp, ok := rawResp.(message.GetBlockRsp)
	if !ok {
		ps.log.Warn().Str("expected", "message.GetBlockRsp").Str("actual", reflect.TypeOf(rawResp).Name()).Msg("chainservice returned unexpected type")
		return
	}
	if resp.Err != nil {
		ps.log.Debug().Str(LogBlkHash, b64hash).Str(LogPeerID, peerID.Pretty()).Msg("chainservice responded that block not found. request back to notifier")
		ps.iServ.SendRequest(message.P2PSvc, &message.GetBlockInfos{ToWhom: peerID,
			Hashes: []message.BlockHash{message.BlockHash(data.BlockHash)}})
	}

}

// this method should be called inside ps.mutex
func (ps *peerManager) insertPeer(ID peer.ID, peer *RemotePeer) {
	ps.remotePeers[ID] = peer

	// TODO need tuning?
	newSlice := make([]*RemotePeer, 0, len(ps.remotePeers))
	for _, peer := range ps.remotePeers {
		newSlice = append(newSlice, peer)
	}
	ps.peerCache = newSlice
}

// this method should be called inside ps.mutex
func (ps *peerManager) deletePeer(ID peer.ID) {
	delete(ps.remotePeers, ID)

	// TODO need tuning?
	newSlice := make([]*RemotePeer, 0, len(ps.remotePeers))
	for _, peer := range ps.remotePeers {
		newSlice = append(newSlice, peer)
	}
	ps.peerCache = newSlice

}
