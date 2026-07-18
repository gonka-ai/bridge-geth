// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package downloader

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
)

func TestReceiptSyncOriginFromCursor(t *testing.T) {
	tests := []struct {
		name       string
		confirmed  uint64
		tail       uint64
		wantOrigin uint64
		wantErr    bool
	}{
		{name: "case B next block is C+1", confirmed: 90, tail: 80, wantOrigin: 90},
		{name: "case A cursor above finality keeps C+1", confirmed: 110, tail: 80, wantOrigin: 110},
		{name: "retained boundary permits tail minus one", confirmed: 79, tail: 80, wantOrigin: 79},
		{name: "cursor below retained range fails for retry", confirmed: 70, tail: 80, wantErr: true},
		{name: "genesis tail does not underflow", confirmed: 0, tail: 0, wantOrigin: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := receiptSyncOriginFromCursor(tt.confirmed, tt.tail)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("deep cursor gap did not fail closed: have origin %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantOrigin {
				t.Fatalf("origin mismatch: have %d, want %d", got, tt.wantOrigin)
			}
		})
	}
}

func TestBeaconHeaderTarget(t *testing.T) {
	head := &types.Header{Number: big.NewInt(120)}
	final := &types.Header{Number: big.NewInt(100)}

	if got, err := beaconHeaderTarget(ethconfig.FullSync, head, nil); err != nil || got != 120 {
		t.Fatalf("full sync target mismatch: have %d, err %v", got, err)
	}
	if got, err := beaconHeaderTarget(ethconfig.ReceiptSync, head, final); err != nil || got != 100 {
		t.Fatalf("receipt sync target mismatch: have %d, err %v", got, err)
	}
	if _, err := beaconHeaderTarget(ethconfig.ReceiptSync, head, nil); err == nil {
		t.Fatal("receipt sync without finalized watermark did not fail closed")
	}
}

func TestEnsureBridgeReceiptResultFinalized(t *testing.T) {
	const (
		tailNumber  = uint64(9)
		finalNumber = uint64(10)
		headNumber  = uint64(12)
	)
	db := rawdb.NewMemoryDatabase()
	tail := &types.Header{Number: new(big.Int).SetUint64(tailNumber)}
	final := &types.Header{Number: new(big.Int).SetUint64(finalNumber), Extra: []byte("canonical")}
	head := &types.Header{Number: new(big.Int).SetUint64(headNumber)}
	for _, header := range []*types.Header{tail, final, head} {
		rawdb.WriteSkeletonHeader(db, header)
	}
	progress := &skeletonProgress{
		Subchains: []*subchain{{Head: headNumber, Tail: tailNumber}},
		Finalized: uint64ptr(finalNumber),
	}
	status, err := json.Marshal(progress)
	if err != nil {
		t.Fatal(err)
	}
	rawdb.WriteSkeletonSyncStatus(db, status)
	d := &Downloader{skeleton: &skeleton{db: db}}

	if err := d.ensureBridgeReceiptResultFinalized(&fetchResult{Header: final}); err != nil {
		t.Fatalf("canonical finalized result rejected: %v", err)
	}
	aboveFinal := &types.Header{Number: new(big.Int).SetUint64(finalNumber + 1)}
	if err := d.ensureBridgeReceiptResultFinalized(&fetchResult{Header: aboveFinal}); err == nil || !strings.Contains(err.Error(), "above finalized") {
		t.Fatalf("above-final result did not fail closed: %v", err)
	}
	mismatch := &types.Header{Number: new(big.Int).SetUint64(finalNumber), Extra: []byte("different")}
	if err := d.ensureBridgeReceiptResultFinalized(&fetchResult{Header: mismatch}); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("non-canonical finalized result accepted: %v", err)
	}
}

func TestSaveSyncStatusPreservesMonotonicFinalized(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	persisted := &skeletonProgress{
		Subchains: []*subchain{{Head: 120, Tail: 80}},
		Finalized: uint64ptr(100),
	}
	status, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	rawdb.WriteSkeletonSyncStatus(db, status)

	s := &skeleton{
		db: db,
		progress: &skeletonProgress{
			Subchains: []*subchain{{Head: 120, Tail: 80}},
			Finalized: uint64ptr(90),
		},
	}
	batch := db.NewBatch()
	s.saveSyncStatus(batch)
	if err := batch.Write(); err != nil {
		t.Fatal(err)
	}
	var got skeletonProgress
	if err := json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &got); err != nil {
		t.Fatal(err)
	}
	if got.Finalized == nil || *got.Finalized != 100 {
		t.Fatalf("finalized watermark regressed: have %v, want 100", got.Finalized)
	}
}

func TestBoundsReturnsRetainedFinalizedBelowTail(t *testing.T) {
	const (
		finalNumber = uint64(100)
		tailNumber  = uint64(110)
		headNumber  = uint64(120)
	)
	db := rawdb.NewMemoryDatabase()
	final := &types.Header{Number: new(big.Int).SetUint64(finalNumber)}
	tail := &types.Header{Number: new(big.Int).SetUint64(tailNumber)}
	head := &types.Header{Number: new(big.Int).SetUint64(headNumber)}
	for _, header := range []*types.Header{final, tail, head} {
		rawdb.WriteSkeletonHeader(db, header)
	}
	progress := &skeletonProgress{
		Subchains: []*subchain{{Head: headNumber, Tail: tailNumber}},
		Finalized: uint64ptr(finalNumber),
	}
	status, err := json.Marshal(progress)
	if err != nil {
		t.Fatal(err)
	}
	rawdb.WriteSkeletonSyncStatus(db, status)

	_, _, gotFinal, err := (&skeleton{db: db}).Bounds()
	if err != nil {
		t.Fatal(err)
	}
	if gotFinal == nil || gotFinal.Number.Uint64() != finalNumber {
		t.Fatalf("retained finalized header missing below tail: have %v, want %d", gotFinal, finalNumber)
	}
}

func uint64ptr(value uint64) *uint64 {
	return &value
}
