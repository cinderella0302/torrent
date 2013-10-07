package torrent

import (
	"bitbucket.org/anacrolix/go.torrent/peer_protocol"
	"bufio"
	"container/list"
	"crypto"
	"crypto/rand"
	"encoding"
	"errors"
	"fmt"
	metainfo "github.com/nsf/libtorgo/torrent"
	"io"
	"launchpad.net/gommap"
	"log"
	"net"
	"os"
	"path/filepath"
)

const (
	PieceHash   = crypto.SHA1
	maxRequests = 10
	chunkSize   = 0x4000 // 16KiB
	BEP20       = "-GT0000-"
)

type InfoHash [20]byte

type pieceSum [20]byte

func copyHashSum(dst, src []byte) {
	if len(dst) != len(src) || copy(dst, src) != len(dst) {
		panic("hash sum sizes differ")
	}
}

func BytesInfoHash(b []byte) (ih InfoHash) {
	if len(b) != len(ih) || copy(ih[:], b) != len(ih) {
		panic("bad infohash bytes")
	}
	return
}

type pieceState uint8

const (
	pieceStateUnknown = iota
	pieceStateComplete
	pieceStateIncomplete
)

type piece struct {
	State             pieceState
	Hash              pieceSum
	PendingChunkSpecs map[chunkSpec]struct{}
}

type chunkSpec struct {
	Begin, Length peer_protocol.Integer
}

type request struct {
	Index peer_protocol.Integer
	chunkSpec
}

type connection struct {
	Socket net.Conn
	post   chan encoding.BinaryMarshaler
	write  chan []byte

	Interested bool
	Choked     bool
	Requests   map[request]struct{}

	PeerId         [20]byte
	PeerInterested bool
	PeerChoked     bool
	PeerRequests   map[request]struct{}
	PeerExtensions [8]byte
	PeerPieces     []bool
}

func (c *connection) PeerHasPiece(index int) bool {
	if c.PeerPieces == nil {
		return false
	}
	return c.PeerPieces[index]
}

func (c *connection) Post(msg encoding.BinaryMarshaler) {
	c.post <- msg
}

func (c *connection) Request(chunk request) bool {
	if len(c.Requests) >= maxRequests {
		return false
	}
	if _, ok := c.Requests[chunk]; !ok {
		c.Post(peer_protocol.Message{
			Type:   peer_protocol.Request,
			Index:  chunk.Index,
			Begin:  chunk.Begin,
			Length: chunk.Length,
		})
	}
	if c.Requests == nil {
		c.Requests = make(map[request]struct{}, maxRequests)
	}
	c.Requests[chunk] = struct{}{}
	return true
}

func (c *connection) SetInterested(interested bool) {
	if c.Interested == interested {
		return
	}
	c.Post(peer_protocol.Message{
		Type: func() peer_protocol.MessageType {
			if interested {
				return peer_protocol.Interested
			} else {
				return peer_protocol.NotInterested
			}
		}(),
	})
	c.Interested = interested
}

func (conn *connection) writer() {
	for {
		b := <-conn.write
		n, err := conn.Socket.Write(b)
		if err != nil {
			log.Print(err)
			close(conn.write)
			break
		}
		if n != len(b) {
			panic("didn't write all bytes")
		}
		log.Printf("wrote %#v", string(b))
	}
}

func (conn *connection) writeOptimizer() {
	pending := list.New()
	var nextWrite []byte
	for {
		write := conn.write
		if pending.Len() == 0 {
			write = nil
		} else {
			var err error
			nextWrite, err = pending.Front().Value.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				panic(err)
			}
		}
		select {
		case msg := <-conn.post:
			pending.PushBack(msg)
		case write <- nextWrite:
			pending.Remove(pending.Front())
		}
	}
}

type Torrent struct {
	InfoHash InfoHash
	Pieces   []piece
	Data     MMapSpan
	MetaInfo *metainfo.MetaInfo
	Conns    []*connection
	Peers    []Peer
}

