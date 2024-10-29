package arbtest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/secp256k1"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbos/util"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/broadcastclient"
	"github.com/offchainlabs/nitro/broadcaster/message"
	"github.com/offchainlabs/nitro/cmd/genericconf"
	"github.com/offchainlabs/nitro/execution/gethexec"
	"github.com/offchainlabs/nitro/pubsub"
	"github.com/offchainlabs/nitro/solgen/go/express_lane_auctiongen"
	"github.com/offchainlabs/nitro/solgen/go/mocksgen"
	"github.com/offchainlabs/nitro/timeboost"
	"github.com/offchainlabs/nitro/timeboost/bindings"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/colors"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/redisutil"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	"github.com/stretchr/testify/require"
)

var blockMetadataInputFeedKey = func(pos uint64) []byte {
	var key []byte
	prefix := []byte("t")
	key = append(key, prefix...)
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, pos)
	key = append(key, data...)
	return key
}

func TestTimeboostedFieldInReceiptsObject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder := NewNodeBuilder(ctx).DefaultConfig(t, false)
	builder.execConfig.BlockMetadataApiCacheSize = 0 // Caching is disabled
	cleanup := builder.Build(t)
	defer cleanup()

	// Generate blocks until current block is totalBlocks
	arbDb := builder.L2.ConsensusNode.ArbDB
	blockNum := big.NewInt(2)
	builder.L2Info.GenerateAccount("User")
	user := builder.L2Info.GetDefaultTransactOpts("User", ctx)
	for i := 0; ; i++ {
		builder.L2.TransferBalanceTo(t, "Owner", util.RemapL1Address(user.From), big.NewInt(1e18), builder.L2Info)
		latestL2, err := builder.L2.Client.BlockNumber(ctx)
		Require(t, err)
		// Clean BlockMetadata from arbDB so that we can modify it at will
		Require(t, arbDb.Delete(blockMetadataInputFeedKey(latestL2)))
		if latestL2 >= blockNum.Uint64() {
			break
		}
	}

	block, err := builder.L2.Client.BlockByNumber(ctx, blockNum)
	Require(t, err)
	if len(block.Transactions()) != 2 {
		t.Fatalf("expecting two txs in the second block, but found: %d txs", len(block.Transactions()))
	}

	// Set first tx (internal tx anyway) to not timeboosted and Second one to timeboosted- BlockMetadata (in bits)-> 00000000 00000010
	arbDb.Put(blockMetadataInputFeedKey(blockNum.Uint64()), []byte{0, 2})
	l2rpc := builder.L2.Stack.Attach()
	// Extra timeboosted field in pointer form to check for its existence
	type timeboostedFromReceipt struct {
		Timeboosted *bool `json:"timeboosted"`
	}
	var receiptResult []timeboostedFromReceipt
	err = l2rpc.CallContext(ctx, &receiptResult, "eth_getBlockReceipts", rpc.BlockNumber(blockNum.Int64()))
	Require(t, err)
	if receiptResult[0].Timeboosted != nil {
		t.Fatal("timeboosted field shouldn't exist in the receipt object of first tx")
	}
	if receiptResult[1].Timeboosted == nil {
		t.Fatal("timeboosted field should exist in the receipt object of second tx")
	}
	if *receiptResult[1].Timeboosted != true {
		t.Fatal("second tx was timeboosted, but the field indicates otherwise")
	}

	// Check that timeboosted is accurate for eth_getTransactionReceipt as well
	var txReceipt timeboostedFromReceipt
	err = l2rpc.CallContext(ctx, &txReceipt, "eth_getTransactionReceipt", block.Transactions()[0].Hash())
	Require(t, err)
	if txReceipt.Timeboosted != nil {
		t.Fatal("timeboosted field shouldn't exist in the receipt object of first tx")
	}
	err = l2rpc.CallContext(ctx, &txReceipt, "eth_getTransactionReceipt", block.Transactions()[1].Hash())
	Require(t, err)
	if txReceipt.Timeboosted == nil {
		t.Fatal("timeboosted field should exist in the receipt object of second tx")
	}
	if *txReceipt.Timeboosted != true {
		t.Fatal("second tx was timeboosted, but the field indicates otherwise")
	}

	// Print the receipt object for reference
	var receiptResultRaw json.RawMessage
	err = l2rpc.CallContext(ctx, &receiptResultRaw, "eth_getBlockReceipts", rpc.BlockNumber(blockNum.Int64()))
	Require(t, err)
	colors.PrintGrey("receipt object- ", string(receiptResultRaw))

}

func TestTimeboostBulkBlockMetadataAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder := NewNodeBuilder(ctx).DefaultConfig(t, false)
	builder.execConfig.BlockMetadataApiCacheSize = 0 // Caching is disabled
	cleanup := builder.Build(t)
	defer cleanup()

	arbDb := builder.L2.ConsensusNode.ArbDB

	// Generate blocks until current block is end
	start := 1
	end := 20
	builder.L2Info.GenerateAccount("User")
	user := builder.L2Info.GetDefaultTransactOpts("User", ctx)
	for i := 0; ; i++ {
		builder.L2.TransferBalanceTo(t, "Owner", util.RemapL1Address(user.From), big.NewInt(1e18), builder.L2Info)
		latestL2, err := builder.L2.Client.BlockNumber(ctx)
		Require(t, err)
		// Clean BlockMetadata from arbDB so that we can modify it at will
		Require(t, arbDb.Delete(blockMetadataInputFeedKey(latestL2)))
		if latestL2 > uint64(end)+10 {
			break
		}
	}
	var sampleBulkData []gethexec.NumberAndBlockMetadata
	for i := start; i <= end; i += 2 {
		sampleData := gethexec.NumberAndBlockMetadata{
			BlockNumber: uint64(i),
			RawMetadata: []byte{0, uint8(i)},
		}
		sampleBulkData = append(sampleBulkData, sampleData)
		arbDb.Put(blockMetadataInputFeedKey(sampleData.BlockNumber), sampleData.RawMetadata)
	}

	l2rpc := builder.L2.Stack.Attach()
	var result []gethexec.NumberAndBlockMetadata
	err := l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), "latest") // Test rpc.BlockNumber feature, send "latest" as an arg instead of blockNumber
	Require(t, err)

	if len(result) != len(sampleBulkData) {
		t.Fatalf("number of entries in arb_getRawBlockMetadata is incorrect. Got: %d, Want: %d", len(result), len(sampleBulkData))
	}
	for i, data := range result {
		if data.BlockNumber != sampleBulkData[i].BlockNumber {
			t.Fatalf("BlockNumber mismatch. Got: %d, Want: %d", data.BlockNumber, sampleBulkData[i].BlockNumber)
		}
		if !bytes.Equal(data.RawMetadata, sampleBulkData[i].RawMetadata) {
			t.Fatalf("RawMetadata. Got: %s, Want: %s", data.RawMetadata, sampleBulkData[i].RawMetadata)
		}
	}

	// Test that without cache the result returned is always in sync with ArbDB
	sampleBulkData[0].RawMetadata = []byte{1, 11}
	arbDb.Put(blockMetadataInputFeedKey(1), sampleBulkData[0].RawMetadata)

	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(1), rpc.BlockNumber(1))
	Require(t, err)
	if len(result) != 1 {
		t.Fatal("result returned with more than one entry")
	}
	if !bytes.Equal(sampleBulkData[0].RawMetadata, result[0].RawMetadata) {
		t.Fatal("BlockMetadata gotten from API doesn't match the latest entry in ArbDB")
	}

	// Test that LRU caching works
	builder.execConfig.BlockMetadataApiCacheSize = 10
	builder.execConfig.BlockMetadataApiBlocksLimit = 25
	builder.RestartL2Node(t)
	l2rpc = builder.L2.Stack.Attach()
	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), rpc.BlockNumber(end))
	Require(t, err)

	arbDb = builder.L2.ConsensusNode.ArbDB
	updatedBlockMetadata := []byte{2, 12}
	arbDb.Put(blockMetadataInputFeedKey(1), updatedBlockMetadata)

	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(1), rpc.BlockNumber(1))
	Require(t, err)
	if len(result) != 1 {
		t.Fatal("result returned with more than one entry")
	}
	if bytes.Equal(updatedBlockMetadata, result[0].RawMetadata) {
		t.Fatal("BlockMetadata should've been fetched from cache and not the db")
	}
	if !bytes.Equal(sampleBulkData[0].RawMetadata, result[0].RawMetadata) {
		t.Fatal("incorrect caching of BlockMetadata")
	}

	// Test that ErrBlockMetadataApiBlocksLimitExceeded is thrown when query range exceeds the limit
	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), rpc.BlockNumber(26))
	if !strings.Contains(err.Error(), gethexec.ErrBlockMetadataApiBlocksLimitExceeded.Error()) {
		t.Fatalf("expecting ErrBlockMetadataApiBlocksLimitExceeded error, got: %v", err)
	}

	// A Reorg event should clear the cache, hence the data fetched now should be accurate
	builder.L2.ConsensusNode.TxStreamer.ReorgTo(10)
	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), rpc.BlockNumber(end))
	Require(t, err)
	if !bytes.Equal(updatedBlockMetadata, result[0].RawMetadata) {
		t.Fatal("BlockMetadata should've been fetched from db and not the cache")
	}
}

