package rpc

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"encoding/hex"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/harmony-one/harmony/core/types"
	internal_bls "github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/hmy"
	"github.com/harmony-one/harmony/internal/chain"
	internal_common "github.com/harmony-one/harmony/internal/common"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/numeric"
	rpc_common "github.com/harmony-one/harmony/rpc/common"
	eth "github.com/harmony-one/harmony/rpc/eth"
	v1 "github.com/harmony-one/harmony/rpc/v1"
	v2 "github.com/harmony-one/harmony/rpc/v2"
	"github.com/harmony-one/harmony/shard"
	stakingReward "github.com/harmony-one/harmony/staking/reward"
)

// PublicBlockchainService provides an API to access the Harmony blockchain.
// It offers only methods that operate on public data that is freely available to anyone.
type PublicBlockchainService struct {
	hmy             *hmy.Harmony
	version         Version
	limiter         *rate.Limiter
	rpcBlockFactory rpc_common.BlockFactory
	helper          *bcServiceHelper
}

const (
	DefaultRateLimiterWaitTimeout = 5 * time.Second
	rpcGetBlocksLimit             = 1024
)

// NewPublicBlockchainAPI creates a new API for the RPC interface
func NewPublicBlockchainAPI(hmy *hmy.Harmony, version Version, limiterEnable bool, limit int) rpc.API {
	var limiter *rate.Limiter
	if limiterEnable {
		limiter := rate.NewLimiter(rate.Limit(limit), 1)
		strLimit := fmt.Sprintf("%d", int64(limiter.Limit()))
		rpcRateLimitCounterVec.With(prometheus.Labels{
			"rate_limit": strLimit,
		}).Add(float64(0))
	}

	s := &PublicBlockchainService{
		hmy:     hmy,
		version: version,
		limiter: limiter,
	}
	s.helper = s.newHelper()

	switch version {
	case V1:
		s.rpcBlockFactory = v1.NewBlockFactory(s.helper)
	case V2:
		s.rpcBlockFactory = v2.NewBlockFactory(s.helper)
	case Eth:
		s.rpcBlockFactory = eth.NewBlockFactory(s.helper)
	default:
		// This shall not happen for legitimate code.
	}

	return rpc.API{
		Namespace: version.Namespace(),
		Version:   APIVersion,
		Service:   s,
		Public:    true,
	}
}

// ChainId returns the chain id of the chain - required by MetaMask
func (s *PublicBlockchainService) ChainId(ctx context.Context) (interface{}, error) {
	// Format return base on version
	switch s.version {
	case V1:
		return hexutil.Uint64(s.hmy.ChainID), nil
	case V2:
		return s.hmy.ChainID, nil
	case Eth:
		ethChainID := nodeconfig.GetDefaultConfig().GetNetworkType().ChainConfig().EthCompatibleChainID
		return hexutil.Uint64(ethChainID.Uint64()), nil
	default:
		return nil, ErrUnknownRPCVersion
	}
}

// Accounts returns the collection of accounts this node manages
// While this JSON-RPC method is supported, it will not return any accounts.
// Similar to e.g. Infura "unlocking" accounts isn't supported.
// Instead, users should send already signed raw transactions using hmy_sendRawTransaction or eth_sendRawTransaction
func (s *PublicBlockchainService) Accounts() []common.Address {
	return []common.Address{}
}

// getBalanceByBlockNumber returns balance by block number at given eth blockNum without checks
func (s *PublicBlockchainService) getBalanceByBlockNumber(
	ctx context.Context, address string, blockNum rpc.BlockNumber,
) (*big.Int, error) {
	addr, err := internal_common.ParseAddr(address)
	if err != nil {
		return nil, err
	}
	balance, err := s.hmy.GetBalance(ctx, addr, blockNum)
	if err != nil {
		return nil, err
	}
	return balance, nil
}

// BlockNumber returns the block number of the chain head.
func (s *PublicBlockchainService) BlockNumber(ctx context.Context) (interface{}, error) {
	// Fetch latest header
	header, err := s.hmy.HeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return nil, err
	}

	// Format return base on version
	switch s.version {
	case V1, Eth:
		return hexutil.Uint64(header.Number().Uint64()), nil
	case V2:
		return header.Number().Uint64(), nil
	default:
		return nil, ErrUnknownRPCVersion
	}
}

func (s *PublicBlockchainService) wait(ctx context.Context) error {
	if s.limiter != nil {
		deadlineCtx, cancel := context.WithTimeout(ctx, DefaultRateLimiterWaitTimeout)
		defer cancel()
		if !s.limiter.Allow() {
			strLimit := fmt.Sprintf("%d", int64(s.limiter.Limit()))
			rpcRateLimitCounterVec.With(prometheus.Labels{
				"rate_limit": strLimit,
			}).Inc()
		}

		return s.limiter.Wait(deadlineCtx)
	}
	return nil
}

