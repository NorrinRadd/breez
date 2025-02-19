package account

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/tyler-smith/go-bip32"

	"github.com/breez/breez/data"

	"github.com/fiatjaf/go-lnurl"
)

type LoginResponse struct {
	lnurl.LNURLResponse
	Token string `json:"token"`
}

func (a *Service) HandleLNURL(rawString string) (*data.LNUrlResponse, error) {
	encodedLnurl, ok := lnurl.FindLNURLInText(rawString)
	if !ok {
		return nil, fmt.Errorf("'%s' does not contain an LNURL.", rawString)
	}

	a.log.Infof("HandleLNURL %v", encodedLnurl)
	rawurl, iparams, err := lnurl.HandleLNURL(encodedLnurl)
	if err != nil {
		return nil, err
	}

	switch params := iparams.(type) {
	case lnurl.LNURLAuthParams:
		return &data.LNUrlResponse{
			Action: &data.LNUrlResponse_Auth{
				Auth: &data.LNURLAuth{
					Tag:      params.Tag,
					Callback: params.Callback,
					K1:       params.K1,
					Host:     params.Host,
				},
			},
		}, nil
	case lnurl.LNURLWithdrawResponse:
		qs := params.CallbackURL.Query()
		qs.Set("k1", params.K1)
		params.CallbackURL.RawQuery = qs.Encode()
		a.lnurlWithdrawing = params.CallbackURL.String()
		a.log.Infof("lnurl response: %v", a.lnurlWithdrawing)
		return &data.LNUrlResponse{
			Action: &data.LNUrlResponse_Withdraw{
				&data.LNUrlWithdraw{
					MinAmount: int64(math.Ceil(
						float64(params.MinWithdrawable) / 1000,
					)),
					MaxAmount: int64(math.Floor(
						float64(params.MaxWithdrawable) / 1000,
					)),
					DefaultDescription: params.DefaultDescription,
				},
			},
		}, nil
	case lnurl.LNURLChannelResponse:
		return &data.LNUrlResponse{
			Action: &data.LNUrlResponse_Channel{
				Channel: &data.LNURLChannel{
					K1:       params.K1,
					Callback: params.Callback,
					Uri:      params.URI,
				},
			},
		}, nil
	case lnurl.LNURLPayResponse1:
		a.log.Info("lnurl.LNURLPayResponse1.")
		host := ""
		if url, err := url.Parse(rawurl); err == nil {
			host = url.Host
		}
		a.lnurlPayMetadata.encoded = params.EncodedMetadata
		a.lnurlPayMetadata.data = params.Metadata
		var metadata []*data.LNUrlPayMetadata
		for _, e := range params.Metadata {
			metadata = append(metadata,
				&data.LNUrlPayMetadata{
					Entry: []string{e[0], e[1]},
				})
		}

		return &data.LNUrlResponse{
			Action: &data.LNUrlResponse_PayResponse1{
				&data.LNURLPayResponse1{
					Host:      host,
					Callback:  params.Callback,
					MinAmount: int64(math.Floor(float64(params.MinSendable) / 1000)),
					MaxAmount: int64(math.Floor(float64(params.MaxSendable) / 1000)),
					Metadata:  metadata,
				},
			},
		}, nil
	default:
		return nil, errors.New("Unsupported LNURL")
	}
}

// FinishLNURLAuth logs in using lnurl auth protocol
func (a *Service) FinishLNURLAuth(authParams *data.LNURLAuth) (string, error) {

	key, err := a.getLNURLAuthKey()
	if err != nil {
		return "", err
	}

	// hash host using master key
	h := hmac.New(sha256.New, key.Key)
	if _, err := h.Write([]byte(authParams.Host)); err != nil {
		return "", err
	}
	sha := h.Sum(nil)

	// create 4 elements derivation path using hashed value.
	first16 := sha[:16]
	for i := 0; i < 4; i++ {
		nextChildIndex := binary.BigEndian.Uint32(first16[i*4 : i*4+4])
		for key, err = key.NewChildKey(nextChildIndex); err != nil; {
			nextChildIndex++
		}
	}

	// this is the result keypair.
	linkingPrivKey, linkingPubKey := btcec.PrivKeyFromBytes(btcec.S256(), key.Key)
	k1Decoded, err := hex.DecodeString(authParams.K1)
	if err != nil {
		return "", fmt.Errorf("failed to decode k1 challenge %w", err)
	}

	// sign the challenge
	sig, err := linkingPrivKey.Sign(k1Decoded)
	if err != nil {
		return "", fmt.Errorf("failed to sign k1 challenge %w", err)
	}

	//convert to DER
	wireSig, err := lnwire.NewSigFromSignature(sig)
	if err != nil {
		return "", fmt.Errorf("can't convert sig to wire format: %v", err)
	}
	der := wireSig.ToSignatureBytes()

	// call the service
	url, err := url.Parse(authParams.Callback)
	if err != nil {
		return "", fmt.Errorf("invalid callback url %v", err)
	}
	query := url.Query()
	query.Add("key", hex.EncodeToString(linkingPubKey.SerializeCompressed()))
	query.Add("sig", hex.EncodeToString(der))
	if authParams.Jwt {
		query.Add("jwt", "true")
	}
	url.RawQuery = query.Encode()
	resp, err := http.Get(url.String())
	if err != nil {
		return "", err
	}

	// check response
	var lnurlresp LoginResponse
	err = json.NewDecoder(resp.Body).Decode(&lnurlresp)
	if err != nil {
		return "", err
	}

	if lnurlresp.Status == "ERROR" {
		return "", errors.New(lnurlresp.Reason)
	}

	return lnurlresp.Token, nil
}

