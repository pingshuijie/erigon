// Copyright 2021 The Erigon Authors
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

package txpool

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	"github.com/holiman/uint256"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/txnprovider/txpool/txpoolcfg"

	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/gointerfaces"
	txpool_proto "github.com/erigontech/erigon-lib/gointerfaces/txpoolproto"
	"github.com/erigontech/erigon-lib/gointerfaces/typesproto"
	"github.com/erigontech/erigon-lib/kv"
)

// TxPoolAPIVersion
var TxPoolAPIVersion = &typesproto.VersionReply{Major: 1, Minor: 0, Patch: 0}

type txPool interface {
	ValidateSerializedTxn(serializedTxn []byte) error

	PeekBest(ctx context.Context, n int, txns *TxnsRlp, onTopOf, availableGas, availableBlobGas uint64, availableRlpSpace int) (bool, error)
	GetRlp(tx kv.Tx, hash []byte) ([]byte, error)
	AddLocalTxns(ctx context.Context, newTxns TxnSlots) ([]txpoolcfg.DiscardReason, error)
	deprecatedForEach(_ context.Context, f func(rlp []byte, sender common.Address, t SubPoolType), tx kv.Tx)
	CountContent() (int, int, int)
	IdHashKnown(tx kv.Tx, hash []byte) (bool, error)
	NonceFromAddress(addr [20]byte) (nonce uint64, inPool bool)
	GetBlobs(blobhashes []common.Hash) (blobBundles []PoolBlobBundle)
}

var _ txpool_proto.TxpoolServer = (*GrpcServer)(nil)   // compile-time interface check
var _ txpool_proto.TxpoolServer = (*GrpcDisabled)(nil) // compile-time interface check

var ErrPoolDisabled = errors.New("TxPool Disabled")

type GrpcDisabled struct {
	txpool_proto.UnimplementedTxpoolServer
}

func (*GrpcDisabled) Version(ctx context.Context, empty *emptypb.Empty) (*typesproto.VersionReply, error) {
	return nil, ErrPoolDisabled
}
func (*GrpcDisabled) FindUnknown(ctx context.Context, hashes *txpool_proto.TxHashes) (*txpool_proto.TxHashes, error) {
	return nil, ErrPoolDisabled
}
func (*GrpcDisabled) Add(ctx context.Context, request *txpool_proto.AddRequest) (*txpool_proto.AddReply, error) {
	return nil, ErrPoolDisabled
}
func (*GrpcDisabled) Transactions(ctx context.Context, request *txpool_proto.TransactionsRequest) (*txpool_proto.TransactionsReply, error) {
	return nil, ErrPoolDisabled
}
func (*GrpcDisabled) All(ctx context.Context, request *txpool_proto.AllRequest) (*txpool_proto.AllReply, error) {
	return nil, ErrPoolDisabled
}
func (*GrpcDisabled) Pending(ctx context.Context, empty *emptypb.Empty) (*txpool_proto.PendingReply, error) {
	return nil, ErrPoolDisabled
}
func (*GrpcDisabled) OnAdd(request *txpool_proto.OnAddRequest, server txpool_proto.Txpool_OnAddServer) error {
	return ErrPoolDisabled
}
func (*GrpcDisabled) Status(ctx context.Context, request *txpool_proto.StatusRequest) (*txpool_proto.StatusReply, error) {
	return nil, ErrPoolDisabled
}
func (*GrpcDisabled) Nonce(ctx context.Context, request *txpool_proto.NonceRequest) (*txpool_proto.NonceReply, error) {
	return nil, ErrPoolDisabled
}

type GrpcServer struct {
	txpool_proto.UnimplementedTxpoolServer
	ctx             context.Context
	txPool          txPool
	db              kv.RoDB
	newSlotsStreams *NewSlotsStreams

	chainID uint256.Int
	logger  log.Logger
}

func NewGrpcServer(ctx context.Context, txPool txPool, db kv.RoDB, newSlotsStreams *NewSlotsStreams, chainID uint256.Int, logger log.Logger) *GrpcServer {
	return &GrpcServer{ctx: ctx, txPool: txPool, db: db, newSlotsStreams: newSlotsStreams, chainID: chainID, logger: logger}
}