// GetBlockByNumber returns the requested block. When blockNum is -1 the chain head is returned. When fullTx is true all
// transactions in the block are returned in full detail, otherwise only the transaction hash is returned.
// When withSigners in BlocksArgs is true it shows block signers for this block in list of one addresses.
func (s *PublicBlockchainService) GetBlockByNumber(
	ctx context.Context, blockNumber BlockNumber, opts interface{},
) (response interface{}, err error) {
	timer := DoMetricRPCRequest(GetBlockByNumber)
	defer DoRPCRequestDuration(GetBlockByNumber, timer)

	err = s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByNumber, FailedNumber)
		return nil, err
	}

	// Process arguments based on version
	blockArgs, err := s.getBlockOptions(opts)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByNumber, FailedNumber)
		return nil, err
	}

	if blockNumber.EthBlockNumber() == rpc.PendingBlockNumber {
		return nil, errors.New("pending block number not implemented")
	}
	var blockNum uint64
	if blockNumber.EthBlockNumber() == rpc.LatestBlockNumber {
		blockNum = s.hmy.BlockChain.CurrentHeader().Number().Uint64()
	} else {
		blockNum = uint64(blockNumber.EthBlockNumber().Int64())
	}

	blk := s.hmy.BlockChain.GetBlockByNumber(blockNum)
	// Some Ethereum tools (such as Truffle) rely on being able to query for future blocks without the chain returning errors.
	// These tools implement retry mechanisms that will query & retry for a given block until it has been finalized.
	// Throwing an error like "requested block number greater than current block number" breaks this retry functionality.
	// Disable isBlockGreaterThanLatest checks for Ethereum RPC:s, but keep them in place for legacy hmy_ RPC:s for now to ensure backwards compatibility
	if blk == nil {
		DoMetricRPCQueryInfo(GetBlockByNumber, FailedNumber)
		if s.version == Eth {
			return nil, nil
		}
		return nil, ErrRequestedBlockTooHigh
	}
	// Format the response according to version
	rpcBlock, err := s.rpcBlockFactory.NewBlock(blk, blockArgs)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByNumber, FailedNumber)
		return nil, err
	}
	return rpcBlock, err
}

// GetBlockByHash returns the requested block. When fullTx is true all transactions in the block are returned in full
// detail, otherwise only the transaction hash is returned. When withSigners in BlocksArgs is true
// it shows block signers for this block in list of one addresses.
func (s *PublicBlockchainService) GetBlockByHash(
	ctx context.Context, blockHash common.Hash, opts interface{},
) (response interface{}, err error) {
	timer := DoMetricRPCRequest(GetBlockByHash)
	defer DoRPCRequestDuration(GetBlockByHash, timer)

	err = s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByHash, FailedNumber)
		return nil, err
	}

	// Process arguments based on version
	blockArgs, err := s.getBlockOptions(opts)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByHash, FailedNumber)
		return nil, err
	}

	// Fetch the block
	blk, err := s.hmy.GetBlock(ctx, blockHash)
	if err != nil || blk == nil {
		DoMetricRPCQueryInfo(GetBlockByHash, FailedNumber)
		return nil, err
	}

	// Format the response according to version
	rpcBlock, err := s.rpcBlockFactory.NewBlock(blk, blockArgs)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByNumber, FailedNumber)
		return nil, err
	}
	return rpcBlock, err
}

// GetBlockByNumberNew is an alias for GetBlockByNumber using rpc_common.BlockArgs
func (s *PublicBlockchainService) GetBlockByNumberNew(
	ctx context.Context, blockNum BlockNumber, blockArgs *rpc_common.BlockArgs,
) (interface{}, error) {
	timer := DoMetricRPCRequest(GetBlockByNumberNew)
	defer DoRPCRequestDuration(GetBlockByNumberNew, timer)

	res, err := s.GetBlockByNumber(ctx, blockNum, blockArgs)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByNumberNew, FailedNumber)
	}
	return res, err
}

// GetBlockByHashNew is an alias for GetBlocksByHash using rpc_common.BlockArgs
func (s *PublicBlockchainService) GetBlockByHashNew(
	ctx context.Context, blockHash common.Hash, blockArgs *rpc_common.BlockArgs,
) (interface{}, error) {
	timer := DoMetricRPCRequest(GetBlockByHashNew)
	defer DoRPCRequestDuration(GetBlockByHashNew, timer)

	res, err := s.GetBlockByHash(ctx, blockHash, blockArgs)
	if err != nil {
		DoMetricRPCQueryInfo(GetBlockByHashNew, FailedNumber)
	}
	return res, err
}

