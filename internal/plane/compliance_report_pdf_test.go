// The compliance report endpoint's PDF half: the renderer it looks for, the arguments
// it builds, the temp files it writes, the 503 it answers when the host has no renderer
// at all, and the template both formats are rendered from.
//
// The renderer is faked through PATH rather than injected, because PATH is the real
// mechanism: findPDFTool resolves a bare program name with exec.LookPath, so a test that
// puts a script named `wkhtmltopdf` into an otherwise empty directory and points PATH at
// that directory exercises the same code a deployment does -- including the failure
// path, where a directory with nothing in it is exactly a machine with no renderer
// installed. No seam exists only for tests, and nothing global is mutated beyond the
// environment of the test process itself.
package plane_test

import (
	"bytes"
	"html"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"synapse/internal/plane"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportTemplatePath is the template the tests render with: the repository's own file,
// reached from this package's directory, so the assertions are about the document a
// deployment ships rather than about a fixture that could drift from it.
const reportTemplatePath = "../../ui/plane/compliance-report.html"

// pdfTestReport is the report value the PDF tests render: small, with distinct values in
// every section, so a template that dropped a field or a whole section would be visible
// in the assertions below.
func pdfTestReport() plane.ComplianceReport {
	return plane.ComplianceReport{
		Header: plane.ComplianceReportHeader{
			TenantID:       testTenantID,
			GeneratedAt:    time.Date(2026, 9, 21, 15, 4, 5, 0, time.UTC),
			SynapseVersion: plane.Version,
		},
		Summary: plane.ComplianceReportSummary{
			TotalCompilations: 12,
			TotalMemoriesUsed: 34,
			AvgReductionPct:   41.5,
		},
		MemoryTypeBreakdown: map[string]float64{"decision": 66.67, "fact": 33.33},
		GlobalBrain: plane.ComplianceReportGlobalBrain{
			CrossAgentRetrievals:     7,
			UniqueContributingAgents: 3,
		},
		ConflictResolution: plane.ComplianceReportConflictResolution{ContradictionsDetected: 2},
		Supersession:       plane.ComplianceReportSupersession{MemoriesSuperseded: 1},
		ChainIntegrity: plane.ComplianceReportChainIntegrity{
			Valid:           true,
			LastVerifiedAt:  time.Date(2026, 9, 21, 15, 4, 6, 0, time.UTC),
			EntriesInPeriod: 12,
		},
		Article50Statement: plane.Article50Statement,
	}
}

// reportConfig returns a config whose report-template points at the shipped template,
// so a PDF test renders the real document.
func reportConfig() *plane.PlaneConfig {
	cfg := newConfig(adminToken)
	cfg.ReportTemplatePath = reportTemplatePath

	return cfg
}

// writeFakeTool writes a stand-in renderer into dir under the given program name, so
// exec.LookPath finds it when PATH is dir.
func writeFakeTool(t *testing.T, dir, name, script string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700))
}

// rendererStubUnsupportedReason is why the PATH-based stubs above cannot stand in for a
// renderer on Windows.
//
// The stubs are `#!/bin/sh` programs with no filename extension, because that is what
// exec.LookPath resolves from the bare candidate names findPDFTool searches for on a
// POSIX host: PATH is the real mechanism a deployment relies on, and a script sitting in
// a directory on PATH is exactly an installed renderer. Windows has no equivalent
// lookup -- LookPath there requires the candidate to carry one of PATHEXT's extensions
// (.exe, .bat, .cmd, ...) -- so an extensionless file on PATH is never found, and the
// endpoint answers 503 for a reason that is a fact about this fixture rather than about
// the endpoint. A .bat stub would not remove the difference: cmd.exe cannot be handed
// the Chromium argument shape this package builds without a parser of its own, and its
// output redirection appends the CRLF the assertions above would then have to special
// case per platform. Making the test platform-conditional is the honest form of that,
// and the rendering path stays covered by the ubuntu and macos CI legs -- the
// deployments this endpoint targets in the first place.
const rendererStubUnsupportedReason = "POSIX-only fixture: the fake renderer is an " +
	"extensionless `#!/bin/sh` script, which exec.LookPath cannot resolve on Windows " +
	"(it requires a PATHEXT extension), so the endpoint answers 503 before any render " +
	"is attempted; the render path is covered by the ubuntu and macos CI legs"

