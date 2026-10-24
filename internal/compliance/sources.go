package compliance

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

const (
	SourceUN     = "un_consolidated"
	SourceEU     = "eu_fsf"
	DefaultUNURL = "https://scsanctions.un.org/resources/xml/en/consolidated.xml"
	// FSF 1.1 export maintained by the European Commission. Operators may
	// need to supply the crawler token issued in their FSF account.
	DefaultEUURL = "https://webgate.ec.europa.eu/fsd/fsf/public/files/xmlFullSanctionsList_1_1/content"
)

// parseConsolidatedList streams official UN and EU XML. Unknown structures
// are errors, never a successful empty refresh. A source's snapshot is only
// replaced after the entire file has been decoded and validated.
func parseConsolidatedList(r io.Reader, source string) ([]*domain.SanctionsEntity, error) {
	if source != SourceUN && source != SourceEU {
		return nil, fmt.Errorf("unsupported sanctions source: %s", source)
	}
	decoder := xml.NewDecoder(io.LimitReader(r, 128<<20))
	now := time.Now().UTC()
	var out []*domain.SanctionsEntity
	seen := make(map[string]struct{})
	records := 0

	add := func(uid, name, kind string) {
		name = strings.TrimSpace(name)
		if uid == "" || name == "" || strings.EqualFold(name, "na") {
			return
		}
		// Prefix IDs to preserve identity across sources in the existing
		// (uid, name, address) unique constraint.
		uid = source + ":" + uid
		key := uid + "\x00" + normalizeName(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, &domain.SanctionsEntity{
			UID: uid, Name: name, Source: source,
			EntityType: kind, RefreshedAt: now,
		})
	}
	var root string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s XML: %w", source, err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if root == "" {
			root = start.Name.Local
			if (source == SourceUN && root != "CONSOLIDATED_LIST") ||
				(source == SourceEU && root != "export") {
				return nil, fmt.Errorf("unexpected %s root: %q", source, root)
			}
		}
		if source == SourceUN {
			switch start.Name.Local {
			case "INDIVIDUAL":
				var row unIndividual
				if err := decoder.DecodeElement(&row, &start); err != nil {
					return nil, err
				}
				uid := strings.TrimSpace(row.ReferenceNumber)
				if uid == "" {
					uid = strings.TrimSpace(row.DataID)
				}
				add(uid, joinUNName(row.FirstName, row.SecondName, row.ThirdName, row.FourthName), "Individual")
				for _, alias := range row.Aliases {
					add(uid, alias.Name, "Individual")
				}
				records++
			case "ENTITY":
				var row unEntity
				if err := decoder.DecodeElement(&row, &start); err != nil {
					return nil, err
				}
				uid := strings.TrimSpace(row.ReferenceNumber)
				if uid == "" {
					uid = strings.TrimSpace(row.DataID)
				}
				add(uid, row.Name, "Entity")
				for _, alias := range row.Aliases {
					add(uid, alias.Name, "Entity")
				}
				records++
			}
		} else if start.Name.Local == "sanctionEntity" {
			var row euEntity
			if err := decoder.DecodeElement(&row, &start); err != nil {
				return nil, err
			}
			uid := strings.TrimSpace(row.LogicalID)
			if uid == "" {
				uid = strings.TrimSpace(row.Reference)
			}
			kind := row.Type.Code
			if kind == "" {
				kind = "Entity"
			}
			for _, alias := range row.Aliases {
				add(uid, alias.WholeName, kind)
			}
			records++
		}
	}
	if records == 0 || len(out) == 0 {
		return nil, fmt.Errorf("%s XML has no valid sanctioned names; refusing replacement", source)
	}
	return out, nil
}

// The United Nations consolidated list has two different record layouts.
type unIndividual struct {
	DataID          string `xml:"DATAID"`
	ReferenceNumber string `xml:"REFERENCE_NUMBER"`
	FirstName       string `xml:"FIRST_NAME"`
	SecondName      string `xml:"SECOND_NAME"`
	ThirdName       string `xml:"THIRD_NAME"`
	FourthName      string `xml:"FOURTH_NAME"`
	Aliases         []struct {
		Name string `xml:"ALIAS_NAME"`
	} `xml:"INDIVIDUAL_ALIAS"`
}
type unEntity struct {
	DataID          string `xml:"DATAID"`
	ReferenceNumber string `xml:"REFERENCE_NUMBER"`
	Name            string `xml:"FIRST_NAME"`
	Aliases         []struct {
		Name string `xml:"ALIAS_NAME"`
	} `xml:"ENTITY_ALIAS"`
}
type euEntity struct {
	LogicalID string `xml:"logicalId,attr"`
	Reference string `xml:"euReferenceNumber,attr"`
	Type      struct {
		Code string `xml:"code,attr"`
	} `xml:"subjectType"`
	Aliases []struct {
		WholeName string `xml:"wholeName,attr"`
	} `xml:"nameAlias"`
}

func joinUNName(parts ...string) string {
	var names []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && !strings.EqualFold(part, "na") {
			names = append(names, part)
		}
	}
	return strings.Join(names, " ")
}

func ParseUN(r io.Reader) ([]*domain.SanctionsEntity, error) {
	return parseConsolidatedList(r, SourceUN)
}
func ParseEU(r io.Reader) ([]*domain.SanctionsEntity, error) {
	return parseConsolidatedList(r, SourceEU)
}