func (t *Torrent) WriteChunk(piece int, begin int64, data []byte) (err error) {
	_, err = t.Data.WriteAt(data, int64(piece)*t.MetaInfo.PieceLength+begin)
	return
}

func (t *Torrent) bitfield() (bf []bool) {
	for _, p := range t.Pieces {
		bf = append(bf, p.State == pieceStateComplete)
	}
	return
}

func (t *Torrent) pieceChunkSpecs(index int) (cs map[chunkSpec]struct{}) {
	cs = make(map[chunkSpec]struct{}, (t.MetaInfo.PieceLength+chunkSize-1)/chunkSize)
	c := chunkSpec{
		Begin: 0,
	}
	for left := peer_protocol.Integer(t.PieceSize(index)); left > 0; left -= c.Length {
		c.Length = left
		if c.Length > chunkSize {
			c.Length = chunkSize
		}
		cs[c] = struct{}{}
		c.Begin += c.Length
	}
	return
}

func (t *Torrent) requestHeat() (ret map[request]int) {
	ret = make(map[request]int)
	for _, conn := range t.Conns {
		for req, _ := range conn.Requests {
			ret[req]++
		}
	}
	return
}

type Peer struct {
	Id   [20]byte
	IP   net.IP
	Port int
}

func (t *Torrent) PieceSize(piece int) (size int64) {
	if piece == len(t.Pieces)-1 {
		size = t.Data.Size() % t.MetaInfo.PieceLength
	}
	if size == 0 {
		size = t.MetaInfo.PieceLength
	}
	return
}

func (t *Torrent) HashPiece(piece int) (ps pieceSum) {
	hash := PieceHash.New()
	n, err := t.Data.WriteSectionTo(hash, int64(piece)*t.MetaInfo.PieceLength, t.MetaInfo.PieceLength)
	if err != nil {
		panic(err)
	}
	if n != t.PieceSize(piece) {
		panic(fmt.Sprintf("hashed wrong number of bytes: expected %d; did %d; piece %d", t.PieceSize(piece), n, piece))
	}
	copyHashSum(ps[:], hash.Sum(nil))
	return
}

type Client struct {
	DataDir       string
	HalfOpenLimit int
	PeerId        [20]byte

	halfOpen int
	torrents map[InfoHash]*Torrent

	noTorrents      chan struct{}
	addTorrent      chan *Torrent
	torrentFinished chan InfoHash
	actorTask       chan func()
}

func NewClient(dataDir string) *Client {
	c := &Client{
		DataDir:       dataDir,
		HalfOpenLimit: 10,

		torrents: make(map[InfoHash]*Torrent),

		noTorrents:      make(chan struct{}),
		addTorrent:      make(chan *Torrent),
		torrentFinished: make(chan InfoHash),
		actorTask:       make(chan func()),
	}
	o := copy(c.PeerId[:], BEP20)
	_, err := rand.Read(c.PeerId[o:])
	if err != nil {
		panic("error generating peer id")
	}
	go c.run()
	return c
}

func mmapTorrentData(metaInfo *metainfo.MetaInfo, location string) (mms MMapSpan, err error) {
	defer func() {
		if err != nil {
			mms.Close()
			mms = nil
		}
	}()
	for _, miFile := range metaInfo.Files {
		fileName := filepath.Join(append([]string{location, metaInfo.Name}, miFile.Path...)...)
		err = os.MkdirAll(filepath.Dir(fileName), 0777)
		if err != nil {
			return
		}
		var file *os.File
		file, err = os.OpenFile(fileName, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			return
		}
		func() {
			defer file.Close()
			var fi os.FileInfo
			fi, err = file.Stat()
			if err != nil {
				return
			}
			if fi.Size() < miFile.Length {
				err = file.Truncate(miFile.Length)
				if err != nil {
					return
				}
			}
			var mMap gommap.MMap
			mMap, err = gommap.MapRegion(file.Fd(), 0, miFile.Length, gommap.PROT_READ|gommap.PROT_WRITE, gommap.MAP_SHARED)
			if err != nil {
				return
			}
			if int64(len(mMap)) != miFile.Length {
				panic("mmap has wrong length")
			}
			mms = append(mms, MMap{mMap})
		}()
		if err != nil {
			return
		}
	}
	return
}

