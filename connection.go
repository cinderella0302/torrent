package torrent

import (
	"bufio"
	"bytes"
	"container/list"
	"encoding"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/anacrolix/missinggo"
	"github.com/anacrolix/missinggo/bitmap"
	"github.com/anacrolix/missinggo/prioritybitmap"
	"github.com/bradfitz/iter"

	"github.com/anacrolix/torrent/bencode"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

var optimizedCancels = expvar.NewInt("optimizedCancels")

type peerSource byte

const (
	peerSourceTracker  = '\x00' // It's the default.
	peerSourceIncoming = 'I'
	peerSourceDHT      = 'H'
	peerSourcePEX      = 'X'
)

// Maintains the state of a connection with a peer.
type connection struct {
	t         *Torrent
	conn      net.Conn
	rw        io.ReadWriter // The real slim shady
	encrypted bool
	Discovery peerSource
	uTP       bool
	closed    missinggo.Event
	post      chan pp.Message
	writeCh   chan []byte

	UnwantedChunksReceived int
	UsefulChunksReceived   int
	chunksSent             int

	lastMessageReceived     time.Time
	completedHandshake      time.Time
	lastUsefulChunkReceived time.Time
	lastChunkSent           time.Time

	// Stuff controlled by the local peer.
	Interested       bool
	Choked           bool
	Requests         map[request]struct{}
	requestsLowWater int
	// Indexed by metadata piece, set to true if posted and pending a
	// response.
	metadataRequests []bool
	sentHaves        []bool

	// Stuff controlled by the remote peer.
	PeerID             [20]byte
	PeerInterested     bool
	PeerChoked         bool
	PeerRequests       map[request]struct{}
	PeerExtensionBytes peerExtensionBytes
	// The pieces the peer has claimed to have.
	peerPieces bitmap.Bitmap
	// The peer has everything. This can occur due to a special message, when
	// we may not even know the number of pieces in the torrent yet.
	peerHasAll bool
	// The highest possible number of pieces the torrent could have based on
	// communication with the peer. Generally only useful until we have the
	// torrent info.
	peerMinPieces int
	// Pieces we've accepted chunks for from the peer.
	peerTouchedPieces map[int]struct{}

	PeerMaxRequests  int // Maximum pending requests the peer allows.
	PeerExtensionIDs map[string]byte
	PeerClientName   string

	pieceInclination  []int
	pieceRequestOrder prioritybitmap.PriorityBitmap
}

func newConnection() (c *connection) {
	c = &connection{
		Choked:          true,
		PeerChoked:      true,
		PeerMaxRequests: 250,

		writeCh: make(chan []byte),
		post:    make(chan pp.Message),
	}
	return
}

func (cn *connection) remoteAddr() net.Addr {
	return cn.conn.RemoteAddr()
}

func (cn *connection) localAddr() net.Addr {
	return cn.conn.LocalAddr()
}

func (cn *connection) supportsExtension(ext string) bool {
	_, ok := cn.PeerExtensionIDs[ext]
	return ok
}

// The best guess at number of pieces in the torrent for this peer.
func (cn *connection) bestPeerNumPieces() int {
	if cn.t.haveInfo() {
		return cn.t.numPieces()
	}
	return cn.peerMinPieces
}

func (cn *connection) completedString() string {
	return fmt.Sprintf("%d/%d", cn.peerPieces.Len(), cn.bestPeerNumPieces())
}

// Correct the PeerPieces slice length. Return false if the existing slice is
// invalid, such as by receiving badly sized BITFIELD, or invalid HAVE
// messages.
func (cn *connection) setNumPieces(num int) error {
	cn.peerPieces.RemoveRange(num, -1)
	cn.peerPiecesChanged()
	return nil
}

func eventAgeString(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%.2fs ago", time.Now().Sub(t).Seconds())
}

func (cn *connection) connectionFlags() (ret string) {
	c := func(b byte) {
		ret += string([]byte{b})
	}
	if cn.encrypted {
		c('E')
	}
	if cn.Discovery != 0 {
		c(byte(cn.Discovery))
	}
	if cn.uTP {
		c('T')
	}
	return
}

// Inspired by https://trac.transmissionbt.com/wiki/PeerStatusText
func (cn *connection) statusFlags() (ret string) {
	c := func(b byte) {
		ret += string([]byte{b})
	}
	if cn.Interested {
		c('i')
	}
	if cn.Choked {
		c('c')
	}
	c('-')
	ret += cn.connectionFlags()
	c('-')
	if cn.PeerInterested {
		c('i')
	}
	if cn.PeerChoked {
		c('c')
	}
	return
}

func (cn *connection) String() string {
	var buf bytes.Buffer
	cn.WriteStatus(&buf, nil)
	return buf.String()
}

func (cn *connection) WriteStatus(w io.Writer, t *Torrent) {
	// \t isn't preserved in <pre> blocks?
	fmt.Fprintf(w, "%+q: %s-%s\n", cn.PeerID, cn.localAddr(), cn.remoteAddr())
	fmt.Fprintf(w, "    last msg: %s, connected: %s, last useful chunk: %s\n",
		eventAgeString(cn.lastMessageReceived),
		eventAgeString(cn.completedHandshake),
		eventAgeString(cn.lastUsefulChunkReceived))
	fmt.Fprintf(w,
		"    %s completed, %d pieces touched, good chunks: %d/%d-%d reqq: %d-%d, flags: %s\n",
		cn.completedString(),
		len(cn.peerTouchedPieces),
		cn.UsefulChunksReceived,
		cn.UnwantedChunksReceived+cn.UsefulChunksReceived,
		cn.chunksSent,
		len(cn.Requests),
		len(cn.PeerRequests),
		cn.statusFlags(),
	)
}

func (cn *connection) Close() {
	cn.closed.Set()
	cn.discardPieceInclination()
	cn.pieceRequestOrder.Clear()
	// TODO: This call blocks sometimes, why?
	go cn.conn.Close()
}

func (cn *connection) PeerHasPiece(piece int) bool {
	return cn.peerHasAll || cn.peerPieces.Contains(piece)
}

func (cn *connection) Post(msg pp.Message) {
	select {
	case cn.post <- msg:
		postedMessageTypes.Add(strconv.FormatInt(int64(msg.Type), 10), 1)
	case <-cn.closed.C():
	}
}

func (cn *connection) RequestPending(r request) bool {
	_, ok := cn.Requests[r]
	return ok
}

func (cn *connection) requestMetadataPiece(index int) {
	eID := cn.PeerExtensionIDs["ut_metadata"]
	if eID == 0 {
		return
	}
	if index < len(cn.metadataRequests) && cn.metadataRequests[index] {
		return
	}
	cn.Post(pp.Message{
		Type:       pp.Extended,
		ExtendedID: eID,
		ExtendedPayload: func() []byte {
			b, err := bencode.Marshal(map[string]int{
				"msg_type": pp.RequestMetadataExtensionMsgType,
				"piece":    index,
			})
			if err != nil {
				panic(err)
			}
			return b
		}(),
	})
	for index >= len(cn.metadataRequests) {
		cn.metadataRequests = append(cn.metadataRequests, false)
	}
	cn.metadataRequests[index] = true
}

func (cn *connection) requestedMetadataPiece(index int) bool {
	return index < len(cn.metadataRequests) && cn.metadataRequests[index]
}

// The actual value to use as the maximum outbound requests.
func (cn *connection) nominalMaxRequests() (ret int) {
	ret = cn.PeerMaxRequests
	if ret > 64 {
		ret = 64
	}
	return
}

// Returns true if more requests can be sent.
func (cn *connection) Request(chunk request) bool {
	if len(cn.Requests) >= cn.nominalMaxRequests() {
		return false
	}
	if !cn.PeerHasPiece(int(chunk.Index)) {
		return true
	}
	if cn.RequestPending(chunk) {
		return true
	}
	cn.SetInterested(true)
	if cn.PeerChoked {
		return false
	}
	if cn.Requests == nil {
		cn.Requests = make(map[request]struct{}, cn.PeerMaxRequests)
	}
	cn.Requests[chunk] = struct{}{}
	cn.requestsLowWater = len(cn.Requests) / 2
	cn.Post(pp.Message{
		Type:   pp.Request,
		Index:  chunk.Index,
		Begin:  chunk.Begin,
		Length: chunk.Length,
	})
	return true
}

// Returns true if an unsatisfied request was canceled.
func (cn *connection) Cancel(r request) bool {
	if cn.Requests == nil {
		return false
	}
	if _, ok := cn.Requests[r]; !ok {
		return false
	}
	delete(cn.Requests, r)
	cn.Post(pp.Message{
		Type:   pp.Cancel,
		Index:  r.Index,
		Begin:  r.Begin,
		Length: r.Length,
	})
	return true
}

// Returns true if an unsatisfied request was canceled.
func (cn *connection) PeerCancel(r request) bool {
	if cn.PeerRequests == nil {
		return false
	}
	if _, ok := cn.PeerRequests[r]; !ok {
		return false
	}
	delete(cn.PeerRequests, r)
	return true
}

func (cn *connection) Choke() {
	if cn.Choked {
		return
	}
	cn.Post(pp.Message{
		Type: pp.Choke,
	})
	cn.PeerRequests = nil
	cn.Choked = true
}

func (cn *connection) Unchoke() {
	if !cn.Choked {
		return
	}
	cn.Post(pp.Message{
		Type: pp.Unchoke,
	})
	cn.Choked = false
}

func (cn *connection) SetInterested(interested bool) {
	if cn.Interested == interested {
		return
	}
	cn.Post(pp.Message{
		Type: func() pp.MessageType {
			if interested {
				return pp.Interested
			} else {
				return pp.NotInterested
			}
		}(),
	})
	cn.Interested = interested
}

var (
	// Track connection writer buffer writes and flushes, to determine its
	// efficiency.
	connectionWriterFlush = expvar.NewInt("connectionWriterFlush")
	connectionWriterWrite = expvar.NewInt("connectionWriterWrite")
)

// Writes buffers to the socket from the write channel.
func (cn *connection) writer() {
	defer func() {
		cn.t.cl.mu.Lock()
		defer cn.t.cl.mu.Unlock()
		cn.Close()
	}()
	// Reduce write syscalls.
	buf := bufio.NewWriter(cn.rw)
	for {
		if buf.Buffered() == 0 {
			// There's nothing to write, so block until we get something.
			select {
			case b, ok := <-cn.writeCh:
				if !ok {
					return
				}
				connectionWriterWrite.Add(1)
				_, err := buf.Write(b)
				if err != nil {
					return
				}
			case <-cn.closed.C():
				return
			}
		} else {
			// We already have something to write, so flush if there's nothing
			// more to write.
			select {
			case b, ok := <-cn.writeCh:
				if !ok {
					return
				}
				connectionWriterWrite.Add(1)
				_, err := buf.Write(b)
				if err != nil {
					return
				}
			case <-cn.closed.C():
				return
			default:
				connectionWriterFlush.Add(1)
				err := buf.Flush()
				if err != nil {
					return
				}
			}
		}
	}
}

func (cn *connection) writeOptimizer(keepAliveDelay time.Duration) {
	defer close(cn.writeCh) // Responsible for notifying downstream routines.
	pending := list.New()   // Message queue.
	var nextWrite []byte    // Set to nil if we need to need to marshal the next message.
	timer := time.NewTimer(keepAliveDelay)
	defer timer.Stop()
	lastWrite := time.Now()
	for {
		write := cn.writeCh // Set to nil if there's nothing to write.
		if pending.Len() == 0 {
			write = nil
		} else if nextWrite == nil {
			var err error
			nextWrite, err = pending.Front().Value.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				panic(err)
			}
		}
	event:
		select {
		case <-timer.C:
			if pending.Len() != 0 {
				break
			}
			keepAliveTime := lastWrite.Add(keepAliveDelay)
			if time.Now().Before(keepAliveTime) {
				timer.Reset(keepAliveTime.Sub(time.Now()))
				break
			}
			pending.PushBack(pp.Message{Keepalive: true})
			postedKeepalives.Add(1)
		case msg, ok := <-cn.post:
			if !ok {
				return
			}
			if msg.Type == pp.Cancel {
				for e := pending.Back(); e != nil; e = e.Prev() {
					elemMsg := e.Value.(pp.Message)
					if elemMsg.Type == pp.Request && msg.Index == elemMsg.Index && msg.Begin == elemMsg.Begin && msg.Length == elemMsg.Length {
						pending.Remove(e)
						optimizedCancels.Add(1)
						break event
					}
				}
			}
			pending.PushBack(msg)
		case write <- nextWrite:
			pending.Remove(pending.Front())
			nextWrite = nil
			lastWrite = time.Now()
			if pending.Len() == 0 {
				timer.Reset(keepAliveDelay)
			}
		case <-cn.closed.C():
			return
		}
	}
}