// GetBlocks method returns blocks in range blockStart, blockEnd just like GetBlockByNumber but all at once.
func (s *PublicBlockchainService) GetBlocks(
	ctx context.Context, blockNumberStart BlockNumber,
	blockNumberEnd BlockNumber, blockArgs *rpc_common.BlockArgs,
) ([]interface{}, error) {
	timer := DoMetricRPCRequest(GetBlocks)
	defer DoRPCRequestDuration(GetBlocks, timer)

	blockStart := blockNumberStart.Int64()
	blockEnd := blockNumberEnd.Int64()
	if blockNumberEnd.EthBlockNumber() == rpc.LatestBlockNumber {
		blockEnd = s.hmy.BlockChain.CurrentHeader().Number().Int64()
	}
	if blockEnd >= blockStart && blockEnd-blockStart > rpcGetBlocksLimit {
		return nil, fmt.Errorf("GetBlocks query must be smaller than size %v", rpcGetBlocksLimit)
	}

	// Fetch blocks within given range
	result := make([]interface{}, 0)
	for i := blockStart; i <= blockEnd; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		blockNum := BlockNumber(i)
		if blockNum.Int64() > s.hmy.CurrentBlock().Number().Int64() {
			break
		}
		// rpcBlock is already formatted according to version
		rpcBlock, err := s.GetBlockByNumber(ctx, blockNum, blockArgs)
		if err != nil {
			DoMetricRPCQueryInfo(GetBlockByNumber, FailedNumber)
			utils.Logger().Warn().Err(err).Msg("RPC Get Blocks Error")
			return nil, err
		}
		if rpcBlock != nil {
			result = append(result, rpcBlock)
		}
	}
	return result, nil
}

// IsLastBlock checks if block is last epoch block.
func (s *PublicBlockchainService) IsLastBlock(ctx context.Context, blockNum uint64) (bool, error) {
	if !isBeaconShard(s.hmy) {
		return false, ErrNotBeaconShard
	}
	return shard.Schedule.IsLastBlock(blockNum), nil
}

// EpochLastBlock returns epoch last block.
func (s *PublicBlockchainService) EpochLastBlock(ctx context.Context, epoch uint64) (uint64, error) {
	if !isBeaconShard(s.hmy) {
		return 0, ErrNotBeaconShard
	}
	return shard.Schedule.EpochLastBlock(epoch), nil
}

// GetBlockSigners returns signers for a particular block.
func (s *PublicBlockchainService) GetBlockSigners(
	ctx context.Context, blockNumber BlockNumber,
) ([]string, error) {
	// Process arguments based on version
	blockNum := blockNumber.EthBlockNumber()
	if blockNum == rpc.PendingBlockNumber {
		return nil, errors.New("cannot get signer keys for pending blocks")
	}
	// Ensure correct block
	if blockNum.Int64() == 0 || blockNum.Int64() >= s.hmy.CurrentBlock().Number().Int64() {
		return []string{}, nil
	}
	if isBlockGreaterThanLatest(s.hmy, blockNum) {
		return nil, ErrRequestedBlockTooHigh
	}
	var bn uint64
	if blockNum == rpc.LatestBlockNumber {
		bn = s.hmy.CurrentBlock().NumberU64()
	} else {
		bn = uint64(blockNum.Int64())
	}
	blk := s.hmy.BlockChain.GetBlockByNumber(bn)
	if blk == nil {
		return nil, errors.New("unknown block")
	}
	// Fetch signers
	return s.helper.GetSigners(blk)
}

// GetBlockSignerKeys returns bls public keys that signed the block.
func (s *PublicBlockchainService) GetBlockSignerKeys(
	ctx context.Context, blockNumber BlockNumber,
) ([]string, error) {
	// Process arguments based on version
	blockNum := blockNumber.EthBlockNumber()
	if blockNum == rpc.PendingBlockNumber {
		return nil, errors.New("cannot get signer keys for pending blocks")
	}
	// Ensure correct block
	if blockNum.Int64() == 0 || blockNum.Int64() >= s.hmy.CurrentBlock().Number().Int64() {
		return []string{}, nil
	}
	if isBlockGreaterThanLatest(s.hmy, blockNum) {
		return nil, ErrRequestedBlockTooHigh
	}
	var bn uint64
	if blockNum == rpc.LatestBlockNumber {
		bn = s.hmy.CurrentBlock().NumberU64()
	} else {
		bn = uint64(blockNum.Int64())
	}
	// Fetch signers
	return s.helper.GetBLSSigners(bn)
}

