package pat

import (
	"crypto"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"io"
	"math/big"

	hpke "github.com/cisco/go-hpke"
	"github.com/cloudflare/circl/blindsign"
	"github.com/cloudflare/circl/blindsign/blindrsa"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/hkdf"

	"github.com/cloudflare/pat-go/ecdsa"
)

var (
	labelResponseKey   = "key"
	labelResponseNonce = "nonce"
)

type OriginTokenRequest struct {
	raw          []byte
	blindedMsg   []byte
	requestKey   []byte
	paddedOrigin []byte
}

func (r *OriginTokenRequest) Marshal() []byte {
	if r.raw != nil {
		return r.raw
	}

	b := cryptobyte.NewBuilder(nil)
	b.AddBytes(r.blindedMsg)
	b.AddBytes(r.requestKey)
	b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddBytes([]byte(r.paddedOrigin))
	})

	r.raw = b.BytesOrPanic()
	return r.raw
}

func (r *OriginTokenRequest) Unmarshal(data []byte) bool {
	s := cryptobyte.String(data)

	if !s.ReadBytes(&r.blindedMsg, 256) ||
		!s.ReadBytes(&r.requestKey, 49) {
		return false
	}

	var paddedOriginName cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&paddedOriginName) {
		return false
	}
	r.paddedOrigin = make([]byte, len(paddedOriginName))
	copy(r.paddedOrigin, paddedOriginName)

	return true
}

func computeIndex(clientKey, indexKey []byte) ([]byte, error) {
	hkdf := hkdf.New(sha512.New384, indexKey, clientKey, []byte("anon_issuer_origin_id"))
	clientOriginIndex := make([]byte, crypto.SHA384.Size())
	if _, err := io.ReadFull(hkdf, clientOriginIndex); err != nil {
		return nil, err
	}
	return clientOriginIndex, nil
}

// https://tfpauly.github.io/privacy-proxy/draft-privacypass-rate-limit-tokens.html#name-attester-behavior-mapping-o
func FinalizeIndex(clientKey, blindEnc, blindedRequestKeyEnc []byte) ([]byte, error) {
	curve := elliptic.P384()
	x, y := elliptic.UnmarshalCompressed(curve, blindedRequestKeyEnc)
	blindedRequestKey := &ecdsa.PublicKey{
		curve, x, y,
	}

	blindKey, err := ecdsa.CreateKey(curve, blindEnc)
	if err != nil {
		return nil, err
	}

	indexKey, err := ecdsa.UnblindPublicKey(curve, blindedRequestKey, blindKey)
	if err != nil {
		return nil, err
	}

	indexKeyEnc := elliptic.MarshalCompressed(curve, indexKey.X, indexKey.Y)

	return computeIndex(clientKey, indexKeyEnc)
}

type RateLimitedClient struct {
	curve     elliptic.Curve
	secretKey *ecdsa.PrivateKey
}

func CreateRateLimitedClientFromSecret(secret []byte) RateLimitedClient {
	curve := elliptic.P384()
	secretKey, err := ecdsa.CreateKey(curve, secret)
	if err != nil {
		panic(err)
	}

	return RateLimitedClient{
		curve:     elliptic.P384(),
		secretKey: secretKey,
	}
}

// https://tfpauly.github.io/privacy-proxy/draft-privacypass-rate-limit-tokens.html#name-encrypting-origin-names
func padOriginName(originName string) []byte {
	N := 31 - ((len(originName) - 1) % 32)
	zeroes := make([]byte, N)
	return append([]byte(originName), zeroes...)
}

func unpadOriginName(paddedOriginName []byte) string {
	lastNonZero := len(paddedOriginName) - 1
	for {
		if lastNonZero < 0 {
			// The plaintext was empty, so the input was the empty string
			return ""
		}
		if paddedOriginName[lastNonZero] != 0x00 {
			break
		}
		lastNonZero--
	}
	return string(paddedOriginName[0 : lastNonZero+1])
}