func TestSequencerFeed_ExpressLaneAuction_ExpressLaneTxsHaveAdvantage_TimeboostedFieldIsCorrect(t *testing.T) {
	t.Parallel()

	// logHandler := testhelpers.InitTestLog(t, log.LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "*")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})
	jwtSecretPath := filepath.Join(tmpDir, "sequencer.jwt")

	seq, seqClient, seqInfo, auctionContractAddr, cleanupSeq, feedListener, cleanupFeedListener := setupExpressLaneAuction(t, tmpDir, ctx, jwtSecretPath)
	defer cleanupSeq()
	defer cleanupFeedListener()

	chainId, err := seqClient.ChainID(ctx)
	Require(t, err)

	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(auctionContractAddr, seqClient)
	Require(t, err)
	info, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	bobPriv := seqInfo.Accounts["Bob"].PrivateKey

	// Prepare a client that can submit txs to the sequencer via the express lane.
	seqDial, err := rpc.Dial(seq.Stack.HTTPEndpoint())
	Require(t, err)
	expressLaneClient := newExpressLaneClient(
		bobPriv,
		chainId,
		time.Unix(int64(info.OffsetTimestamp), 0),
		time.Duration(info.RoundDurationSeconds)*time.Second,
		auctionContractAddr,
		seqDial,
	)
	expressLaneClient.Start(ctx)

	// During the express lane around, Bob sends txs always 150ms later than Alice, but Alice's
	// txs end up getting delayed by 200ms as she is not the express lane controller.
	// In the end, Bob's txs should be ordered before Alice's during the round.
	var wg sync.WaitGroup
	wg.Add(2)
	ownerAddr := seqInfo.GetAddress("Owner")
	aliceData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		GasFeeCap: new(big.Int).Set(seqInfo.GasPrice),
		Value:     big.NewInt(1e12),
		Nonce:     3,
		Data:      nil,
	}
	aliceTx := seqInfo.SignTxAs("Alice", aliceData)
	go func(w *sync.WaitGroup) {
		defer w.Done()
		err = seqClient.SendTransaction(ctx, aliceTx)
		Require(t, err)
	}(&wg)

	bobData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		GasFeeCap: new(big.Int).Set(seqInfo.GasPrice),
		Value:     big.NewInt(1e12),
		Nonce:     3,
		Data:      nil,
	}
	bobBoostableTx := seqInfo.SignTxAs("Bob", bobData)
	go func(w *sync.WaitGroup) {
		defer w.Done()
		time.Sleep(time.Millisecond * 10)
		err = expressLaneClient.SendTransaction(ctx, bobBoostableTx)
		Require(t, err)
	}(&wg)
	wg.Wait()

	// After round is done, verify that Bob beats Alice in the final sequence.
	aliceReceipt, err := seqClient.TransactionReceipt(ctx, aliceTx.Hash())
	Require(t, err)
	aliceBlock := aliceReceipt.BlockNumber.Uint64()
	bobReceipt, err := seqClient.TransactionReceipt(ctx, bobBoostableTx.Hash())
	Require(t, err)
	bobBlock := bobReceipt.BlockNumber.Uint64()

	if aliceBlock < bobBlock {
		t.Fatal("Alice's tx should not have been sequenced before Bob's in different blocks")
	} else if aliceBlock == bobBlock {
		if aliceReceipt.TransactionIndex < bobReceipt.TransactionIndex {
			t.Fatal("Bob should have been sequenced before Alice with express lane")
		}
	}

	// verifyTimeboostedCorrectness is used to check if the timeboosted byte array in both the sequencer's tx streamer and the client node's tx streamer (which is connected
	// to the sequencer feed) is accurate, i.e it represents correctly whether a tx is timeboosted or not
	verifyTimeboostedCorrectness := func(user string, tNode *arbnode.Node, tClient *ethclient.Client, isTimeboosted bool, userTx *types.Transaction, userTxBlockNum uint64) {
		blockMetadataOfBlock, err := tNode.TxStreamer.BlockMetadataAtCount(arbutil.MessageIndex(userTxBlockNum) + 1)
		Require(t, err)
		if len(blockMetadataOfBlock) == 0 {
			t.Fatal("got empty blockMetadata byte array")
		}
		if blockMetadataOfBlock[0] != message.TimeboostedVersion {
			t.Fatalf("blockMetadata byte array has invalid version. Want: %d, Got: %d", message.TimeboostedVersion, blockMetadataOfBlock[0])
		}
		userTxBlock, err := tClient.BlockByNumber(ctx, new(big.Int).SetUint64(userTxBlockNum))
		Require(t, err)
		var foundUserTx bool
		for txIndex, tx := range userTxBlock.Transactions() {
			if tx.Hash() == userTx.Hash() {
				foundUserTx = true
				if !isTimeboosted && blockMetadataOfBlock.IsTxTimeboosted(txIndex) {
					t.Fatalf("incorrect timeboosted bit for %s's tx, it shouldn't be timeboosted", user)
				} else if isTimeboosted && !blockMetadataOfBlock.IsTxTimeboosted(txIndex) {
					t.Fatalf("incorrect timeboosted bit for %s's tx, it should be timeboosted", user)
				}
			} else if blockMetadataOfBlock.IsTxTimeboosted(txIndex) {
				// Other tx's right now shouln't be timeboosted
				t.Fatalf("incorrect timeboosted bit for nonspecified tx with index: %d, it shouldn't be timeboosted", txIndex)
			}
		}
		if !foundUserTx {
			t.Fatalf("%s's tx wasn't found in the block with blockNum retrieved from its receipt", user)
		}
	}

	// First test that timeboosted byte array is correct on sequencer side
	verifyTimeboostedCorrectness("alice", seq, seqClient, false, aliceTx, aliceBlock)
	verifyTimeboostedCorrectness("bob", seq, seqClient, true, bobBoostableTx, bobBlock)

	// Verify that timeboosted byte array receieved via sequencer feed is correct
	_, err = WaitForTx(ctx, feedListener.Client, bobBoostableTx.Hash(), time.Second*5)
	Require(t, err)
	_, err = WaitForTx(ctx, feedListener.Client, aliceTx.Hash(), time.Second*5)
	Require(t, err)
	verifyTimeboostedCorrectness("alice", feedListener.ConsensusNode, feedListener.Client, false, aliceTx, aliceBlock)
	verifyTimeboostedCorrectness("bob", feedListener.ConsensusNode, feedListener.Client, true, bobBoostableTx, bobBlock)

	// arbnode.BlockHashMismatchLogMsg has been randomly appearing and disappearing when running this test, not sure why that might be happening
	// if logHandler.WasLogged(arbnode.BlockHashMismatchLogMsg) {
	// 	t.Fatal("BlockHashMismatchLogMsg was logged unexpectedly")
	// }
}