// skipRendererStubOnWindows skips a test whose only means of exercising the endpoint is
// the POSIX renderer stub above. Tests that need no renderer -- the 503 case and the
// template cases -- call nothing here and run on every platform, so a Windows leg still
// covers everything on this endpoint that does not depend on a fake executable.
func skipRendererStubOnWindows(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip(rendererStubUnsupportedReason)
	}
}

// wkhtmltopdfStub is a stand-in wkhtmltopdf: record the arguments, copy the input
// document where the test can read it, write the output file it was given.
//
// The indices are wkhtmltopdf's own shape: $3 is the input HTML, $4 the output path.
func wkhtmltopdfStub(argvLog, htmlCopy string) string {
	return "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvLog + "\n" +
		copyTo(htmlCopy, "\"$3\"") +
		"printf '%%PDF-1.4 fake-wkhtmltopdf\\n' > \"$4\"\n"
}

// copyTo returns shell lines that write the contents of the already-quoted source file
// into dst.
//
// It is builtins only -- read and printf -- and that is not a stylistic choice: the
// stub runs with PATH pointing at a directory that holds nothing but the stub itself, so
// `cp` and `cat` are not on it. A stub that shelled out to coreutils would silently
// produce no copy, which is exactly the bug this helper was written after.
func copyTo(dst, quotedSource string) string {
	return "while IFS= read -r line; do printf '%s\\n' \"$line\"; done < " + quotedSource + " > " + dst + "\n"
}

// chromiumStub is a stand-in headless Chromium: record the arguments, copy the document
// behind the file:// URL where the test can read it, and write the path named by
// --print-to-pdf.
func chromiumStub(argvLog, htmlCopy string) string {
	return "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvLog + "\n" +
		"out=\"\"; url=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"    --print-to-pdf=*) out=\"${a#--print-to-pdf=}\" ;;\n" +
		"    file://*) url=\"${a#file://}\" ;;\n" +
		"  esac\n" +
		"done\n" +
		copyTo(htmlCopy, "\"$url\"") +
		"printf '%%PDF-1.4 fake-chromium\\n' > \"$out\"\n"
}

