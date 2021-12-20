package torrent

import (
	"container/heap"
	"context"
	"encoding/gob"
	"reflect"
	"runtime/pprof"
	"time"
	"unsafe"

	"github.com/anacrolix/log"
	"github.com/anacrolix/multiless"

	request_strategy "github.com/anacrolix/torrent/request-strategy"
)

func (t *Torrent) requestStrategyPieceOrderState(i int) request_strategy.PieceRequestOrderState {
	return request_strategy.PieceRequestOrderState{
		Priority:     t.piece(i).purePriority(),
		Partial:      t.piecePartiallyDownloaded(i),
		Availability: t.piece(i).availability(),
	}
}

func init() {
	gob.Register(peerId{})
}

type peerId struct {
	*Peer
	ptr uintptr
}

func (p peerId) Uintptr() uintptr {
	return p.ptr
}

func (p peerId) GobEncode() (b []byte, _ error) {
	*(*reflect.SliceHeader)(unsafe.Pointer(&b)) = reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(&p.ptr)),
		Len:  int(unsafe.Sizeof(p.ptr)),
		Cap:  int(unsafe.Sizeof(p.ptr)),
	}
	return
}

func (p *peerId) GobDecode(b []byte) error {
	if uintptr(len(b)) != unsafe.Sizeof(p.ptr) {
		panic(len(b))
	}
	ptr := unsafe.Pointer(&b[0])
	p.ptr = *(*uintptr)(ptr)
	log.Printf("%p", ptr)
	dst := reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(&p.Peer)),
		Len:  int(unsafe.Sizeof(p.Peer)),
		Cap:  int(unsafe.Sizeof(p.Peer)),
	}
	copy(*(*[]byte)(unsafe.Pointer(&dst)), b)
	return nil
}

type (
	RequestIndex   = request_strategy.RequestIndex
	chunkIndexType = request_strategy.ChunkIndex
)

type peerRequests struct {
	requestIndexes []RequestIndex
	peer           *Peer
}

func (p *peerRequests) Len() int {
	return len(p.requestIndexes)
}

func (p *peerRequests) Less(i, j int) bool {
	leftRequest := p.requestIndexes[i]
	rightRequest := p.requestIndexes[j]
	t := p.peer.t
	leftPieceIndex := leftRequest / t.chunksPerRegularPiece()
	rightPieceIndex := rightRequest / t.chunksPerRegularPiece()
	ml := multiless.New()
	// Push requests that can't be served right now to the end. But we don't throw them away unless
	// there's a better alternative. This is for when we're using the fast extension and get choked
	// but our requests could still be good when we get unchoked.
	if p.peer.peerChoking {
		ml = ml.Bool(
			!p.peer.peerAllowedFast.Contains(leftPieceIndex),
			!p.peer.peerAllowedFast.Contains(rightPieceIndex),
		)
	}
	leftPeer := t.pendingRequests[leftRequest]
	rightPeer := t.pendingRequests[rightRequest]
	ml = ml.Bool(rightPeer == p.peer, leftPeer == p.peer)
	ml = ml.Bool(rightPeer == nil, leftPeer == nil)
	if ml.Ok() {
		return ml.MustLess()
	}
	if leftPeer != nil {
		// The right peer should also be set, or we'd have resolved the computation by now.
		ml = ml.Uint64(
			rightPeer.requestState.Requests.GetCardinality(),
			leftPeer.requestState.Requests.GetCardinality(),
		)
		// Could either of the lastRequested be Zero? That's what checking an existing peer is for.
		leftLast := t.lastRequested[leftRequest]
		rightLast := t.lastRequested[rightRequest]
		if leftLast.IsZero() || rightLast.IsZero() {
			panic("expected non-zero last requested times")
		}
		// We want the most-recently requested on the left. Clients like Transmission serve requests
		// in received order, so the most recently-requested is the one that has the longest until
		// it will be served and therefore is the best candidate to cancel.
		ml = ml.CmpInt64(rightLast.Sub(leftLast).Nanoseconds())
	}
	leftPiece := t.piece(int(leftPieceIndex))
	rightPiece := t.piece(int(rightPieceIndex))
	ml = ml.Int(
		// Technically we would be happy with the cached priority here, except we don't actually
		// cache it anymore, and Torrent.piecePriority just does another lookup of *Piece to resolve
		// the priority through Piece.purePriority, which is probably slower.
		-int(leftPiece.purePriority()),
		-int(rightPiece.purePriority()),
	)
	ml = ml.Int(
		int(leftPiece.relativeAvailability),
		int(rightPiece.relativeAvailability))
	return ml.Less()
}