func (cn *connection) Have(piece int) {
	for piece >= len(cn.sentHaves) {
		cn.sentHaves = append(cn.sentHaves, false)
	}
	if cn.sentHaves[piece] {
		return
	}
	cn.Post(pp.Message{
		Type:  pp.Have,
		Index: pp.Integer(piece),
	})
	cn.sentHaves[piece] = true
}

func (cn *connection) Bitfield(haves []bool) {
	if cn.sentHaves != nil {
		panic("bitfield must be first have-related message sent")
	}
	cn.Post(pp.Message{
		Type:     pp.Bitfield,
		Bitfield: haves,
	})
	cn.sentHaves = haves
}

func (cn *connection) updateRequests() {
	if !cn.t.haveInfo() {
		return
	}
	if cn.Interested {
		if cn.PeerChoked {
			return
		}
		if len(cn.Requests) > cn.requestsLowWater {
			return
		}
	}
	cn.fillRequests()
	if len(cn.Requests) == 0 && !cn.PeerChoked {
		// So we're not choked, but we don't want anything right now. We may
		// have completed readahead, and the readahead window has not rolled
		// over to the next piece. Better to stay interested in case we're
		// going to want data in the near future.
		cn.SetInterested(!cn.t.haveAllPieces())
	}
}

func (cn *connection) fillRequests() {
	cn.pieceRequestOrder.IterTyped(func(piece int) (more bool) {
		if cn.t.cl.config.Debug && cn.t.havePiece(piece) {
			panic(piece)
		}
		return cn.requestPiecePendingChunks(piece)
	})
}

