// Copyright 2015 The go-ethereum Authors
// (original work)
// Copyright 2024 The Erigon Authors
// (modifications)
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

package bind

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/holiman/uint256"

	ethereum "github.com/erigontech/erigon"
	"github.com/erigontech/erigon-lib/abi"
	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/crypto"
	"github.com/erigontech/erigon-lib/types"
	"github.com/erigontech/erigon/p2p/event"
)

// SignerFn is a signer function callback when a contract requires a method to
// sign the transaction before submission.
type SignerFn func(common.Address, types.Transaction) (types.Transaction, error)

// CallOpts is the collection of options to fine tune a contract call request.
type CallOpts struct {
	Pending     bool            // Whether to operate on the pending state or the last known one
	From        common.Address  // Optional the sender address, otherwise the first account is used
	BlockNumber *big.Int        // Optional the block number on which the call should be performed
	Context     context.Context // Network context to support cancellation and timeouts (nil = no timeout)
}

// TransactOpts is the collection of authorization data required to create a
// valid Ethereum transaction.
type TransactOpts struct {
	From   common.Address // Ethereum account to send the transaction from
	Nonce  *big.Int       // Nonce to use for the transaction execution (nil = use pending state)
	Signer SignerFn       // Method to use for signing the transaction (mandatory)

	Value    *big.Int // Funds to transfer along the transaction (nil = 0 = no funds)
	GasPrice *big.Int // Gas price to use for the transaction execution (nil = gas price oracle)
	GasLimit uint64   // Gas limit to set for the transaction execution (0 = estimate)

	Context context.Context // Network context to support cancellation and timeouts (nil = no timeout)
}

// FilterOpts is the collection of options to fine tune filtering for events
// within a bound contract.
type FilterOpts struct {
	Start uint64  // Start of the queried range
	End   *uint64 // End of the range (nil = latest)

	Context context.Context // Network context to support cancellation and timeouts (nil = no timeout)
}

// WatchOpts is the collection of options to fine tune subscribing for events
// within a bound contract.
type WatchOpts struct {
	Start   *uint64         // Start of the queried range (nil = latest)
	Context context.Context // Network context to support cancellation and timeouts (nil = no timeout)
}

// BoundContract is the base wrapper object that reflects a contract on the
// Ethereum network. It contains a collection of methods that are used by the
// higher level contract bindings to operate.
type BoundContract struct {
	address    common.Address     // Deployment address of the contract on the Ethereum blockchain
	abi        abi.ABI            // Reflect based ABI to access the correct Ethereum methods
	caller     ContractCaller     // Read interface to interact with the blockchain
	transactor ContractTransactor // Write interface to interact with the blockchain
	filterer   ContractFilterer   // Event filtering to interact with the blockchain
}

// NewBoundContract creates a low level contract interface through which calls
// and transactions may be made through.
func NewBoundContract(address common.Address, abi abi.ABI, caller ContractCaller, transactor ContractTransactor, filterer ContractFilterer) *BoundContract {
	return &BoundContract{
		address:    address,
		abi:        abi,
		caller:     caller,
		transactor: transactor,
		filterer:   filterer,
	}
}