func TestSequencerFeed_ExpressLaneAuction_InnerPayloadNoncesAreRespected(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "*")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})
	jwtSecretPath := filepath.Join(tmpDir, "sequencer.jwt")
	seq, seqClient, seqInfo, auctionContractAddr, cleanupSeq, _, _ := setupExpressLaneAuction(t, tmpDir, ctx, jwtSecretPath)
	defer cleanupSeq()
	chainId, err := seqClient.ChainID(ctx)
	Require(t, err)

	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(auctionContractAddr, seqClient)
	Require(t, err)
	info, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	bobPriv := seqInfo.Accounts["Bob"].PrivateKey

	// Prepare a client that can submit txs to the sequencer via the express lane.
	seqDial, err := rpc.Dial(seq.Stack.HTTPEndpoint())
	Require(t, err)
	expressLaneClient := newExpressLaneClient(
		bobPriv,
		chainId,
		time.Unix(int64(info.OffsetTimestamp), 0),
		time.Duration(info.RoundDurationSeconds)*time.Second,
		auctionContractAddr,
		seqDial,
	)
	expressLaneClient.Start(ctx)

	// We first generate an account for Charlie and transfer some balance to him.
	seqInfo.GenerateAccount("Charlie")
	TransferBalance(t, "Owner", "Charlie", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)

	// During the express lane, Bob sends txs that do not belong to him, but he is the express lane controller so they
	// will go through the express lane.
	// These tx payloads are sent with nonces out of order, and those with nonces too high should fail.
	var wg sync.WaitGroup
	wg.Add(2)
	ownerAddr := seqInfo.GetAddress("Owner")
	aliceData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		GasFeeCap: new(big.Int).Set(seqInfo.GasPrice),
		Value:     big.NewInt(1e12),
		Nonce:     3,
		Data:      nil,
	}
	aliceTx := seqInfo.SignTxAs("Alice", aliceData)
	go func(w *sync.WaitGroup) {
		defer w.Done()
		err = seqClient.SendTransaction(ctx, aliceTx)
		Require(t, err)
	}(&wg)

	txData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		Value:     big.NewInt(1e12),
		Nonce:     1,
		GasFeeCap: aliceTx.GasFeeCap(),
		Data:      nil,
	}
	charlie1 := seqInfo.SignTxAs("Charlie", txData)
	txData = &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		Value:     big.NewInt(1e12),
		Nonce:     0,
		GasFeeCap: aliceTx.GasFeeCap(),
		Data:      nil,
	}
	charlie0 := seqInfo.SignTxAs("Charlie", txData)
	var err2 error
	go func(w *sync.WaitGroup) {
		defer w.Done()
		time.Sleep(time.Millisecond * 10)
		// Send the express lane txs with nonces out of order
		err2 = expressLaneClient.SendTransaction(ctx, charlie1)
		err = expressLaneClient.SendTransaction(ctx, charlie0)
		Require(t, err)
	}(&wg)
	wg.Wait()
	if err2 == nil {
		t.Fatal("Charlie should not be able to send tx with nonce 1")
	}
	// After round is done, verify that Charlie beats Alice in the final sequence, and that the emitted txs
	// for Charlie are correct.
	aliceReceipt, err := seqClient.TransactionReceipt(ctx, aliceTx.Hash())
	Require(t, err)
	aliceBlock := aliceReceipt.BlockNumber.Uint64()
	charlieReceipt, err := seqClient.TransactionReceipt(ctx, charlie0.Hash())
	Require(t, err)
	charlieBlock := charlieReceipt.BlockNumber.Uint64()

	if aliceBlock < charlieBlock {
		t.Fatal("Alice's tx should not have been sequenced before Charlie's in different blocks")
	} else if aliceBlock == charlieBlock {
		if aliceReceipt.TransactionIndex < charlieReceipt.TransactionIndex {
			t.Fatal("Charlie should have been sequenced before Alice with express lane")
		}
	}
}

