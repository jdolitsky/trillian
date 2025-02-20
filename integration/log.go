// Copyright 2016 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/golang/glog"
	"github.com/google/trillian"
	"github.com/google/trillian/client/backoff"
	"github.com/google/trillian/types"
	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"
	inmemory "github.com/transparency-dev/merkle/testonly"
)

// TestParameters bundles up all the settings for a test run
type TestParameters struct {
	TreeID              int64
	CheckLogEmpty       bool
	QueueLeaves         bool
	AwaitSequencing     bool
	StartLeaf           int64
	LeafCount           int64
	UniqueLeaves        int64
	QueueBatchSize      int
	SequencerBatchSize  int
	ReadBatchSize       int64
	SequencingWaitTotal time.Duration
	SequencingPollWait  time.Duration
	RPCRequestDeadline  time.Duration
	CustomLeafPrefix    string
}

// DefaultTestParameters builds a TestParameters object for a normal
// test of the given log.
func DefaultTestParameters(treeID int64) TestParameters {
	return TestParameters{
		TreeID:              treeID,
		CheckLogEmpty:       true,
		QueueLeaves:         true,
		AwaitSequencing:     true,
		StartLeaf:           0,
		LeafCount:           1000,
		UniqueLeaves:        1000,
		QueueBatchSize:      50,
		SequencerBatchSize:  100,
		ReadBatchSize:       50,
		SequencingWaitTotal: 10 * time.Second * 60,
		SequencingPollWait:  time.Second * 5,
		RPCRequestDeadline:  time.Second * 30,
		CustomLeafPrefix:    "",
	}
}

type consistencyProofParams struct {
	size1 int64
	size2 int64
}

// inclusionProofTestIndices are the 0 based leaf indices to probe inclusion proofs at.
var inclusionProofTestIndices = []int64{5, 27, 31, 80, 91}

// consistencyProofTestParams are the intervals
// to test proofs at
var consistencyProofTestParams = []consistencyProofParams{{1, 2}, {2, 3}, {1, 3}, {2, 4}}

// consistencyProofBadTestParams are the intervals to probe for consistency proofs, none of
// these should succeed. Zero is not a valid tree size, nor is -1. 10000000 is outside the
// range we'll reasonably queue (multiple of batch size).
var consistencyProofBadTestParams = []consistencyProofParams{{0, 0}, {-1, 0}, {10000000, 10000000}}

