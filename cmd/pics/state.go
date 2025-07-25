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

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon-lib/chain"
	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/crypto"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/kv/memdb"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/trie"
	"github.com/erigontech/erigon-lib/types"
	"github.com/erigontech/erigon/cmd/pics/contracts"
	"github.com/erigontech/erigon/cmd/pics/visual"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/execution/abi/bind"
	"github.com/erigontech/erigon/execution/abi/bind/backends"
	"github.com/erigontech/erigon/execution/stages/mock"
)

/*func statePicture(t *trie.Trie, number int, keyCompression int, codeCompressed bool, valCompressed bool,
	quadTrie bool, quadColors bool, highlights [][]byte) (*trie.Trie, error) {
	filename := fmt.Sprintf("state_%d.dot", number)
	f, err := os.Create(filename)
	if err != nil {
		return nil, err
	}
	indexColors := visual.HexIndexColors
	fontColors := visual.HexFontColors
	if quadTrie {
		t = trie.HexToQuad(t)
	}
	if quadColors {
		indexColors = visual.QuadIndexColors
		fontColors = visual.QuadFontColors
	}
	visual.StartGraph(f, false)
	trie.Visual(t, f, &trie.VisualOpts{
		Highlights:     highlights,
		IndexColors:    indexColors,
		FontColors:     fontColors,
		Values:         true,
		CutTerminals:   keyCompression,
		CodeCompressed: codeCompressed,
		ValCompressed:  valCompressed,
		ValHex:         true,
	})
	visual.EndGraph(f)
	if err := f.Close(); err != nil {
		return nil, err
	}
	//nolint:gosec
	cmd := exec.Command("dot", "-Tpng:gd", "-o"+dot2png(filename), filename)
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("error: %v, output: %s\n", err, output)
	}
	return t, nil
}*/

var bucketLabels = map[string]string{
	kv.Headers:                  "Headers",
	kv.HeaderCanonical:          "Canonical headers",
	kv.HeaderTD:                 "Headers TD",
	kv.BlockBody:                "Block Bodies",
	kv.HeaderNumber:             "Header Numbers",
	kv.TxLookup:                 "Transaction Index",
	kv.Code:                     "Code Of Contracts",
	kv.SyncStageProgress:        "Sync Progress",
	kv.PlainState:               "Plain State",
	kv.HashedAccountsDeprecated: "Hashed Accounts",
	kv.HashedStorageDeprecated:  "Hashed Storage",
	kv.IncarnationMap:           "Incarnations",
	kv.Senders:                  "Transaction Senders",
}

/*dbutils.PlainContractCode,
dbutils.Code,
dbutils.AccountsHistory,
dbutils.StorageHistory,
dbutils.TxLookup,*/

func hexPalette() error {
	filename := "hex_palette.dot"
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	visual.StartGraph(f, true)
	p := common.FromHex("0x000102030405060708090a0b0c0d0e0f")
	visual.Horizontal(f, p, len(p), "p", visual.HexIndexColors, visual.HexFontColors, 0)
	visual.EndGraph(f)
	if err := f.Close(); err != nil {
		return err
	}
	//nolint:gosec
	cmd := exec.Command("dot", "-Tpng:gd", "-o"+dot2png(filename), filename)
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("error: %v, output: %s\n", err, output)
	}
	return nil
}