func (me *Client) torrent(ih InfoHash) *Torrent {
	for _, t := range me.torrents {
		if t.InfoHash == ih {
			return t
		}
	}
	return nil
}

func (me *Client) initiateConn(peer Peer, torrent *Torrent) {
	if peer.Id == me.PeerId {
		return
	}
	me.halfOpen++
	go func() {
		conn, err := net.DialTCP("tcp", nil, &net.TCPAddr{
			IP:   peer.IP,
			Port: peer.Port,
		})
		me.withContext(func() {
			me.halfOpen--
			me.openNewConns()
		})
		if err != nil {
			log.Printf("error connecting to peer: %s", err)
			return
		}
		log.Printf("connected to %s", conn.RemoteAddr())
		me.handshake(conn, torrent, peer.Id)
	}()
}

func (me *Torrent) haveAnyPieces() bool {
	for _, piece := range me.Pieces {
		if piece.State == pieceStateComplete {
			return true
		}
	}
	return false
}

func (me *Client) handshake(sock net.Conn, torrent *Torrent, peerId [20]byte) {
	conn := &connection{
		Socket:     sock,
		Choked:     true,
		PeerChoked: true,
		write:      make(chan []byte),
		post:       make(chan encoding.BinaryMarshaler),
	}
	go conn.writer()
	go conn.writeOptimizer()
	conn.post <- peer_protocol.Bytes(peer_protocol.Protocol)
	conn.post <- peer_protocol.Bytes("\x00\x00\x00\x00\x00\x00\x00\x00")
	if torrent != nil {
		conn.post <- peer_protocol.Bytes(torrent.InfoHash[:])
		conn.post <- peer_protocol.Bytes(me.PeerId[:])
	}
	var b [28]byte
	_, err := io.ReadFull(conn.Socket, b[:])
	if err != nil {
		log.Fatal(err)
	}
	if string(b[:20]) != peer_protocol.Protocol {
		log.Printf("wrong protocol: %#v", string(b[:20]))
		return
	}
	if 8 != copy(conn.PeerExtensions[:], b[20:]) {
		panic("wtf")
	}
	log.Printf("peer extensions: %#v", string(conn.PeerExtensions[:]))
	var infoHash [20]byte
	_, err = io.ReadFull(conn.Socket, infoHash[:])
	if err != nil {
		return
	}
	_, err = io.ReadFull(conn.Socket, conn.PeerId[:])
	if err != nil {
		return
	}
	if torrent == nil {
		torrent = me.torrent(infoHash)
		if torrent == nil {
			return
		}
		conn.post <- peer_protocol.Bytes(torrent.InfoHash[:])
		conn.post <- peer_protocol.Bytes(me.PeerId[:])
	}
	me.withContext(func() {
		me.addConnection(torrent, conn)
		if torrent.haveAnyPieces() {
			conn.Post(peer_protocol.Message{
				Type:     peer_protocol.Bitfield,
				Bitfield: torrent.bitfield(),
			})
		}
		go func() {
			defer me.withContext(func() {
				me.dropConnection(torrent, conn)
			})
			err := me.runConnection(torrent, conn)
			if err != nil {
				log.Print(err)
			}
		}()
	})
}

func (me *Client) peerGotPiece(torrent *Torrent, conn *connection, piece int) {
	if conn.PeerPieces == nil {
		conn.PeerPieces = make([]bool, len(torrent.Pieces))
	}
	conn.PeerPieces[piece] = true
	if torrent.wantPiece(piece) {
		conn.SetInterested(true)
		me.replenishConnRequests(torrent, conn)
	}
}

func (t *Torrent) wantPiece(index int) bool {
	return t.Pieces[index].State == pieceStateIncomplete
}