// IsBlockSigner returns true if validator with address signed blockNum block.
func (s *PublicBlockchainService) IsBlockSigner(
	ctx context.Context, blockNumber BlockNumber, address string,
) (bool, error) {
	// Process arguments based on version
	blockNum := blockNumber.EthBlockNumber()

	// Ensure correct block
	if blockNum.Int64() == 0 {
		return false, nil
	}
	if isBlockGreaterThanLatest(s.hmy, blockNum) {
		return false, ErrRequestedBlockTooHigh
	}
	var bn uint64
	if blockNum == rpc.PendingBlockNumber {
		return false, errors.New("no signing data for pending block number")
	} else if blockNum == rpc.LatestBlockNumber {
		bn = s.hmy.BlockChain.CurrentBlock().NumberU64()
	} else {
		bn = uint64(blockNum.Int64())
	}

	// Fetch signers
	return s.helper.IsSigner(address, bn)
}

// GetSignedBlocks returns how many blocks a particular validator signed for
// last blocksPeriod (1 epoch's worth of blocks).
func (s *PublicBlockchainService) GetSignedBlocks(
	ctx context.Context, address string,
) (interface{}, error) {
	// Fetch the number of signed blocks within default period
	curEpoch := s.hmy.CurrentBlock().Epoch()
	var totalSigned uint64
	if !s.hmy.ChainConfig().IsStaking(curEpoch) {
		// calculate signed before staking epoch
		totalSigned := uint64(0)
		lastBlock := uint64(0)
		blockHeight := s.hmy.CurrentBlock().Number().Uint64()
		instance := shard.Schedule.InstanceForEpoch(s.hmy.CurrentBlock().Epoch())
		if blockHeight >= instance.BlocksPerEpoch() {
			lastBlock = blockHeight - instance.BlocksPerEpoch() + 1
		}
		for i := lastBlock; i <= blockHeight; i++ {
			signed, err := s.IsBlockSigner(ctx, BlockNumber(i), address)
			if err == nil && signed {
				totalSigned++
			}
		}
	} else {
		ethAddr, err := internal_common.Bech32ToAddress(address)
		if err != nil {
			return nil, err
		}
		curVal, err := s.hmy.BlockChain.ReadValidatorInformation(ethAddr)
		if err != nil {
			return nil, err
		}
		prevVal, err := s.hmy.BlockChain.ReadValidatorSnapshot(ethAddr)
		if err != nil {
			return nil, err
		}
		signedInEpoch := new(big.Int).Sub(curVal.Counters.NumBlocksSigned, prevVal.Validator.Counters.NumBlocksSigned)
		if signedInEpoch.Cmp(common.Big0) < 0 {
			return nil, errors.New("negative signed in epoch")
		}
		totalSigned = signedInEpoch.Uint64()
	}

	// Format the response according to the version
	switch s.version {
	case V1, Eth:
		return hexutil.Uint64(totalSigned), nil
	case V2:
		return totalSigned, nil
	default:
		return nil, ErrUnknownRPCVersion
	}
}

// GetEpoch returns current epoch.
func (s *PublicBlockchainService) GetEpoch(ctx context.Context) (interface{}, error) {
	// Fetch Header
	header, err := s.hmy.HeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return "", err
	}
	epoch := header.Epoch().Uint64()

	// Format the response according to the version
	switch s.version {
	case V1, Eth:
		return hexutil.Uint64(epoch), nil
	case V2:
		return epoch, nil
	default:
		return nil, ErrUnknownRPCVersion
	}
}

// GetLeader returns current shard leader.
func (s *PublicBlockchainService) GetLeader(ctx context.Context) (string, error) {
	// Fetch Header
	blk := s.hmy.BlockChain.CurrentBlock()
	// Response output is the same for all versions
	leader := s.helper.GetLeader(blk)
	return leader, nil
}

// GetShardingStructure returns an array of sharding structures.
func (s *PublicBlockchainService) GetShardingStructure(
	ctx context.Context,
) ([]StructuredResponse, error) {
	timer := DoMetricRPCRequest(GetShardingStructure)
	defer DoRPCRequestDuration(GetShardingStructure, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetShardingStructure, FailedNumber)
		return nil, err
	}

	// Get header and number of shards.
	epoch := s.hmy.CurrentBlock().Epoch()
	numShard := shard.Schedule.InstanceForEpoch(epoch).NumShards()

	// Return sharding structure for each case (response output is the same for all versions)
	return shard.Schedule.GetShardingStructure(int(numShard), int(s.hmy.ShardID)), nil
}

