package compliance

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const MaxIVMS101PayloadBytes = 64 * 1024

var ErrInvalidIVMS101 = errors.New("invalid IVMS 101 payload")

// IVMS101Payload models the IVMS 101 party envelope using the standard's
// camelCase field names. Unknown optional fields are preserved in the original
// JSON sent to the counterparty; these types validate required identity data.
type IVMS101Payload struct {
	Originator      IVMS101Originator  `json:"originator"`
	Beneficiary     IVMS101Beneficiary `json:"beneficiary"`
	OriginatingVASP json.RawMessage    `json:"originatingVASP,omitempty"`
	BeneficiaryVASP json.RawMessage    `json:"beneficiaryVASP,omitempty"`
}

type IVMS101Originator struct {
	OriginatorPersons      []IVMS101Person `json:"originatorPersons"`
	AccountNumber          []string        `json:"accountNumber,omitempty"`
	CustomerIdentification string          `json:"customerIdentification,omitempty"`
}

type IVMS101Beneficiary struct {
	BeneficiaryPersons     []IVMS101Person `json:"beneficiaryPersons"`
	AccountNumber          []string        `json:"accountNumber,omitempty"`
	CustomerIdentification string          `json:"customerIdentification,omitempty"`
}

type IVMS101Person struct {
	NaturalPerson *IVMS101NaturalPerson `json:"naturalPerson,omitempty"`
	LegalPerson   *IVMS101LegalPerson   `json:"legalPerson,omitempty"`
}

type IVMS101NaturalPerson struct {
	Name                   IVMS101Name                     `json:"name"`
	GeographicAddress      []IVMS101GeographicAddress      `json:"geographicAddress,omitempty"`
	NationalIdentification []IVMS101NationalIdentification `json:"nationalIdentification,omitempty"`
	CustomerIdentification string                          `json:"customerIdentification,omitempty"`
}

type IVMS101LegalPerson struct {
	Name                   IVMS101Name                     `json:"name"`
	GeographicAddress      []IVMS101GeographicAddress      `json:"geographicAddress,omitempty"`
	NationalIdentification []IVMS101NationalIdentification `json:"nationalIdentification,omitempty"`
	CustomerIdentification string                          `json:"customerIdentification,omitempty"`
}

type IVMS101Name struct {
	NameIdentifier []IVMS101NameIdentifier `json:"nameIdentifier"`
}

type IVMS101NameIdentifier struct {
	PrimaryIdentifier             string `json:"primaryIdentifier,omitempty"`
	SecondaryIdentifier           string `json:"secondaryIdentifier,omitempty"`
	NameIdentifierType            string `json:"nameIdentifierType,omitempty"`
	LegalPersonName               string `json:"legalPersonName,omitempty"`
	LegalPersonNameIdentifierType string `json:"legalPersonNameIdentifierType,omitempty"`
	ScriptCode                    string `json:"scriptCode,omitempty"`
}

type IVMS101GeographicAddress struct {
	AddressType        string   `json:"addressType,omitempty"`
	Department         string   `json:"department,omitempty"`
	SubDepartment      string   `json:"subDepartment,omitempty"`
	StreetName         string   `json:"streetName,omitempty"`
	BuildingNumber     string   `json:"buildingNumber,omitempty"`
	BuildingName       string   `json:"buildingName,omitempty"`
	Floor              string   `json:"floor,omitempty"`
	PostBox            string   `json:"postBox,omitempty"`
	Room               string   `json:"room,omitempty"`
	Postcode           string   `json:"postCode,omitempty"`
	TownName           string   `json:"townName,omitempty"`
	TownLocationName   string   `json:"townLocationName,omitempty"`
	DistrictName       string   `json:"districtName,omitempty"`
	CountrySubDivision string   `json:"countrySubDivision,omitempty"`
	AddressLine        []string `json:"addressLine,omitempty"`
	Country            string   `json:"country,omitempty"`
}

type IVMS101NationalIdentification struct {
	NationalIdentifier     string `json:"nationalIdentifier"`
	NationalIdentifierType string `json:"nationalIdentifierType"`
	CountryOfIssue         string `json:"countryOfIssue,omitempty"`
	RegistrationAuthority  string `json:"registrationAuthority,omitempty"`
}

// BuildIVMS101Payload generates a standardized IVMS 101 party envelope from
// validated originator and beneficiary records. Optional VASP records can be
// supplied as JSON to preserve the standard's extensible legal-person schema.
func BuildIVMS101Payload(originator IVMS101Originator, beneficiary IVMS101Beneficiary, originatingVASP, beneficiaryVASP json.RawMessage) ([]byte, error) {
	payload := IVMS101Payload{
		Originator: originator, Beneficiary: beneficiary,
		OriginatingVASP: originatingVASP, BeneficiaryVASP: beneficiaryVASP,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalidIVMS101
	}
	return ValidateIVMS101(raw)
}

// ValidateIVMS101 validates the required IVMS 101 party fields and returns a
// compact copy of the original JSON. It never reflects PII in an error.
func ValidateIVMS101(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxIVMS101PayloadBytes {
		return nil, ErrInvalidIVMS101
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var payload IVMS101Payload
	if err := decoder.Decode(&payload); err != nil {
		return nil, ErrInvalidIVMS101
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrInvalidIVMS101
	}
	if len(payload.Originator.OriginatorPersons) == 0 || len(payload.Originator.OriginatorPersons) > 10 ||
		len(payload.Beneficiary.BeneficiaryPersons) == 0 || len(payload.Beneficiary.BeneficiaryPersons) > 10 {
		return nil, ErrInvalidIVMS101
	}
	if !hasAccountReference(payload.Originator.AccountNumber, payload.Originator.CustomerIdentification) ||
		!hasAccountReference(payload.Beneficiary.AccountNumber, payload.Beneficiary.CustomerIdentification) {
		return nil, ErrInvalidIVMS101
	}
	for _, person := range payload.Originator.OriginatorPersons {
		if !validPerson(person) {
			return nil, ErrInvalidIVMS101
		}
	}
	for _, person := range payload.Beneficiary.BeneficiaryPersons {
		if !validPerson(person) {
			return nil, ErrInvalidIVMS101
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, ErrInvalidIVMS101
	}
	return compact.Bytes(), nil
}

func hasAccountReference(accounts []string, customerID string) bool {
	if cleanRequired(customerID) {
		return true
	}
	for _, account := range accounts {
		if cleanRequired(account) {
			return true
		}
	}
	return false
}

func validPerson(person IVMS101Person) bool {
	if (person.NaturalPerson == nil) == (person.LegalPerson == nil) {
		return false
	}
	var identifiers []IVMS101NameIdentifier
	legal := person.LegalPerson != nil
	if person.NaturalPerson != nil {
		identifiers = person.NaturalPerson.Name.NameIdentifier
	} else {
		identifiers = person.LegalPerson.Name.NameIdentifier
	}
	if len(identifiers) == 0 || len(identifiers) > 20 {
		return false
	}
	for _, id := range identifiers {
		name := id.PrimaryIdentifier
		if legal {
			name = id.LegalPersonName
		}
		if !cleanRequired(name) || len(id.SecondaryIdentifier) > 256 {
			return false
		}
	}
	return true
}

func cleanRequired(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r == 0 || r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}
