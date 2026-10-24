package compliance

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/hkdf"
)

const (
	TravelRuleProtocolTRP        = "trp"
	TravelRuleProtocolOpenVASP   = "openvasp"
	TravelRuleConfirmationMaxAge = 24 * time.Hour
)

var (
	ErrTravelRuleDetailsRequired    = errors.New("travel rule details are required for this transfer")
	ErrUnknownRecipientVASP         = errors.New("recipient VASP is not in the trusted directory")
	ErrInvalidTravelRuleConfirmation = errors.New("travel rule confirmation could not be verified")
	)

type VASP struct {
	ID                    string `json:"id"`
	Protocol              string `json:"protocol"`
	Endpoint              string `json:"endpoint"`
	EncryptionPublicKey   string `json:"encryption_public_key"`
	ConfirmationPublicKey string `json:"confirmation_public_key"`
	BearerToken           string `json:"bearer_token,omitempty"`
	ClientCertFile        string `json:"client_cert_file,omitempty"`
	ClientKeyFile         string `json:"client_key_file,omitempty"`
}

type VASPDirectory map[string]VASP

// LoadVASPDirectory loads the operator-managed allowlist. Only HTTPS endpoints
// and explicit X25519/Ed25519 public keys are accepted; request bodies cannot
// introduce endpoints or keys, avoiding a caller-controlled SSRF primitive.
func LoadVASPDirectory(raw string) (VASPDirectory, error) {
	var entries []VASP
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &entries) != nil || len(entries) == 0 {
		return nil, errors.New("TRAVEL_RULE_VASP_DIRECTORY_JSON must be a non-empty JSON array")
	}
	directory := make(VASPDirectory, len(entries))
	for _, vasp := range entries {
		if vasp.ID == "" || vasp.ID != strings.TrimSpace(vasp.ID) {
			return nil, errors.New("each trusted VASP must have a non-empty trimmed ID")
		}
		vasp.Protocol = strings.ToLower(strings.TrimSpace(vasp.Protocol))
		vasp.Endpoint = strings.TrimSpace(vasp.Endpoint)
		if _, exists := directory[vasp.ID]; exists {
			return nil, fmt.Errorf("duplicate VASP ID %q", vasp.ID)
		}
		if (vasp.ClientCertFile == "") != (vasp.ClientKeyFile == "") {
			return nil, fmt.Errorf("VASP %q must configure both client_cert_file and client_key_file for mTLS", vasp.ID)
		}
		if vasp.Protocol != TravelRuleProtocolTRP && vasp.Protocol != TravelRuleProtocolOpenVASP {
			return nil, fmt.Errorf("unsupported Travel Rule protocol for VASP %q", vasp.ID)
		}
		endpoint, err := url.Parse(vasp.Endpoint)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" ||
			endpoint.User != nil || endpoint.Fragment != "" || endpoint.RawQuery != "" {
			return nil, fmt.Errorf("VASP %q endpoint must be an HTTPS URL without credentials, query, or fragment", vasp.ID)
		}
		encKey, err := decodeTravelRuleKey(vasp.EncryptionPublicKey)
		if err != nil || len(encKey) != 32 {
			return nil, fmt.Errorf("VASP %q has an invalid X25519 encryption public key", vasp.ID)
		}
		verifyKey, err := decodeTravelRuleKey(vasp.ConfirmationPublicKey)
		if err != nil || len(verifyKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("VASP %q has an invalid Ed25519 confirmation public key", vasp.ID)
		}
		directory[vasp.ID] = vasp
	}
	return directory, nil
}

type USDValuator interface {
	USDValue(ctx context.Context, asset string, amount decimal.Decimal) (decimal.Decimal, error)
}

type TravelRuleRepository interface {
	CreateTravelRuleExchange(ctx context.Context, exchange *domain.TravelRuleExchange) error
	GetTravelRuleExchange(ctx context.Context, transferID string) (*domain.TravelRuleExchange, error)
	SaveTravelRuleConfirmation(ctx context.Context, transferID string, status domain.TravelRuleExchangeStatus, confirmationID string, at time.Time) error
}