// GetShardID returns shard ID of the requested node.
func (s *PublicBlockchainService) GetShardID(ctx context.Context) (int, error) {
	// Response output is the same for all versions
	return int(s.hmy.ShardID), nil
}

// GetBalanceByBlockNumber returns balance by block number
func (s *PublicBlockchainService) GetBalanceByBlockNumber(
	ctx context.Context, address string, blockNumber BlockNumber,
) (interface{}, error) {
	timer := DoMetricRPCRequest(GetBalanceByBlockNumber)
	defer DoRPCRequestDuration(GetBalanceByBlockNumber, timer)

	// Process number based on version
	blockNum := blockNumber.EthBlockNumber()

	// Fetch balance
	if isBlockGreaterThanLatest(s.hmy, blockNum) {
		DoMetricRPCQueryInfo(GetBalanceByBlockNumber, FailedNumber)
		return nil, ErrRequestedBlockTooHigh
	}
	balance, err := s.getBalanceByBlockNumber(ctx, address, blockNum)
	if err != nil {
		DoMetricRPCQueryInfo(GetBalanceByBlockNumber, FailedNumber)
		return nil, err
	}

	// Format return base on version
	switch s.version {
	case V1, Eth:
		return (*hexutil.Big)(balance), nil
	case V2:
		return balance, nil
	default:
		return nil, ErrUnknownRPCVersion
	}
}

// LatestHeader returns the latest header information
func (s *PublicBlockchainService) LatestHeader(ctx context.Context) (StructuredResponse, error) {
	timer := DoMetricRPCRequest(LatestHeader)
	defer DoRPCRequestDuration(LatestHeader, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(LatestHeader, FailedNumber)
		return nil, err
	}

	// Fetch Header
	header, err := s.hmy.HeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		DoMetricRPCQueryInfo(LatestHeader, FailedNumber)
		return nil, err
	}

	// Response output is the same for all versions
	leader := s.hmy.GetLeaderAddress(header.Coinbase(), header.Epoch())
	return NewStructuredResponse(NewHeaderInformation(header, leader))
}

// GetLatestChainHeaders ..
func (s *PublicBlockchainService) GetLatestChainHeaders(
	ctx context.Context,
) (StructuredResponse, error) {
	// Response output is the same for all versions
	timer := DoMetricRPCRequest(GetLatestChainHeaders)
	defer DoRPCRequestDuration(GetLatestChainHeaders, timer)
	return NewStructuredResponse(s.hmy.GetLatestChainHeaders())
}

// GetLastCrossLinks ..
func (s *PublicBlockchainService) GetLastCrossLinks(
	ctx context.Context,
) ([]StructuredResponse, error) {
	timer := DoMetricRPCRequest(GetLastCrossLinks)
	defer DoRPCRequestDuration(GetLastCrossLinks, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetLastCrossLinks, FailedNumber)
		return nil, err
	}

	if !isBeaconShard(s.hmy) {
		DoMetricRPCQueryInfo(GetLastCrossLinks, FailedNumber)
		return nil, ErrNotBeaconShard
	}

	// Fetch crosslinks
	crossLinks, err := s.hmy.GetLastCrossLinks()
	if err != nil {
		DoMetricRPCQueryInfo(GetLastCrossLinks, FailedNumber)
		return nil, err
	}

	// Format response, all output is the same for all versions
	responseSlice := []StructuredResponse{}
	for _, el := range crossLinks {
		response, err := NewStructuredResponse(el)
		if err != nil {
			DoMetricRPCQueryInfo(GetLastCrossLinks, FailedNumber)
			return nil, err
		}
		responseSlice = append(responseSlice, response)
	}
	return responseSlice, nil
}

// GetHeaderByNumber returns block header at given number
func (s *PublicBlockchainService) GetHeaderByNumber(
	ctx context.Context, blockNumber BlockNumber,
) (StructuredResponse, error) {
	timer := DoMetricRPCRequest(GetHeaderByNumber)
	defer DoRPCRequestDuration(GetHeaderByNumber, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetHeaderByNumber, FailedNumber)
		return nil, err
	}

	// Process number based on version
	blockNum := blockNumber.EthBlockNumber()

	// Ensure valid block number
	if s.version != Eth && isBlockGreaterThanLatest(s.hmy, blockNum) {
		DoMetricRPCQueryInfo(GetHeaderByNumber, FailedNumber)
		return nil, ErrRequestedBlockTooHigh
	}

	// Fetch Header
	header, err := s.hmy.HeaderByNumber(ctx, blockNum)
	if header != nil && err == nil {
		// Response output is the same for all versions
		leader := s.hmy.GetLeaderAddress(header.Coinbase(), header.Epoch())
		return NewStructuredResponse(NewHeaderInformation(header, leader))
	}
	return nil, err
}