// https://tfpauly.github.io/privacy-proxy/draft-privacypass-rate-limit-tokens.html#name-encrypting-origin-names
func encryptOriginTokenRequest(nameKey PublicNameKey, tokenKeyID uint8, blindedMessage []byte, requestKey []byte, originName string) ([]byte, []byte, []byte, error) {
	issuerKeyEnc := nameKey.Marshal()
	issuerKeyID := sha256.Sum256(issuerKeyEnc)

	enc, context, err := hpke.SetupBaseS(nameKey.suite, rand.Reader, nameKey.publicKey, []byte("TokenRequest"))
	if err != nil {
		return nil, nil, nil, err
	}

	b := cryptobyte.NewBuilder(nil)
	b.AddUint8(nameKey.id)
	b.AddUint16(uint16(nameKey.suite.KEM.ID()))
	b.AddUint16(uint16(nameKey.suite.KDF.ID()))
	b.AddUint16(uint16(nameKey.suite.AEAD.ID()))
	b.AddUint16(RateLimitedTokenType)
	b.AddUint8(tokenKeyID)
	b.AddBytes(issuerKeyID[:])

	tokenRequest := OriginTokenRequest{
		blindedMsg:   blindedMessage,
		requestKey:   requestKey,
		paddedOrigin: padOriginName(originName),
	}
	input := tokenRequest.Marshal()

	aad := b.BytesOrPanic()
	ct := context.Seal(aad, input)
	encryptedTokenRequest := append(enc, ct...)

	secret := context.Export([]byte("OriginTokenResponse"), nameKey.suite.AEAD.KeySize())

	return issuerKeyID[:], encryptedTokenRequest, secret, nil
}

type RateLimitedTokenRequestState struct {
	tokenInput        []byte
	blindedRequestKey []byte
	request           *RateLimitedTokenRequest
	encapSecret       []byte
	encapEnc          []byte
	nameKey           PublicNameKey
	verificationKey   *rsa.PublicKey
	verifier          blindsign.VerifierState
}

func (s RateLimitedTokenRequestState) Request() *RateLimitedTokenRequest {
	return s.request
}

func (s RateLimitedTokenRequestState) BlindedRequestKey() []byte {
	return s.blindedRequestKey
}

func (s RateLimitedTokenRequestState) FinalizeToken(encryptedtokenResponse []byte) (Token, error) {
	// response_nonce = random(max(Nn, Nk)), taken from the encapsualted response
	responseNonceLen := max(s.nameKey.suite.AEAD.KeySize(), s.nameKey.suite.AEAD.NonceSize())

	// salt = concat(enc, response_nonce)
	salt := append(s.encapEnc, encryptedtokenResponse[:responseNonceLen]...)

	// prk = Extract(salt, secret)
	prk := s.nameKey.suite.KDF.Extract(salt, s.encapSecret)

	// aead_key = Expand(prk, "key", Nk)
	key := s.nameKey.suite.KDF.Expand(prk, []byte(labelResponseKey), s.nameKey.suite.AEAD.KeySize())

	// aead_nonce = Expand(prk, "nonce", Nn)
	nonce := s.nameKey.suite.KDF.Expand(prk, []byte(labelResponseNonce), s.nameKey.suite.AEAD.NonceSize())

	cipher, err := s.nameKey.suite.AEAD.New(key)
	if err != nil {
		return Token{}, err
	}

	// reponse, error = Open(aead_key, aead_nonce, "", ct)
	blindSignature, err := cipher.Open(nil, nonce, encryptedtokenResponse[responseNonceLen:], nil)
	if err != nil {
		return Token{}, err
	}

	signature, err := s.verifier.Finalize(blindSignature)
	if err != nil {
		return Token{}, err
	}

	tokenData := append(s.tokenInput, signature...)
	token, err := UnmarshalToken(tokenData)
	if err != nil {
		return Token{}, err
	}

	// Sanity check: verify the token signature
	hash := sha512.New384()
	_, err = hash.Write(token.AuthenticatorInput())
	if err != nil {
		return Token{}, err
	}
	digest := hash.Sum(nil)

	err = rsa.VerifyPSS(s.verificationKey, crypto.SHA384, digest, token.Authenticator, &rsa.PSSOptions{
		Hash:       crypto.SHA384,
		SaltLength: crypto.SHA384.Size(),
	})
	if err != nil {
		return Token{}, err
	}

	return token, nil
}