func (p *peerRequests) Swap(i, j int) {
	p.requestIndexes[i], p.requestIndexes[j] = p.requestIndexes[j], p.requestIndexes[i]
}

func (p *peerRequests) Push(x interface{}) {
	p.requestIndexes = append(p.requestIndexes, x.(RequestIndex))
}

func (p *peerRequests) Pop() interface{} {
	last := len(p.requestIndexes) - 1
	x := p.requestIndexes[last]
	p.requestIndexes = p.requestIndexes[:last]
	return x
}

type desiredRequestState struct {
	Requests   peerRequests
	Interested bool
}

func (p *Peer) getDesiredRequestState() (desired desiredRequestState) {
	if !p.t.haveInfo() {
		return
	}
	if p.t.closed.IsSet() {
		return
	}
	input := p.t.getRequestStrategyInput()
	requestHeap := peerRequests{
		peer: p,
	}
	request_strategy.GetRequestablePieces(
		input,
		p.t.getPieceRequestOrder(),
		func(ih InfoHash, pieceIndex int) {
			if ih != p.t.infoHash {
				return
			}
			if !p.peerHasPiece(pieceIndex) {
				return
			}
			allowedFast := p.peerAllowedFast.ContainsInt(pieceIndex)
			p.t.piece(pieceIndex).undirtiedChunksIter.Iter(func(ci request_strategy.ChunkIndex) {
				r := p.t.pieceRequestIndexOffset(pieceIndex) + ci
				if !allowedFast {
					// We must signal interest to request this. TODO: We could set interested if the
					// peers pieces (minus the allowed fast set) overlap with our missing pieces if
					// there are any readers, or any pending pieces.
					desired.Interested = true
					// We can make or will allow sustaining a request here if we're not choked, or
					// have made the request previously (presumably while unchoked), and haven't had
					// the peer respond yet (and the request was retained because we are using the
					// fast extension).
					if p.peerChoking && !p.requestState.Requests.Contains(r) {
						// We can't request this right now.
						return
					}
				}
				if p.requestState.Cancelled.Contains(r) {
					// Can't re-request while awaiting acknowledgement.
					return
				}
				requestHeap.requestIndexes = append(requestHeap.requestIndexes, r)
			})
		},
	)
	p.t.assertPendingRequests()
	desired.Requests = requestHeap
	return
}

func (p *Peer) maybeUpdateActualRequestState() bool {
	if p.needRequestUpdate == "" {
		return true
	}
	var more bool
	pprof.Do(
		context.Background(),
		pprof.Labels("update request", p.needRequestUpdate),
		func(_ context.Context) {
			next := p.getDesiredRequestState()
			more = p.applyRequestState(next)
		},
	)
	return more
}

// Transmit/action the request state to the peer.
func (p *Peer) applyRequestState(next desiredRequestState) bool {
	current := &p.requestState
	if !p.setInterested(next.Interested) {
		return false
	}
	more := true
	requestHeap := &next.Requests
	t := p.t
	heap.Init(requestHeap)
	for requestHeap.Len() != 0 && maxRequests(current.Requests.GetCardinality()) < p.nominalMaxRequests() {
		req := heap.Pop(requestHeap).(RequestIndex)
		existing := t.requestingPeer(req)
		if existing != nil && existing != p {
			// Don't steal from the poor.
			diff := int64(current.Requests.GetCardinality()) + 1 - (int64(existing.uncancelledRequests()) - 1)
			// Steal a request that leaves us with one more request than the existing peer
			// connection if the stealer more recently received a chunk.
			if diff > 1 || (diff == 1 && p.lastUsefulChunkReceived.Before(existing.lastUsefulChunkReceived)) {
				continue
			}
			t.cancelRequest(req)
		}
		more = p.mustRequest(req)
		if !more {
			break
		}
	}
	// TODO: This may need to change, we might want to update even if there were no requests due to
	// filtering them for being recently requested already.
	p.updateRequestsTimer.Stop()
	if more {
		p.needRequestUpdate = ""
		if current.Interested {
			p.updateRequestsTimer.Reset(3 * time.Second)
		}
	}
	return more
}