func setupExpressLaneAuction(
	t *testing.T,
	dbDirPath string,
	ctx context.Context,
	jwtSecretPath string,
) (*arbnode.Node, *ethclient.Client, *BlockchainTestInfo, common.Address, func(), *TestClient, func()) {

	builderSeq := NewNodeBuilder(ctx).DefaultConfig(t, true)

	seqPort := getRandomPort(t)
	seqAuthPort := getRandomPort(t)
	builderSeq.l2StackConfig.HTTPHost = "localhost"
	builderSeq.l2StackConfig.HTTPPort = seqPort
	builderSeq.l2StackConfig.HTTPModules = []string{"eth", "arb", "debug", "timeboost"}
	builderSeq.l2StackConfig.AuthPort = seqAuthPort
	builderSeq.l2StackConfig.AuthModules = []string{"eth", "arb", "debug", "timeboost", "auctioneer"}
	builderSeq.l2StackConfig.JWTSecret = jwtSecretPath
	builderSeq.nodeConfig.Feed.Output = *newBroadcasterConfigTest()
	builderSeq.execConfig.Sequencer.Enable = true
	builderSeq.execConfig.Sequencer.Timeboost = gethexec.TimeboostConfig{
		Enable:                false, // We need to start without timeboost initially to create the auction contract
		ExpressLaneAdvantage:  time.Second * 5,
		SequencerHTTPEndpoint: fmt.Sprintf("http://localhost:%d", seqPort),
	}
	cleanupSeq := builderSeq.Build(t)
	seqInfo, seqNode, seqClient := builderSeq.L2Info, builderSeq.L2.ConsensusNode, builderSeq.L2.Client

	port := seqNode.BroadcastServer.ListenerAddr().(*net.TCPAddr).Port
	builderFeedListener := NewNodeBuilder(ctx).DefaultConfig(t, true)
	builderFeedListener.isSequencer = false
	builderFeedListener.nodeConfig.Feed.Input = *newBroadcastClientConfigTest(port)
	builderFeedListener.nodeConfig.Feed.Input.Timeout = broadcastclient.DefaultConfig.Timeout
	cleanupFeedListener := builderFeedListener.Build(t)

	// Send an L2 tx in the background every two seconds to keep the chain moving.
	go func() {
		tick := time.NewTicker(time.Second * 2)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				tx := seqInfo.PrepareTx("Owner", "Owner", seqInfo.TransferGas, big.NewInt(1), nil)
				err := seqClient.SendTransaction(ctx, tx)
				t.Log("Failed to send test tx", err)
			}
		}
	}()

	// Set up the auction contracts on L2.
	// Deploy the express lane auction contract and erc20 to the parent chain.
	ownerOpts := seqInfo.GetDefaultTransactOpts("Owner", ctx)
	erc20Addr, tx, erc20, err := bindings.DeployMockERC20(&ownerOpts, seqClient)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Initialize(&ownerOpts, "LANE", "LNE", 18)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	// Fund the auction contract.
	seqInfo.GenerateAccount("AuctionContract")
	TransferBalance(t, "Owner", "AuctionContract", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)

	// Mint some tokens to Alice and Bob.
	seqInfo.GenerateAccount("Alice")
	seqInfo.GenerateAccount("Bob")
	TransferBalance(t, "Faucet", "Alice", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)
	TransferBalance(t, "Faucet", "Bob", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)
	aliceOpts := seqInfo.GetDefaultTransactOpts("Alice", ctx)
	bobOpts := seqInfo.GetDefaultTransactOpts("Bob", ctx)
	tx, err = erc20.Mint(&ownerOpts, aliceOpts.From, big.NewInt(100))
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Mint(&ownerOpts, bobOpts.From, big.NewInt(100))
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	// Calculate the number of seconds until the next minute
	// and the next timestamp that is a multiple of a minute.
	now := time.Now()
	roundDuration := time.Minute
	// Correctly calculate the remaining time until the next minute
	waitTime := roundDuration - time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond())*time.Nanosecond
	// Get the current Unix timestamp at the start of the minute
	initialTimestamp := big.NewInt(now.Add(waitTime).Unix())
	initialTimestampUnix := time.Unix(initialTimestamp.Int64(), 0)

	// Deploy the auction manager contract.
	auctionContractAddr, tx, _, err := express_lane_auctiongen.DeployExpressLaneAuction(&ownerOpts, seqClient)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	proxyAddr, tx, _, err := mocksgen.DeploySimpleProxy(&ownerOpts, seqClient, auctionContractAddr)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(proxyAddr, seqClient)
	Require(t, err)

	auctioneerAddr := seqInfo.GetDefaultTransactOpts("AuctionContract", ctx).From
	beneficiary := auctioneerAddr
	biddingToken := erc20Addr
	bidRoundSeconds := uint64(60)
	auctionClosingSeconds := uint64(15)
	reserveSubmissionSeconds := uint64(15)
	minReservePrice := big.NewInt(1) // 1 wei.
	roleAdmin := auctioneerAddr
	tx, err = auctionContract.Initialize(
		&ownerOpts,
		express_lane_auctiongen.InitArgs{
			Auctioneer:   auctioneerAddr,
			BiddingToken: biddingToken,
			Beneficiary:  beneficiary,
			RoundTimingInfo: express_lane_auctiongen.RoundTimingInfo{
				OffsetTimestamp:          initialTimestamp.Uint64(),
				RoundDurationSeconds:     bidRoundSeconds,
				AuctionClosingSeconds:    auctionClosingSeconds,
				ReserveSubmissionSeconds: reserveSubmissionSeconds,
			},
			MinReservePrice:       minReservePrice,
			AuctioneerAdmin:       roleAdmin,
			MinReservePriceSetter: roleAdmin,
			ReservePriceSetter:    roleAdmin,
			BeneficiarySetter:     roleAdmin,
			RoundTimingSetter:     roleAdmin,
			MasterAdmin:           roleAdmin,
		},
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	t.Log("Deployed all the auction manager stuff", auctionContractAddr)
	// We approve the spending of the erc20 for the autonomous auction contract and bid receiver
	// for both Alice and Bob.
	bidReceiverAddr := common.HexToAddress("0x2424242424242424242424242424242424242424")
	maxUint256 := big.NewInt(1)
	maxUint256.Lsh(maxUint256, 256).Sub(maxUint256, big.NewInt(1))

	tx, err = erc20.Approve(
		&aliceOpts, proxyAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Approve(
		&aliceOpts, bidReceiverAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Approve(
		&bobOpts, proxyAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Approve(
		&bobOpts, bidReceiverAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	// This is hacky- we are manually starting the ExpressLaneService here instead of letting it be started
	// by the sequencer. This is due to needing to deploy the auction contract first.
	builderSeq.execConfig.Sequencer.Timeboost.Enable = true
	builderSeq.L2.ExecNode.Sequencer.StartExpressLane(ctx, proxyAddr, seqInfo.GetAddress("AuctionContract"))
	t.Log("Started express lane service in sequencer")

	// Set up an autonomous auction contract service that runs in the background in this test.
	redisURL := redisutil.CreateTestRedis(ctx, t)

	// Set up the auctioneer RPC service.
	bidValidatorPort := getRandomPort(t)
	bidValidatorWsPort := getRandomPort(t)
	stackConf := node.Config{
		DataDir:             "", // ephemeral.
		HTTPPort:            bidValidatorPort,
		HTTPHost:            "localhost",
		HTTPModules:         []string{timeboost.AuctioneerNamespace},
		HTTPVirtualHosts:    []string{"localhost"},
		HTTPTimeouts:        rpc.DefaultHTTPTimeouts,
		WSHost:              "localhost",
		WSPort:              bidValidatorWsPort,
		WSModules:           []string{timeboost.AuctioneerNamespace},
		GraphQLVirtualHosts: []string{"localhost"},
		P2P: p2p.Config{
			ListenAddr:  "",
			NoDial:      true,
			NoDiscovery: true,
		},
	}
	stack, err := node.New(&stackConf)
	Require(t, err)
	cfg := &timeboost.BidValidatorConfig{
		SequencerEndpoint:      fmt.Sprintf("http://localhost:%d", seqPort),
		AuctionContractAddress: proxyAddr.Hex(),
		RedisURL:               redisURL,
		ProducerConfig:         pubsub.TestProducerConfig,
	}
	fetcher := func() *timeboost.BidValidatorConfig {
		return cfg
	}
	bidValidator, err := timeboost.NewBidValidator(
		ctx, stack, fetcher,
	)
	Require(t, err)
	Require(t, stack.Start())
	Require(t, bidValidator.Initialize(ctx))
	bidValidator.Start(ctx)

	auctioneerCfg := &timeboost.AuctioneerServerConfig{
		SequencerEndpoint:      fmt.Sprintf("http://localhost:%d", seqAuthPort),
		AuctionContractAddress: proxyAddr.Hex(),
		RedisURL:               redisURL,
		ConsumerConfig:         pubsub.TestConsumerConfig,
		SequencerJWTPath:       jwtSecretPath,
		DbDirectory:            dbDirPath,
		Wallet: genericconf.WalletConfig{
			PrivateKey: fmt.Sprintf("00%x", seqInfo.Accounts["AuctionContract"].PrivateKey.D.Bytes()),
		},
	}
	auctioneerFetcher := func() *timeboost.AuctioneerServerConfig {
		return auctioneerCfg
	}
	am, err := timeboost.NewAuctioneerServer(
		ctx,
		auctioneerFetcher,
	)
	Require(t, err)
	am.Start(ctx)

	// Set up a bidder client for Alice and Bob.
	alicePriv := seqInfo.Accounts["Alice"].PrivateKey
	cfgFetcherAlice := func() *timeboost.BidderClientConfig {
		return &timeboost.BidderClientConfig{
			AuctionContractAddress: proxyAddr.Hex(),
			BidValidatorEndpoint:   fmt.Sprintf("http://localhost:%d", bidValidatorPort),
			ArbitrumNodeEndpoint:   fmt.Sprintf("http://localhost:%d", seqPort),
			Wallet: genericconf.WalletConfig{
				PrivateKey: fmt.Sprintf("00%x", alicePriv.D.Bytes()),
			},
		}
	}
	alice, err := timeboost.NewBidderClient(
		ctx,
		cfgFetcherAlice,
	)
	Require(t, err)

	bobPriv := seqInfo.Accounts["Bob"].PrivateKey
	cfgFetcherBob := func() *timeboost.BidderClientConfig {
		return &timeboost.BidderClientConfig{
			AuctionContractAddress: proxyAddr.Hex(),
			BidValidatorEndpoint:   fmt.Sprintf("http://localhost:%d", bidValidatorPort),
			ArbitrumNodeEndpoint:   fmt.Sprintf("http://localhost:%d", seqPort),
			Wallet: genericconf.WalletConfig{
				PrivateKey: fmt.Sprintf("00%x", bobPriv.D.Bytes()),
			},
		}
	}
	bob, err := timeboost.NewBidderClient(
		ctx,
		cfgFetcherBob,
	)
	Require(t, err)

	alice.Start(ctx)
	bob.Start(ctx)

	// Wait until the initial round.
	info, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	timeToWait := time.Until(initialTimestampUnix)
	t.Logf("Waiting until the initial round %v and %v, current time %v", timeToWait, initialTimestampUnix, time.Now())
	<-time.After(timeToWait)

	t.Log("Started auction master stack and bid clients")
	Require(t, alice.Deposit(ctx, big.NewInt(5)))
	Require(t, bob.Deposit(ctx, big.NewInt(5)))

	// Wait until the next timeboost round + a few milliseconds.
	now = time.Now()
	waitTime = roundDuration - time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond())
	t.Logf("Alice and Bob are now deposited into the autonomous auction contract, waiting %v for bidding round..., timestamp %v", waitTime, time.Now())
	time.Sleep(waitTime)
	t.Logf("Reached the bidding round at %v", time.Now())
	time.Sleep(time.Second * 5)

	// We are now in the bidding round, both issue their bids. Bob will win.
	t.Logf("Alice and Bob now submitting their bids at %v", time.Now())
	aliceBid, err := alice.Bid(ctx, big.NewInt(1), aliceOpts.From)
	Require(t, err)
	bobBid, err := bob.Bid(ctx, big.NewInt(2), bobOpts.From)
	Require(t, err)
	t.Logf("Alice bid %+v", aliceBid)
	t.Logf("Bob bid %+v", bobBid)

	// Subscribe to auction resolutions and wait for Bob to win the auction.
	winner, winnerRound := awaitAuctionResolved(t, ctx, seqClient, auctionContract)

	// Verify Bob owns the express lane this round.
	if winner != bobOpts.From {
		t.Fatal("Bob should have won the express lane auction")
	}
	t.Log("Bob won the express lane auction for upcoming round, now waiting for that round to start...")

	// Wait until the round that Bob owns the express lane for.
	now = time.Now()
	waitTime = roundDuration - time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond())
	time.Sleep(waitTime)

	currRound := timeboost.CurrentRound(time.Unix(int64(info.OffsetTimestamp), 0), roundDuration)
	t.Log("curr round", currRound)
	if currRound != winnerRound {
		now = time.Now()
		waitTime = roundDuration - time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond())
		t.Log("Not express lane round yet, waiting for next round", waitTime)
		time.Sleep(waitTime)
	}
	filterOpts := &bind.FilterOpts{
		Context: ctx,
		Start:   0,
		End:     nil,
	}
	it, err := auctionContract.FilterAuctionResolved(filterOpts, nil, nil, nil)
	Require(t, err)
	bobWon := false
	for it.Next() {
		if it.Event.FirstPriceBidder == bobOpts.From {
			bobWon = true
		}
	}
	if !bobWon {
		t.Fatal("Bob should have won the auction")
	}
	return seqNode, seqClient, seqInfo, proxyAddr, cleanupSeq, builderFeedListener.L2, cleanupFeedListener
}

func awaitAuctionResolved(
	t *testing.T,
	ctx context.Context,
	client *ethclient.Client,
	contract *express_lane_auctiongen.ExpressLaneAuction,
) (common.Address, uint64) {
	fromBlock, err := client.BlockNumber(ctx)
	Require(t, err)
	ticker := time.NewTicker(time.Millisecond * 100)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return common.Address{}, 0
		case <-ticker.C:
			latestBlock, err := client.HeaderByNumber(ctx, nil)
			if err != nil {
				t.Log("Could not get latest header", err)
				continue
			}
			toBlock := latestBlock.Number.Uint64()
			if fromBlock == toBlock {
				continue
			}
			filterOpts := &bind.FilterOpts{
				Context: ctx,
				Start:   fromBlock,
				End:     &toBlock,
			}
			it, err := contract.FilterAuctionResolved(filterOpts, nil, nil, nil)
			if err != nil {
				t.Log("Could not filter auction resolutions", err)
				continue
			}
			for it.Next() {
				return it.Event.FirstPriceBidder, it.Event.Round
			}
			fromBlock = toBlock
		}
	}
}