// GetHeaderByNumberRLPHex returns block header at given number by `hex(rlp(header))`
func (s *PublicBlockchainService) GetHeaderByNumberRLPHex(
	ctx context.Context, blockNumber BlockNumber,
) (string, error) {
	timer := DoMetricRPCRequest(GetHeaderByNumber)
	defer DoRPCRequestDuration(GetHeaderByNumber, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetHeaderByNumber, FailedNumber)
		return "", err
	}

	// Process number based on version
	blockNum := blockNumber.EthBlockNumber()

	// Ensure valid block number
	if s.version != Eth && isBlockGreaterThanLatest(s.hmy, blockNum) {
		DoMetricRPCQueryInfo(GetHeaderByNumber, FailedNumber)
		return "", ErrRequestedBlockTooHigh
	}

	// Fetch Header
	header, err := s.hmy.HeaderByNumber(ctx, blockNum)
	if header != nil && err == nil {
		// Response output is the same for all versions
		val, _ := rlp.EncodeToBytes(header)
		return hex.EncodeToString(val), nil
	}
	return "", err
}

// GetCurrentUtilityMetrics ..
func (s *PublicBlockchainService) GetCurrentUtilityMetrics(
	ctx context.Context,
) (StructuredResponse, error) {
	timer := DoMetricRPCRequest(GetCurrentUtilityMetrics)
	defer DoRPCRequestDuration(GetCurrentUtilityMetrics, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetCurrentUtilityMetrics, FailedNumber)
		return nil, err
	}

	if !isBeaconShard(s.hmy) {
		DoMetricRPCQueryInfo(GetCurrentUtilityMetrics, FailedNumber)
		return nil, ErrNotBeaconShard
	}

	// Fetch metrics
	metrics, err := s.hmy.GetCurrentUtilityMetrics()
	if err != nil {
		DoMetricRPCQueryInfo(GetCurrentUtilityMetrics, FailedNumber)
		return nil, err
	}

	// Response output is the same for all versions
	return NewStructuredResponse(metrics)
}

// GetSuperCommittees ..
func (s *PublicBlockchainService) GetSuperCommittees(
	ctx context.Context,
) (StructuredResponse, error) {
	timer := DoMetricRPCRequest(GetSuperCommittees)
	defer DoRPCRequestDuration(GetSuperCommittees, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetSuperCommittees, FailedNumber)
		return nil, err
	}

	if !isBeaconShard(s.hmy) {
		DoMetricRPCQueryInfo(GetSuperCommittees, FailedNumber)
		return nil, ErrNotBeaconShard
	}

	// Fetch super committees
	cmt, err := s.hmy.GetSuperCommittees()
	if err != nil {
		DoMetricRPCQueryInfo(GetSuperCommittees, FailedNumber)
		return nil, err
	}

	// Response output is the same for all versions
	return NewStructuredResponse(cmt)
}

// GetCurrentBadBlocks ..
func (s *PublicBlockchainService) GetCurrentBadBlocks(
	ctx context.Context,
) ([]StructuredResponse, error) {
	timer := DoMetricRPCRequest(GetCurrentBadBlocks)
	defer DoRPCRequestDuration(GetCurrentBadBlocks, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetCurrentBadBlocks, FailedNumber)
		return nil, err
	}

	// Fetch bad blocks and format
	badBlocks := []StructuredResponse{}
	for _, blk := range s.hmy.GetCurrentBadBlocks() {
		// Response output is the same for all versions
		fmtBadBlock, err := NewStructuredResponse(blk)
		if err != nil {
			DoMetricRPCQueryInfo(GetCurrentBadBlocks, FailedNumber)
			return nil, err
		}
		badBlocks = append(badBlocks, fmtBadBlock)
	}

	return badBlocks, nil
}

// GetTotalSupply ..
func (s *PublicBlockchainService) GetTotalSupply(
	ctx context.Context,
) (numeric.Dec, error) {
	return stakingReward.GetTotalTokens(s.hmy.BlockChain)
}

// GetCirculatingSupply ...
func (s *PublicBlockchainService) GetCirculatingSupply(
	ctx context.Context,
) (numeric.Dec, error) {
	return chain.GetCirculatingSupply(s.hmy.BlockChain)
}