func (s *GrpcServer) Version(context.Context, *emptypb.Empty) (*typesproto.VersionReply, error) {
	return TxPoolAPIVersion, nil
}
func convertSubPoolType(t SubPoolType) txpool_proto.AllReply_TxnType {
	switch t {
	case PendingSubPool:
		return txpool_proto.AllReply_PENDING
	case BaseFeeSubPool:
		return txpool_proto.AllReply_BASE_FEE
	case QueuedSubPool:
		return txpool_proto.AllReply_QUEUED
	default:
		panic("unknown")
	}
}
func (s *GrpcServer) All(ctx context.Context, _ *txpool_proto.AllRequest) (*txpool_proto.AllReply, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	reply := &txpool_proto.AllReply{}
	reply.Txs = make([]*txpool_proto.AllReply_Tx, 0, 32)
	s.txPool.deprecatedForEach(ctx, func(rlp []byte, sender common.Address, t SubPoolType) {
		reply.Txs = append(reply.Txs, &txpool_proto.AllReply_Tx{
			Sender:  gointerfaces.ConvertAddressToH160(sender),
			TxnType: convertSubPoolType(t),
			RlpTx:   common.Copy(rlp),
		})
	}, tx)
	return reply, nil
}

func (s *GrpcServer) Pending(ctx context.Context, _ *emptypb.Empty) (*txpool_proto.PendingReply, error) {
	reply := &txpool_proto.PendingReply{}
	reply.Txs = make([]*txpool_proto.PendingReply_Tx, 0, 32)
	txnsRlp := TxnsRlp{}
	if _, err := s.txPool.PeekBest(ctx, math.MaxInt16, &txnsRlp, 0 /* onTopOf */, math.MaxUint64 /* availableGas */, math.MaxUint64 /* availableBlobGas */, math.MaxInt /* availableRlpSpace */); err != nil {
		return nil, err
	}
	var senderArr [20]byte
	for i := range txnsRlp.Txns {
		copy(senderArr[:], txnsRlp.Senders.At(i)) // TODO: optimize
		reply.Txs = append(reply.Txs, &txpool_proto.PendingReply_Tx{
			Sender:  gointerfaces.ConvertAddressToH160(senderArr),
			RlpTx:   txnsRlp.Txns[i],
			IsLocal: txnsRlp.IsLocal[i],
		})
	}
	return reply, nil
}

func (s *GrpcServer) FindUnknown(ctx context.Context, in *txpool_proto.TxHashes) (*txpool_proto.TxHashes, error) {
	return nil, errors.New("unimplemented")
}

func (s *GrpcServer) Add(ctx context.Context, in *txpool_proto.AddRequest) (*txpool_proto.AddReply, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var slots TxnSlots
	parseCtx := NewTxnParseContext(s.chainID).ChainIDRequired()
	parseCtx.ValidateRLP(s.txPool.ValidateSerializedTxn)

	reply := &txpool_proto.AddReply{Imported: make([]txpool_proto.ImportResult, len(in.RlpTxs)), Errors: make([]string, len(in.RlpTxs))}

	for i := 0; i < len(in.RlpTxs); i++ {
		j := len(slots.Txns) // some incoming txns may be rejected, so - need second index
		slots.Resize(uint(j + 1))
		slots.Txns[j] = &TxnSlot{}
		slots.IsLocal[j] = true
		if _, err := parseCtx.ParseTransaction(in.RlpTxs[i], 0, slots.Txns[j], slots.Senders.At(j), false /* hasEnvelope */, true /* wrappedWithBlobs */, func(hash []byte) error {
			if known, _ := s.txPool.IdHashKnown(tx, hash); known {
				return ErrAlreadyKnown
			}
			return nil
		}); err != nil {
			slots.Resize(uint(j))                // remove erroneous transaction
			if errors.Is(err, ErrAlreadyKnown) { // Noop, but need to handle to not count these
				reply.Errors[i] = txpoolcfg.AlreadyKnown.String()
				reply.Imported[i] = txpool_proto.ImportResult_ALREADY_EXISTS
			} else if errors.Is(err, ErrRlpTooBig) { // Noop, but need to handle to not count these
				reply.Errors[i] = txpoolcfg.RLPTooLong.String()
				reply.Imported[i] = txpool_proto.ImportResult_INVALID
			} else {
				reply.Errors[i] = err.Error()
				reply.Imported[i] = txpool_proto.ImportResult_INTERNAL_ERROR
			}
		}
	}

	discardReasons, err := s.txPool.AddLocalTxns(ctx, slots)
	if err != nil {
		return nil, err
	}

	j := 0
	for i := range reply.Imported {
		if reply.Imported[i] != txpool_proto.ImportResult_SUCCESS {
			j++
			continue
		}

		reply.Imported[i] = mapDiscardReasonToProto(discardReasons[j])
		reply.Errors[i] = discardReasons[j].String()
		j++
	}
	return reply, nil
}

