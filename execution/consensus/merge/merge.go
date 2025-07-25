// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package merge

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon-lib/chain"
	"github.com/erigontech/erigon-lib/chain/params"
	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/empty"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/tracing"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	"github.com/erigontech/erigon/execution/consensus"
	"github.com/erigontech/erigon/execution/consensus/aura"
	"github.com/erigontech/erigon/execution/consensus/misc"
	"github.com/erigontech/erigon/rpc"
)

// Constants for The Merge as specified by EIP-3675: Upgrade consensus to Proof-of-Stake
var (
	ProofOfStakeDifficulty = common.Big0        // PoS block's difficulty is always 0
	ProofOfStakeNonce      = types.BlockNonce{} // PoS block's have all-zero nonces
)

var (
	// errInvalidDifficulty is returned if the difficulty is non-zero.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// errInvalidNonce is returned if the nonce is non-zero.
	errInvalidNonce = errors.New("invalid nonce")

	// errInvalidUncleHash is returned if a block contains a non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	errOlderBlockTime = errors.New("timestamp older than parent")
)

// Merge Consensus Engine for the Execution Layer.
// Merge is a consensus engine that combines the eth1 consensus and proof-of-stake
// algorithm. The transition rule is described in the eth1/2 merge spec:
// https://eips.ethereum.org/EIPS/eip-3675
//
// Note: After the Merge the work is mostly done on the Consensus Layer, so nothing much is to be added on this side.
type Merge struct {
	eth1Engine consensus.Engine // Original consensus engine used in eth1, e.g. ethash or clique
}

// New creates a new instance of the Merge Engine with the given embedded eth1 engine.
func New(eth1Engine consensus.Engine) *Merge {
	if _, ok := eth1Engine.(*Merge); ok {
		panic("nested consensus engine")
	}
	return &Merge{eth1Engine: eth1Engine}
}

// InnerEngine returns the embedded eth1 consensus engine.
func (s *Merge) InnerEngine() consensus.Engine {
	return s.eth1Engine
}

// Type returns the type of the underlying consensus engine.
func (s *Merge) Type() chain.ConsensusName {
	return s.eth1Engine.Type()
}

// Author implements consensus.Engine, returning the header's coinbase as the
// proof-of-stake verified author of the block.
// This is thread-safe (only access the header.Coinbase or the underlying engine's thread-safe method)
func (s *Merge) Author(header *types.Header) (common.Address, error) {
	if !misc.IsPoSHeader(header) {
		return s.eth1Engine.Author(header)
	}
	return header.Coinbase, nil
}