// TestComplianceReportPDFRendersThroughTheInstalledRenderer is the phase's PDF
// contract, with the renderer faked through PATH: the endpoint finds the tool, hands it
// a pre-rendered HTML file, returns what the tool wrote with the PDF content type, and
// never lets anything a request carried near the argument list.
//
// The fake records its own argv, which is what makes the last claim checkable: the
// arguments are read back and asserted to be the flags this build defines plus two paths
// inside a temp directory, with no query value in sight.
func TestComplianceReportPDFRendersThroughTheInstalledRenderer(t *testing.T) {
	skipRendererStubOnWindows(t)

	dir := t.TempDir()
	t.Setenv("PATH", dir)

	argvLog := filepath.Join(dir, "argv.log")
	htmlCopy := filepath.Join(dir, "renderer-input.html")
	writeFakeTool(t, dir, "wkhtmltopdf", wkhtmltopdfStub(argvLog, htmlCopy))

	cfg := reportConfig()
	auditor := &fakeAuditor{facts: reportFacts(t)}
	token := enterpriseToken(t, cfg, "enterprise")
	verifier := &fakeVerifier{result: plane.ChainIntegrityResult{
		EntriesChecked: 1,
		ChainValid:     true,
		CheckedAt:      time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC),
	}}
	router, logs := newComplianceReportRouter(t, cfg, auditor, verifier)

	rec := getComplianceReport(router, "Bearer "+token, "format=pdf")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/pdf", rec.Header().Get("Content-Type"))
	assert.Equal(t, "%PDF-1.4 fake-wkhtmltopdf\n", rec.Body.String())
	assert.Equal(t, `attachment; filename="compliance-report.pdf"`,
		rec.Header().Get("Content-Disposition"),
		"an unbounded window says nothing about a period rather than naming a date nobody asked for")

	// What the renderer was handed: this build's constants plus our own temp paths.
	recorded, err := os.ReadFile(argvLog)
	require.NoError(t, err, "the renderer must have been invoked")

	args := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	require.Len(t, args, 4, "wkhtmltopdf's own argument shape: %v", args)
	assert.Equal(t, "--quiet", args[0])
	assert.Equal(t, "--print-media-type", args[1])

	htmlPath, pdfPath := args[2], args[3]
	assert.Equal(t, "compliance-report.html", filepath.Base(htmlPath))
	assert.Equal(t, "compliance-report.pdf", filepath.Base(pdfPath))
	assert.Equal(t, filepath.Dir(htmlPath), filepath.Dir(pdfPath), "one working directory per render")
	assert.Contains(t, filepath.Base(filepath.Dir(htmlPath)), "synapse-compliance-report-")

	for _, forbidden := range []string{"format=", "since=", "until=", "pdf;", "--no-sandbox"} {
		assert.NotContains(t, string(recorded), forbidden,
			"nothing a request carried may appear in the renderer's arguments")
	}

	// The document the renderer was given is the rendered report, not a placeholder:
	// the template's own sections and this report's own statement are in it.
	page, err := os.ReadFile(htmlCopy)
	require.NoError(t, err)

	rendered := string(page)
	assert.Contains(t, rendered, testTenantID)
	assert.Contains(t, rendered, html.EscapeString(plane.Article50Statement),
		"the statement is in the filed document, escaped as HTML text")
	assert.Contains(t, rendered, "Chain verified")
	assert.Contains(t, rendered, "Compilations")

	// The working directory is gone by the time the bytes are in the response.
	_, err = os.Stat(filepath.Dir(htmlPath))
	assert.True(t, os.IsNotExist(err), "the working directory is removed, not left behind")

	assert.Contains(t, logs.String(), "Compliance report rendered")
	assert.Contains(t, logs.String(), "renderer=wkhtmltopdf")
	assert.NotContains(t, logs.String(), token, "the presented token is never logged")

	require.Len(t, auditor.records, 1, "the render is recorded before the body is written")
	assert.Equal(t, http.StatusOK, auditor.records[0].ResponseCode)
	assert.Equal(t, complianceReportPath, auditor.records[0].Endpoint)
	assert.Contains(t, auditor.records[0].QueryParamsRedacted, "format=pdf")
}

// TestComplianceReportPDFRendersThroughChromium is the second supported renderer
// family: nothing named wkhtmltopdf is installed, `chromium` is, and the endpoint still
// answers with a PDF. It is the case this repository's own development machine hits, and
// the argument shape is different enough (a flag carrying the output path, a file:// URL
// for the input) that it is worth asserting rather than assuming.
func TestComplianceReportPDFRendersThroughChromium(t *testing.T) {
	skipRendererStubOnWindows(t)

	dir := t.TempDir()
	t.Setenv("PATH", dir)

	argvLog := filepath.Join(dir, "argv.log")
	htmlCopy := filepath.Join(dir, "renderer-input.html")
	writeFakeTool(t, dir, "chromium", chromiumStub(argvLog, htmlCopy))

	cfg := reportConfig()
	router, _ := newComplianceReportRouter(t, cfg, &fakeAuditor{facts: reportFacts(t)}, &fakeVerifier{})

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "format=pdf")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/pdf", rec.Header().Get("Content-Type"))
	assert.Equal(t, "%PDF-1.4 fake-chromium\n", rec.Body.String())

	recorded, err := os.ReadFile(argvLog)
	require.NoError(t, err)

	assert.Contains(t, string(recorded), "--headless")
	assert.Contains(t, string(recorded), "--no-pdf-header-footer",
		"the template renders its own headings; the browser's would stamp a file path on a filed document")
	assert.Contains(t, string(recorded), "--print-to-pdf=")
	assert.Contains(t, string(recorded), "--user-data-dir=")
	assert.Contains(t, string(recorded), "file://", "the input is the temp file this process wrote")

	page, err := os.ReadFile(htmlCopy)
	require.NoError(t, err)
	assert.Contains(t, string(page), html.EscapeString(plane.Article50Statement),
		"the statement reaches the renderer, escaped as HTML text")
}