type ExchangeSender interface {
	Send(ctx context.Context, recipient VASP, envelope TravelRuleEnvelope) (*domain.TravelRuleConfirmation, error)
}

type EncryptedTravelRulePayload struct {
	Version            int    `json:"version"`
	EphemeralPublicKey string `json:"ephemeral_public_key"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
}

type TravelRuleEnvelope struct {
	Version         int                        `json:"version"`
	MessageID       string                     `json:"message_id"`
	TransferID      string                     `json:"transfer_id"`
	SenderVASPID    string                     `json:"sender_vasp_id"`
	RecipientVASPID string                     `json:"recipient_vasp_id"`
	Protocol        string                     `json:"protocol"`
	CreatedAt       time.Time                  `json:"created_at"`
	Encrypted       EncryptedTravelRulePayload `json:"encrypted_payload"`
	EnvelopeSHA256  string                     `json:"envelope_sha256"`
}

type envelopeDigestInput struct {
	Version         int                        `json:"version"`
	MessageID       string                     `json:"message_id"`
	TransferID      string                     `json:"transfer_id"`
	SenderVASPID    string                     `json:"sender_vasp_id"`
	RecipientVASPID string                     `json:"recipient_vasp_id"`
	Protocol        string                     `json:"protocol"`
	CreatedAt       time.Time                  `json:"created_at"`
	Encrypted       EncryptedTravelRulePayload `json:"encrypted_payload"`
}

// EnvelopeDigest binds a confirmation to the exact ciphertext and routing
// metadata, without publishing a hash of guessable plaintext PII.
func EnvelopeDigest(envelope TravelRuleEnvelope) string {
	input := envelopeDigestInput{
		Version: envelope.Version, MessageID: envelope.MessageID, TransferID: envelope.TransferID,
		SenderVASPID: envelope.SenderVASPID, RecipientVASPID: envelope.RecipientVASPID,
		Protocol: envelope.Protocol, CreatedAt: envelope.CreatedAt, Encrypted: envelope.Encrypted,
	}
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type travelRuleMessage struct {
	Transfer struct {
		ID     string `json:"id"`
		Asset  string `json:"asset"`
		Amount string `json:"amount"`
	} `json:"transfer"`
	IVMS101 json.RawMessage `json:"ivms101"`
}

type TravelRuleService struct {
	repo         TravelRuleRepository
	valuator     USDValuator
	vasps        VASPDirectory
	senderVASPID string
	sender       ExchangeSender
	txGate       TransactionGate
	queue        Enqueuer
	thresholdUSD decimal.Decimal
	now          func() time.Time
}

func NewTravelRuleService(
	repo TravelRuleRepository,
	valuator USDValuator,
	directoryJSON string,
	senderVASPID string,
	sender ExchangeSender,
	txGate TransactionGate,
	queue Enqueuer,
	thresholdUSD string,
) (*TravelRuleService, error) {
	if repo == nil || valuator == nil || txGate == nil || queue == nil {
		return nil, errors.New("Travel Rule repository, USD valuator, transaction gate, and queue are required")
	}
	senderVASPID = strings.TrimSpace(senderVASPID)
	if senderVASPID == "" {
		return nil, errors.New("TRAVEL_RULE_ORIGINATING_VASP_ID is required")
	}
	threshold, err := decimal.NewFromString(thresholdUSD)
	if err != nil || threshold.Sign() <= 0 {
		return nil, errors.New("TRAVEL_RULE_THRESHOLD_USD must be a positive decimal")
	}
	directory, err := LoadVASPDirectory(directoryJSON)
	if err != nil {
		return nil, err
	}
	if sender == nil {
		sender = NewHTTPExchangeSender()
	}
	return &TravelRuleService{
		repo: repo, valuator: valuator, vasps: directory, senderVASPID: senderVASPID,
		sender: sender, txGate: txGate, queue: queue, thresholdUSD: threshold,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// Requires is called before transaction persistence. A missing market price or
// missing required PII fails closed; clients cannot submit their own USD value.
func (s *TravelRuleService) Requires(ctx context.Context, asset string, amount decimal.Decimal, request *domain.TravelRuleRequest) (bool, error) {
	valueUSD, err := s.valuator.USDValue(ctx, asset, amount)
	if err != nil || valueUSD.Sign() < 0 {
		if err == nil {
			err = errors.New("USD valuation must be non-negative")
		}
		return false, fmt.Errorf("value transfer for Travel Rule threshold: %w", err)
	}
	if valueUSD.LessThan(s.thresholdUSD) {
		return false, nil
	}
	if request == nil || strings.TrimSpace(request.RecipientVASPID) == "" || len(request.IVMS101) == 0 {
		return false, ErrTravelRuleDetailsRequired
	}
	if _, ok := s.vasps[request.RecipientVASPID]; !ok {
		return false, ErrUnknownRecipientVASP
	}
	if _, err := ValidateIVMS101(request.IVMS101); err != nil {
		return false, ErrInvalidIVMS101
	}
	return true, nil
}

// Begin durably stores an encrypted envelope before making the network call.
// A nil confirmation means the VASP accepted the message asynchronously; the
// transfer remains held until HandleConfirmation verifies a signed reply.
func (s *TravelRuleService) Begin(
	ctx context.Context,
	tx *domain.Transaction,
	request *domain.TravelRuleRequest,
	releaseStatus domain.TransactionStatus,
) (domain.TravelRuleExchangeStatus, error) {
	if tx == nil || request == nil {
		return "", ErrTravelRuleDetailsRequired
	}
	vasp, ok := s.vasps[request.RecipientVASPID]
	if !ok {
		return "", ErrUnknownRecipientVASP
	}
	ivms, err := ValidateIVMS101(request.IVMS101)
	if err != nil {
		return "", ErrInvalidIVMS101
	}
	messageID := uuid.NewString()
	message := travelRuleMessage{IVMS101: json.RawMessage(ivms)}
	message.Transfer.ID = tx.ID
	message.Transfer.Asset = tx.Asset
	message.Transfer.Amount = tx.Amount.StringFixed(7)
	plaintext, err := json.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("marshal Travel Rule message: %w", err)
	}
	encrypted, err := encryptTravelRulePayload(tx.ID, messageID, s.senderVASPID, vasp, plaintext)
	if err != nil {
		return "", fmt.Errorf("encrypt Travel Rule message: %w", err)
	}
	now := s.now()
	envelope := TravelRuleEnvelope{
		Version: 1, MessageID: messageID, TransferID: tx.ID, SenderVASPID: s.senderVASPID,
		RecipientVASPID: vasp.ID, Protocol: vasp.Protocol, CreatedAt: now, Encrypted: encrypted,
	}
	envelope.EnvelopeSHA256 = EnvelopeDigest(envelope)
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("marshal encrypted Travel Rule envelope: %w", err)
	}
	tenantID := ""
	if tx.TenantID != nil {
		tenantID = *tx.TenantID
	}
	exchange := &domain.TravelRuleExchange{
		MessageID: messageID, TransferID: tx.ID, TenantID: tenantID, Mode: tx.Mode,
		RecipientVASPID: vasp.ID, Protocol: vasp.Protocol, EnvelopeJSON: envelopeJSON,
		EnvelopeSHA256: envelope.EnvelopeSHA256, Status: domain.TravelRuleExchangePending,
		ReleaseStatus: releaseStatus, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.CreateTravelRuleExchange(ctx, exchange); err != nil {
		return "", fmt.Errorf("persist encrypted Travel Rule exchange: %w", err)
	}
	confirmation, err := s.sender.Send(ctx, vasp, envelope)
	if err != nil {
		return "", fmt.Errorf("send Travel Rule message: %w", err)
	}
	if confirmation == nil {
		return domain.TravelRuleExchangePending, nil
	}
	status, err := s.processConfirmation(ctx, *confirmation, false)
	if err != nil {
		return "", err
	}
	return status, nil
}

// HandleConfirmation is mounted on a public callback route. It accepts only a
// fresh, signed confirmation matching a previously stored outbound message.
func (s *TravelRuleService) HandleConfirmation(ctx context.Context, confirmation domain.TravelRuleConfirmation) error {
	_, err := s.processConfirmation(ctx, confirmation, true)
	return err
}

func (s *TravelRuleService) processConfirmation(
	ctx context.Context,
	confirmation domain.TravelRuleConfirmation,
	release bool,
) (domain.TravelRuleExchangeStatus, error) {
	if confirmation.Status != string(domain.TravelRuleExchangeConfirmed) &&
		confirmation.Status != string(domain.TravelRuleExchangeRejected) {
		return "", ErrInvalidTravelRuleConfirmation
	}
	exchange, err := s.repo.GetTravelRuleExchange(ctx, confirmation.TransferID)
	if err != nil || exchange == nil {
		return "", ErrInvalidTravelRuleConfirmation
	}
	vasp, ok := s.vasps[exchange.RecipientVASPID]
	if !ok || confirmation.VASPID != exchange.RecipientVASPID ||
		confirmation.MessageID != exchange.MessageID ||
		confirmation.EnvelopeSHA256 != exchange.EnvelopeSHA256 ||
		confirmation.ConfirmationID == "" {
		return "", ErrInvalidTravelRuleConfirmation
	}
	now := s.now()
	if confirmation.IssuedAt.IsZero() ||
		confirmation.IssuedAt.After(now.Add(2*time.Minute)) ||
		now.Sub(confirmation.IssuedAt) > TravelRuleConfirmationMaxAge {
		return "", ErrInvalidTravelRuleConfirmation
	}
	publicKeyBytes, err := decodeTravelRuleKey(vasp.ConfirmationPublicKey)
	if err != nil || len(publicKeyBytes) != ed25519.PublicKeySize {
		return "", ErrInvalidTravelRuleConfirmation
	}
	signature, err := decodeTravelRuleKey(confirmation.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize ||
		!ed25519.Verify(ed25519.PublicKey(publicKeyBytes), ConfirmationSigningBytes(confirmation), signature) {
		return "", ErrInvalidTravelRuleConfirmation
	}
	status := domain.TravelRuleExchangeStatus(confirmation.Status)
	if err := s.repo.SaveTravelRuleConfirmation(ctx, exchange.TransferID, status, confirmation.ConfirmationID, confirmation.IssuedAt.UTC()); err != nil {
		return "", fmt.Errorf("persist Travel Rule confirmation: %w", err)
	}
	if release {
		if err := s.applyConfirmation(ctx, exchange, status); err != nil {
			return "", err
		}
	}
	return status, nil
}

type confirmationSigningPayload struct {
	ConfirmationID string `json:"confirmation_id"`
	MessageID      string `json:"message_id"`
	TransferID     string `json:"transfer_id"`
	VASPID         string `json:"vasp_id"`
	Status         string `json:"status"`
	EnvelopeSHA256 string `json:"envelope_sha256"`
	IssuedAt       string `json:"issued_at"`
}

// ConfirmationSigningBytes is the exact canonical JSON sequence signed by
// recipient VASPs with their configured Ed25519 confirmation key.
func ConfirmationSigningBytes(c domain.TravelRuleConfirmation) []byte {
	payload := confirmationSigningPayload{
		ConfirmationID: c.ConfirmationID, MessageID: c.MessageID, TransferID: c.TransferID,
		VASPID: c.VASPID, Status: c.Status, EnvelopeSHA256: c.EnvelopeSHA256,
		IssuedAt: c.IssuedAt.UTC().Format(time.RFC3339Nano),
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func (s *TravelRuleService) applyConfirmation(ctx context.Context, exchange *domain.TravelRuleExchange, status domain.TravelRuleExchangeStatus) error {
	ctx = tenant.WithMode(ctx, exchange.Mode)
	if exchange.TenantID != "" {
		ctx = tenant.WithID(ctx, exchange.TenantID)
	}
	if status == domain.TravelRuleExchangeRejected {
		if err := s.txGate.UpdateStatus(ctx, exchange.TransferID, domain.StatusFailed, ""); err != nil {
			return fmt.Errorf("mark Travel Rule rejected transfer failed: %w", err)
		}
		return nil
	}
	if exchange.ReleaseStatus == domain.StatusComplianceHold {
		return nil // independent sanctions/manual-review hold remains in force
	}
	if exchange.ReleaseStatus != domain.StatusPending && exchange.ReleaseStatus != domain.StatusApprovalPending {
		return errors.New("invalid stored Travel Rule release status")
	}
	if err := s.txGate.UpdateStatus(ctx, exchange.TransferID, exchange.ReleaseStatus, ""); err != nil {
		return fmt.Errorf("release Travel Rule transfer: %w", err)
	}
	if exchange.ReleaseStatus == domain.StatusPending {
		if err := s.queue.EnqueueTransfer(ctx, exchange.TransferID); err != nil {
			return fmt.Errorf("enqueue Travel Rule-cleared transfer: %w", err)
		}
	}
	return nil
}

func encryptTravelRulePayload(transferID, messageID, senderVASPID string, recipient VASP, plaintext []byte) (EncryptedTravelRulePayload, error) {
	publicBytes, err := decodeTravelRuleKey(recipient.EncryptionPublicKey)
	if err != nil || len(publicBytes) != 32 {
		return EncryptedTravelRulePayload{}, errors.New("invalid recipient X25519 key")
	}
	publicKey, err := ecdh.X25519().NewPublicKey(publicBytes)
	if err != nil {
		return EncryptedTravelRulePayload{}, errors.New("invalid recipient X25519 key")
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return EncryptedTravelRulePayload{}, err
	}
	shared, err := ephemeral.ECDH(publicKey)
	if err != nil {
		return EncryptedTravelRulePayload{}, errors.New("derive Travel Rule encryption key")
	}
	key, err := deriveTravelRuleKey(shared, transferID, messageID, senderVASPID, recipient.ID)
	if err != nil {
		return EncryptedTravelRulePayload{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return EncryptedTravelRulePayload{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedTravelRulePayload{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return EncryptedTravelRulePayload{}, err
	}
	aad := travelRuleAAD(transferID, messageID, senderVASPID, recipient.ID)
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	return EncryptedTravelRulePayload{
		Version: 1,
		EphemeralPublicKey: base64.RawURLEncoding.EncodeToString(ephemeral.PublicKey().Bytes()),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}, nil
}

// DecryptTravelRulePayload is exported for interoperability tests and
// counterparties implementing this encrypted envelope profile.
func DecryptTravelRulePayload(transferID, messageID, senderVASPID, recipientVASPID string, recipientPrivateKey []byte, encrypted EncryptedTravelRulePayload) ([]byte, error) {
	if encrypted.Version != 1 || len(recipientPrivateKey) != 32 {
		return nil, errors.New("invalid Travel Rule envelope")
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(recipientPrivateKey)
	if err != nil {
		return nil, errors.New("invalid recipient X25519 private key")
	}
	ephemeralBytes, err := decodeTravelRuleKey(encrypted.EphemeralPublicKey)
	if err != nil {
		return nil, errors.New("invalid ephemeral X25519 key")
	}
	ephemeral, err := ecdh.X25519().NewPublicKey(ephemeralBytes)
	if err != nil {
		return nil, errors.New("invalid ephemeral X25519 key")
	}
	shared, err := privateKey.ECDH(ephemeral)
	if err != nil {
		return nil, errors.New("derive Travel Rule decryption key")
	}
	key, err := deriveTravelRuleKey(shared, transferID, messageID, senderVASPID, recipientVASPID)
	if err != nil {
		return nil, err
	}
	nonce, err := decodeTravelRuleKey(encrypted.Nonce)
	if err != nil {
		return nil, errors.New("invalid Travel Rule nonce")
	}
	ciphertext, err := decodeTravelRuleKey(encrypted.Ciphertext)
	if err != nil {
		return nil, errors.New("invalid Travel Rule ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, travelRuleAAD(transferID, messageID, senderVASPID, recipientVASPID))
	if err != nil {
		return nil, errors.New("Travel Rule ciphertext authentication failed")
	}
	return plaintext, nil
}

func deriveTravelRuleKey(shared []byte, transferID, messageID, senderVASPID, recipientVASPID string) ([]byte, error) {
	info := []byte("Nexora/TravelRule/v1|" + messageID + "|" + senderVASPID + "|" + recipientVASPID)
	reader := hkdf.New(sha256.New, shared, []byte(transferID), info)
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, err
	}
	return key, nil
}

func travelRuleAAD(transferID, messageID, senderVASPID, recipientVASPID string) []byte {
	return []byte("v1|" + transferID + "|" + messageID + "|" + senderVASPID + "|" + recipientVASPID)
}

func decodeTravelRuleKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64 key")
}

// HTTPExchangeSender provides distinct TRP and OpenVASP HTTP envelope profiles.
// The operator must configure the actual partner endpoint, authentication/mTLS,
// and wire profile; this does not claim vendor-independent certification.
type HTTPExchangeSender struct {
	client *http.Client
}

func NewHTTPExchangeSender() *HTTPExchangeSender {
	return &HTTPExchangeSender{client: &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (s *HTTPExchangeSender) clientFor(recipient VASP) (*http.Client, error) {
	base := s.client
	if base == nil {
		base = NewHTTPExchangeSender().client
	}
	if recipient.ClientCertFile == "" && recipient.ClientKeyFile == "" {
		return base, nil
	}
	certificate, err := tls.LoadX509KeyPair(recipient.ClientCertFile, recipient.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load VASP mTLS client certificate: %w", err)
	}
	var transport *http.Transport
	if base.Transport == nil {
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("default HTTP transport does not support mTLS")
		}
		transport = defaultTransport.Clone()
	} else {
		existing, ok := base.Transport.(*http.Transport)
		if !ok {
			return nil, errors.New("configured HTTP transport does not support mTLS")
		}
		transport = existing.Clone()
	}
	tlsConfig := transport.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tlsConfig = tlsConfig.Clone()
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	tlsConfig.Certificates = []tls.Certificate{certificate}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport, Timeout: base.Timeout, CheckRedirect: base.CheckRedirect}, nil
}

func (s *HTTPExchangeSender) Send(ctx context.Context, recipient VASP, envelope TravelRuleEnvelope) (*domain.TravelRuleConfirmation, error) {
	var body interface{}
	switch recipient.Protocol {
	case TravelRuleProtocolTRP:
		body = struct {
			Type    string             `json:"type"`
			Message TravelRuleEnvelope `json:"message"`
		}{Type: "trp.message.v1", Message: envelope}
	case TravelRuleProtocolOpenVASP:
		body = struct {
			Type    string             `json:"type"`
			ID      string             `json:"id"`
			Payload TravelRuleEnvelope `json:"payload"`
		}{Type: "openvasp.message.v1", ID: envelope.MessageID, Payload: envelope}
	default:
		return nil, errors.New("unsupported Travel Rule protocol")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, recipient.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Travel-Rule-Protocol", recipient.Protocol)
	req.Header.Set("Idempotency-Key", envelope.MessageID)
	if recipient.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+recipient.BearerToken)
	}
	client, err := s.clientFor(recipient)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send Travel Rule request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("recipient VASP returned HTTP %d", resp.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil || len(responseBody) > 64*1024 {
		return nil, errors.New("invalid Travel Rule response body")
	}
	var wrapped struct {
		Confirmation *domain.TravelRuleConfirmation `json:"confirmation"`
	}
	if err := json.Unmarshal(responseBody, &wrapped); err == nil && wrapped.Confirmation != nil {
		return wrapped.Confirmation, nil
	}
	var confirmation domain.TravelRuleConfirmation
	if err := json.Unmarshal(responseBody, &confirmation); err != nil || confirmation.MessageID == "" {
		return nil, errors.New("recipient VASP response did not contain a confirmation")
	}
	return &confirmation, nil
}