// RunLogIntegration runs a log integration test using the given client and test
// parameters.
func RunLogIntegration(client trillian.TrillianLogClient, params TestParameters) error {
	// Step 1 - Optionally check log starts empty then optionally queue leaves on server
	if params.CheckLogEmpty {
		glog.Infof("Checking log is empty before starting test")
		resp, err := getLatestSignedLogRoot(client, params)
		if err != nil {
			return fmt.Errorf("failed to get latest log root: %v %v", resp, err)
		}

		var root types.LogRootV1
		if err := root.UnmarshalBinary(resp.SignedLogRoot.GetLogRoot()); err != nil {
			return fmt.Errorf("could not read current log root: %v", err)
		}

		if root.TreeSize > 0 {
			return fmt.Errorf("expected an empty log but got tree head response: %v", resp)
		}
	}

	preEntries := genEntries(params)
	if params.QueueLeaves {
		glog.Infof("Queueing %d leaves to log server ...", params.LeafCount)
		if err := queueLeaves(client, params, preEntries); err != nil {
			return fmt.Errorf("failed to queue leaves: %v", err)
		}
	}

	// Step 2 - Wait for queue to drain when server sequences, give up if it doesn't happen (optional)
	if params.AwaitSequencing {
		glog.Infof("Waiting for log to sequence ...")
		if err := waitForSequencing(params.TreeID, client, params); err != nil {
			return fmt.Errorf("leaves were not sequenced: %v", err)
		}
	}

	// Step 3 - Use get entries to read back what was written, check leaves are correct
	glog.Infof("Reading back leaves from log ...")
	entries, err := readEntries(params.TreeID, client, params)
	if err != nil {
		return fmt.Errorf("could not read back log entries: %v", err)
	}
	if err := verifyEntries(preEntries, entries); err != nil {
		return fmt.Errorf("written and read entries mismatch: %v", err)
	}

	// Step 4 - Cross validation between log and memory tree root hashes
	glog.Infof("Checking log STH with our constructed in-memory tree ...")
	tree := buildMerkleTree(entries, params)
	if err := checkLogRootHashMatches(tree, client, params); err != nil {
		return fmt.Errorf("log consistency check failed: %v", err)
	}

	// Now that the basic tree has passed validation we can start testing proofs

	// Step 5 - Test some inclusion proofs
	glog.Info("Testing inclusion proofs")

	// Ensure log doesn't serve a proof for a leaf index outside the tree size
	if err := checkInclusionProofLeafOutOfRange(params.TreeID, client, params); err != nil {
		return fmt.Errorf("log served out of range proof (index): %v", err)
	}

	// Ensure that log doesn't serve a proof for a valid index at a size outside the tree
	if err := checkInclusionProofTreeSizeOutOfRange(params.TreeID, client, params); err != nil {
		return fmt.Errorf("log served out of range proof (tree size): %v", err)
	}

	// Probe the log at several leaf indices each with a range of tree sizes
	for _, testIndex := range inclusionProofTestIndices {
		if err := checkInclusionProofsAtIndex(testIndex, params.TreeID, tree, client, params); err != nil {
			return fmt.Errorf("log inclusion index: %d proof checks failed: %v", testIndex, err)
		}
	}

	// TODO(al): test some inclusion proofs by Merkle hash too.

	// Step 6 - Test some consistency proofs
	glog.Info("Testing consistency proofs")

	// Make some consistency proof requests that we know should not succeed
	for _, consistParams := range consistencyProofBadTestParams {
		if err := checkConsistencyProof(consistParams, params.TreeID, tree, client, params, int64(params.QueueBatchSize)); err == nil {
			return fmt.Errorf("log consistency for %v: unexpected proof returned", consistParams)
		}
	}

	// Probe the log between some tree sizes we know are included and check the results against
	// the in memory tree. Request proofs at both STH and non STH sizes unless batch size is one,
	// when these would be equivalent requests.
	for _, consistParams := range consistencyProofTestParams {
		if err := checkConsistencyProof(consistParams, params.TreeID, tree, client, params, int64(params.QueueBatchSize)); err != nil {
			return fmt.Errorf("log consistency for %v: proof checks failed: %v", consistParams, err)
		}

		// Only do this if the batch size changes when halved
		if params.QueueBatchSize > 1 {
			if err := checkConsistencyProof(consistParams, params.TreeID, tree, client, params, int64(params.QueueBatchSize/2)); err != nil {
				return fmt.Errorf("log consistency for %v: proof checks failed (Non STH size): %v", consistParams, err)
			}
		}
	}
	return nil
}

func genEntries(params TestParameters) []*trillian.LogLeaf {
	if params.UniqueLeaves == 0 {
		params.UniqueLeaves = params.LeafCount
	}

	uniqueLeaves := make([]*trillian.LogLeaf, 0, params.UniqueLeaves)
	for i := int64(0); i < params.UniqueLeaves; i++ {
		index := params.StartLeaf + i
		leaf := &trillian.LogLeaf{
			LeafValue: []byte(fmt.Sprintf("%sLeaf %d", params.CustomLeafPrefix, index)),
			ExtraData: []byte(fmt.Sprintf("%sExtra %d", params.CustomLeafPrefix, index)),
		}
		uniqueLeaves = append(uniqueLeaves, leaf)
	}

	// Shuffle the leaves to see if that breaks things, but record the rand seed
	// so we can reproduce failures.
	seed := time.Now().UnixNano()
	rand.Seed(seed)
	perm := rand.Perm(int(params.LeafCount))
	glog.Infof("Generating %d leaves, %d unique, using permutation seed %d", params.LeafCount, params.UniqueLeaves, seed)

	leaves := make([]*trillian.LogLeaf, 0, params.LeafCount)
	for l := int64(0); l < params.LeafCount; l++ {
		leaves = append(leaves, uniqueLeaves[int64(perm[l])%params.UniqueLeaves])
	}
	return leaves
}

