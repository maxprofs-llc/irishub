package auth

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/irisnet/irishub/codec"
	sdk "github.com/irisnet/irishub/types"
	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/crypto/multisig"
	"github.com/tendermint/tendermint/crypto/secp256k1"
)

const (
	ed25519VerifyCost   = 59
	secp256k1VerifyCost = 100

	// if gas > gasShift, gas = log(gas)/log(gasBase)
	// else gasConsumed = gas
	gasBase  = 1.02 // gas logarithm base
	gasShift = 285  // gas logarithm shift
)

// NewAnteHandler returns an AnteHandler that checks
// and increments sequence numbers, checks signatures & account numbers,
// and deducts fees from the first signer.
func NewAnteHandler(am AccountKeeper, fck FeeKeeper) sdk.AnteHandler {
	return func(
		ctx sdk.Context, tx sdk.Tx, simulate bool,
	) (newCtx sdk.Context, res sdk.Result, abort bool) {

		// This AnteHandler requires Txs to be StdTxs
		stdTx, ok := tx.(StdTx)
		if !ok {
			// Set a gas meter with limit 0 as to prevent an infinite gas meter attack
			// during runTx.
			newCtx = setGasMeter(simulate, ctx, 0)
			return newCtx, sdk.ErrInternal("tx must be StdTx").Result(), true
		}

		// Ensure that the provided fees meet a minimum threshold for the validator, if this is a CheckTx.
		// This is only for local mempool purposes, and thus is only ran on check tx.
		if ctx.IsCheckTx() && !simulate {
			res := ensureSufficientMempoolFees(ctx, stdTx)
			if !res.IsOK() {
				return newCtx, res, true
			}
		}

		newCtx = setGasMeter(simulate, ctx, stdTx.Fee.Gas)

		// AnteHandlers must have their own defer/recover in order
		// for the BaseApp to know how much gas was used!
		// This is because the GasMeter is created in the AnteHandler,
		// but if it panics the context won't be set properly in runTx's recover ...
		defer func() {
			if r := recover(); r != nil {
				switch rType := r.(type) {
				case sdk.ErrorOutOfGas:
					log := fmt.Sprintf("out of gas in location: %v", rType.Descriptor)
					res = sdk.ErrOutOfGas(log).Result()
					res.GasWanted = stdTx.Fee.Gas
					res.GasUsed = newCtx.GasMeter().GasConsumed()
					abort = true
				default:
					panic(r)
				}
			}
		}()

		if err := tx.ValidateBasic(); err != nil {
			return newCtx, err.Result(), true
		}

		// stdSigs contains the sequence number, account number, and signatures.
		// When simulating, this would just be a 0-length slice.
		stdSigs := stdTx.GetSignatures()
		signerAddrs := stdTx.GetSigners()

		// create the list of all sign bytes
		signBytesList := getSignBytesList(newCtx.ChainID(), stdTx, stdSigs)
		signerAccs, res := getSignerAccs(newCtx, am, signerAddrs)
		if !res.IsOK() {
			return newCtx, res, true
		}
		res = validateAccNumAndSequence(ctx, signerAccs, stdSigs)
		if !res.IsOK() {
			return newCtx, res, true
		}

		// first sig pays the fees
		if !stdTx.Fee.Amount.IsZero() {
			// signerAccs[0] is the fee payer
			signerAccs[0], res = deductFees(signerAccs[0], stdTx.Fee)
			if !res.IsOK() {
				return newCtx, res, true
			}
			fck.AddCollectedFees(newCtx, stdTx.Fee.Amount)
		}

		for i := 0; i < len(stdSigs); i++ {
			// check signature, return account with incremented nonce
			signerAccs[i], res = processSig(newCtx, signerAccs[i], stdSigs[i], signBytesList[i], simulate)
			if !res.IsOK() {
				return newCtx, res, true
			}

			// Save the account.
			am.SetAccount(newCtx, signerAccs[i])
		}

		// cache the signer accounts in the context
		newCtx = WithSigners(newCtx, signerAccs)

		// TODO: tx tags (?)
		return newCtx, sdk.Result{GasWanted: stdTx.Fee.Gas}, false // continue...
	}
}