func (a *Service) FinishLNURLWithdraw(bolt11 string) error {
	callback := a.lnurlWithdrawing

	resp, err := http.Get(callback + "&pr=" + bolt11)
	if err != nil {
		return err
	}

	var lnurlresp lnurl.LNURLResponse
	err = json.NewDecoder(resp.Body).Decode(&lnurlresp)
	if err != nil {
		return err
	}

	if lnurlresp.Status == "ERROR" {
		return errors.New(lnurlresp.Reason)
	}

	return nil
}

func (a *Service) getLNURLAuthKey() (*bip32.Key, error) {
	needsBackup := false
	key, err := a.breezDB.FetchLNURLAuthKey(func() ([]byte, error) {
		needsBackup = true
		return bip32.NewSeed()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch lnurl key %w", err)
	}
	if needsBackup {
		a.requestBackup()
	}

	// Create master private key from seed
	masterKey, err := bip32.NewMasterKey(key)
	if err != nil {
		return nil, fmt.Errorf("error creating lnurl master key: %w", err)
	}

	return masterKey, nil
}

func (a *Service) FinishLNURLPay(params *data.LNURLPayResponse1) (*data.LNUrlPayInfo, error) {

	// Ref. https://github.com/fiatjaf/lnurl-rfc/blob/master/lnurl-pay.md
	// TODO Check for response elements that might be null before using them.

	a.log.Infof("FinishLNURLPay: params: %+v", params)

	/*
	   5. LN WALLET makes a GET request using callback with the following query parameters:
	   amount (input) - user specified sum in MilliSatoshi
	   nonce - an optional parameter used to prevent server response caching
	   fromnodes - an optional parameter with value set to comma separated nodeIds if payer wishes a service to provide payment routes starting from specified LN nodeIds
	   comment (input) - an optional parameter to pass the LN WALLET user's comment to LN SERVICE

	   result: obtain bolt11 invoice
	*/

	url, err := url.Parse(params.Callback)
	if err != nil {
		return nil, fmt.Errorf("invalid callback url %v", err)
	}
	query := url.Query()
	query.Add("amount", fmt.Sprintf("%d", params.Amount))

	// source: backup/crypto.go
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	query.Add("nonce", hex.EncodeToString(nonce))

	/* TODO FINDOUT Should we populate the FromNodes optional parameter?
	   If yes, then how?
	   if params.FromNodes != "" {
	       query.Add("fromnodes", params.FromNodes)
	   }
	*/

	if params.Comment != "" {
		query.Add("comment", params.Comment)
	}

	url.RawQuery = query.Encode()
	a.log.Infof("FinishLNURLPay: request.url: %v", url)
	resp, err := http.Get(url.String())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 320 {
		return nil, fmt.Errorf("Error in http request: %s", resp.Status)
	}

	a.log.Infof("FinishLNURLPay: response.Status: %s", resp.Status)

	/*
	   6. `LN Service` takes the GET request and returns JSON response of form:

	   ```
	   {
	       pr: String, // bech32-serialized lightning invoice
	       successAction: Object or null, // An optional action to be executed after successfully paying an invoice
	       disposable: Boolean or null, // An optional flag to let a wallet know whether to persist the link from step 1, if null should be interpreted as true
	       routes:
	       [
	       [
	       {
	           nodeId: String,
	           channelUpdate: String // hex-encoded serialized ChannelUpdate gossip message
	       },
	       ... // next hop
	       ],
	       ... // next route
	       ] // array with payment routes, should be left empty if no routes are to be provided
	   }
	   ```

	   or

	   ```
	   {"status":"ERROR", "reason":"error details..."}
	   ```

	   `pr` must have the [`h` tag (`description_hash`)](https://github.com/lightningnetwork/lightning-rfc/blob/master/11-payment-encoding.md#tagged-fields) set to `sha256(utf8ByteArray(metadata))`.

	   Currently supported tags for `successAction` object are `url`, `message`, and `aes`. If there is no action then `successAction` value must be set to `null`.

	   ```
	   {
	       tag: String, // action type
	       ... rest of fields depends on tag value
	   }
	   ```

	   Examples of `successAction`:

	   ```
	   {
	       tag: 'url'
	       description: 'Thank you for your purchase. Here is your order details' // Up to 144 characters
	       url: 'https://www.ln-service.com/order/<orderId>' // url domain must be the same as `callback` domain at step 3
	   }

	   {
	       tag: 'message'
	       message: 'Thank you for using bike-over-ln CO! Your rental bike is unlocked now' // Up to 144 characters
	   }

	   {
	       tag: 'aes'
	       description: 'Here is your redeem code' // Up to 144 characters
	       ciphertext: <base64> // an AES-encrypted data where encryption key is payment preimage, up to 4kb of characters
	       iv: <base64> // initialization vector, exactly 24 characters
	   }

	*/

	var payResponse2 lnurl.LNURLPayResponse2
	if err = json.NewDecoder(resp.Body).Decode(&payResponse2); err != nil {
		return nil, err
	}

	if payResponse2.Status == "ERROR" {
		return nil, errors.New(payResponse2.Reason)
	}

	a.log.Infof("FinishLNURLPay: payResponse2 %+v", payResponse2)

	// 7. `LN WALLET` Verifies that `h` tag in provided invoice is a hash of `metadata` string converted to byte array in UTF-8 encoding.
	invoice, err := zpay32.Decode(payResponse2.PR, a.activeParams)
	if err != nil {
		return nil, err
	}

	if invoice.DescriptionHash == nil {
		return nil, errors.New("Description hash not found in invoice.")
	}

	if sum := sha256.Sum256([]byte(a.lnurlPayMetadata.encoded)); hex.EncodeToString(sum[:]) != hex.EncodeToString((*invoice.DescriptionHash)[:]) {
		err = errors.New("Invoice description hash does not match metadata.")
		a.log.Error(fmt.Sprintf("FinishLNURLPay: %v", err))
		return nil, err
	}

	/* 8. `LN WALLET` Verifies that amount in provided invoice equals an amount previously specified by user.
	   amount from client is in millisats.
	*/

	a.log.Info("FinishLNURLPay: verify invoice.amount == params.Amount.")
	if params.Amount != uint64(*invoice.MilliSat) {
		return nil, errors.New("Invoice amount does not match the amount set by user.")
	}

	/* TODO 9. If routes array is not empty: verifies signature for every provided `ChannelUpdate`, may use these routes if fee levels are acceptable.

	   ref. https://github.com/lightningnetwork/lightning-rfc/blob/master/07-routing-gossip.md#the-channel_update-message
	   ref. github.com\lightningnetwork\lnd\lnwire\channel_update.go

	   ref. github.com/lightningnetwork/lnd/lnwire/channel_update.go
	   ref. github.com/lightningnetwork/lnd/lnwire/message.go
	*/
	if len(payResponse2.Routes) > 0 {
		a.log.Info("FinishLNURLPay: response has routes.")
	}

	// 10. If `successAction` is not null: `LN WALLET` makes sure that `tag` value of is of supported type, aborts a payment otherwise.
	sa := payResponse2.SuccessAction
	var _sa *data.SuccessAction
	if sa != nil {

		if t := sa.Tag; t != "message" &&
			t != "url" &&
			t != "aes" {
			return nil, fmt.Errorf("Unknown SuccessAction: %s", t)
		}

		_sa = &data.SuccessAction{
			Tag:         sa.Tag,
			Description: sa.Description,
			Url:         sa.URL,
			Message:     sa.Message,
			Ciphertext:  sa.Ciphertext,
			Iv:          sa.IV,
		}

		a.log.Infof("FinishLNURLPay: Found SuccessAction: %+v", *sa)

	}

	// 11. `LN WALLET` pays the invoice, no additional user confirmation is required at this point.
	info := &data.LNUrlPayInfo{
		PaymentHash:        hex.EncodeToString(invoice.PaymentHash[:]),
		SuccessAction:      _sa,
		Comment:            params.Comment,
		InvoiceDescription: lnurl.Metadata(a.lnurlPayMetadata.data).Description(),
		Host:               params.Host,
		Metadata:           params.Metadata,
		Invoice:            payResponse2.PR,
	}
	a.breezDB.SaveLNUrlPayInfo(info)
	return info, nil

}

func (a *Service) DecryptLNUrlPayMessage(paymentHash string, preimage []byte) (string, error) {

	info, err := a.breezDB.FetchLNUrlPayInfo(paymentHash)
	if err != nil {
		return "",
			fmt.Errorf("Unable to get LNUrl-Pay info from database: %s", err)
	}

	if info != nil {
		sa := &lnurl.SuccessAction{
			Tag:         info.SuccessAction.Tag,
			Description: info.SuccessAction.Description,
			URL:         info.SuccessAction.Url,
			Message:     info.SuccessAction.Message,
			Ciphertext:  info.SuccessAction.Ciphertext,
			IV:          info.SuccessAction.Iv,
		}
		if sa.Ciphertext == "" {
			return "", errors.New("LNUrl-Pay CipherText is empty.")
		}

		if info.SuccessAction.Message, err = sa.Decipher(preimage); err != nil {
			return "", fmt.Errorf("Could not decrypt message: %v", err)
		}

		if err = a.breezDB.SaveLNUrlPayInfo(info); err != nil {
			return "", fmt.Errorf("Could not save deciphered message: %s", err)
		}

		a.log.Info("DecryptLNUrlPayMessage: message = %q", info.SuccessAction.Message)
		return info.SuccessAction.Message, nil
	}

	return "", errors.New("DecryptLNUrlPayMessage: could not find lnUrlPayInfo with given paymentHash.")
}