type expressLaneClient struct {
	stopwaiter.StopWaiter
	sync.Mutex
	privKey               *ecdsa.PrivateKey
	chainId               *big.Int
	initialRoundTimestamp time.Time
	roundDuration         time.Duration
	auctionContractAddr   common.Address
	client                *rpc.Client
	sequence              uint64
}

func newExpressLaneClient(
	privKey *ecdsa.PrivateKey,
	chainId *big.Int,
	initialRoundTimestamp time.Time,
	roundDuration time.Duration,
	auctionContractAddr common.Address,
	client *rpc.Client,
) *expressLaneClient {
	return &expressLaneClient{
		privKey:               privKey,
		chainId:               chainId,
		initialRoundTimestamp: initialRoundTimestamp,
		roundDuration:         roundDuration,
		auctionContractAddr:   auctionContractAddr,
		client:                client,
		sequence:              0,
	}
}

func (elc *expressLaneClient) Start(ctxIn context.Context) {
	elc.StopWaiter.Start(ctxIn, elc)
}

func (elc *expressLaneClient) SendTransaction(ctx context.Context, transaction *types.Transaction) error {
	elc.Lock()
	defer elc.Unlock()
	encodedTx, err := transaction.MarshalBinary()
	if err != nil {
		return err
	}
	msg := &timeboost.JsonExpressLaneSubmission{
		ChainId:                (*hexutil.Big)(elc.chainId),
		Round:                  hexutil.Uint64(timeboost.CurrentRound(elc.initialRoundTimestamp, elc.roundDuration)),
		AuctionContractAddress: elc.auctionContractAddr,
		Transaction:            encodedTx,
		Sequence:               hexutil.Uint64(elc.sequence),
		Signature:              hexutil.Bytes{},
	}
	msgGo, err := timeboost.JsonSubmissionToGo(msg)
	if err != nil {
		return err
	}
	signingMsg, err := msgGo.ToMessageBytes()
	if err != nil {
		return err
	}
	signature, err := signSubmission(signingMsg, elc.privKey)
	if err != nil {
		return err
	}
	msg.Signature = signature
	promise := elc.sendExpressLaneRPC(msg)
	if _, err := promise.Await(ctx); err != nil {
		return err
	}
	elc.sequence += 1
	return nil
}

func (elc *expressLaneClient) sendExpressLaneRPC(msg *timeboost.JsonExpressLaneSubmission) containers.PromiseInterface[struct{}] {
	return stopwaiter.LaunchPromiseThread(elc, func(ctx context.Context) (struct{}, error) {
		err := elc.client.CallContext(ctx, nil, "timeboost_sendExpressLaneTransaction", msg)
		return struct{}{}, err
	})
}

func signSubmission(message []byte, key *ecdsa.PrivateKey) ([]byte, error) {
	prefixed := crypto.Keccak256(append([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))), message...))
	sig, err := secp256k1.Sign(prefixed, math.PaddedBigBytes(key.D, 32))
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

func getRandomPort(t testing.TB) int {
	listener, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
