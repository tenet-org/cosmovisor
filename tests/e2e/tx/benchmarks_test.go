package tx_test

import (
	"context"
	"testing"

	"gotest.tools/v3/assert"

	sdkmath "cosmossdk.io/math"
	"cosmossdk.io/simapp"

	"github.com/cosmos/cosmos-sdk/client"
	clienttx "github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/cosmos/cosmos-sdk/testutil/cli"
	"github.com/cosmos/cosmos-sdk/testutil/network"
	"github.com/cosmos/cosmos-sdk/testutil/testdata"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authclient "github.com/cosmos/cosmos-sdk/x/auth/client"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

type E2EBenchmarkSuite struct {
	cfg     network.Config
	network *network.Network

	txHeight    int64
	queryClient tx.ServiceClient
}

// BenchmarkTx is lifted from E2ETestSuite from this package, with irrelevant state checks removed.
//
// Benchmark results:
//
// On a M1 macOS, on commit ab77fe20d3c00ef4cb73dfd0c803c4593a3c8233:
//
//	BenchmarkTx-8 4108 268935 ns/op
//
// By comparison, the benchmark modified for v0.47.0:
//
//	BenchmarkTx-8 3772 301750 ns/op
func BenchmarkTx(b *testing.B) {
	s := NewE2EBenchmarkSuite(b)
	b.Cleanup(s.Close)

	val := s.network.Validators[0]
	txBuilder := mkTxBuilder(b, s)
	// Convert the txBuilder to a tx.Tx.
	protoTx, err := txBuilderToProtoTx(txBuilder)
	assert.NilError(b, err)
	// Encode the txBuilder to txBytes.
	txBytes, err := val.ClientCtx.TxConfig.TxEncoder()(txBuilder.GetTx())
	assert.NilError(b, err)

	testCases := []struct {
		name string
		req  *tx.SimulateRequest
	}{
		{"valid request with proto tx (deprecated)", &tx.SimulateRequest{Tx: protoTx}},
		{"valid request with tx_bytes", &tx.SimulateRequest{TxBytes: txBytes}},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, tc := range testCases {
			// Broadcast the tx via gRPC via the validator's clientCtx (which goes
			// through Tendermint).
			res, err := s.queryClient.Simulate(context.Background(), tc.req)
			assert.NilError(b, err)
			// Check the result and gas used are correct.
			//
			// The 12 events are:
			// - Sending Fee to the pool: coin_spent, coin_received, transfer and message.sender=<val1>
			// - tx.* events: tx.fee, tx.acc_seq, tx.signature
			// - Sending Amount to recipient: coin_spent, coin_received, transfer and message.sender=<val1>
			// - Msg events: message.module=bank and message.action=/cosmos.bank.v1beta1.MsgSend (in one message)
			assert.Equal(b, 10, len(res.GetResult().GetEvents()))
			assert.Assert(b, res.GetGasInfo().GetGasUsed() > 0) // Gas used sometimes change, just check it's not empty.
		}
	}
}

func NewE2EBenchmarkSuite(tb testing.TB) *E2EBenchmarkSuite {
	tb.Helper()

	s := new(E2EBenchmarkSuite)

	cfg := network.DefaultConfig(simapp.NewTestNetworkFixture)
	cfg.NumValidators = 1
	s.cfg = cfg

	var err error
	s.network, err = network.New(tb, tb.TempDir(), s.cfg)
	assert.NilError(tb, err)

	val := s.network.Validators[0]
	assert.NilError(tb, s.network.WaitForNextBlock())

	s.queryClient = tx.NewServiceClient(val.ClientCtx)

	msgSend := &banktypes.MsgSend{
		FromAddress: val.Address.String(),
		ToAddress:   val.Address.String(),
		Amount:      sdk.NewCoins(sdk.NewCoin(s.cfg.BondDenom, sdkmath.NewInt(10))),
	}

	// Create a new MsgSend tx from val to itself.
	out, err := cli.SubmitTestTx(
		val.ClientCtx,
		msgSend,
		val.Address,
		cli.TestTxConfig{
			Memo: "foobar",
		},
	)

	assert.NilError(tb, err)

	var txRes tx.BroadcastTxResponse
	assert.NilError(tb, val.ClientCtx.Codec.UnmarshalJSON(out.Bytes(), &txRes))
	assert.Equal(tb, uint32(0), txRes.TxResponse.Code, txRes)

	msgSend1 := &banktypes.MsgSend{
		FromAddress: val.Address.String(),
		ToAddress:   val.Address.String(),
		Amount:      sdk.NewCoins(sdk.NewCoin(s.cfg.BondDenom, sdkmath.NewInt(1))),
	}

	out1, err := cli.SubmitTestTx(
		val.ClientCtx,
		msgSend1,
		val.Address,
		cli.TestTxConfig{
			Offline: true,
			AccNum:  0,
			Seq:     2,
			Memo:    "foobar",
		},
	)

	assert.NilError(tb, err)
	var tr tx.BroadcastTxResponse
	assert.NilError(tb, val.ClientCtx.Codec.UnmarshalJSON(out1.Bytes(), &tr))
	assert.Equal(tb, uint32(0), tr.TxResponse.Code)

	resp, err := cli.GetTxResponse(s.network, val.ClientCtx, tr.TxResponse.TxHash)
	assert.NilError(tb, err)
	s.txHeight = resp.Height
	return s
}

func (s *E2EBenchmarkSuite) Close() {
	s.network.Cleanup()
}

func mkTxBuilder(tb testing.TB, s *E2EBenchmarkSuite) client.TxBuilder {
	tb.Helper()

	val := s.network.Validators[0]
	assert.NilError(tb, s.network.WaitForNextBlock())

	// prepare txBuilder with msg
	txBuilder := val.ClientCtx.TxConfig.NewTxBuilder()
	feeAmount := sdk.Coins{sdk.NewInt64Coin(s.cfg.BondDenom, 10)}
	gasLimit := testdata.NewTestGasLimit()
	assert.NilError(tb,
		txBuilder.SetMsgs(&banktypes.MsgSend{
			FromAddress: val.Address.String(),
			ToAddress:   val.Address.String(),
			Amount:      sdk.Coins{sdk.NewInt64Coin(s.cfg.BondDenom, 10)},
		}),
	)
	txBuilder.SetFeeAmount(feeAmount)
	txBuilder.SetGasLimit(gasLimit)
	txBuilder.SetMemo("foobar")

	// setup txFactory
	txFactory := clienttx.Factory{}.
		WithChainID(val.ClientCtx.ChainID).
		WithKeybase(val.ClientCtx.Keyring).
		WithTxConfig(val.ClientCtx.TxConfig).
		WithSignMode(signing.SignMode_SIGN_MODE_DIRECT)

	// Sign Tx.
	err := authclient.SignTx(txFactory, val.ClientCtx, val.Moniker, txBuilder, false, true)
	assert.NilError(tb, err)

	return txBuilder
}
