package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/breez/breez/data"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
)

var targets = []int{2, 6, 25}

func (a *Service) PublishTransaction(txHex []byte) error {
	walletKitClient := a.daemonAPI.WalletKitClient()
	pr, err := walletKitClient.PublishTransaction(context.Background(), &walletrpc.Transaction{TxHex: txHex})
	if err != nil {
		a.log.Errorf("walletKitClient.PublishTransaction(%x): %v", txHex, err)
		return fmt.Errorf("walletKitClient.PublishTransaction(%x): %w", txHex, err)
	}
	if pr.PublishError != "" {
		a.log.Errorf("walletKitClient.PublishTransaction(%x): %v", txHex, pr.PublishError)
		return fmt.Errorf("walletKitClient.PublishTransaction(%x): %v", txHex, pr.PublishError)
	}

	return nil
}

func (a *Service) determineFeePerKw(confTarget int) (chainfee.SatPerKWeight, error) {
	walletKitClient := a.daemonAPI.WalletKitClient()
	if walletKitClient == nil {
		return 0, fmt.Errorf("API not ready")
	}
	feeResponse, err := walletKitClient.EstimateFee(context.Background(),
		&walletrpc.EstimateFeeRequest{ConfTarget: int32(confTarget)})
	if err != nil {
		return 0, fmt.Errorf("walletKitClient.EstimateFee(%v): %w", confTarget, err)
	}
	return chainfee.SatPerKWeight(feeResponse.SatPerKw), nil
}

/*
SweepAllCoinsTransactions executes a request to send wallet coins to a particular address.
*/
func (a *Service) SweepAllCoinsTransactions(address string) (*data.SweepAllCoinsTransactions, error) {

	// Decode the address receiving the coins, we need to check whether the
	// address is valid for this network.
	targetAddr, err := btcutil.DecodeAddress(address, a.activeParams)
	if err != nil {
		return nil, err
	}

	// Make the check on the decoded address according to the active network.
	if !targetAddr.IsForNet(a.activeParams) {
		return nil, fmt.Errorf("address: %v is not valid for this "+
			"network: %v", targetAddr.String(),
			a.activeParams.Name)
	}

	// If the destination address parses to a valid pubkey, we assume the user
	// accidentally tried to send funds to a bare pubkey address. This check is
	// here to prevent unintended transfers.
	decodedAddr, _ := hex.DecodeString(address)
	_, err = btcec.ParsePubKey(decodedAddr, btcec.S256())
	if err == nil {
		return nil, fmt.Errorf("cannot send coins to pubkeys")
	}

	lnClient := a.daemonAPI.APIClient()
	info, err := lnClient.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
	if err != nil {
		a.log.Errorf("lnClient.GetInfo: %v", err)
		return nil, fmt.Errorf("lnClient.GetInfo: %w", err)
	}
	td := make(map[int32]*data.TransactionDetails)
	var totalAmount int64
	for _, confTarget := range targets {
		feePerKw, err := a.determineFeePerKw(confTarget)
		if err != nil {
			return nil, fmt.Errorf("a.determineFeePerKw(%v): %w", confTarget, err)
		}
		rus := NewRpcUtxoSource(lnClient)
		sweepTxPkg, err := sweep.CraftSweepAllTx(
			feePerKw,
			lnwallet.DefaultDustLimit(),
			info.BlockHeight,
			nil,
			targetAddr,
			&nilCoinSelectionLocker,
			rus,
			&nilOutpointLocker,
			nil,
			NewRpcSigner(a.daemonAPI.SignerClient()),
		)

		if err != nil {
			// ignore validation errors of crafting specific transaction.
			var ruleErr blockchain.RuleError
			if errors.As(err, &ruleErr) {
				continue
			}
			return nil, fmt.Errorf("sweep.CraftSweepAllTx(): %w", err)
		}

		var amtOut int64
		for _, output := range sweepTxPkg.SweepTx.TxOut {
			amtOut += output.Value
		}

		var rawTx bytes.Buffer
		err = sweepTxPkg.SweepTx.Serialize(&rawTx)
		if err != nil {
			return nil, fmt.Errorf("tx.Serialize %#v: %w", sweepTxPkg.SweepTx, err)
		}
		td[int32(confTarget)] = &data.TransactionDetails{
			Tx:     rawTx.Bytes(),
			TxHash: sweepTxPkg.SweepTx.TxHash().String(),
			Fees:   rus.totalAmount - amtOut,
		}
		totalAmount = rus.totalAmount
	}
	return &data.SweepAllCoinsTransactions{Amt: totalAmount, Transactions: td}, nil
}

type coinSelectionLocker struct{}

func (m *coinSelectionLocker) WithCoinSelectLock(f func() error) error {
	return f()
}

var nilCoinSelectionLocker coinSelectionLocker

type outpointLocker struct{}

func (m *outpointLocker) LockOutpoint(o wire.OutPoint)   {}
func (m *outpointLocker) UnlockOutpoint(o wire.OutPoint) {}

var nilOutpointLocker outpointLocker

type rpcUtxoSource struct {
	lightningClient lnrpc.LightningClient
	totalAmount     int64
}

func NewRpcUtxoSource(c lnrpc.LightningClient) *rpcUtxoSource {
	return &rpcUtxoSource{
		lightningClient: c,
	}
}