func (me *Client) peerUnchoked(torrent *Torrent, conn *connection) {
	me.replenishConnRequests(torrent, conn)
}

func (me *Client) runConnection(torrent *Torrent, conn *connection) error {
	decoder := peer_protocol.Decoder{
		R:         bufio.NewReader(conn.Socket),
		MaxLength: 256 * 1024,
	}
	for {
		msg := new(peer_protocol.Message)
		err := decoder.Decode(msg)
		if err != nil {
			return err
		}
		if msg.Keepalive {
			continue
		}
		go me.withContext(func() {
			// log.Print(msg)
			var err error
			switch msg.Type {
			case peer_protocol.Choke:
				conn.PeerChoked = true
			case peer_protocol.Unchoke:
				conn.PeerChoked = false
				me.peerUnchoked(torrent, conn)
			case peer_protocol.Interested:
				conn.PeerInterested = true
			case peer_protocol.NotInterested:
				conn.PeerInterested = false
			case peer_protocol.Have:
				me.peerGotPiece(torrent, conn, int(msg.Index))
			case peer_protocol.Request:
				conn.PeerRequests[request{
					Index:     msg.Index,
					chunkSpec: chunkSpec{msg.Begin, msg.Length},
				}] = struct{}{}
			case peer_protocol.Bitfield:
				if len(msg.Bitfield) < len(torrent.Pieces) {
					err = errors.New("received invalid bitfield")
					break
				}
				if conn.PeerPieces != nil {
					err = errors.New("received unexpected bitfield")
					break
				}
				conn.PeerPieces = msg.Bitfield[:len(torrent.Pieces)]
				for index, has := range conn.PeerPieces {
					if has {
						me.peerGotPiece(torrent, conn, index)
					}
				}
			case peer_protocol.Piece:
				request_ := request{msg.Index, chunkSpec{msg.Begin, peer_protocol.Integer(len(msg.Piece))}}
				if _, ok := conn.Requests[request_]; !ok {
					err = errors.New("unexpected piece")
					break
				}
				delete(conn.Requests, request_)
				if _, ok := torrent.Pieces[request_.Index].PendingChunkSpecs[request_.chunkSpec]; !ok {
					log.Printf("got unnecessary chunk: %s", request_)
					break
				}
				err = torrent.WriteChunk(int(msg.Index), int64(msg.Begin), msg.Piece)
				if err != nil {
					break
				}
				delete(torrent.Pieces[request_.Index].PendingChunkSpecs, request_.chunkSpec)
				if len(torrent.Pieces[request_.Index].PendingChunkSpecs) == 0 {
					torrent.Pieces[request_.Index].State = pieceStateUnknown
					go me.verifyPiece(torrent, int(request_.Index))
				}
			default:
				log.Printf("received unknown message type: %#v", msg.Type)
			}
			if err != nil {
				log.Print(err)
				me.dropConnection(torrent, conn)
				return
			}
			me.replenishConnRequests(torrent, conn)
		})
	}
}

func (me *Client) dropConnection(torrent *Torrent, conn *connection) {
	conn.Socket.Close()
	for i0, c := range torrent.Conns {
		if c != conn {
			continue
		}
		i1 := len(torrent.Conns) - 1
		if i0 != i1 {
			torrent.Conns[i0] = torrent.Conns[i1]
		}
		torrent.Conns = torrent.Conns[:i1]
		return
	}
	panic("no such connection")
}

func (me *Client) addConnection(t *Torrent, c *connection) bool {
	for _, c := range t.Conns {
		if c.PeerId == c.PeerId {
			return false
		}
	}
	t.Conns = append(t.Conns, c)
	return true
}

func (me *Client) openNewConns() {
	for _, t := range me.torrents {
		for len(t.Peers) != 0 {
			if me.halfOpen >= me.HalfOpenLimit {
				return
			}
			p := t.Peers[0]
			t.Peers = t.Peers[1:]
			me.initiateConn(p, t)
		}
	}
}

