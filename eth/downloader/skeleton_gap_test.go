package downloader

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
)

// TestSkeletonFinalizedRecoversAfterHeadGap covers a consensus client that
// restarts from a fresh checkpoint above the skeleton head (Prysm with
// --force-clear-db after a peerless hour): blocks between the old head and the
// checkpoint never reach the engine API, and F must still advance.
func TestSkeletonFinalizedRecoversAfterHeadGap(t *testing.T) {
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < 1200; i++ {
		chain = append(chain, &types.Header{ParentHash: chain[i-1].Hash(), Number: big.NewInt(int64(i))})
	}
	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), 0, types.Receipts{})

	peer := newSkeletonTestPeer("peer", chain)
	peerset := newPeerSet()
	peerset.Register(newPeerConnection(peer.id, eth.ETH68, peer, log.New("id", peer.id)))
	skeleton := newSkeleton(db, peerset, func(string) {}, newHookedBackfiller())
	defer skeleton.Terminate()

	progress := func() skeletonProgress {
		var p skeletonProgress
		json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &p)
		return p
	}
	waitFor := func(what string, cond func(skeletonProgress) bool) {
		t.Helper()
		for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
			if cond(progress()) {
				return
			}
		}
		p := progress()
		var fin uint64
		if p.Finalized != nil {
			fin = *p.Finalized
		}
		t.Fatalf("%s: head %d tail %d subchains %d finalized %d", what, p.Subchains[0].Head, p.Subchains[0].Tail, len(p.Subchains), fin)
	}
	finalAt := func(p skeletonProgress, n uint64) bool { return p.Finalized != nil && *p.Finalized >= n }

	skeleton.Sync(chain[100], nil, true)
	waitFor("initial link", func(p skeletonProgress) bool { return len(p.Subchains) == 1 && p.Subchains[0].Tail == 1 })
	for i := 101; i <= 120; i++ {
		skeleton.Sync(chain[i], nil, false)
	}
	skeleton.Sync(chain[120], chain[110], true)
	waitFor("contiguous finality", func(p skeletonProgress) bool { return finalAt(p, 110) })

	// Blocks 121..999 are never announced; the client resumes at 1000.
	for i := 1000; i < 1100; i++ {
		var final *types.Header
		if i%10 == 0 {
			final = chain[i-50]
		}
		skeleton.Sync(chain[i], final, final != nil)
	}
	waitFor("finality after gap", func(p skeletonProgress) bool {
		return finalAt(p, 1000) && len(p.Subchains) == 1 && p.Subchains[0].Tail == 1
	})
}