// GetStakingNetworkInfo ..
func (s *PublicBlockchainService) GetStakingNetworkInfo(
	ctx context.Context,
) (StructuredResponse, error) {
	timer := DoMetricRPCRequest(GetStakingNetworkInfo)
	defer DoRPCRequestDuration(GetStakingNetworkInfo, timer)

	err := s.wait(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetStakingNetworkInfo, FailedNumber)
		return nil, err
	}

	if !isBeaconShard(s.hmy) {
		DoMetricRPCQueryInfo(GetStakingNetworkInfo, FailedNumber)
		return nil, ErrNotBeaconShard
	}
	totalStaking := s.hmy.GetTotalStakingSnapshot()
	header, err := s.hmy.HeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		DoMetricRPCQueryInfo(GetStakingNetworkInfo, FailedNumber)
		return nil, err
	}
	medianSnapshot, err := s.hmy.GetMedianRawStakeSnapshot()
	if err != nil {
		DoMetricRPCQueryInfo(GetStakingNetworkInfo, FailedNumber)
		return nil, err
	}
	epochLastBlock, err := s.EpochLastBlock(ctx, header.Epoch().Uint64())
	if err != nil {
		DoMetricRPCQueryInfo(GetStakingNetworkInfo, FailedNumber)
		return nil, err
	}
	totalSupply, err := s.GetTotalSupply(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetStakingNetworkInfo, FailedNumber)
		return nil, err
	}
	circulatingSupply, err := s.GetCirculatingSupply(ctx)
	if err != nil {
		DoMetricRPCQueryInfo(GetStakingNetworkInfo, FailedNumber)
		return nil, err
	}

	// Response output is the same for all versions
	return NewStructuredResponse(StakingNetworkInfo{
		TotalSupply:       totalSupply,
		CirculatingSupply: circulatingSupply,
		EpochLastBlock:    epochLastBlock,
		TotalStaking:      totalStaking,
		MedianRawStake:    medianSnapshot.MedianStake,
	})
}

// InSync returns if shard chain is syncing
func (s *PublicBlockchainService) InSync(ctx context.Context) (bool, error) {
	inSync, _, _ := s.hmy.NodeAPI.SyncStatus(s.hmy.BlockChain.ShardID())
	return inSync, nil
}

// BeaconInSync returns if beacon chain is syncing
func (s *PublicBlockchainService) BeaconInSync(ctx context.Context) (bool, error) {
	inSync, _, _ := s.hmy.NodeAPI.SyncStatus(s.hmy.BeaconChain.ShardID())
	return inSync, nil
}

// getBlockOptions block args given an interface option from RPC params.
func (s *PublicBlockchainService) getBlockOptions(opts interface{}) (*rpc_common.BlockArgs, error) {
	blockArgs, ok := opts.(*rpc_common.BlockArgs)
	if ok {
		return blockArgs, nil
	}
	switch s.version {
	case V1, Eth:
		fullTx, ok := opts.(bool)
		if !ok {
			return nil, fmt.Errorf("invalid type for block arguments")
		}
		return &rpc_common.BlockArgs{
			WithSigners: false,
			FullTx:      fullTx,
			InclStaking: true,
		}, nil
	case V2:
		parsedBlockArgs := rpc_common.BlockArgs{}
		if err := parsedBlockArgs.UnmarshalFromInterface(opts); err != nil {
			return nil, err
		}
		return &parsedBlockArgs, nil
	default:
		return nil, ErrUnknownRPCVersion
	}
}

func isBlockGreaterThanLatest(hmy *hmy.Harmony, blockNum rpc.BlockNumber) bool {
	// rpc.BlockNumber is int64 (latest = -1. pending = -2) and currentBlockNum is uint64.
	if blockNum == rpc.PendingBlockNumber {
		return true
	}
	if blockNum == rpc.LatestBlockNumber {
		return false
	}
	return uint64(blockNum) > hmy.CurrentBlock().NumberU64()
}

func (s *PublicBlockchainService) SetNodeToBackupMode(ctx context.Context, isBackup bool) (bool, error) {
	return s.hmy.NodeAPI.SetNodeBackupMode(isBackup), nil
}

const (
	blockCacheSize      = 2048
	signersCacheSize    = blockCacheSize
	stakingTxsCacheSize = blockCacheSize
	leaderCacheSize     = blockCacheSize
)

type (
	// bcServiceHelper is the getHelper for block factory. Implements
	// rpc_common.BlockDataProvider
	bcServiceHelper struct {
		version Version
		hmy     *hmy.Harmony
		cache   *bcServiceCache
	}

	bcServiceCache struct {
		signersCache    *lru.Cache // numberU64 -> []string
		stakingTxsCache *lru.Cache // numberU64 -> interface{} (v1.StakingTransactions / v2.StakingTransactions)
		leaderCache     *lru.Cache // numberUint64 -> string
	}
)