func (me *Client) AddPeers(infoHash InfoHash, peers []Peer) (err error) {
	me.withContext(func() {
		t := me.torrent(infoHash)
		if t == nil {
			err = errors.New("no such torrent")
			return
		}
		t.Peers = append(t.Peers, peers...)
		me.openNewConns()
	})
	return
}

func (me *Client) AddTorrent(metaInfo *metainfo.MetaInfo) error {
	torrent := &Torrent{
		InfoHash: BytesInfoHash(metaInfo.InfoHash),
	}
	for offset := 0; offset < len(metaInfo.Pieces); offset += PieceHash.Size() {
		hash := metaInfo.Pieces[offset : offset+PieceHash.Size()]
		if len(hash) != PieceHash.Size() {
			return errors.New("bad piece hash in metainfo")
		}
		piece := piece{}
		copyHashSum(piece.Hash[:], hash)
		torrent.Pieces = append(torrent.Pieces, piece)
	}
	var err error
	torrent.Data, err = mmapTorrentData(metaInfo, me.DataDir)
	if err != nil {
		return err
	}
	torrent.MetaInfo = metaInfo
	me.addTorrent <- torrent
	return nil
}

func (me *Client) WaitAll() {
	<-me.noTorrents
}

func (me *Client) Close() {
}

func (me *Client) withContext(f func()) {
	me.actorTask <- f
}

func (me *Client) replenishConnRequests(torrent *Torrent, conn *connection) {
	if len(conn.Requests) >= maxRequests {
		return
	}
	if conn.PeerChoked {
		return
	}
	requestHeatMap := torrent.requestHeat()
	for index, has := range conn.PeerPieces {
		if !has {
			continue
		}
		for chunkSpec, _ := range torrent.Pieces[index].PendingChunkSpecs {
			request := request{peer_protocol.Integer(index), chunkSpec}
			if heat := requestHeatMap[request]; heat > 0 {
				continue
			}
			conn.SetInterested(true)
			if !conn.Request(request) {
				return
			}
		}
	}
	//conn.SetInterested(false)

}

func (me *Client) pieceHashed(ih InfoHash, piece int, correct bool) {
	torrent := me.torrents[ih]
	newState := func() pieceState {
		if correct {
			return pieceStateComplete
		} else {
			return pieceStateIncomplete
		}
	}()
	oldState := torrent.Pieces[piece].State
	if newState == oldState {
		return
	}
	torrent.Pieces[piece].State = newState
	if newState == pieceStateIncomplete {
		torrent.Pieces[piece].PendingChunkSpecs = torrent.pieceChunkSpecs(piece)
	}
	for _, conn := range torrent.Conns {
		if correct {
			conn.Post(peer_protocol.Message{
				Type:  peer_protocol.Have,
				Index: peer_protocol.Integer(piece),
			})
		} else {
			if conn.PeerHasPiece(piece) {
				me.replenishConnRequests(torrent, conn)
			}
		}
	}
}

func (me *Client) verifyPiece(torrent *Torrent, index int) {
	sum := torrent.HashPiece(index)
	me.withContext(func() {
		me.pieceHashed(torrent.InfoHash, index, sum == torrent.Pieces[index].Hash)
	})
}

func (me *Client) run() {
	for {
		noTorrents := me.noTorrents
		if len(me.torrents) != 0 {
			noTorrents = nil
		}
		select {
		case noTorrents <- struct{}{}:
		case torrent := <-me.addTorrent:
			if _, ok := me.torrents[torrent.InfoHash]; ok {
				break
			}
			me.torrents[torrent.InfoHash] = torrent
			go func() {
				for index := range torrent.Pieces {
					me.verifyPiece(torrent, index)
				}
			}()
		case infoHash := <-me.torrentFinished:
			delete(me.torrents, infoHash)
		case task := <-me.actorTask:
			task()
		}
	}
}

func (me *Client) Torrents() (ret []*Torrent) {
	done := make(chan struct{})
	me.withContext(func() {
		for _, t := range me.torrents {
			ret = append(ret, t)
		}
		close(done)
	})
	<-done
	return
}