// DeployContract deploys a contract onto the Ethereum blockchain and binds the
// deployment address with a Go wrapper.
func DeployContract(opts *TransactOpts, abi abi.ABI, bytecode []byte, backend ContractBackend, params ...interface{}) (common.Address, types.Transaction, *BoundContract, error) {
	// Otherwise try to deploy the contract
	c := NewBoundContract(common.Address{}, abi, backend, backend, backend)

	input, err := c.abi.Pack("", params...)
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	tx, err := c.transact(opts, nil, append(bytecode, input...))
	if err != nil {
		return common.Address{}, nil, nil, err
	}
	c.address = crypto.CreateAddress(opts.From, tx.GetNonce())
	return c.address, tx, c, nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (c *BoundContract) Call(opts *CallOpts, results *[]interface{}, method string, params ...interface{}) error {
	// Don't crash on a lazy user
	if opts == nil {
		opts = new(CallOpts)
	}
	if results == nil {
		results = new([]interface{})
	}
	// Pack the input, call and unpack the results
	input, err := c.abi.Pack(method, params...)
	if err != nil {
		return err
	}
	var (
		msg    = ethereum.CallMsg{From: opts.From, To: &c.address, Data: input}
		ctx    = ensureContext(opts.Context)
		code   []byte
		output []byte
	)
	if opts.Pending {
		pb, ok := c.caller.(PendingContractCaller)
		if !ok {
			return ErrNoPendingState
		}
		output, err = pb.PendingCallContract(ctx, msg)
		if err == nil && len(output) == 0 {
			// Make sure we have a contract to operate on, and bail out otherwise.
			if code, err = pb.PendingCodeAt(ctx, c.address); err != nil {
				return err
			} else if len(code) == 0 {
				return ErrNoCode
			}
		}
	} else {
		output, err = c.caller.CallContract(ctx, msg, opts.BlockNumber)
		if err != nil {
			return err
		}
		if len(output) == 0 {
			// Make sure we have a contract to operate on, and bail out otherwise.
			if code, err = c.caller.CodeAt(ctx, c.address, opts.BlockNumber); err != nil {
				return err
			} else if len(code) == 0 {
				return ErrNoCode
			}
		}
	}

	if len(*results) == 0 {
		res, err := c.abi.Unpack(method, output)
		*results = res
		return err
	}
	res := *results
	return c.abi.UnpackIntoInterface(res[0], method, output)
}

// Transact invokes the (paid) contract method with params as input values.
func (c *BoundContract) Transact(opts *TransactOpts, method string, params ...interface{}) (types.Transaction, error) {
	// Otherwise pack up the parameters and invoke the contract
	input, err := c.abi.Pack(method, params...)
	if err != nil {
		return nil, err
	}
	// todo(rjl493456442) check the method is payable or not,
	// reject invalid transaction at the first place
	return c.transact(opts, &c.address, input)
}

// RawTransact initiates a transaction with the given raw calldata as the input.
// It's usually used to initiate transactions for invoking **Fallback** function.
func (c *BoundContract) RawTransact(opts *TransactOpts, calldata []byte) (types.Transaction, error) {
	// todo(rjl493456442) check the method is payable or not,
	// reject invalid transaction at the first place
	return c.transact(opts, &c.address, calldata)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (c *BoundContract) Transfer(opts *TransactOpts) (types.Transaction, error) {
	// todo(rjl493456442) check the payable fallback or receive is defined
	// or not, reject invalid transaction at the first place
	return c.transact(opts, &c.address, nil)
}

// transact executes an actual transaction invocation, first deriving any missing
// authorization fields, and then scheduling the transaction for execution.
func (c *BoundContract) transact(opts *TransactOpts, contract *common.Address, input []byte) (types.Transaction, error) {
	var err error

	// Ensure a valid value field and resolve the account nonce
	value := uint256.NewInt(0)
	if opts.Value != nil {
		overflow := value.SetFromBig(opts.Value)
		if overflow {
			return nil, errors.New("opts.Value higher than 2^256-1")
		}
	}
	var nonce uint64
	if opts.Nonce == nil {
		nonce, err = c.transactor.PendingNonceAt(ensureContext(opts.Context), opts.From)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve account nonce: %w", err)
		}
	} else {
		nonce = opts.Nonce.Uint64()
	}
	// Figure out the gas allowance and gas price values
	gasPriceBig := opts.GasPrice
	if gasPriceBig == nil {
		gasPriceBig, err = c.transactor.SuggestGasPrice(ensureContext(opts.Context))
		if err != nil {
			return nil, fmt.Errorf("failed to suggest gas price: %v", err)
		}
	}
	gasPrice, overflow := uint256.FromBig(gasPriceBig)
	if overflow {
		return nil, errors.New("gasPriceBig higher than 2^256-1")
	}
	gasLimit := opts.GasLimit
	if gasLimit == 0 {
		// Gas estimation cannot succeed without code for method invocations
		if contract != nil {
			if code, codeErr := c.transactor.PendingCodeAt(ensureContext(opts.Context), c.address); codeErr != nil {
				return nil, codeErr
			} else if len(code) == 0 {
				return nil, ErrNoCode
			}
		}
		// If the contract surely has code (or code is not needed), estimate the transaction
		msg := ethereum.CallMsg{From: opts.From, To: contract, GasPrice: gasPrice, Value: value, Data: input}
		gasLimit, err = c.transactor.EstimateGas(ensureContext(opts.Context), msg)
		if err != nil {
			return nil, fmt.Errorf("failed to estimate gas needed: %w", err)
		}
	}
	// Create the transaction, sign it and schedule it for execution
	var rawTx types.Transaction
	if contract == nil {
		rawTx = types.NewContractCreation(nonce, value, gasLimit, gasPrice, input)
	} else {
		rawTx = types.NewTransaction(nonce, c.address, value, gasLimit, gasPrice, input)
	}
	if opts.Signer == nil {
		return nil, errors.New("no signer to authorize the transaction with")
	}
	signedTx, err := opts.Signer(opts.From, rawTx)
	if err != nil {
		return nil, err
	}
	if err := c.transactor.SendTransaction(ensureContext(opts.Context), signedTx); err != nil {
		return nil, err
	}
	return signedTx, nil
}

// FilterLogs filters contract logs for past blocks, returning the necessary
// channels to construct a strongly typed bound iterator on top of them.
func (c *BoundContract) FilterLogs(opts *FilterOpts, name string, query ...[]interface{}) (chan types.Log, event.Subscription, error) {
	// Don't crash on a lazy user
	if opts == nil {
		opts = new(FilterOpts)
	}
	// Append the event selector to the query parameters and construct the topic set
	query = append([][]interface{}{{c.abi.Events[name].ID}}, query...)

	topics, err := abi.MakeTopics(query...)
	if err != nil {
		return nil, nil, err
	}
	// Start the background filtering
	logs := make(chan types.Log, 128)

	config := ethereum.FilterQuery{
		Addresses: []common.Address{c.address},
		Topics:    topics,
		FromBlock: new(big.Int).SetUint64(opts.Start),
	}
	if opts.End != nil {
		config.ToBlock = new(big.Int).SetUint64(*opts.End)
	}
	/* TODO(karalabe): Replace the rest of the method below with this when supported
	sub, err := c.filterer.SubscribeFilterLogs(ensureContext(opts.Context), config, logs)
	*/
	buff, err := c.filterer.FilterLogs(ensureContext(opts.Context), config)
	if err != nil {
		return nil, nil, err
	}
	sub := event.NewSubscription(func(quit <-chan struct{}) error {
		for _, log := range buff {
			select {
			case logs <- log:
			case <-quit:
				return nil
			}
		}
		return nil
	})

	return logs, sub, nil
}

// WatchLogs filters subscribes to contract logs for future blocks, returning a
// subscription object that can be used to tear down the watcher.
func (c *BoundContract) WatchLogs(opts *WatchOpts, name string, query ...[]interface{}) (chan types.Log, event.Subscription, error) {
	// Don't crash on a lazy user
	if opts == nil {
		opts = new(WatchOpts)
	}
	// Append the event selector to the query parameters and construct the topic set
	query = append([][]interface{}{{c.abi.Events[name].ID}}, query...)

	topics, err := abi.MakeTopics(query...)
	if err != nil {
		return nil, nil, err
	}
	// Start the background filtering
	logs := make(chan types.Log, 128)

	config := ethereum.FilterQuery{
		Addresses: []common.Address{c.address},
		Topics:    topics,
	}
	if opts.Start != nil {
		config.FromBlock = new(big.Int).SetUint64(*opts.Start)
	}
	sub, err := c.filterer.SubscribeFilterLogs(ensureContext(opts.Context), config, logs)
	if err != nil {
		return nil, nil, err
	}
	return logs, sub, nil
}

// UnpackLog unpacks a retrieved log into the provided output structure.
func (c *BoundContract) UnpackLog(out interface{}, event string, log types.Log) error {
	if len(log.Data) > 0 {
		if err := c.abi.UnpackIntoInterface(out, event, log.Data); err != nil {
			return err
		}
	}
	var indexed abi.Arguments
	for _, arg := range c.abi.Events[event].Inputs {
		if arg.Indexed {
			indexed = append(indexed, arg)
		}
	}
	return abi.ParseTopics(out, indexed, log.Topics[1:])
}

// UnpackLogIntoMap unpacks a retrieved log into the provided map.
func (c *BoundContract) UnpackLogIntoMap(out map[string]interface{}, event string, log types.Log) error {
	if len(log.Data) > 0 {
		if err := c.abi.UnpackIntoMap(out, event, log.Data); err != nil {
			return err
		}
	}
	var indexed abi.Arguments
	for _, arg := range c.abi.Events[event].Inputs {
		if arg.Indexed {
			indexed = append(indexed, arg)
		}
	}
	return abi.ParseTopicsIntoMap(out, indexed, log.Topics[1:])
}

// ensureContext is a helper method to ensure a context is not nil, even if the
// user specified it as such.
func ensureContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.TODO()
	}
	return ctx
}