func stateDatabaseComparison(first kv.RwDB, second kv.RwDB, number int) error {
	filename := fmt.Sprintf("changes_%d.dot", number)
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	i := 0
	visual.StartGraph(f, true)
	m := make(map[string][]int)
	noValues := make(map[int]struct{})
	perBucketFiles := make(map[string]*os.File)

	if err = second.View(context.Background(), func(readTx kv.Tx) error {
		return first.View(context.Background(), func(firstTx kv.Tx) error {
			for bucketName := range bucketLabels {
				bucketName := bucketName
				if err := readTx.ForEach(bucketName, nil, func(k, v []byte) error {
					if firstV, _ := firstTx.GetOne(bucketName, k); firstV != nil && bytes.Equal(v, firstV) {
						// Skip the record that is the same as in the first Db
						return nil
					}
					// Produce pair of nodes
					keyKeyBytes := &trie.Keybytes{
						Data:        k,
						Odd:         false,
						Terminating: false,
					}
					valKeyBytes := &trie.Keybytes{
						Data:        v,
						Odd:         false,
						Terminating: false,
					}
					val := valKeyBytes.ToHex()
					key := keyKeyBytes.ToHex()
					var f1 *os.File
					var ok bool
					if f1, ok = perBucketFiles[bucketName]; !ok {
						f1, err = os.Create(fmt.Sprintf("changes_%d_%s.dot", number, strings.ReplaceAll(bucketLabels[bucketName], " ", "")))
						if err != nil {
							return err
						}
						visual.StartGraph(f1, true)
						var clusterLabel string
						var ok bool
						if clusterLabel, ok = bucketLabels[bucketName]; !ok {
							clusterLabel = bucketName
						}
						visual.StartCluster(f1, 0, clusterLabel)
						perBucketFiles[bucketName] = f1
					}
					visual.Horizontal(f1, key, len(key), fmt.Sprintf("k_%d", i), visual.HexIndexColors, visual.HexFontColors, 0)
					if len(val) > 0 {
						if len(val) > 64 {
							visual.HexBox(f1, fmt.Sprintf("v_%d", i), val, 64, false /*compresses*/, true /*highlighted*/)
						} else {
							visual.Horizontal(f1, val, len(val), fmt.Sprintf("v_%d", i), visual.HexIndexColors, visual.HexFontColors, 0)
						}
						// Produce edge
						fmt.Fprintf(f1, "k_%d -> v_%d;\n", i, i)
					} else {
						noValues[i] = struct{}{}
					}
					visual.Horizontal(f, key, 0, fmt.Sprintf("k_%d", i), visual.HexIndexColors, visual.HexFontColors, 0)
					if len(val) > 0 {
						if len(val) > 64 {
							visual.HexBox(f, fmt.Sprintf("v_%d", i), val, 64, false /*compressed*/, false /*highlighted*/)
						} else {
							visual.Horizontal(f, val, 0, fmt.Sprintf("v_%d", i), visual.HexIndexColors, visual.HexFontColors, 0)
						}
						// Produce edge
						fmt.Fprintf(f, "k_%d -> v_%d;\n", i, i)
					} else {
						noValues[i] = struct{}{}
					}
					lst := m[bucketName]
					lst = append(lst, i)
					m[bucketName] = lst
					i++
					return nil
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}); err != nil {
		return err
	}
	n := 0
	for prefix, lst := range m {
		var clusterLabel string
		var ok bool
		if clusterLabel, ok = bucketLabels[prefix]; !ok {
			clusterLabel = prefix
		}
		if len(lst) == 0 {
			continue
		}
		visual.StartCluster(f, n, clusterLabel)
		for _, item := range lst {
			if _, ok1 := noValues[item]; ok1 {
				fmt.Fprintf(f, "k_%d;", item)
			} else {
				fmt.Fprintf(f, "k_%d;v_%d;", item, item)
			}
		}
		fmt.Fprintf(f, "\n")
		visual.EndCluster(f)
		n++
	}
	visual.EndGraph(f)
	if err := f.Close(); err != nil {
		return err
	}
	//nolint:gosec
	cmd := exec.Command("dot", "-Tpng:gd", "-o"+dot2png(filename), filename)
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("error: %v, output: %s\n", err, output)
	}
	for _, f1 := range perBucketFiles {
		fmt.Fprintf(f1, "\n")
		visual.EndCluster(f1)
		visual.EndGraph(f1)
		if err := f1.Close(); err != nil {
			return err
		}
		//nolint:gosec
		cmd := exec.Command("dot", "-Tpng:gd", "-o"+dot2png(f1.Name()), f1.Name())
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("error: %v, output: %s\n", err, output)
		}
	}
	return nil
}

func dot2png(dotFileName string) string {
	return strings.TrimSuffix(dotFileName, filepath.Ext(dotFileName)) + ".png"
}

func initialState1() error {
	defer log.Root().SetHandler(log.Root().GetHandler())
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlInfo, log.StderrHandler))
	fmt.Printf("Initial state 1\n")
	// Configure and generate a sample block chain
	var (
		key, _   = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key1, _  = crypto.HexToECDSA("49a7b37aa6f6645917e7b807e9d1c00d4fa71f18343b0d4122a4d2df64dd6fee")
		key2, _  = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		address  = crypto.PubkeyToAddress(key.PublicKey)
		address1 = crypto.PubkeyToAddress(key1.PublicKey)
		address2 = crypto.PubkeyToAddress(key2.PublicKey)
		theAddr  = common.Address{1}
		gspec    = &types.Genesis{
			Config: chain.AllProtocolChanges,
			Alloc: types.GenesisAlloc{
				address:  {Balance: big.NewInt(9000000000000000000)},
				address1: {Balance: big.NewInt(200000000000000000)},
				address2: {Balance: big.NewInt(300000000000000000)},
			},
			GasLimit: 10000000,
		}
		// this code generates a log
		signer = types.MakeSigner(chain.AllProtocolChanges, 1, 0)
	)
	m := mock.MockWithGenesis(nil, gspec, key, false)
	defer m.DB.Close()

	contractBackend := backends.NewSimulatedBackendWithConfig(nil, gspec.Alloc, gspec.Config, gspec.GasLimit)
	defer contractBackend.Close()
	transactOpts, err := bind.NewKeyedTransactorWithChainID(key, m.ChainConfig.ChainID)
	if err != nil {
		panic(err)
	}
	transactOpts1, err := bind.NewKeyedTransactorWithChainID(key1, m.ChainConfig.ChainID)
	if err != nil {
		panic(err)
	}
	transactOpts2, err := bind.NewKeyedTransactorWithChainID(key2, m.ChainConfig.ChainID)
	if err != nil {
		panic(err)
	}

	var tokenContract *contracts.Token
	// We generate the blocks without plainstant because it's not supported in core.GenerateChain
	chain, err := core.GenerateChain(m.ChainConfig, m.Genesis, m.Engine, m.DB, 8, func(i int, block *core.BlockGen) {
		var (
			txn types.Transaction
			txs []types.Transaction
			err error
		)

		ctx := context.Background()
		switch i {
		case 0:
			txn, err = types.SignTx(types.NewTransaction(0, theAddr, uint256.NewInt(1000000000000000), 21000, new(uint256.Int), nil), *signer, key)
			if err != nil {
				panic(err)
			}
			err = contractBackend.SendTransaction(ctx, txn)
			if err != nil {
				panic(err)
			}
		case 1:
			txn, err = types.SignTx(types.NewTransaction(1, theAddr, uint256.NewInt(1000000000000000), 21000, new(uint256.Int), nil), *signer, key)
			if err != nil {
				panic(err)
			}
			err = contractBackend.SendTransaction(ctx, txn)
			if err != nil {
				panic(err)
			}
		case 2:
			_, txn, tokenContract, err = contracts.DeployToken(transactOpts, contractBackend, address1)
		case 3:
			txn, err = tokenContract.Mint(transactOpts1, address2, big.NewInt(10))
		case 4:
			txn, err = tokenContract.Transfer(transactOpts2, address, big.NewInt(3))
		case 5:
			// Multiple transactions sending small amounts of ether to various accounts
			var j uint64
			var toAddr common.Address
			nonce := block.TxNonce(address)
			for j = 1; j <= 32; j++ {
				binary.BigEndian.PutUint64(toAddr[:], j)
				txn, err = types.SignTx(types.NewTransaction(nonce, toAddr, uint256.NewInt(1000000000000000), 21000, new(uint256.Int), nil), *signer, key)
				if err != nil {
					panic(err)
				}
				err = contractBackend.SendTransaction(ctx, txn)
				if err != nil {
					panic(err)
				}
				txs = append(txs, txn)
				nonce++
			}
		case 6:
			_, txn, tokenContract, err = contracts.DeployToken(transactOpts, contractBackend, address1)
			if err != nil {
				panic(err)
			}
			txs = append(txs, txn)
			txn, err = tokenContract.Mint(transactOpts1, address2, big.NewInt(100))
			if err != nil {
				panic(err)
			}
			txs = append(txs, txn)
			// Multiple transactions sending small amounts of ether to various accounts
			var j uint64
			var toAddr common.Address
			for j = 1; j <= 32; j++ {
				binary.BigEndian.PutUint64(toAddr[:], j)
				txn, err = tokenContract.Transfer(transactOpts2, toAddr, big.NewInt(1))
				if err != nil {
					panic(err)
				}
				txs = append(txs, txn)
			}
		case 7:
			var toAddr common.Address
			nonce := block.TxNonce(address)
			binary.BigEndian.PutUint64(toAddr[:], 4)
			txn, err = types.SignTx(types.NewTransaction(nonce, toAddr, uint256.NewInt(1000000000000000), 21000, new(uint256.Int), nil), *signer, key)
			if err != nil {
				panic(err)
			}
			err = contractBackend.SendTransaction(ctx, txn)
			if err != nil {
				panic(err)
			}
			txs = append(txs, txn)
			binary.BigEndian.PutUint64(toAddr[:], 12)
			txn, err = tokenContract.Transfer(transactOpts2, toAddr, big.NewInt(1))
			if err != nil {
				panic(err)
			}
			txs = append(txs, txn)
		}

		if err != nil {
			panic(err)
		}
		if txs == nil && txn != nil {
			txs = append(txs, txn)
		}

		for _, txn := range txs {
			block.AddTx(txn)
		}
		contractBackend.Commit()
	})
	if err != nil {
		return err
	}
	m2 := mock.MockWithGenesis(nil, gspec, key, false)
	defer m2.DB.Close()

	if err = hexPalette(); err != nil {
		return err
	}

	emptyKv := memdb.New("", kv.ChainDB)
	if err = stateDatabaseComparison(emptyKv, m.DB, 0); err != nil {
		return err
	}
	defer emptyKv.Close()

	// BLOCKS

	for i := 0; i < chain.Length(); i++ {
		if err = m2.InsertChain(chain.Slice(i, i+1)); err != nil {
			return err
		}
		if err = stateDatabaseComparison(m.DB, m2.DB, i+1); err != nil {
			return err
		}
		if err = m.InsertChain(chain.Slice(i, i+1)); err != nil {
			return err
		}
	}

	if err = stateDatabaseComparison(emptyKv, m.DB, 9); err != nil {
		return err
	}
	return nil
}