func queueLeaves(client trillian.TrillianLogClient, params TestParameters, entries []*trillian.LogLeaf) error {
	glog.Infof("Queueing %d leaves...", len(entries))

	for _, leaf := range entries {
		ctx, cancel := getRPCDeadlineContext(params)
		b := &backoff.Backoff{
			Min:    100 * time.Millisecond,
			Max:    10 * time.Second,
			Factor: 2,
			Jitter: true,
		}
		err := b.Retry(ctx, func() error {
			_, err := client.QueueLeaf(ctx, &trillian.QueueLeafRequest{
				LogId: params.TreeID,
				Leaf:  leaf,
			})
			return err
		})
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func waitForSequencing(treeID int64, client trillian.TrillianLogClient, params TestParameters) error {
	endTime := time.Now().Add(params.SequencingWaitTotal)

	glog.Infof("Waiting for sequencing until: %v", endTime)

	for endTime.After(time.Now()) {
		req := trillian.GetLatestSignedLogRootRequest{LogId: treeID}
		ctx, cancel := getRPCDeadlineContext(params)
		resp, err := client.GetLatestSignedLogRoot(ctx, &req)
		cancel()

		if err != nil {
			return err
		}

		var root types.LogRootV1
		if err := root.UnmarshalBinary(resp.SignedLogRoot.GetLogRoot()); err != nil {
			return err
		}

		glog.Infof("Leaf count: %d", root.TreeSize)

		if root.TreeSize >= uint64(params.LeafCount+params.StartLeaf) {
			return nil
		}

		glog.Infof("Leaves sequenced: %d. Still waiting ...", root.TreeSize)

		time.Sleep(params.SequencingPollWait)
	}

	return errors.New("wait time expired")
}

func readEntries(logID int64, client trillian.TrillianLogClient, params TestParameters) ([]*trillian.LogLeaf, error) {
	if start := params.StartLeaf; start != 0 {
		return nil, fmt.Errorf("non-zero StartLeaf is not supported: %d", start)
	}

	leaves := make([]*trillian.LogLeaf, 0, params.LeafCount)
	for index, end := params.StartLeaf, params.StartLeaf+params.LeafCount; index < end; {
		count := end - index
		if max := params.ReadBatchSize; count > max {
			count = max
		}

		glog.Infof("Reading %d leaves from %d ...", count, index)
		req := &trillian.GetLeavesByRangeRequest{LogId: logID, StartIndex: index, Count: count}
		ctx, cancel := getRPCDeadlineContext(params)
		response, err := client.GetLeavesByRange(ctx, req)
		cancel()
		if err != nil {
			return nil, err
		}

		// Check we got the right number of leaves.
		if got, want := int64(len(response.Leaves)), count; got != want {
			return nil, fmt.Errorf("expected %d leaves, got %d", want, got)
		}

		leaves = append(leaves, response.Leaves...)
		index += int64(len(response.Leaves))
	}

	return leaves, nil
}

func verifyEntries(written, read []*trillian.LogLeaf) error {
	counts := make(map[string]int, len(written))

	for _, e := range read {
		counts[string(e.LeafValue)]++

		// Check that the MerkleLeafHash field is computed correctly.
		hash := rfc6962.DefaultHasher.HashLeaf(e.LeafValue)
		if got, want := e.MerkleLeafHash, hash; !bytes.Equal(got, want) {
			return fmt.Errorf("leaf %d hash mismatch: got %x want %x", e.LeafIndex, got, want)
		}
		// Ensure that the ExtraData in the leaf made it through the roundtrip.
		// This was set up when we queued the leaves.
		if got, want := e.ExtraData, bytes.Replace(e.LeafValue, []byte("Leaf"), []byte("Extra"), 1); !bytes.Equal(got, want) {
			return fmt.Errorf("leaf %d ExtraData: got %x, want %xv", e.LeafIndex, got, want)
		}
	}

	for _, e := range written {
		counts[string(e.LeafValue)]--
		if counts[string(e.LeafValue)] == 0 {
			delete(counts, string(e.LeafValue))
		}
	}

	if len(counts) != 0 {
		return fmt.Errorf("entry leaf values don't match: diff (-expected +got)\n%v", counts)
	}
	return nil
}

func checkLogRootHashMatches(tree *inmemory.Tree, client trillian.TrillianLogClient, params TestParameters) error {
	// Check the STH against the hash we got from our tree
	resp, err := getLatestSignedLogRoot(client, params)
	if err != nil {
		return err
	}
	var root types.LogRootV1
	if err := root.UnmarshalBinary(resp.SignedLogRoot.GetLogRoot()); err != nil {
		return err
	}

	// Hash must not be empty and must match the one we built ourselves
	if got, want := root.RootHash, tree.Hash(); !bytes.Equal(got, want) {
		return fmt.Errorf("root hash mismatch expected got: %x want: %x", got, want)
	}

	return nil
}

// checkInclusionProofLeafOutOfRange requests an inclusion proof beyond the current tree size. This
// should fail
func checkInclusionProofLeafOutOfRange(logID int64, client trillian.TrillianLogClient, params TestParameters) error {
	// Test is a leaf index bigger than the current tree size
	ctx, cancel := getRPCDeadlineContext(params)
	proof, err := client.GetInclusionProof(ctx, &trillian.GetInclusionProofRequest{
		LogId:     logID,
		LeafIndex: params.LeafCount + 1,
		TreeSize:  int64(params.LeafCount),
	})
	cancel()

	if err == nil {
		return fmt.Errorf("log returned proof for leaf index outside tree: %d v %d: %v", params.LeafCount+1, params.LeafCount, proof)
	}

	return nil
}

// checkInclusionProofTreeSizeOutOfRange requests an inclusion proof for a leaf within the tree size at
// a tree size larger than the current tree size. This should succeed but with an STH for the current
// tree and an empty proof, because it is a result of skew.
func checkInclusionProofTreeSizeOutOfRange(logID int64, client trillian.TrillianLogClient, params TestParameters) error {
	// Test is an in range leaf index for a tree size that doesn't exist
	ctx, cancel := getRPCDeadlineContext(params)
	req := &trillian.GetInclusionProofRequest{
		LogId:     logID,
		LeafIndex: int64(params.SequencerBatchSize),
		TreeSize:  params.LeafCount + int64(params.SequencerBatchSize),
	}
	proof, err := client.GetInclusionProof(ctx, req)
	cancel()
	if err != nil {
		return fmt.Errorf("log returned error for tree size outside tree: %d v %d: %v", params.LeafCount, req.TreeSize, err)
	}

	var root types.LogRootV1
	if err := root.UnmarshalBinary(proof.SignedLogRoot.LogRoot); err != nil {
		return fmt.Errorf("could not read current log root: %v", err)
	}

	if proof.Proof != nil {
		return fmt.Errorf("log returned proof for tree size outside tree: %d v %d: %v", params.LeafCount, req.TreeSize, proof)
	}
	if int64(root.TreeSize) >= req.TreeSize {
		return fmt.Errorf("log returned bad root for tree size outside tree: %d v %d: %v", params.LeafCount, req.TreeSize, proof)
	}

	return nil
}

// checkInclusionProofsAtIndex obtains and checks proofs at tree sizes from zero up to 2 x the sequencing
// batch size (or number of leaves queued if less). The log should only serve proofs for indices in a tree
// at least as big as the index where STHs where the index is a multiple of the sequencer batch size. All
// proofs returned should match ones computed by the alternate Merkle Tree implementation, which differs
// from what the log uses.
func checkInclusionProofsAtIndex(index int64, logID int64, tree *inmemory.Tree, client trillian.TrillianLogClient, params TestParameters) error {
	for treeSize := int64(0); treeSize < min(params.LeafCount, int64(2*params.SequencerBatchSize)); treeSize++ {
		ctx, cancel := getRPCDeadlineContext(params)
		resp, err := client.GetInclusionProof(ctx, &trillian.GetInclusionProofRequest{
			LogId:     logID,
			LeafIndex: index,
			TreeSize:  treeSize,
		})
		cancel()

		// If the index is larger than the tree size we cannot have a valid proof
		shouldHaveProof := index < treeSize
		if got, want := err == nil, shouldHaveProof; got != want {
			return fmt.Errorf("GetInclusionProof(index: %d, treeSize %d): %v, want nil: %v", index, treeSize, err, want)
		}
		if !shouldHaveProof {
			continue
		}

		// Verify inclusion proof.
		root := tree.HashAt(uint64(treeSize))
		merkleLeafHash := tree.LeafHash(uint64(index))
		if err := proof.VerifyInclusion(rfc6962.DefaultHasher, uint64(index), uint64(treeSize), merkleLeafHash, resp.Proof.Hashes, root); err != nil {
			return err
		}
	}

	return nil
}

func checkConsistencyProof(consistParams consistencyProofParams, treeID int64, tree *inmemory.Tree, client trillian.TrillianLogClient, params TestParameters, batchSize int64) error {
	// We expect the proof request to succeed
	ctx, cancel := getRPCDeadlineContext(params)
	req := &trillian.GetConsistencyProofRequest{
		LogId:          treeID,
		FirstTreeSize:  consistParams.size1 * int64(batchSize),
		SecondTreeSize: consistParams.size2 * int64(batchSize),
	}
	resp, err := client.GetConsistencyProof(ctx, req)
	cancel()
	if err != nil {
		return fmt.Errorf("GetConsistencyProof(%v) = %v %v", consistParams, err, resp)
	}

	if resp.SignedLogRoot == nil || resp.SignedLogRoot.LogRoot == nil {
		return fmt.Errorf("received invalid response: %v", resp)
	}
	var root types.LogRootV1
	if err := root.UnmarshalBinary(resp.SignedLogRoot.LogRoot); err != nil {
		return fmt.Errorf("could not read current log root: %v", err)
	}

	if req.SecondTreeSize > int64(root.TreeSize) {
		return fmt.Errorf("requested tree size %d > available tree size %d", req.SecondTreeSize, root.TreeSize)
	}

	root1 := tree.HashAt(uint64(req.FirstTreeSize))
	root2 := tree.HashAt(uint64(req.SecondTreeSize))
	return proof.VerifyConsistency(rfc6962.DefaultHasher, uint64(req.FirstTreeSize), uint64(req.SecondTreeSize),
		resp.Proof.Hashes, root1, root2)
}

// buildMerkleTree returns an in-memory Merkle tree built on the given leaves.
func buildMerkleTree(leaves []*trillian.LogLeaf, params TestParameters) *inmemory.Tree {
	merkleTree := inmemory.New(rfc6962.DefaultHasher)
	for _, leaf := range leaves {
		merkleTree.AppendData(leaf.LeafValue)
	}
	return merkleTree
}

func getLatestSignedLogRoot(client trillian.TrillianLogClient, params TestParameters) (*trillian.GetLatestSignedLogRootResponse, error) {
	req := trillian.GetLatestSignedLogRootRequest{LogId: params.TreeID}
	ctx, cancel := getRPCDeadlineContext(params)
	resp, err := client.GetLatestSignedLogRoot(ctx, &req)
	cancel()

	return resp, err
}

// getRPCDeadlineTime calculates the future time an RPC should expire based on our config
func getRPCDeadlineContext(params TestParameters) (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), time.Now().Add(params.RPCRequestDeadline))
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}

	return b
}
