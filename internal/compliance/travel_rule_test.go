package compliance

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/shopspring/decimal"
)

const validIVMS101 = `{
  "originator": {
    "originatorPersons": [{
      "naturalPerson": {
        "name": {"nameIdentifier": [{"primaryIdentifier": "DOE", "secondaryIdentifier": "JANE", "nameIdentifierType": "LEGL"}]}
      }
    }],
    "accountNumber": ["originator-ref"]
  },
  "beneficiary": {
    "beneficiaryPersons": [{
      "naturalPerson": {
        "name": {"nameIdentifier": [{"primaryIdentifier": "SMITH", "secondaryIdentifier": "ALEX", "nameIdentifierType": "LEGL"}]}
      }
    }],
    "accountNumber": ["beneficiary-ref"]
  },
  "partnerExtension": {"retained": true}
}`

type fixedUSDValuator struct {
	value decimal.Decimal
	err   error
}

func (v fixedUSDValuator) USDValue(context.Context, string, decimal.Decimal) (decimal.Decimal, error) {
	return v.value, v.err
}

type memoryTravelRuleRepo struct {
	exchanges map[string]*domain.TravelRuleExchange
}

func newMemoryTravelRuleRepo() *memoryTravelRuleRepo {
	return &memoryTravelRuleRepo{exchanges: make(map[string]*domain.TravelRuleExchange)}
}

func (r *memoryTravelRuleRepo) CreateTravelRuleExchange(_ context.Context, e *domain.TravelRuleExchange) error {
	if _, exists := r.exchanges[e.TransferID]; exists {
		return errors.New("duplicate transfer exchange")
	}
	copy := *e
	copy.EnvelopeJSON = append([]byte(nil), e.EnvelopeJSON...)
	r.exchanges[e.TransferID] = &copy
	return nil
}

func (r *memoryTravelRuleRepo) GetTravelRuleExchange(_ context.Context, transferID string) (*domain.TravelRuleExchange, error) {
	e, ok := r.exchanges[transferID]
	if !ok {
		return nil, errors.New("not found")
	}
	copy := *e
	copy.EnvelopeJSON = append([]byte(nil), e.EnvelopeJSON...)
	return &copy, nil
}

func (r *memoryTravelRuleRepo) SaveTravelRuleConfirmation(_ context.Context, transferID string, status domain.TravelRuleExchangeStatus, confirmationID string, at time.Time) error {
	e, ok := r.exchanges[transferID]
	if !ok {
		return errors.New("not found")
	}
	if e.Status == domain.TravelRuleExchangePending {
		e.Status = status
		e.ConfirmationID = confirmationID
		e.ConfirmedAt = &at
		e.UpdatedAt = at
		return nil
	}
	if e.Status == status && e.ConfirmationID == confirmationID {
		return nil
	}
	return errors.New("conflicting confirmation")
}

type recordingTransactionGate struct {
	statuses []domain.TransactionStatus
	ids      []string
}

func (g *recordingTransactionGate) GetByID(context.Context, string) (*domain.Transaction, error) {
	return nil, domain.ErrTransactionNotFound
}

func (g *recordingTransactionGate) UpdateStatus(_ context.Context, id string, status domain.TransactionStatus, _ string) error {
	g.ids = append(g.ids, id)
	g.statuses = append(g.statuses, status)
	return nil
}

type recordingTravelRuleQueue struct {
	ids []string
}

func (q *recordingTravelRuleQueue) EnqueueTransfer(_ context.Context, id string) error {
	q.ids = append(q.ids, id)
	return nil
}

type testExchangeSender struct {
	privateKey ed25519.PrivateKey
	immediate  bool
	envelope   TravelRuleEnvelope
}

func (s *testExchangeSender) Send(_ context.Context, vasp VASP, envelope TravelRuleEnvelope) (*domain.TravelRuleConfirmation, error) {
	s.envelope = envelope
	if !s.immediate {
		return nil, nil
	}
	confirmation := signedTestConfirmation(envelope, vasp.ID, s.privateKey, string(domain.TravelRuleExchangeConfirmed))
	return &confirmation, nil
}

func signedTestConfirmation(envelope TravelRuleEnvelope, vaspID string, privateKey ed25519.PrivateKey, status string) domain.TravelRuleConfirmation {
	confirmation := domain.TravelRuleConfirmation{
		ConfirmationID: "confirmation-" + envelope.MessageID,
		MessageID: envelope.MessageID,
		TransferID: envelope.TransferID,
		VASPID: vaspID,
		Status: status,
		EnvelopeSHA256: envelope.EnvelopeSHA256,
		IssuedAt: time.Now().UTC(),
	}
	confirmation.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, ConfirmationSigningBytes(confirmation)))
	return confirmation
}

