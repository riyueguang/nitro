package arbnode

import (
	"bytes"
	"context"
	"encoding/binary"
	"time"

	"github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/execution"
	"github.com/offchainlabs/nitro/execution/gethexec"
	"github.com/offchainlabs/nitro/util/rpcclient"
	"github.com/offchainlabs/nitro/util/stopwaiter"
)

type BlockMetadataRebuilderConfig struct {
	Enable         bool                   `koanf:"enable"`
	Source         rpcclient.ClientConfig `koanf:"source"`
	SyncInterval   time.Duration          `koanf:"sync-interval"`
	APIBlocksLimit uint64                 `koanf:"api-blocks-limit"`
}

var DefaultBlockMetadataRebuilderConfig = BlockMetadataRebuilderConfig{
	Enable:         false,
	Source:         rpcclient.DefaultClientConfig,
	SyncInterval:   time.Minute * 5,
	APIBlocksLimit: 100,
}

func BlockMetadataRebuilderConfigAddOptions(prefix string, f *pflag.FlagSet) {
	f.Bool(prefix+".enable", DefaultBlockMetadataRebuilderConfig.Enable, "enable syncing blockMetadata using a bulk blockMetadata api")
	rpcclient.RPCClientAddOptions(prefix+".source", f, &DefaultBlockMetadataRebuilderConfig.Source)
	f.Duration(prefix+".rebuild-interval", DefaultBlockMetadataRebuilderConfig.SyncInterval, "interval at which blockMetadata are synced regularly")
	f.Uint64(prefix+".api-blocks-limit", DefaultBlockMetadataRebuilderConfig.APIBlocksLimit, "maximum number of blocks allowed to be queried for blockMetadata per arb_getRawBlockMetadata query.\n"+
		"This should be set lesser than or equal to the limit on the api provider side")
}

type BlockMetadataRebuilder struct {
	stopwaiter.StopWaiter
	config BlockMetadataRebuilderConfig
	db     ethdb.Database
	client *rpcclient.RpcClient
	exec   execution.ExecutionClient
}

func NewBlockMetadataRebuilder(ctx context.Context, c BlockMetadataRebuilderConfig, db ethdb.Database, exec execution.ExecutionClient) (*BlockMetadataRebuilder, error) {
	client := rpcclient.NewRpcClient(func() *rpcclient.ClientConfig { return &c.Source }, nil)
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	return &BlockMetadataRebuilder{
		config: c,
		db:     db,
		client: client,
		exec:   exec,
	}, nil
}

func (b *BlockMetadataRebuilder) Fetch(ctx context.Context, fromBlock, toBlock uint64) ([]gethexec.NumberAndBlockMetadata, error) {
	var result []gethexec.NumberAndBlockMetadata
	err := b.client.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(fromBlock), rpc.BlockNumber(toBlock))
	if err != nil {
		return nil, err
	}
	return result, nil
}

func ArrayToMap[T comparable](arr []T) map[T]struct{} {
	ret := make(map[T]struct{})
	for _, elem := range arr {
		ret[elem] = struct{}{}
	}
	return ret
}

func (b *BlockMetadataRebuilder) PersistBlockMetadata(query []uint64, result []gethexec.NumberAndBlockMetadata) error {
	batch := b.db.NewBatch()
	queryMap := ArrayToMap(query)
	for _, elem := range result {
		pos, err := b.exec.BlockNumberToMessageIndex(elem.BlockNumber)
		if err != nil {
			return err
		}
		if _, ok := queryMap[uint64(pos)]; ok {
			if err := batch.Put(dbKey(blockMetadataInputFeedPrefix, uint64(pos)), elem.RawMetadata); err != nil {
				return err
			}
			if err := batch.Delete(dbKey(missingBlockMetadataInputFeedPrefix, uint64(pos))); err != nil {
				return err
			}
			// If we exceeded the ideal batch size, commit and reset
			if batch.ValueSize() >= ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					return err
				}
				batch.Reset()
			}
		}
	}
	return batch.Write()
}

func (b *BlockMetadataRebuilder) Update(ctx context.Context) time.Duration {
	handleQuery := func(query []uint64) bool {
		result, err := b.Fetch(
			ctx,
			b.exec.MessageIndexToBlockNumber(arbutil.MessageIndex(query[0])),
			b.exec.MessageIndexToBlockNumber(arbutil.MessageIndex(query[len(query)-1])),
		)
		if err != nil {
			log.Error("Error getting result from bulk blockMetadata API", "err", err)
			return false
		}
		if err = b.PersistBlockMetadata(query, result); err != nil {
			log.Error("Error committing result from bulk blockMetadata API to ArbDB", "err", err)
			return false
		}
		return true
	}
	iter := b.db.NewIterator(missingBlockMetadataInputFeedPrefix, nil)
	defer iter.Release()
	var query []uint64
	for iter.Next() {
		keyBytes := bytes.TrimPrefix(iter.Key(), missingBlockMetadataInputFeedPrefix)
		query = append(query, binary.BigEndian.Uint64(keyBytes))
		end := len(query) - 1
		if query[end]-query[0]+1 >= uint64(b.config.APIBlocksLimit) {
			if query[end]-query[0]+1 > uint64(b.config.APIBlocksLimit) && len(query) >= 2 {
				end -= 1
			}
			if success := handleQuery(query[:end+1]); !success {
				return b.config.SyncInterval
			}
			query = query[end+1:]
		}
	}
	if len(query) > 0 {
		if success := handleQuery(query); !success {
			return b.config.SyncInterval
		}
	}
	return b.config.SyncInterval
}

func (b *BlockMetadataRebuilder) Start(ctx context.Context) {
	b.StopWaiter.Start(ctx, b)
	b.CallIteratively(b.Update)
}

func (b *BlockMetadataRebuilder) StopAndWait() {
	b.StopWaiter.StopAndWait()
	b.client.Close()
}