func (s *Merge) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	reached, err := IsTTDReached(chain, header.ParentHash, header.Number.Uint64()-1)
	if err != nil {
		return err
	}
	if !reached {
		// Not verifying seals if the TTD is passed
		return s.eth1Engine.VerifyHeader(chain, header, !chain.Config().TerminalTotalDifficultyPassed)
	}
	// Short circuit if the parent is not known
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	return s.verifyHeader(chain, header, parent)
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (s *Merge) VerifyUncles(chain consensus.ChainReader, header *types.Header, uncles []*types.Header) error {
	if !misc.IsPoSHeader(header) {
		return s.eth1Engine.VerifyUncles(chain, header, uncles)
	}
	if len(uncles) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

// Prepare makes sure difficulty and nonce are correct
func (s *Merge) Prepare(chain consensus.ChainHeaderReader, header *types.Header, state *state.IntraBlockState) error {
	reached, err := IsTTDReached(chain, header.ParentHash, header.Number.Uint64()-1)
	if err != nil {
		return err
	}
	if !reached {
		return s.eth1Engine.Prepare(chain, header, state)
	}
	header.Difficulty = ProofOfStakeDifficulty
	header.Nonce = ProofOfStakeNonce
	return nil
}

func (s *Merge) CalculateRewards(config *chain.Config, header *types.Header, uncles []*types.Header, syscall consensus.SystemCall,
) ([]consensus.Reward, error) {
	_, isAura := s.eth1Engine.(*aura.AuRa)
	if !misc.IsPoSHeader(header) || isAura {
		return s.eth1Engine.CalculateRewards(config, header, uncles, syscall)
	}
	return []consensus.Reward{}, nil
}

func (s *Merge) Finalize(config *chain.Config, header *types.Header, state *state.IntraBlockState,
	txs types.Transactions, uncles []*types.Header, receipts types.Receipts, withdrawals []*types.Withdrawal,
	chain consensus.ChainReader, syscall consensus.SystemCall, skipReceiptsEval bool, logger log.Logger,
) (types.FlatRequests, error) {
	if !misc.IsPoSHeader(header) {
		return s.eth1Engine.Finalize(config, header, state, txs, uncles, receipts, withdrawals, chain, syscall, skipReceiptsEval, logger)
	}

	rewards, err := s.CalculateRewards(config, header, uncles, syscall)
	if err != nil {
		return nil, err
	}
	for _, r := range rewards {
		switch r.Kind {
		case consensus.RewardAuthor:
			state.AddBalance(r.Beneficiary, r.Amount, tracing.BalanceIncreaseRewardMineBlock)
		case consensus.RewardUncle:
			state.AddBalance(r.Beneficiary, r.Amount, tracing.BalanceIncreaseRewardMineUncle)
		default:
			state.AddBalance(r.Beneficiary, r.Amount, tracing.BalanceChangeUnspecified)
		}
	}

	if withdrawals != nil {
		if auraEngine, ok := s.eth1Engine.(*aura.AuRa); ok {
			if err := auraEngine.ExecuteSystemWithdrawals(withdrawals, syscall); err != nil {
				return nil, err
			}
		} else {
			for _, w := range withdrawals {
				amountInWei := new(uint256.Int).Mul(uint256.NewInt(w.Amount), uint256.NewInt(common.GWei))
				state.AddBalance(w.Address, *amountInWei, tracing.BalanceIncreaseWithdrawal)
			}
		}
	}

	var rs types.FlatRequests
	if config.IsPrague(header.Time) && !skipReceiptsEval {
		rs = make(types.FlatRequests, 0)
		allLogs := make(types.Logs, 0)
		for i, rec := range receipts {
			if rec == nil {
				return nil, fmt.Errorf("nil receipt: block %d, txId %d, receipts %s", header.Number, i, receipts)
			}
			allLogs = append(allLogs, rec.Logs...)
		}
		depositReqs, err := misc.ParseDepositLogs(allLogs, config.DepositContract)
		if err != nil {
			return nil, fmt.Errorf("error: could not parse requests logs: %v", err)
		}
		if depositReqs != nil {
			rs = append(rs, *depositReqs)
		}
		withdrawalReq, err := misc.DequeueWithdrawalRequests7002(syscall, state)
		if err != nil {
			return nil, err
		}
		if withdrawalReq != nil {
			rs = append(rs, *withdrawalReq)
		}
		consolidations, err := misc.DequeueConsolidationRequests7251(syscall, state)
		if err != nil {
			return nil, err
		}
		if consolidations != nil {
			rs = append(rs, *consolidations)
		}
		if header.RequestsHash != nil {
			rh := rs.Hash()
			if *header.RequestsHash != *rh {
				return nil, fmt.Errorf("error: invalid requests root hash in header, expected: %v, got :%v", header.RequestsHash, rh)
			}
		}
	}

	return rs, nil
}

func (s *Merge) FinalizeAndAssemble(config *chain.Config, header *types.Header, state *state.IntraBlockState,
	txs types.Transactions, uncles []*types.Header, receipts types.Receipts, withdrawals []*types.Withdrawal, chain consensus.ChainReader, syscall consensus.SystemCall, call consensus.Call, logger log.Logger,
) (*types.Block, types.FlatRequests, error) {
	if !misc.IsPoSHeader(header) {
		return s.eth1Engine.FinalizeAndAssemble(config, header, state, txs, uncles, receipts, withdrawals, chain, syscall, call, logger)
	}
	header.RequestsHash = nil
	outRequests, err := s.Finalize(config, header, state, txs, uncles, receipts, withdrawals, chain, syscall, false, logger)
	if err != nil {
		return nil, nil, err
	}
	if config.IsPrague(header.Time) {
		header.RequestsHash = outRequests.Hash()
	}
	return types.NewBlockForAsembling(header, txs, uncles, receipts, withdrawals), outRequests, nil
}

func (s *Merge) SealHash(header *types.Header) (hash common.Hash) {
	return s.eth1Engine.SealHash(header)
}

func (s *Merge) CalcDifficulty(chain consensus.ChainHeaderReader, time, parentTime uint64, parentDifficulty *big.Int, parentNumber uint64, parentHash, parentUncleHash common.Hash, parentAuRaStep uint64) *big.Int {
	reached, err := IsTTDReached(chain, parentHash, parentNumber)
	if err != nil {
		return nil
	}
	if !reached {
		return s.eth1Engine.CalcDifficulty(chain, time, parentTime, parentDifficulty, parentNumber, parentHash, parentUncleHash, parentAuRaStep)
	}
	return ProofOfStakeDifficulty
}

func (c *Merge) TxDependencies(h *types.Header) [][]int {
	return nil
}

// verifyHeader checks whether a Proof-of-Stake header conforms to the consensus rules of the
// stock Ethereum consensus engine with EIP-3675 modifications.
func (s *Merge) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header) error {

	if uint64(len(header.Extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra-data longer than %d bytes (%d)", params.MaximumExtraDataSize, len(header.Extra))
	}

	if header.Time <= parent.Time {
		return errOlderBlockTime
	}

	if header.Difficulty.Cmp(ProofOfStakeDifficulty) != 0 {
		return errInvalidDifficulty
	}

	if !bytes.Equal(header.Nonce[:], ProofOfStakeNonce[:]) {
		return errInvalidNonce
	}

	// Verify that the gas limit is within cap
	if header.GasLimit > params.MaxBlockGasLimit {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, params.MaxBlockGasLimit)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}

	// Verify that the block number is parent's +1
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(common.Big1) != 0 {
		return consensus.ErrInvalidNumber
	}

	if header.UncleHash != empty.UncleHash {
		return errInvalidUncleHash
	}

	if err := misc.VerifyEip1559Header(chain.Config(), parent, header, false); err != nil {
		return err
	}

	// Verify existence / non-existence of withdrawalsHash
	shanghai := chain.Config().IsShanghai(header.Time)
	if shanghai && header.WithdrawalsHash == nil {
		return errors.New("missing withdrawalsHash")
	}
	if !shanghai && header.WithdrawalsHash != nil {
		return consensus.ErrUnexpectedWithdrawals
	}

	if !chain.Config().IsCancun(header.Time) {
		return misc.VerifyAbsenceOfCancunHeaderFields(header)
	}
	if err := misc.VerifyPresenceOfCancunHeaderFields(header); err != nil {
		return err
	}
	expectedExcessBlobGas := misc.CalcExcessBlobGas(chain.Config(), parent, header.Time)
	if *header.ExcessBlobGas != expectedExcessBlobGas {
		return fmt.Errorf("invalid excessBlobGas: have %d, want %d", *header.ExcessBlobGas, expectedExcessBlobGas)
	}

	// Verify existence / non-existence of requestsHash
	prague := chain.Config().IsPrague(header.Time)
	if prague && header.RequestsHash == nil {
		return errors.New("missing requestsHash")
	}
	if !prague && header.RequestsHash != nil {
		return consensus.ErrUnexpectedRequests
	}

	return nil
}