// https://tfpauly.github.io/privacy-proxy/draft-privacypass-rate-limit-tokens.html#name-client-to-attester-request
// https://tfpauly.github.io/privacy-proxy/draft-privacypass-rate-limit-tokens.html#name-index-computation
func (c RateLimitedClient) CreateTokenRequest(challenge, nonce, blindKeyEnc []byte, tokenKeyID []byte, tokenKey *rsa.PublicKey, originName string, nameKey PublicNameKey) (RateLimitedTokenRequestState, error) {
	blindKey, err := ecdsa.CreateKey(c.curve, blindKeyEnc)
	if err != nil {
		return RateLimitedTokenRequestState{}, err
	}

	blindedPublicKey, err := ecdsa.BlindPublicKey(c.curve, &c.secretKey.PublicKey, blindKey)
	if err != nil {
		return RateLimitedTokenRequestState{}, err
	}
	blindedPublicKeyEnc := elliptic.MarshalCompressed(c.curve, blindedPublicKey.X, blindedPublicKey.Y)

	verifier := blindrsa.NewRSAVerifier(tokenKey, sha512.New384())

	context := sha256.Sum256(challenge)
	token := Token{
		TokenType:     RateLimitedTokenType,
		Nonce:         nonce,
		Context:       context[:],
		KeyID:         tokenKeyID,
		Authenticator: nil, // No signature computed yet
	}
	tokenInput := token.AuthenticatorInput()
	blindedMessage, verifierState, err := verifier.Blind(rand.Reader, tokenInput)
	if err != nil {
		return RateLimitedTokenRequestState{}, err
	}

	nameKeyID, encryptedTokenRequest, secret, err := encryptOriginTokenRequest(nameKey, tokenKeyID[0], blindedMessage, blindedPublicKeyEnc, originName)
	if err != nil {
		return RateLimitedTokenRequestState{}, err
	}

	b := cryptobyte.NewBuilder(nil)
	b.AddUint16(RateLimitedTokenType)
	b.AddUint8(tokenKeyID[0])
	b.AddBytes(nameKeyID)
	b.AddBytes(encryptedTokenRequest)
	message := b.BytesOrPanic()

	hash := sha512.New384()
	hash.Write(message)
	digest := hash.Sum(nil)

	r, s, err := ecdsa.BlindKeySign(rand.Reader, c.secretKey, blindKey, digest)
	if err != nil {
		return RateLimitedTokenRequestState{}, err
	}
	scalarLen := (c.curve.Params().Params().BitSize + 7) / 8
	rEnc := make([]byte, scalarLen)
	sEnc := make([]byte, scalarLen)
	r.FillBytes(rEnc)
	s.FillBytes(sEnc)
	signature := append(rEnc, sEnc...)

	request := &RateLimitedTokenRequest{
		tokenKeyID:            tokenKeyID[0],
		nameKeyID:             nameKeyID,
		encryptedTokenRequest: encryptedTokenRequest,
		signature:             signature,
	}

	requestState := RateLimitedTokenRequestState{
		tokenInput:        tokenInput,
		blindedRequestKey: blindedPublicKeyEnc,
		request:           request,
		encapSecret:       secret,
		encapEnc:          encryptedTokenRequest[0:nameKey.suite.KEM.PublicKeySize()],
		nameKey:           nameKey,
		verifier:          verifierState,
		verificationKey:   tokenKey,
	}

	return requestState, nil
}

type RateLimitedIssuer struct {
	curve           elliptic.Curve
	nameKey         PrivateNameKey
	tokenKey        *rsa.PrivateKey
	originIndexKeys map[string]*ecdsa.PrivateKey
}

func NewRateLimitedIssuer(key *rsa.PrivateKey) *RateLimitedIssuer {
	suite, err := hpke.AssembleCipherSuite(hpke.DHKEM_X25519, hpke.KDF_HKDF_SHA256, hpke.AEAD_AESGCM128)
	if err != nil {
		return nil
	}

	ikm := make([]byte, suite.KEM.PrivateKeySize())
	rand.Reader.Read(ikm)
	privateKey, publicKey, err := suite.KEM.DeriveKeyPair(ikm)
	if err != nil {
		return nil
	}

	nameKey := PrivateNameKey{
		id:         0x00,
		suite:      suite,
		publicKey:  publicKey,
		privateKey: privateKey,
	}

	return &RateLimitedIssuer{
		curve:           elliptic.P384(),
		nameKey:         nameKey,
		tokenKey:        key,
		originIndexKeys: make(map[string]*ecdsa.PrivateKey),
	}
}

func (i *RateLimitedIssuer) NameKey() PublicNameKey {
	return i.nameKey.Public()
}

func (i *RateLimitedIssuer) AddOrigin(origin string) error {
	privateKey, err := ecdsa.GenerateKey(i.curve, rand.Reader)
	if err != nil {
		return err
	}

	i.originIndexKeys[origin] = privateKey

	return nil
}