// TestComplianceReportPDFNamesABoundedWindow is the attachment name for a bounded
// window: built from the parsed window's own halves, formatted here rather than echoed
// from the query, so no caller-supplied text can reach a Content-Disposition header.
func TestComplianceReportPDFNamesABoundedWindow(t *testing.T) {
	skipRendererStubOnWindows(t)

	dir := t.TempDir()
	t.Setenv("PATH", dir)

	argvLog := filepath.Join(dir, "argv.log")
	htmlCopy := filepath.Join(dir, "renderer-input.html")
	writeFakeTool(t, dir, "wkhtmltopdf", wkhtmltopdfStub(argvLog, htmlCopy))

	cfg := reportConfig()
	router, _ := newComplianceReportRouter(t, cfg, &fakeAuditor{facts: reportFacts(t)}, &fakeVerifier{})

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"),
		"format=pdf&since=2026-09-20T00:00:00Z&until=2026-09-21T00:00:00Z")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `attachment; filename="compliance-report-2026-09-20_2026-09-21.pdf"`,
		rec.Header().Get("Content-Disposition"))
}

// TestComplianceReportPDFAnswers503WithoutARenderer is the deployment case: a host with
// no HTML-to-PDF renderer at all. The endpoint says so in the one body a caller can act
// on -- an error code and the fix -- answers 503 rather than 500, records the attempt,
// and does not read a chain it cannot deliver.
func TestComplianceReportPDFAnswers503WithoutARenderer(t *testing.T) {
	// An empty directory on PATH is a machine with no renderer installed, which is also
	// what the compose image is (see PROGRESS.md's Phase 20 findings).
	t.Setenv("PATH", t.TempDir())

	cfg := reportConfig()
	auditor := &fakeAuditor{facts: reportFacts(t)}
	router, _ := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "format=pdf")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"error":"pdf_tool_unavailable","message":"install wkhtmltopdf to enable PDF reports"}`,
		rec.Body.String())

	assert.Zero(t, auditor.factsCalls, "an unfulfillable request is also a cheap one: the window is not read")
	assert.Zero(t, auditor.pageCalls, "nor the chain walked")

	require.Len(t, auditor.records, 1, "the attempt is recorded")
	assert.Equal(t, http.StatusServiceUnavailable, auditor.records[0].ResponseCode)
	assert.Contains(t, auditor.records[0].QueryParamsRedacted, "format=pdf")
}

// TestComplianceReportPDFRefusesAMissingTemplate: the renderer is installed but the
// template is not, which is a deployment error rather than a client one. The caller gets
// this package's generic 500 -- the path is deployment configuration -- and the failure
// reaches the log with the format, so an operator can see which request failed and why.
func TestComplianceReportPDFRefusesAMissingTemplate(t *testing.T) {
	skipRendererStubOnWindows(t)

	dir := t.TempDir()
	t.Setenv("PATH", dir)

	argvLog := filepath.Join(dir, "argv.log")
	htmlCopy := filepath.Join(dir, "renderer-input.html")
	writeFakeTool(t, dir, "wkhtmltopdf", wkhtmltopdfStub(argvLog, htmlCopy))

	cfg := reportConfig()
	cfg.ReportTemplatePath = filepath.Join(t.TempDir(), "not-a-template.html")

	auditor := &fakeAuditor{facts: reportFacts(t)}
	router, logs := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "format=pdf")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "not-a-template")

	assert.Contains(t, logs.String(), "Compliance report PDF render failed")
	assert.Contains(t, logs.String(), "format=pdf")

	_, err := os.Stat(argvLog)
	assert.True(t, os.IsNotExist(err),
		"the renderer is never invoked for a document that could not be rendered")

	require.Len(t, auditor.records, 1)
	assert.Equal(t, http.StatusInternalServerError, auditor.records[0].ResponseCode)
}

// TestComplianceReportTemplateSaysWhatTheReportSays renders the shipped template
// directly and asserts the document carries this report's own values: the identity, the
// figures, the breakdown with both types, the counts, the chain verdict, and the Article
// 50 statement.
//
// It is the check that the two formats cannot drift. The template is the artifact a
// compliance officer files, and a field renamed in Go without the template being updated
// would otherwise produce a document with a hole in it while the JSON body kept looking
// right.
func TestComplianceReportTemplateSaysWhatTheReportSays(t *testing.T) {
	rendered := renderTemplate(t, reportTemplatePath, pdfTestReport())

	for _, expected := range []string{
		testTenantID,
		"41.50",              // average reduction, to two decimals
		"decision", "66.67%", // the breakdown, alphabetically by type
		"fact", "33.33%",
		"Chain verified",           // the verdict banner
		"21 Sep 2026 15:04:06 UTC", // last verified at
		"21 Sep 2026 15:04 UTC",    // generated at
		"start of this tenant's ledger",
		plane.Version,
		html.EscapeString(plane.Article50Statement),
	} {
		assert.Contains(t, rendered, expected)
	}

	// The statement is the last box on the page, not a footnote among the figures.
	assert.Greater(t, strings.Index(rendered, html.EscapeString(plane.Article50Statement)),
		strings.Index(rendered, "Chain verified"),
		"the Article 50 statement is rendered after the findings")

	assert.Contains(t, rendered, ">34<", "memories used is rendered as a figure of its own")
	assert.Contains(t, rendered, ">12<", "so are compilations")
	assert.Contains(t, rendered, "<dd>7</dd>", "cross-agent retrievals")
}

// TestComplianceReportTemplateSignalsABrokenChain is the template's other verdict: a
// chain that did not verify must not render as a verified one. The report value says
// which, and the document has to follow it.
func TestComplianceReportTemplateSignalsABrokenChain(t *testing.T) {
	report := pdfTestReport()
	report.ChainIntegrity.Valid = false

	rendered := renderTemplate(t, reportTemplatePath, report)

	assert.Contains(t, rendered, "Chain integrity failure")
	assert.NotContains(t, rendered, "Chain verified")
	assert.Contains(t, rendered, "verdict broken", "and the banner is styled as a failure")
	assert.Contains(t, rendered, "unverified until it is resolved")
}

// TestComplianceReportTemplateRendersAnEmptyPeriod: a period with no memories used
// renders an explanation rather than an empty table, and the figures are zeros rather
// than blanks -- a report is read by someone who needs to see that the answer was
// "nothing happened", not an unrendered field.
func TestComplianceReportTemplateRendersAnEmptyPeriod(t *testing.T) {
	report := pdfTestReport()
	report.Summary = plane.ComplianceReportSummary{}
	report.MemoryTypeBreakdown = map[string]float64{}
	report.GlobalBrain = plane.ComplianceReportGlobalBrain{}
	report.ConflictResolution = plane.ComplianceReportConflictResolution{}
	report.Supersession = plane.ComplianceReportSupersession{}
	report.ChainIntegrity.EntriesInPeriod = 0

	rendered := renderTemplate(t, reportTemplatePath, report)

	assert.Contains(t, rendered, ">0.00<", "the figures are zeros, not blank fields")
	assert.Contains(t, rendered, "No memories were used in this period.")
	assert.NotContains(t, rendered, "66.67", "a previous breakdown is not carried over")
}

// renderTemplate executes the Go html/template at path with data, failing the test on
// any error. It is the same parse-and-execute the plane performs per request; the plane's
// own call is covered by the PDF tests above, through the renderer's copy of the document
// it was given.
func renderTemplate(t *testing.T, path string, data any) string {
	t.Helper()

	tmpl, err := template.ParseFiles(path)
	require.NoError(t, err, "the shipped template must parse")

	var out bytes.Buffer
	require.NoError(t, tmpl.Execute(&out, data))

	return out.String()
}