func (s *Merge) Seal(chain consensus.ChainHeaderReader, blockWithReceipts *types.BlockWithReceipts, results chan<- *types.BlockWithReceipts, stop <-chan struct{}) error {
	block := blockWithReceipts.Block
	receipts := blockWithReceipts.Receipts
	requests := blockWithReceipts.Requests
	if !misc.IsPoSHeader(block.HeaderNoCopy()) {
		return s.eth1Engine.Seal(chain, blockWithReceipts, results, stop)
	}

	header := block.Header()
	header.Nonce = ProofOfStakeNonce

	select {
	case results <- &types.BlockWithReceipts{Block: block.WithSeal(header), Receipts: receipts, Requests: requests}:
	default:
		log.Warn("Sealing result is not read", "sealhash", block.Hash())
	}

	return nil
}

func (s *Merge) IsServiceTransaction(sender common.Address, syscall consensus.SystemCall) bool {
	return s.eth1Engine.IsServiceTransaction(sender, syscall)
}

func (s *Merge) Initialize(config *chain.Config, chain consensus.ChainHeaderReader, header *types.Header,
	state *state.IntraBlockState, syscall consensus.SysCallCustom, logger log.Logger, tracer *tracing.Hooks,
) {
	if !misc.IsPoSHeader(header) {
		s.eth1Engine.Initialize(config, chain, header, state, syscall, logger, tracer)
	}
	if chain.Config().IsCancun(header.Time) {
		misc.ApplyBeaconRootEip4788(header.ParentBeaconBlockRoot, func(addr common.Address, data []byte) ([]byte, error) {
			return syscall(addr, data, state, header, false /* constCall */)
		}, tracer)
	}
	if chain.Config().IsPrague(header.Time) {
		misc.StoreBlockHashesEip2935(header, state, config, chain)
	}
}

func (s *Merge) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return s.eth1Engine.APIs(chain)
}

func (s *Merge) GetTransferFunc() evmtypes.TransferFunc {
	return s.eth1Engine.GetTransferFunc()
}

func (s *Merge) GetPostApplyMessageFunc() evmtypes.PostApplyMessageFunc {
	return s.eth1Engine.GetPostApplyMessageFunc()
}

func (s *Merge) Close() error {
	return s.eth1Engine.Close()
}

// IsTTDReached checks if the TotalTerminalDifficulty has been surpassed on the `parentHash` block.
// It depends on the parentHash already being stored in the database.
// If the total difficulty is not stored in the database a ErrUnknownAncestorTD error is returned.
func IsTTDReached(chain consensus.ChainHeaderReader, parentHash common.Hash, number uint64) (bool, error) {
	if chain.Config().TerminalTotalDifficulty == nil {
		return false, nil
	}
	td := chain.GetTd(parentHash, number)
	if td == nil {
		return false, consensus.ErrUnknownAncestorTD
	}
	return td.Cmp(chain.Config().TerminalTotalDifficulty) >= 0, nil
}