func (s *GrpcServer) GetBlobs(ctx context.Context, in *txpool_proto.GetBlobsRequest) (*txpool_proto.GetBlobsReply, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	hashes := make([]common.Hash, len(in.BlobHashes))
	for i := range in.BlobHashes {
		hashes[i] = gointerfaces.ConvertH256ToHash(in.BlobHashes[i])
	}
	blobBundles := s.txPool.GetBlobs(hashes)
	blobs := make([][]byte, 0)
	proofs := make([][]byte, 0)
	for _, bb := range blobBundles {
		blobs = append(blobs, bb.Blob)
		if len(bb.Proofs) == 0 {
			proofs = append(proofs, nil)
		}
		for _, p := range bb.Proofs {
			proofs = append(proofs, p[:])
		}
	}
	reply := &txpool_proto.GetBlobsReply{Blobs: blobs, Proofs: proofs}
	return reply, nil
}

func mapDiscardReasonToProto(reason txpoolcfg.DiscardReason) txpool_proto.ImportResult {
	switch reason {
	case txpoolcfg.Success:
		return txpool_proto.ImportResult_SUCCESS
	case txpoolcfg.AlreadyKnown:
		return txpool_proto.ImportResult_ALREADY_EXISTS
	case txpoolcfg.UnderPriced, txpoolcfg.ReplaceUnderpriced, txpoolcfg.FeeTooLow:
		return txpool_proto.ImportResult_FEE_TOO_LOW
	case txpoolcfg.InvalidSender, txpoolcfg.NegativeValue, txpoolcfg.OversizedData, txpoolcfg.InitCodeTooLarge,
		txpoolcfg.RLPTooLong, txpoolcfg.InvalidCreateTxn, txpoolcfg.NoBlobs, txpoolcfg.TooManyBlobs,
		txpoolcfg.TypeNotActivated, txpoolcfg.UnequalBlobTxExt, txpoolcfg.BlobHashCheckFail,
		txpoolcfg.UnmatchedBlobTxExt, txpoolcfg.NoAuthorizations:
		// TODO(EIP-7702) TypeNotActivated may be transient (e.g. a set code transaction is submitted 1 sec prior to the Pectra activation)
		return txpool_proto.ImportResult_INVALID
	default:
		return txpool_proto.ImportResult_INTERNAL_ERROR
	}
}

