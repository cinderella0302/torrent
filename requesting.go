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
		Availability: t.piece(i).availability,
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
		ml = ml.Uint64(
			rightPeer.actualRequestState.Requests.GetCardinality(),
			leftPeer.actualRequestState.Requests.GetCardinality(),
		)
	}
	ml = ml.CmpInt64(t.lastRequested[rightRequest].Sub(t.lastRequested[leftRequest]).Nanoseconds())
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
		int(leftPiece.availability),
		int(rightPiece.availability))
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
	input := p.t.getRequestStrategyInput()
	requestHeap := peerRequests{
		peer: p,
	}
	request_strategy.GetRequestablePieces(
		input,
		p.t.cl.pieceRequestOrder[p.t.storage.Capacity],
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
				// if p.t.pendingRequests.Get(r) != 0 && !p.actualRequestState.Requests.Contains(r) {
				//	return
				// }
				if !allowedFast {
					// We must signal interest to request this. TODO: We could set interested if the
					// peers pieces (minus the allowed fast set) overlap with our missing pieces if
					// there are any readers, or any pending pieces.
					desired.Interested = true
					// We can make or will allow sustaining a request here if we're not choked, or
					// have made the request previously (presumably while unchoked), and haven't had
					// the peer respond yet (and the request was retained because we are using the
					// fast extension).
					if p.peerChoking && !p.actualRequestState.Requests.Contains(r) {
						// We can't request this right now.
						return
					}
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
	current := &p.actualRequestState
	if !p.setInterested(next.Interested) {
		return false
	}
	more := true
	requestHeap := &next.Requests
	t := p.t
	heap.Init(requestHeap)
	for requestHeap.Len() != 0 && maxRequests(current.Requests.GetCardinality()) < p.nominalMaxRequests() {
		req := heap.Pop(requestHeap).(RequestIndex)
		if p.cancelledRequests.Contains(req) {
			// Waiting for a reject or piece message, which will suitably trigger us to update our
			// requests, so we can skip this one with no additional consideration.
			continue
		}
		existing := t.requestingPeer(req)
		if existing != nil && existing != p && existing.uncancelledRequests() > current.Requests.GetCardinality() {
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