func (s *PublicBlockchainService) newHelper() *bcServiceHelper {
	signerCache, _ := lru.New(signersCacheSize)
	stakingTxsCache, _ := lru.New(stakingTxsCacheSize)
	leaderCache, _ := lru.New(leaderCacheSize)
	cache := &bcServiceCache{
		signersCache:    signerCache,
		stakingTxsCache: stakingTxsCache,
		leaderCache:     leaderCache,
	}
	return &bcServiceHelper{
		version: s.version,
		hmy:     s.hmy,
		cache:   cache,
	}
}

func (helper *bcServiceHelper) GetLeader(b *types.Block) string {
	x, ok := helper.cache.leaderCache.Get(b.NumberU64())
	if ok && x != nil {
		return x.(string)
	}
	leader := helper.hmy.GetLeaderAddress(b.Coinbase(), b.Epoch())
	helper.cache.leaderCache.Add(b.NumberU64(), leader)
	return leader
}

func (helper *bcServiceHelper) GetStakingTxs(b *types.Block) (interface{}, error) {
	x, ok := helper.cache.stakingTxsCache.Get(b.NumberU64())
	if ok && x != nil {
		return x, nil
	}
	var (
		rpcStakings interface{}
		err         error
	)
	switch helper.version {
	case V1:
		rpcStakings, err = v1.StakingTransactionsFromBlock(b)
	case V2:
		rpcStakings, err = v2.StakingTransactionsFromBlock(b)
	case Eth:
		err = errors.New("staking transaction data is unsupported to Eth service")
	default:
		err = fmt.Errorf("unsupported version %v", helper.version)
	}
	if err != nil {
		return nil, err
	}
	helper.cache.stakingTxsCache.Add(b.NumberU64(), rpcStakings)
	return rpcStakings, nil
}

func (helper *bcServiceHelper) GetStakingTxHashes(b *types.Block) []common.Hash {
	stkTxs := b.StakingTransactions()

	res := make([]common.Hash, 0, len(stkTxs))
	for _, tx := range stkTxs {
		res = append(res, tx.Hash())
	}
	return res
}

// signerData is the cached signing data for a block
type signerData struct {
	signers    []string           // one address for signers
	blsSigners []string           // bls hex for signers
	slots      shard.SlotList     // computed slots for epoch shard committee
	mask       *internal_bls.Mask // mask for the block
}

func (helper *bcServiceHelper) GetSigners(b *types.Block) ([]string, error) {
	sd, err := helper.getSignerData(b.NumberU64())
	if err != nil {
		return nil, err
	}
	return sd.signers, nil
}

func (helper *bcServiceHelper) GetBLSSigners(bn uint64) ([]string, error) {
	sd, err := helper.getSignerData(bn)
	if err != nil {
		return nil, err
	}
	return sd.blsSigners, nil
}

func (helper *bcServiceHelper) IsSigner(oneAddr string, bn uint64) (bool, error) {
	sd, err := helper.getSignerData(bn)
	if err != nil {
		return false, err
	}
	for _, signer := range sd.signers {
		if oneAddr == signer {
			return true, nil
		}
	}
	return false, nil
}

func (helper *bcServiceHelper) getSignerData(bn uint64) (*signerData, error) {
	x, ok := helper.cache.signersCache.Get(bn)
	if ok && x != nil {
		return x.(*signerData), nil
	}
	sd, err := getSignerData(helper.hmy, bn)
	if err != nil {
		return nil, errors.Wrap(err, "getSignerData")
	}
	helper.cache.signersCache.Add(bn, sd)
	return sd, nil
}

func getSignerData(hmy *hmy.Harmony, number uint64) (*signerData, error) {
	slots, mask, err := hmy.GetBlockSigners(context.Background(), rpc.BlockNumber(number))
	if err != nil {
		return nil, err
	}
	signers := make([]string, 0, len(slots))
	blsSigners := make([]string, 0, len(slots))
	for _, validator := range slots {
		oneAddress, err := internal_common.AddressToBech32(validator.EcdsaAddress)
		if err != nil {
			return nil, err
		}
		if ok, err := mask.KeyEnabled(validator.BLSPublicKey); err == nil && ok {
			blsSigners = append(blsSigners, validator.BLSPublicKey.Hex())
			signers = append(signers, oneAddress)
		}
	}
	return &signerData{
		signers:    signers,
		blsSigners: blsSigners,
		slots:      slots,
		mask:       mask,
	}, nil
}