func (u *rpcUtxoSource) ListUnspentWitness(minConfs, maxConfs int32) ([]*lnwallet.Utxo, error) {
	utxoOutputs, err := u.lightningClient.ListUnspent(context.Background(), &lnrpc.ListUnspentRequest{
		MinConfs: 1, MaxConfs: math.MaxInt32,
	})
	if err != nil {
		//a.log.Errorf("u.lightningClient.ListUnspent: %v", err)
		return nil, fmt.Errorf("u.lightningClient.ListUnspent: %w", err)
	}
	lu := make([]*lnwallet.Utxo, 0, len(utxoOutputs.Utxos))
	u.totalAmount = 0
	for _, utxo := range utxoOutputs.Utxos {

		var addrType lnwallet.AddressType
		switch utxo.AddressType {
		case lnrpc.AddressType_WITNESS_PUBKEY_HASH:
			addrType = lnwallet.WitnessPubKey

		case lnrpc.AddressType_NESTED_PUBKEY_HASH:
			addrType = lnwallet.NestedWitnessPubKey

		default:
			return nil, fmt.Errorf("invalid utxo address type")
		}

		pkScript, err := hex.DecodeString(utxo.PkScript)
		if err != nil {
			return nil, fmt.Errorf("hex.DecodeString(%v): %w", utxo.PkScript, err)
		}

		var outPointHash chainhash.Hash
		err = outPointHash.SetBytes(utxo.Outpoint.TxidBytes)
		if err != nil {
			return nil, fmt.Errorf("outPointHash.SetBytes(%x): %w", utxo.Outpoint.TxidBytes, err)
		}

		lu = append(lu, &lnwallet.Utxo{
			AddressType:   addrType,
			Value:         btcutil.Amount(utxo.AmountSat),
			Confirmations: utxo.Confirmations,
			PkScript:      pkScript,
			OutPoint: wire.OutPoint{
				Hash:  outPointHash,
				Index: utxo.Outpoint.OutputIndex,
			},
		})
		u.totalAmount += utxo.AmountSat
	}
	return lu, nil
}

type rpcSigner struct {
	signerClient signrpc.SignerClient
}

func NewRpcSigner(s signrpc.SignerClient) *rpcSigner {
	return &rpcSigner{
		signerClient: s,
	}
}

func (s *rpcSigner) SignOutputRaw(tx *wire.MsgTx, signDesc *input.SignDescriptor) (input.Signature, error) {
	//if s.tx == nil {
	//	s.tx = tx.Copy()
	//}
	var rawTx bytes.Buffer
	err := tx.Serialize(&rawTx)
	if err != nil {
		return nil, fmt.Errorf("tx.Serialize %#v: %w", tx, err)
	}
	signDescriptor := []*signrpc.SignDescriptor{&signrpc.SignDescriptor{
		Output: &signrpc.TxOut{
			Value:    signDesc.Output.Value,
			PkScript: signDesc.Output.PkScript,
		},
		Sighash:       uint32(signDesc.HashType),
		WitnessScript: signDesc.WitnessScript,
		InputIndex:    int32(signDesc.InputIndex),
	}}

	srpc := s.signerClient
	r, err := srpc.SignOutputRaw(context.Background(), &signrpc.SignReq{
		RawTxBytes: rawTx.Bytes(),
		SignDescs:  signDescriptor,
		SigHashes: &signrpc.TxSigHashes{
			HashPrevOuts: signDesc.SigHashes.HashPrevOuts[:],
			HashSequence: signDesc.SigHashes.HashSequence[:],
			HashOutputs:  signDesc.SigHashes.HashOutputs[:],
		},
	})
	if err != nil {
		return nil, fmt.Errorf("srpc.SignOutputRaw %#v: %w", tx, err)
	}
	return btcec.ParseDERSignature(r.RawSigs[0], btcec.S256())
}

func (s *rpcSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *input.SignDescriptor) (*input.Script, error) {
	//if s.tx == nil {
	//	s.tx = tx.Copy()
	//}
	var rawTx bytes.Buffer
	err := tx.Serialize(&rawTx)
	if err != nil {
		return nil, fmt.Errorf("tx.Serialize %#v: %w", tx, err)
	}
	signDescriptor := []*signrpc.SignDescriptor{&signrpc.SignDescriptor{
		Output: &signrpc.TxOut{
			Value:    signDesc.Output.Value,
			PkScript: signDesc.Output.PkScript,
		},
		Sighash:       uint32(signDesc.HashType),
		WitnessScript: signDesc.WitnessScript,
		InputIndex:    int32(signDesc.InputIndex),
	}}
	srpc := s.signerClient
	r, err := srpc.ComputeInputScript(context.Background(), &signrpc.SignReq{
		RawTxBytes: rawTx.Bytes(),
		SignDescs:  signDescriptor,
		SigHashes: &signrpc.TxSigHashes{
			HashPrevOuts: signDesc.SigHashes.HashPrevOuts[:],
			HashSequence: signDesc.SigHashes.HashSequence[:],
			HashOutputs:  signDesc.SigHashes.HashOutputs[:],
		},
	})
	if err != nil {
		return nil, fmt.Errorf("srpc.ComputeInputScript: %w", err)
	}
	var hashPrevOuts, hashSequence, hashOutputs chainhash.Hash
	if err := hashPrevOuts.SetBytes(r.SigHashes.HashPrevOuts); err != nil {
		return nil, fmt.Errorf("bad HashPrevOuts: %w", err)
	}
	if err := hashSequence.SetBytes(r.SigHashes.HashSequence); err != nil {
		return nil, fmt.Errorf("bad HashSequence: %w", err)
	}
	if err := hashOutputs.SetBytes(r.SigHashes.HashOutputs); err != nil {
		return nil, fmt.Errorf("bad HashOutputs: %w", err)
	}
	signDesc.SigHashes.HashPrevOuts = hashPrevOuts
	signDesc.SigHashes.HashSequence = hashSequence
	signDesc.SigHashes.HashOutputs = hashOutputs
	return &input.Script{
		Witness:   r.InputScripts[0].Witness,
		SigScript: r.InputScripts[0].SigScript,
	}, nil
}