func getSignerAccs(ctx sdk.Context, am AccountKeeper, addrs []sdk.AccAddress) (accs []Account, res sdk.Result) {
	accs = make([]Account, len(addrs))
	for i := 0; i < len(accs); i++ {
		accs[i] = am.GetAccount(ctx, addrs[i])
		if accs[i] == nil {
			return nil, sdk.ErrUnknownAddress(addrs[i].String()).Result()
		}
	}
	return
}

func validateAccNumAndSequence(ctx sdk.Context, accs []Account, sigs []StdSignature) sdk.Result {
	for i := 0; i < len(accs); i++ {
		// On InitChain, make sure account number == 0
		if ctx.BlockHeight() == 0 && sigs[i].AccountNumber != 0 {
			return sdk.ErrInvalidSequence(
				fmt.Sprintf("Invalid account number for BlockHeight == 0. Got %d, expected 0", sigs[i].AccountNumber)).Result()
		}

		// Check account number.
		accnum := accs[i].GetAccountNumber()
		if ctx.BlockHeight() != 0 && accnum != sigs[i].AccountNumber {
			return sdk.ErrInvalidSequence(
				fmt.Sprintf("Invalid account number. Got %d, expected %d", sigs[i].AccountNumber, accnum)).Result()
		}

		// Check sequence number.
		seq := accs[i].GetSequence()
		if seq != sigs[i].Sequence {
			return sdk.ErrInvalidSequence(
				fmt.Sprintf("Invalid sequence. Got %d, expected %d", sigs[i].Sequence, seq)).Result()
		}
	}
	return sdk.Result{}
}

// verify the signature and increment the sequence.
// if the account doesn't have a pubkey, set it.
func processSig(ctx sdk.Context,
	acc Account, sig StdSignature, signBytes []byte, simulate bool) (updatedAcc Account, res sdk.Result) {
	pubKey, res := processPubKey(acc, sig, simulate)
	if !res.IsOK() {
		return nil, res
	}
	err := acc.SetPubKey(pubKey)
	if err != nil {
		return nil, sdk.ErrInternal("setting PubKey on signer's account").Result()
	}

	if res := consumeSignatureVerificationGas(ctx.GasMeter(), sig.Signature, pubKey); !res.IsOK() {
		return nil, res
	}

	if !simulate && !pubKey.VerifyBytes(signBytes, sig.Signature) {
		return nil, sdk.ErrUnauthorized("signature verification failed").Result()
	}

	// increment the sequence number
	err = acc.SetSequence(acc.GetSequence() + 1)
	if err != nil {
		// Handle w/ #870
		panic(err)
	}

	return acc, res
}

var dummySecp256k1Pubkey secp256k1.PubKeySecp256k1

func init() {
	bz, _ := hex.DecodeString("035AD6810A47F073553FF30D2FCC7E0D3B1C0B74B61A1AAA2582344037151E143A")
	copy(dummySecp256k1Pubkey[:], bz)
}

func processPubKey(acc Account, sig StdSignature, simulate bool) (crypto.PubKey, sdk.Result) {
	// If pubkey is not known for account,
	// set it from the StdSignature.
	pubKey := acc.GetPubKey()
	if simulate {
		// In simulate mode the transaction comes with no signatures, thus
		// if the account's pubkey is nil, both signature verification
		// and gasKVStore.Set() shall consume the largest amount, i.e.
		// it takes more gas to verifiy secp256k1 keys than ed25519 ones.
		if pubKey == nil {
			return dummySecp256k1Pubkey, sdk.Result{}
		}
		return pubKey, sdk.Result{}
	}
	if pubKey == nil {
		pubKey = sig.PubKey
		if pubKey == nil {
			return nil, sdk.ErrInvalidPubKey("PubKey not found").Result()
		}
		if !bytes.Equal(pubKey.Address(), acc.GetAddress()) {
			return nil, sdk.ErrInvalidPubKey(
				fmt.Sprintf("PubKey does not match Signer address %v", acc.GetAddress())).Result()
		}
	}
	return pubKey, sdk.Result{}
}