func (cn *connection) requestPiecePendingChunks(piece int) (again bool) {
	return cn.t.connRequestPiecePendingChunks(cn, piece)
}

func (cn *connection) stopRequestingPiece(piece int) {
	cn.pieceRequestOrder.Remove(piece)
}

func (cn *connection) updatePiecePriority(piece int) {
	tpp := cn.t.piecePriority(piece)
	if !cn.PeerHasPiece(piece) {
		tpp = PiecePriorityNone
	}
	if tpp == PiecePriorityNone {
		cn.stopRequestingPiece(piece)
		return
	}
	prio := cn.getPieceInclination()[piece]
	switch tpp {
	case PiecePriorityNormal:
	case PiecePriorityReadahead:
		prio -= cn.t.numPieces()
	case PiecePriorityNext, PiecePriorityNow:
		prio -= 2 * cn.t.numPieces()
	default:
		panic(tpp)
	}
	prio += piece
	cn.pieceRequestOrder.Set(piece, prio)
	cn.updateRequests()
}

func (cn *connection) getPieceInclination() []int {
	if cn.pieceInclination == nil {
		cn.pieceInclination = cn.t.getConnPieceInclination()
	}
	return cn.pieceInclination
}

func (cn *connection) discardPieceInclination() {
	if cn.pieceInclination == nil {
		return
	}
	cn.t.putPieceInclination(cn.pieceInclination)
	cn.pieceInclination = nil
}