func setupTravelRuleTest(t *testing.T, immediate bool) (*TravelRuleService, *memoryTravelRuleRepo, *testExchangeSender, *recordingTransactionGate, *recordingTravelRuleQueue, *domain.Transaction, *domain.TravelRuleRequest, VASP) {
	t.Helper()
	recipientPrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifyPublic, verifyPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vasp := VASP{
		ID: "vasp-recipient",
		Protocol: TravelRuleProtocolTRP,
		Endpoint: "https://recipient.example/trp",
		EncryptionPublicKey: base64.RawURLEncoding.EncodeToString(recipientPrivate.PublicKey().Bytes()),
		ConfirmationPublicKey: base64.RawURLEncoding.EncodeToString(verifyPublic),
	}
	directory, err := json.Marshal([]VASP{vasp})
	if err != nil {
		t.Fatal(err)
	}
	repo := newMemoryTravelRuleRepo()
	sender := &testExchangeSender{privateKey: verifyPrivate, immediate: immediate}
	gate := &recordingTransactionGate{}
	queue := &recordingTravelRuleQueue{}
	service, err := NewTravelRuleService(repo, fixedUSDValuator{value: decimal.NewFromInt(1500)}, string(directory), "nexora", sender, gate, queue, "1000")
	if err != nil {
		t.Fatal(err)
	}
	tenantID := "8d66a42b-3b61-4e18-8d2e-3dd03c27e4f8"
	tx := &domain.Transaction{
		ID: "6c9d7f6e-b43d-4b4b-9f2e-47b1d4e5eaf0",
		Status: domain.StatusComplianceHold,
		Mode: domain.ModeLive,
		TenantID: &tenantID,
		Asset: "USDC",
		Amount: decimal.NewFromInt(1500),
	}
	request := &domain.TravelRuleRequest{RecipientVASPID: vasp.ID, IVMS101: json.RawMessage(validIVMS101)}
	_ = recipientPrivate
	return service, repo, sender, gate, queue, tx, request, vasp
}

func TestValidateIVMS101UsesStandardCamelCaseAndPreservesOptionalFields(t *testing.T) {
	compact, err := ValidateIVMS101([]byte(validIVMS101))
	if err != nil {
		t.Fatalf("ValidateIVMS101: %v", err)
	}
	if !strings.Contains(string(compact), `"originatorPersons"`) || !strings.Contains(string(compact), `"partnerExtension"`) {
		t.Fatalf("payload lost standard fields or unknown optional fields: %s", compact)
	}
}

func TestBuildIVMS101PayloadGeneratesStandardCamelCaseJSON(t *testing.T) {
	var input IVMS101Payload
	if err := json.Unmarshal([]byte(validIVMS101), &input); err != nil {
		t.Fatal(err)
	}
	raw, err := BuildIVMS101Payload(input.Originator, input.Beneficiary, input.OriginatingVASP, input.BeneficiaryVASP)
	if err != nil {
		t.Fatalf("BuildIVMS101Payload: %v", err)
	}
	var generated map[string]interface{}
	if err := json.Unmarshal(raw, &generated); err != nil {
		t.Fatal(err)
	}
	if _, ok := generated["originator"]; !ok {
		t.Fatalf("generated payload has no originator: %s", raw)
	}
	if _, ok := generated["beneficiary"]; !ok {
		t.Fatalf("generated payload has no beneficiary: %s", raw)
	}
	originator, ok := generated["originator"].(map[string]interface{})
	if !ok || originator["originatorPersons"] == nil {
		t.Fatalf("generated originator does not use IVMS 101 field names: %s", raw)
	}
}

func TestValidateIVMS101RejectsMissingPartyIdentity(t *testing.T) {
	invalid := `{"originator":{"originatorPersons":[],"accountNumber":["a"]},"beneficiary":{"beneficiaryPersons":[],"accountNumber":["b"]}}`
	if _, err := ValidateIVMS101([]byte(invalid)); !errors.Is(err, ErrInvalidIVMS101) {
		t.Fatalf("err = %v, want ErrInvalidIVMS101", err)
	}
	if _, err := ValidateIVMS101([]byte(strings.Repeat("x", MaxIVMS101PayloadBytes+1))); !errors.Is(err, ErrInvalidIVMS101) {
		t.Fatalf("oversized payload err = %v, want ErrInvalidIVMS101", err)
	}
}