func consumeSignatureVerificationGas(meter sdk.GasMeter, sig []byte, pubkey crypto.PubKey) sdk.Result {

	pubkeyType := strings.ToLower(fmt.Sprintf("%T", pubkey))

	switch {
	case strings.Contains(pubkeyType, "ed25519"):
		meter.ConsumeGas(ed25519VerifyCost, "ante verify: ed25519")
		return sdk.Result{}

	case strings.Contains(pubkeyType, "secp256k1"):
		meter.ConsumeGas(secp256k1VerifyCost, "ante verify: secp256k1")
		return sdk.Result{}

	case strings.Contains(pubkeyType, "multisigthreshold"):
		var multisignature multisig.Multisignature
		codec.Cdc.MustUnmarshalBinaryBare(sig, &multisignature)

		multisigPubKey := pubkey.(multisig.PubKeyMultisigThreshold)
		consumeMultisignatureVerificationGas(meter, multisignature, multisigPubKey)
		return sdk.Result{}

	default:
		return sdk.ErrInvalidPubKey(fmt.Sprintf("unrecognized public key type: %s", pubkeyType)).Result()
	}
}

func consumeMultisignatureVerificationGas(meter sdk.GasMeter,
	sig multisig.Multisignature, pubkey multisig.PubKeyMultisigThreshold) {

	size := sig.BitArray.Size()
	sigIndex := 0
	for i := 0; i < size; i++ {
		if sig.BitArray.GetIndex(i) {
			consumeSignatureVerificationGas(meter, sig.Sigs[sigIndex], pubkey.PubKeys[i])
			sigIndex++
		}
	}
}

// Deduct the fee from the account.
// We could use the CoinKeeper (in addition to the AccountKeeper,
// because the CoinKeeper doesn't give us accounts), but it seems easier to do this.
func deductFees(acc Account, fee StdFee) (Account, sdk.Result) {
	coins := acc.GetCoins()
	feeAmount := fee.Amount

	newCoins, hasNeg := coins.SafeSub(feeAmount)
	if hasNeg {
		errMsg := fmt.Sprintf("account balance [%s] is not enough to cover fee [%s]", coins, feeAmount)
		return nil, sdk.ErrInsufficientFunds(errMsg).Result()
	}

	err := acc.SetCoins(newCoins)
	if err != nil {
		// Handle w/ #870
		panic(err)
	}

	return acc, sdk.Result{}
}

func ensureSufficientMempoolFees(ctx sdk.Context, stdTx StdTx) sdk.Result {
	// currently we use a very primitive gas pricing model with a constant gasPrice.
	// adjustFeesByGas handles calculating the amount of fees required based on the provided gas.
	//
	// TODO:
	// - Make the gasPrice not a constant, and account for tx size.
	// - Make Gas an unsigned integer and use tx basic validation
	if stdTx.Fee.Gas <= 0 {
		return sdk.ErrInternal(fmt.Sprintf("invalid gas supplied: %d", stdTx.Fee.Gas)).Result()
	}

	// NOTE: !A.IsAllGTE(B) is not the same as A.IsAllLT(B).
	if !ctx.MinimumFees().IsZero() && !stdTx.Fee.Amount.IsAllGTE(ctx.MinimumFees()) {
		// validators reject any tx from the mempool with less than the minimum fee per gas * gas factor
		return sdk.ErrInsufficientFee(fmt.Sprintf(
			"insufficient fee, got: %q required: %q", stdTx.Fee.Amount, ctx.MinimumFees())).Result()
	}
	return sdk.Result{}
}

func setGasMeter(simulate bool, ctx sdk.Context, gasLimit uint64) sdk.Context {
	// In various cases such as simulation and during the genesis block, we do not
	// meter any gas utilization.
	if simulate || ctx.BlockHeight() == 0 {
		return ctx.WithGasMeter(sdk.NewInfiniteGasMeter())
	}
	return ctx.WithGasMeter(sdk.NewGasMeterWithBase(gasLimit, gasBase, gasShift))
}

func getSignBytesList(chainID string, stdTx StdTx, stdSigs []StdSignature) (signatureBytesList [][]byte) {
	signatureBytesList = make([][]byte, len(stdSigs))
	for i := 0; i < len(stdSigs); i++ {
		signatureBytesList[i] = StdSignBytes(chainID,
			stdSigs[i].AccountNumber, stdSigs[i].Sequence,
			stdTx.Fee, stdTx.Msgs, stdTx.Memo)
	}
	return
}
