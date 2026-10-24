package reconcile

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"
)

// Workbook writes only inline text cells: provider-controlled references cannot
// become spreadsheet formulas, and decimal amounts retain their exact precision.
func (report *ThreeWayReport) Workbook() ([]byte, error) {
	summary := [][]string{{"Field", "Value"}, {"Tenant", report.TenantID}, {"Mode", string(report.Mode)}, {"Provider", report.Provider}, {"UTC day", report.Day.Format("2006-01-02")}, {"Checked at", report.CheckedAt.Format(time.RFC3339)},
		{"Scope", "Internal transfers created on the UTC day plus older transfers referenced by the bank report. Horizon evidence is checked at run time."},
		{"Limitations", "Not a scan of all Stellar activity. Unknown bank references cannot establish chain absence. Existing payment-operation auditing remains separate."}}
	headers := []string{"Reference", "Transfer ID", "Stellar hash", "Bank amount", "Bank currency", "Internal fiat amount", "Internal fiat currency", "Bank status", "Internal status", "Chain status", "Findings", "Recommendation"}
	all := [][]string{headers}
	exceptions := [][]string{headers}
	for _, r := range report.Rows {
		cells := []string{r.Reference, r.TransferID, r.Hash, r.BankAmount, r.BankCurrency, r.InternalAmount, r.InternalCurrency, r.BankStatus, r.InternalStatus, r.ChainStatus, strings.Join(r.Findings, ", "), r.Recommendation}
		all = append(all, cells)
		if len(r.Findings) != 1 || r.Findings[0] != "matched" {
			exceptions = append(exceptions, cells)
		}
	}
	summary = append(summary, []string{"Rows", fmt.Sprint(len(all) - 1)}, []string{"Exceptions", fmt.Sprint(len(exceptions) - 1)})
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	add := func(name, content string) error {
		w, err := archive.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, content)
		return err
	}
	parts := []struct{ name, content string }{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/worksheets/sheet2.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/worksheets/sheet3.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`},
		{"_rels/.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Summary" sheetId="1" r:id="rId1"/><sheet name="All transfers" sheetId="2" r:id="rId2"/><sheet name="Exceptions" sheetId="3" r:id="rId3"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet3.xml"/></Relationships>`},
	}
	for _, part := range parts {
		if err := add(part.name, part.content); err != nil {
			return nil, fmt.Errorf("write workbook part: %w", err)
		}
	}
	for i, data := range [][][]string{summary, all, exceptions} {
		var sheet strings.Builder
		sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews><sheetData>`)
		for rowIndex, cells := range data {
			fmt.Fprintf(&sheet, `<row r="%d">`, rowIndex+1)
			for col, value := range cells {
				fmt.Fprintf(&sheet, `<c r="%c%d" t="inlineStr"><is><t xml:space="preserve">`, 'A'+col, rowIndex+1)
				if err := xml.EscapeText(&sheet, []byte(value)); err != nil {
					return nil, fmt.Errorf("escape workbook cell: %w", err)
				}
				sheet.WriteString(`</t></is></c>`)
			}
			sheet.WriteString(`</row>`)
		}
		sheet.WriteString(`</sheetData></worksheet>`)
		if err := add(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), sheet.String()); err != nil {
			return nil, fmt.Errorf("write worksheet: %w", err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close workbook: %w", err)
	}
	return buffer.Bytes(), nil
}