func TestEncryptTravelRulePayloadRoundTripAndAuthenticatedMetadata(t *testing.T) {
	recipientPrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vasp := VASP{
		ID: "recipient",
		EncryptionPublicKey: base64.RawURLEncoding.EncodeToString(recipientPrivate.PublicKey().Bytes()),
	}
	plaintext := []byte(`{"originator":{"name":"private"}}`)
	encrypted, err := encryptTravelRulePayload("transfer-1", "message-1", "nexora", vasp, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := DecryptTravelRulePayload("transfer-1", "message-1", "nexora", "recipient", recipientPrivate.Bytes(), encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}
	if _, err := DecryptTravelRulePayload("transfer-other", "message-1", "nexora", "recipient", recipientPrivate.Bytes(), encrypted); err == nil {
		t.Fatal("decryption succeeded after transfer ID changed")
	}
	encrypted.Ciphertext = encrypted.Ciphertext[:len(encrypted.Ciphertext)-2] + "AA"
	if _, err := DecryptTravelRulePayload("transfer-1", "message-1", "nexora", "recipient", recipientPrivate.Bytes(), encrypted); err == nil {
		t.Fatal("tampered ciphertext decrypted successfully")
	}
}

func TestMissingTravelRuleDataFailsClosedAtThreshold(t *testing.T) {
	service, _, _, _, _, _, _, _ := setupTravelRuleTest(t, false)
	required, err := service.Requires(context.Background(), "USDC", decimal.NewFromInt(1000), nil)
	if required || !errors.Is(err, ErrTravelRuleDetailsRequired) {
		t.Fatalf("Requires = (%v, %v), want false and ErrTravelRuleDetailsRequired", required, err)
	}
}

func TestSignedConfirmationReleasesTransferAndEnqueuesSettlement(t *testing.T) {
	service, repo, sender, gate, queue, tx, request, _ := setupTravelRuleTest(t, false)
	required, err := service.Requires(context.Background(), tx.Asset, tx.Amount, request)
	if err != nil || !required {
		t.Fatalf("Requires = (%v, %v), want true, nil", required, err)
	}
	status, err := service.Begin(context.Background(), tx, request, domain.StatusPending)
	if err != nil || status != domain.TravelRuleExchangePending {
		t.Fatalf("Begin = (%v, %v), want pending, nil", status, err)
	}
	stored, err := repo.GetTravelRuleExchange(context.Background(), tx.ID)
	if err != nil || stored.Status != domain.TravelRuleExchangePending {
		t.Fatalf("stored exchange = (%+v, %v), want pending", stored, err)
	}
	confirmation := signedTestConfirmation(sender.envelope, "vasp-recipient", sender.privateKey, string(domain.TravelRuleExchangeConfirmed))
	if err := service.HandleConfirmation(context.Background(), confirmation); err != nil {
		t.Fatalf("HandleConfirmation: %v", err)
	}
	if len(gate.statuses) != 1 || gate.statuses[0] != domain.StatusPending {
		t.Fatalf("status updates = %v, want [pending]", gate.statuses)
	}
	if len(queue.ids) != 1 || queue.ids[0] != tx.ID {
		t.Fatalf("queued IDs = %v, want transfer %s", queue.ids, tx.ID)
	}
	stored, err = repo.GetTravelRuleExchange(context.Background(), tx.ID)
	if err != nil || stored.Status != domain.TravelRuleExchangeConfirmed {
		t.Fatalf("stored status = %v, err = %v, want confirmed", stored.Status, err)
	}
}

func TestInvalidSignatureCannotReleaseTransfer(t *testing.T) {
	service, _, sender, gate, queue, tx, request, _ := setupTravelRuleTest(t, false)
	if _, err := service.Begin(context.Background(), tx, request, domain.StatusPending); err != nil {
		t.Fatal(err)
	}
	confirmation := signedTestConfirmation(sender.envelope, "vasp-recipient", sender.privateKey, string(domain.TravelRuleExchangeConfirmed))
	confirmation.Signature = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	err := service.HandleConfirmation(context.Background(), confirmation)
	if !errors.Is(err, ErrInvalidTravelRuleConfirmation) {
		t.Fatalf("HandleConfirmation err = %v, want invalid confirmation", err)
	}
	if len(gate.statuses) != 0 || len(queue.ids) != 0 {
		t.Fatalf("invalid confirmation changed transfer: statuses=%v queue=%v", gate.statuses, queue.ids)
	}
}

func TestTravelRuleConfirmationDoesNotClearIndependentComplianceHold(t *testing.T) {
	service, _, sender, gate, queue, tx, request, _ := setupTravelRuleTest(t, false)
	if _, err := service.Begin(context.Background(), tx, request, domain.StatusComplianceHold); err != nil {
		t.Fatal(err)
	}
	confirmation := signedTestConfirmation(sender.envelope, "vasp-recipient", sender.privateKey, string(domain.TravelRuleExchangeConfirmed))
	if err := service.HandleConfirmation(context.Background(), confirmation); err != nil {
		t.Fatalf("HandleConfirmation: %v", err)
	}
	if len(gate.statuses) != 0 || len(queue.ids) != 0 {
		t.Fatalf("independent hold was cleared: statuses=%v queue=%v", gate.statuses, queue.ids)
	}
}

func TestStaleConfirmationCannotReleaseTransfer(t *testing.T) {
	service, _, sender, gate, queue, tx, request, _ := setupTravelRuleTest(t, false)
	if _, err := service.Begin(context.Background(), tx, request, domain.StatusPending); err != nil {
		t.Fatal(err)
	}
	confirmation := signedTestConfirmation(sender.envelope, "vasp-recipient", sender.privateKey, string(domain.TravelRuleExchangeConfirmed))
	confirmation.IssuedAt = time.Now().UTC().Add(-TravelRuleConfirmationMaxAge - time.Minute)
	confirmation.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(sender.privateKey, ConfirmationSigningBytes(confirmation)))
	if err := service.HandleConfirmation(context.Background(), confirmation); !errors.Is(err, ErrInvalidTravelRuleConfirmation) {
		t.Fatalf("HandleConfirmation err = %v, want stale confirmation rejected", err)
	}
	if len(gate.statuses) != 0 || len(queue.ids) != 0 {
		t.Fatalf("stale confirmation changed transfer: statuses=%v queue=%v", gate.statuses, queue.ids)
	}
}