func (cn *connection) peerHasPieceChanged(piece int) {
	cn.updatePiecePriority(piece)
}

func (cn *connection) peerPiecesChanged() {
	if cn.t.haveInfo() {
		for i := range iter.N(cn.t.numPieces()) {
			cn.peerHasPieceChanged(i)
		}
	}
}

func (cn *connection) raisePeerMinPieces(newMin int) {
	if newMin > cn.peerMinPieces {
		cn.peerMinPieces = newMin
	}
}

func (cn *connection) peerSentHave(piece int) error {
	if cn.t.haveInfo() && piece >= cn.t.numPieces() {
		return errors.New("invalid piece")
	}
	if cn.PeerHasPiece(piece) {
		return nil
	}
	cn.raisePeerMinPieces(piece + 1)
	cn.peerPieces.Set(piece, true)
	cn.peerHasPieceChanged(piece)
	return nil
}

func (cn *connection) peerSentBitfield(bf []bool) error {
	cn.peerHasAll = false
	if len(bf)%8 != 0 {
		panic("expected bitfield length divisible by 8")
	}
	// We know that the last byte means that at most the last 7 bits are
	// wasted.
	cn.raisePeerMinPieces(len(bf) - 7)
	if cn.t.haveInfo() && len(bf) > cn.t.numPieces() {
		// Ignore known excess pieces.
		bf = bf[:cn.t.numPieces()]
	}
	for i, have := range bf {
		if have {
			cn.raisePeerMinPieces(i + 1)
		}
		cn.peerPieces.Set(i, have)
	}
	cn.peerPiecesChanged()
	return nil
}

func (cn *connection) peerSentHaveAll() error {
	cn.peerHasAll = true
	cn.peerPieces.Clear()
	cn.peerPiecesChanged()
	return nil
}

func (cn *connection) peerSentHaveNone() error {
	cn.peerPieces.Clear()
	cn.peerHasAll = false
	cn.peerPiecesChanged()
	return nil
}
