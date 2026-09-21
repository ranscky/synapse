// The compliance report endpoint: GET /v2/compliance/report.
//
// Split out of handlers.go for the same reason the audit, sync, search, and
// ledger endpoints live in their own files, and split from the report's own
// contract, arithmetic, and renderer for the 300-line ceiling this project holds
// every file to. What is left here is the request's whole story: who is asking,
// whether they may, what they asked for, which dependency answers each half, and
// what goes back.
//
// Phase 19's GET /v2/compliance/audit answers "what does the ledger say?" one page
// at a time. This answers "summarise it, and give me something I can file." That is
// why the two are separate endpoints rather than one with a mode: a page is read by
// a tool that will follow it with another page, while a report is read by a person
// who wants a verdict, a period, and a document -- and the second is allowed to
// spend a full chain walk on one request, which a pager is not.
//
// Three properties are what this file is built around.
//
//   - The tenant is the verified token's and nothing else, exactly as the audit
//     endpoint's is: no path, query, body, or header parameter can name a tenant,
//     so this route cannot be made to report on another one.
//   - The tier gate is the same gate, in the same place in the order -- identity,
//     dependency, tier, parameters, read -- and reads the verified
//     compliance_tier claim rather than the registry row, for the reason Phase 19
//     documented: the claim is what the plane signed for the token the caller is
//     actually holding.
//   - Every call that reaches a verified tenant is recorded in
//     synapse_global.compliance_access_log before it is answered, refusals
//     included, and a read that cannot be recorded is refused. A report is the
//     most quotable artifact this plane produces; where it went is not optional.
package plane

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const (
	// complianceReportTimeout bounds the report's two reads -- the window's
	// ledger rows and the tenant's chain walk -- so a large ledger costs this
	// endpoint a bounded wait rather than an open request. It is deliberately
	// separate from compliancePDFTimeout, which bounds the renderer.
	complianceReportTimeout = 60 * time.Second

	// complianceReportAttachment is the PDF's download name when the window has
	// no bounds at all. A bounded window names itself; see reportFilename.
	complianceReportAttachment = "compliance-report.pdf"

	// pdfToolUnavailableMessage is the whole of what a caller can act on when
	// this plane has no renderer: the fix, named. It travels with the 503 as its
	// own field so a client can show it without parsing prose out of an error
	// code.
	pdfToolUnavailableMessage = "install wkhtmltopdf to enable PDF reports"
)