func TestMismatchedEnvelopeDigestCannotReleaseTransfer(t *testing.T) {
	service, _, sender, gate, queue, tx, request, _ := setupTravelRuleTest(t, false)
	if _, err := service.Begin(context.Background(), tx, request, domain.StatusPending); err != nil {
		t.Fatal(err)
	}
	confirmation := signedTestConfirmation(sender.envelope, "vasp-recipient", sender.privateKey, string(domain.TravelRuleExchangeConfirmed))
	confirmation.EnvelopeSHA256 = strings.Repeat("0", 64)
	confirmation.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(sender.privateKey, ConfirmationSigningBytes(confirmation)))
	if err := service.HandleConfirmation(context.Background(), confirmation); !errors.Is(err, ErrInvalidTravelRuleConfirmation) {
		t.Fatalf("HandleConfirmation err = %v, want mismatched digest rejected", err)
	}
	if len(gate.statuses) != 0 || len(queue.ids) != 0 {
		t.Fatalf("mismatched digest changed transfer: statuses=%v queue=%v", gate.statuses, queue.ids)
	}
}

func TestUSDValuationFailureFailsClosed(t *testing.T) {
	service, _, _, _, _, _, request, _ := setupTravelRuleTest(t, false)
	service.valuator = fixedUSDValuator{err: errors.New("rate unavailable")}
	required, err := service.Requires(context.Background(), "XLM", decimal.NewFromInt(1500), request)
	if required || err == nil {
		t.Fatalf("Requires = (%v, %v), want valuation failure", required, err)
	}
}

func TestRejectedSignedConfirmationFailsTransfer(t *testing.T) {
	service, _, sender, gate, queue, tx, request, _ := setupTravelRuleTest(t, false)
	if _, err := service.Begin(context.Background(), tx, request, domain.StatusPending); err != nil {
		t.Fatal(err)
	}
	confirmation := signedTestConfirmation(sender.envelope, "vasp-recipient", sender.privateKey, string(domain.TravelRuleExchangeRejected))
	if err := service.HandleConfirmation(context.Background(), confirmation); err != nil {
		t.Fatalf("HandleConfirmation: %v", err)
	}
	if len(gate.statuses) != 1 || gate.statuses[0] != domain.StatusFailed || len(queue.ids) != 0 {
		t.Fatalf("rejected transfer state = statuses %v, queue %v", gate.statuses, queue.ids)
	}
}

func TestImmediateConfirmationIsVerifiedButCallerOwnsInitialRelease(t *testing.T) {
	service, repo, _, gate, queue, tx, request, _ := setupTravelRuleTest(t, true)
	status, err := service.Begin(context.Background(), tx, request, domain.StatusPending)
	if err != nil || status != domain.TravelRuleExchangeConfirmed {
		t.Fatalf("Begin = (%v, %v), want confirmed, nil", status, err)
	}
	exchange, err := repo.GetTravelRuleExchange(context.Background(), tx.ID)
	if err != nil || exchange.Status != domain.TravelRuleExchangeConfirmed {
		t.Fatalf("exchange status = %v, err = %v", exchange.Status, err)
	}
	if len(gate.statuses) != 0 || len(queue.ids) != 0 {
		t.Fatalf("Begin must leave initial release to transfer service: statuses=%v queue=%v", gate.statuses, queue.ids)
	}
}