func (i *RateLimitedIssuer) OriginIndexKey(origin string) *ecdsa.PrivateKey {
	key, ok := i.originIndexKeys[origin]
	if !ok {
		return nil
	}
	return key
}

func (i *RateLimitedIssuer) TokenKey() *rsa.PublicKey {
	return &i.tokenKey.PublicKey
}

func (i *RateLimitedIssuer) TokenKeyID() []byte {
	publicKey := i.TokenKey()
	publicKeyEnc, err := MarshalTokenKeyPSSOID(publicKey)
	if err != nil {
		panic(err)
	}
	keyID := sha256.Sum256(publicKeyEnc)
	return keyID[:]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func decryptOriginTokenRequest(nameKey PrivateNameKey, tokenKeyID uint8, encryptedTokenRequest []byte) (OriginTokenRequest, []byte, error) {
	issuerConfigID := sha256.Sum256(nameKey.Public().Marshal())

	// Decrypt the origin name
	b := cryptobyte.NewBuilder(nil)
	b.AddUint8(nameKey.id)
	b.AddUint16(uint16(nameKey.suite.KEM.ID()))
	b.AddUint16(uint16(nameKey.suite.KDF.ID()))
	b.AddUint16(uint16(nameKey.suite.AEAD.ID()))
	b.AddUint16(RateLimitedTokenType)
	b.AddUint8(tokenKeyID)
	b.AddBytes(issuerConfigID[:])
	aad := b.BytesOrPanic()

	enc := encryptedTokenRequest[0:nameKey.suite.KEM.PublicKeySize()]
	ct := encryptedTokenRequest[nameKey.suite.KEM.PublicKeySize():]

	context, err := hpke.SetupBaseR(nameKey.suite, nameKey.privateKey, enc, []byte("TokenRequest"))
	if err != nil {
		return OriginTokenRequest{}, nil, err
	}

	tokenRequestEnc, err := context.Open(aad, ct)
	if err != nil {
		return OriginTokenRequest{}, nil, err
	}

	tokenRequest := &OriginTokenRequest{}
	if !tokenRequest.Unmarshal(tokenRequestEnc) {
		return OriginTokenRequest{}, nil, err
	}

	secret := context.Export([]byte("OriginTokenResponse"), nameKey.suite.AEAD.KeySize())

	return *tokenRequest, secret, err
}

func (i RateLimitedIssuer) Evaluate(req *RateLimitedTokenRequest) ([]byte, []byte, error) {
	// Recover and validate the origin name
	originTokenRequest, secret, err := decryptOriginTokenRequest(i.nameKey, req.tokenKeyID, req.encryptedTokenRequest)
	if err != nil {
		return nil, nil, err
	}

	originName := unpadOriginName(originTokenRequest.paddedOrigin)

	originIndexKey, ok := i.originIndexKeys[originName]
	if !ok {
		return nil, nil, fmt.Errorf("Unknown origin: %s", originName)
	}

	// XXX(caw): factor out functionality above, and also check the keyID
	x, y := elliptic.UnmarshalCompressed(i.curve, originTokenRequest.requestKey)
	requestKey := &ecdsa.PublicKey{
		i.curve, x, y,
	}

	scalarLen := (i.curve.Params().Params().BitSize + 7) / 8
	r := new(big.Int).SetBytes(req.signature[:scalarLen])
	s := new(big.Int).SetBytes(req.signature[scalarLen:])

	// Verify the request signature
	b := cryptobyte.NewBuilder(nil)
	b.AddUint16(RateLimitedTokenType)
	b.AddUint8(req.tokenKeyID)
	b.AddBytes(req.nameKeyID)
	b.AddBytes(req.encryptedTokenRequest)
	message := b.BytesOrPanic()

	hash := sha512.New384()
	hash.Write(message)
	digest := hash.Sum(nil)

	valid := ecdsa.Verify(requestKey, digest, r, s)
	if !valid {
		return nil, nil, fmt.Errorf("Invalid request signature")
	}

	// Blinded key
	blindedRequestKey, err := ecdsa.BlindPublicKey(i.curve, requestKey, originIndexKey)
	if err != nil {
		return nil, nil, err
	}
	blindedRequestKeyEnc := elliptic.MarshalCompressed(i.curve, blindedRequestKey.X, blindedRequestKey.Y)

	// Blinded signature
	signer := blindrsa.NewRSASigner(i.tokenKey)
	blindSignature, err := signer.BlindSign(originTokenRequest.blindedMsg)
	if err != nil {
		return nil, nil, err
	}

	// Encrypt the response back to the client
	// XXX(caw): pull out into subroutine and clean up with the decryption path above
	responseNonceLen := max(i.nameKey.suite.AEAD.KeySize(), i.nameKey.suite.AEAD.NonceSize())
	responseNonce := make([]byte, responseNonceLen)
	_, err = rand.Read(responseNonce)
	if err != nil {
		return nil, nil, err
	}

	// salt = concat(enc, response_nonce)
	enc := req.encryptedTokenRequest[0:i.nameKey.suite.KEM.PublicKeySize()]
	salt := append(append(enc, responseNonce...))

	// prk = Extract(salt, secret)
	prk := i.nameKey.suite.KDF.Extract(salt, secret)

	// aead_key = Expand(prk, "key", Nk)
	key := i.nameKey.suite.KDF.Expand(prk, []byte(labelResponseKey), i.nameKey.suite.AEAD.KeySize())

	// aead_nonce = Expand(prk, "nonce", Nn)
	nonce := i.nameKey.suite.KDF.Expand(prk, []byte(labelResponseNonce), i.nameKey.suite.AEAD.NonceSize())

	// ct = Seal(aead_key, aead_nonce, "", response)
	cipher, err := i.nameKey.suite.AEAD.New(key)
	if err != nil {
		return nil, nil, err
	}

	encryptedTokenResponse := append(responseNonce, cipher.Seal(nil, nonce, blindSignature, nil)...)

	return encryptedTokenResponse, blindedRequestKeyEnc, nil
}

func (i RateLimitedIssuer) EvaluateWithoutCheck(req *RateLimitedTokenRequest) ([]byte, []byte, error) {
	// Recover and validate the origin name
	originTokenRequest, secret, err := decryptOriginTokenRequest(i.nameKey, req.tokenKeyID, req.encryptedTokenRequest)
	if err != nil {
		return nil, nil, err
	}

	originName := unpadOriginName(originTokenRequest.paddedOrigin)

	originIndexKey, ok := i.originIndexKeys[string(originName)]
	if !ok {
		return nil, nil, fmt.Errorf("Unknown origin: %s", string(originName))
	}

	// Blinded key
	x, y := elliptic.UnmarshalCompressed(i.curve, originTokenRequest.requestKey)
	requestKey := &ecdsa.PublicKey{
		i.curve, x, y,
	}
	blindedRequestKey, err := ecdsa.BlindPublicKey(i.curve, requestKey, originIndexKey)
	if err != nil {
		return nil, nil, err
	}
	blindedRequestKeyEnc := elliptic.MarshalCompressed(i.curve, blindedRequestKey.X, blindedRequestKey.Y)

	// Blinded signature
	signer := blindrsa.NewRSASigner(i.tokenKey)
	blindSignature, err := signer.BlindSign(originTokenRequest.blindedMsg)
	if err != nil {
		return nil, nil, err
	}

	// Encrypt the response back to the client
	responseNonceLen := max(i.nameKey.suite.AEAD.KeySize(), i.nameKey.suite.AEAD.NonceSize())
	responseNonce := make([]byte, responseNonceLen)
	_, err = rand.Read(responseNonce)
	if err != nil {
		return nil, nil, err
	}

	// salt = concat(enc, response_nonce)
	enc := req.encryptedTokenRequest[0:i.nameKey.suite.KEM.PublicKeySize()]
	salt := append(append(enc, responseNonce...))

	// prk = Extract(salt, secret)
	prk := i.nameKey.suite.KDF.Extract(salt, secret)

	// aead_key = Expand(prk, "key", Nk)
	key := i.nameKey.suite.KDF.Expand(prk, []byte(labelResponseKey), i.nameKey.suite.AEAD.KeySize())

	// aead_nonce = Expand(prk, "nonce", Nn)
	nonce := i.nameKey.suite.KDF.Expand(prk, []byte(labelResponseNonce), i.nameKey.suite.AEAD.NonceSize())

	// ct = Seal(aead_key, aead_nonce, "", response)
	cipher, err := i.nameKey.suite.AEAD.New(key)
	if err != nil {
		return nil, nil, err
	}
	encryptedTokenResponse := cipher.Seal(nil, nonce, blindSignature, nil)

	return encryptedTokenResponse, blindedRequestKeyEnc, nil
}