func (s *GrpcServer) OnAdd(req *txpool_proto.OnAddRequest, stream txpool_proto.Txpool_OnAddServer) error {
	s.logger.Info("New txns subscriber joined")
	//txpool.Loop does send messages to this streams
	remove := s.newSlotsStreams.Add(stream)
	defer remove()
	select {
	case <-stream.Context().Done():
		return stream.Context().Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *GrpcServer) Transactions(ctx context.Context, in *txpool_proto.TransactionsRequest) (*txpool_proto.TransactionsReply, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	reply := &txpool_proto.TransactionsReply{RlpTxs: make([][]byte, len(in.Hashes))}
	for i := range in.Hashes {
		h := gointerfaces.ConvertH256ToHash(in.Hashes[i])
		txnRlp, err := s.txPool.GetRlp(tx, h[:])
		if err != nil {
			return nil, err
		}
		if txnRlp == nil {
			reply.RlpTxs[i] = []byte{}
			continue
		}
		reply.RlpTxs[i] = txnRlp
	}

	return reply, nil
}

func (s *GrpcServer) Status(_ context.Context, _ *txpool_proto.StatusRequest) (*txpool_proto.StatusReply, error) {
	pending, baseFee, queued := s.txPool.CountContent()
	return &txpool_proto.StatusReply{
		PendingCount: uint32(pending),
		QueuedCount:  uint32(queued),
		BaseFeeCount: uint32(baseFee),
	}, nil
}

// returns nonce for address
func (s *GrpcServer) Nonce(ctx context.Context, in *txpool_proto.NonceRequest) (*txpool_proto.NonceReply, error) {
	addr := gointerfaces.ConvertH160toAddress(in.Address)
	nonce, inPool := s.txPool.NonceFromAddress(addr)
	return &txpool_proto.NonceReply{
		Nonce: nonce,
		Found: inPool,
	}, nil
}

// NewSlotsStreams - it's safe to use this class as non-pointer
type NewSlotsStreams struct {
	chans map[uint]txpool_proto.Txpool_OnAddServer
	mu    sync.Mutex
	id    uint
}

func (s *NewSlotsStreams) Add(stream txpool_proto.Txpool_OnAddServer) (remove func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chans == nil {
		s.chans = make(map[uint]txpool_proto.Txpool_OnAddServer)
	}
	s.id++
	id := s.id
	s.chans[id] = stream
	return func() { s.remove(id) }
}

func (s *NewSlotsStreams) Broadcast(reply *txpool_proto.OnAddReply, logger log.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, stream := range s.chans {
		err := stream.Send(reply)
		if err != nil {
			logger.Debug("failed send to mined block stream", "err", err)
			select {
			case <-stream.Context().Done():
				delete(s.chans, id)
			default:
			}
		}
	}
}

func (s *NewSlotsStreams) remove(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.chans[id]
	if !ok { // double-unsubscribe support
		return
	}
	delete(s.chans, id)
}

func StartGrpc(txPoolServer txpool_proto.TxpoolServer, miningServer txpool_proto.MiningServer, addr string, creds *credentials.TransportCredentials, logger log.Logger) (*grpc.Server, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("could not create listener: %w, addr=%s", err, addr)
	}

	var (
		streamInterceptors []grpc.StreamServerInterceptor
		unaryInterceptors  []grpc.UnaryServerInterceptor
	)
	streamInterceptors = append(streamInterceptors, grpc_recovery.StreamServerInterceptor())
	unaryInterceptors = append(unaryInterceptors, grpc_recovery.UnaryServerInterceptor())

	//if metrics.Enabled {
	//	streamInterceptors = append(streamInterceptors, grpc_prometheus.StreamServerInterceptor)
	//	unaryInterceptors = append(unaryInterceptors, grpc_prometheus.UnaryServerInterceptor)
	//}

	//cpus := uint32(runtime.GOMAXPROCS(-1))
	opts := []grpc.ServerOption{
		//grpc.NumStreamWorkers(cpus), // reduce amount of goroutines
		grpc.ReadBufferSize(0),  // reduce buffers to save mem
		grpc.WriteBufferSize(0), // reduce buffers to save mem
		// Don't drop the connection, settings accordign to this comment on GitHub
		// https://github.com/grpc/grpc-go/issues/3171#issuecomment-552796779
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(streamInterceptors...)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(unaryInterceptors...)),
	}
	if creds == nil {
		// no specific opts
	} else {
		opts = append(opts, grpc.Creds(*creds))
	}
	grpcServer := grpc.NewServer(opts...)
	reflection.Register(grpcServer) // Register reflection service on gRPC server.
	if txPoolServer != nil {
		txpool_proto.RegisterTxpoolServer(grpcServer, txPoolServer)
	}
	if miningServer != nil {
		txpool_proto.RegisterMiningServer(grpcServer, miningServer)
	}

	//if metrics.Enabled {
	//	grpc_prometheus.Register(grpcServer)
	//}

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	go func() {
		defer healthServer.Shutdown()
		if err := grpcServer.Serve(lis); err != nil {
			logger.Error("private RPC server fail", "err", err)
		}
	}()
	logger.Info("Started gRPC server", "on", addr)
	return grpcServer, nil
}