// handleComplianceReport serves GET /v2/compliance/report: the calling tenant's
// own compliance report for the window it asks for, as JSON or as the PDF
// rendering of the same document.
//
// The order of the checks is the endpoint's authorization story, the same one the
// audit endpoint tells: identity, then dependency, then the tier gate, then the
// caller's parameters, then the read. Two consequences are deliberate. A
// non-enterprise caller is refused before its window is validated, so this route
// cannot be used to probe what a valid query looks like. And for format=pdf the
// renderer check happens *before* the reads, so a plane deployed without a
// renderer answers 503 without walking a chain it cannot deliver -- an
// unfulfillable request should also be a cheap one.
//
// Every answer but the 401 is preceded by its own access record. See
// requireAccessRecord for why a record that cannot be written refuses the read
// rather than failing open.
func (s *Server) handleComplianceReport(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantIDFromCtx(r.Context())
	if tenantID == "" {
		// Unreachable behind requireJWT, which rejects a token that names no
		// tenant. Checked anyway so a rewired middleware cannot turn this into a
		// report on an unnamed tenant's history -- and this is the one refusal
		// that gets no access record, because there is no verified tenant for the
		// row to name.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if s.auditor == nil || s.ledger == nil {
		// Two dependencies, because a report needs both halves: the window's
		// facts and the chain verdict the caller cannot get from the facts alone.
		// A plane wired with one and not the other cannot answer, and answering
		// from the half it has would be a report certifying a chain nobody
		// walked.
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	params, reason := parseReportParams(r)

	if ComplianceTierFromCtx(r.Context()) != complianceTierEnterprise {
		if !s.requireAccessRecord(w, r, complianceReportRoute, tenantID, params.redacted, http.StatusForbidden) {
			return
		}

		writeJSON(w, http.StatusForbidden, complianceTierRequiredResponse{
			Error:      "compliance_tier_required",
			UpgradeURL: complianceUpgradeURL,
		})
		return
	}

	if reason != "" {
		if !s.requireAccessRecord(w, r, complianceReportRoute, tenantID, params.redacted, http.StatusBadRequest) {
			return
		}

		writeError(w, http.StatusBadRequest, reason)
		return
	}

	// Resolved before the reads: a plane with no renderer cannot answer this
	// request at all, and the caller is told which fix is available to them
	// rather than being handed a report in the wrong format.
	var tool pdfTool

	if params.format == formatPDF {
		found := false
		if tool, found = findPDFTool(); !found {
			if !s.requireAccessRecord(w, r, complianceReportRoute, tenantID, params.redacted, http.StatusServiceUnavailable) {
				return
			}

			writeJSON(w, http.StatusServiceUnavailable, pdfToolUnavailableResponse{
				Error:   "pdf_tool_unavailable",
				Message: pdfToolUnavailableMessage,
			})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), complianceReportTimeout)
	defer cancel()

	facts, err := s.auditor.ReportFacts(ctx, tenantID, params.window)
	if err != nil {
		s.reportFailure(w, r, tenantID, params, "Compliance report read failed", err)
		return
	}

	report, err := buildComplianceReport(tenantID, params.window, facts, time.Now())
	if err != nil {
		s.reportFailure(w, r, tenantID, params, "Compliance report build failed", err)
		return
	}

	// The two fields that are process facts rather than stored data: what this
	// report claims about itself, and this request's own chain verdict.
	report.Article50Statement = Article50Statement

	verdict, err := s.ledger.VerifyChain(ctx, tenantID)
	if err != nil {
		s.reportFailure(w, r, tenantID, params, "Compliance report chain verification failed", err)
		return
	}

	// Merged rather than assigned wholesale: the walk answers "is the chain
	// intact" over the whole chain, while the count of entries the *window* covers
	// came from the report's own read. Assigning the whole section would replace a
	// period's coverage with a chain's length.
	report.ChainIntegrity.Valid = verdict.ChainValid
	report.ChainIntegrity.LastVerifiedAt = verdict.CheckedAt

	if params.format == formatPDF {
		s.respondComplianceReportPDF(w, r, tenantID, params, report, tool)
		return
	}

	if !s.requireAccessRecord(w, r, complianceReportRoute, tenantID, params.redacted, http.StatusOK) {
		return
	}

	// Counts, the window, and the format: no trace field, no memory preview, no
	// hash, and never the presented token. The window lives in the access table,
	// where it belongs.
	if s.logger != nil {
		s.logger.Info("Compliance report read",
			"tenant_slug", TenantSlugFromCtx(r.Context()), "tenant_id", tenantID,
			"compilations", report.Summary.TotalCompilations,
			"entries_in_period", report.ChainIntegrity.EntriesInPeriod,
			"chain_valid", report.ChainIntegrity.Valid, "format", params.format)
	}

	writeJSON(w, http.StatusOK, report)
}

// respondComplianceReportPDF renders the report to PDF, records the call, and
// writes the bytes.
//
// The record is written after the render but before the body, because the body is
// the artifact: this is the last moment at which a failure to record can still
// withhold it. A render that failed is recorded as a 500 and answered with this
// package's generic body -- the renderer's own output stops at the log, since it
// can name local paths and says nothing a client could act on.
func (s *Server) respondComplianceReportPDF(
	w http.ResponseWriter, r *http.Request, tenantID string, params reportParams,
	report ComplianceReport, tool pdfTool,
) {
	pdf, err := renderComplianceReportPDF(r.Context(), report, s.reportTemplatePath(), tool)
	if err != nil {
		s.reportFailure(w, r, tenantID, params, "Compliance report PDF render failed", err)
		return
	}

	if !s.requireAccessRecord(w, r, complianceReportRoute, tenantID, params.redacted, http.StatusOK) {
		return
	}

	if s.logger != nil {
		s.logger.Info("Compliance report rendered",
			"tenant_slug", TenantSlugFromCtx(r.Context()), "tenant_id", tenantID,
			"format", params.format, "renderer", tool.name, "bytes", len(pdf))
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+reportFilename(params.window)+`"`)
	w.WriteHeader(http.StatusOK)

	// A failed write means the client went away; there is nothing useful to do
	// about it on either side.
	_, _ = w.Write(pdf)
}

// reportFailure logs a failed report and answers with the generic 500, recording
// the call if it can.
//
// Nothing from err is reflected: a pgx error can quote the connection target, a
// renderer's error can quote local paths, and a DSN carries a password. The caller
// gets the one error body every other endpoint uses, and the tenant id plus the
// error go to the log.
func (s *Server) reportFailure(w http.ResponseWriter, r *http.Request, tenantID string, params reportParams, message string, err error) {
	if s.logger != nil {
		s.logger.Error(message, "tenant_id", tenantID, "format", params.format, "error", err)
	}

	// The answer is a 500 either way, so a failed record changes nothing the
	// caller sees: requireAccessRecord has already written the 500 when it could
	// not record, and this writes the same body when it could.
	if s.requireAccessRecord(w, r, complianceReportRoute, tenantID, params.redacted, http.StatusInternalServerError) {
		writeError(w, http.StatusInternalServerError, "internal")
	}
}

// reportFilename names the PDF attachment after the period it covers.
//
// The name is built from the parsed window's own halves -- formatted here, never
// echoed from the query string -- so no caller-supplied text can reach the
// Content-Disposition header, and an unbounded half says so instead of naming a
// date nobody asked for.
func reportFilename(window ReportWindow) string {
	if window.Since == nil && window.Until == nil {
		return complianceReportAttachment
	}

	since, until := "start", "now"
	if window.Since != nil {
		since = window.Since.UTC().Format(time.DateOnly)
	}
	if window.Until != nil {
		until = window.Until.UTC().Format(time.DateOnly)
	}

	return fmt.Sprintf("compliance-report-%s_%s.pdf", since, until)
}

// reportTemplatePath resolves the HTML template this plane renders PDFs with: the
// configured path, or the documented default when configuration left it blank.
//
// A blank value is the normal case for a plane started from the environment alone
// and for every test that builds a PlaneConfig by hand, so the default is applied
// by the reader rather than required of the writer.
func (s *Server) reportTemplatePath() string {
	if s.cfg != nil && s.cfg.ReportTemplatePath != "" {
		return s.cfg.ReportTemplatePath
	}

	return DefaultReportTemplatePath
}
