// The PDF half of GET /v2/compliance/report: render the report into the HTML
// template, hand that file to an HTML-to-PDF renderer, and return the bytes.
//
// Split from compliance_report.go for the 300-line ceiling this project holds
// every file to, and along a line that is real: nothing here reads a request or
// writes a response, and everything here is about one external program that may
// or may not be installed.
//
// The security property this file is built around: *no caller-controlled text is
// ever an argument to the renderer.* The renderer is invoked directly (exec, not
// a shell), its arguments are constants plus paths this process minted inside a
// directory it created, and the report's own content travels only inside the
// pre-rendered HTML file's bytes -- where html/template has already escaped it.
// That is why the format parameter is validated to one of two literals before this
// code runs at all, and why the temp paths are built with filepath.Join from
// os.MkdirTemp's output rather than from anything a request supplied.
package plane

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// compliancePDFTimeout bounds one renderer invocation. A browser that hangs
	// must cost this endpoint a bounded wait rather than a hung request, and the
	// measured cost on a static A4 document is a few seconds.
	compliancePDFTimeout = 60 * time.Second

	// pdfToolOutputLimit is how much of the renderer's combined output is kept
	// for the error log. It is a log line, not an artifact: the renderer's own
	// chatty output (certificate warnings, GPU notices) is noise, and an
	// unbounded copy of it is memory a failed request should not hold.
	pdfToolOutputLimit = 4 << 10
)

// pdfToolKind is which argument shape a renderer takes. The two supported
// families disagree about how the output file is named, so the kind is resolved
// with the program rather than guessed at call time.
type pdfToolKind int

const (
	// pdfToolWKHTMLTOPDF writes the PDF to a trailing positional argument.
	pdfToolWKHTMLTOPDF pdfToolKind = iota
	// pdfToolChromium writes the PDF to --print-to-pdf=<path> and reads the
	// input as a URL.
	pdfToolChromium
)

// pdfTool is a resolved renderer: the program's own name (for logs), its absolute
// path (what is executed), and its argument shape.
type pdfTool struct {
	name string
	path string
	kind pdfToolKind
}

// findPDFTool resolves the first installed renderer, or reports that none is.
//
// The order is deliberate rather than alphabetical: wkhtmltopdf is purpose-built
// for this (it honours @media print and writes a clean A4 document) and is the
// program the endpoint's own 503 message names; a Chromium-family browser is the
// fallback, because a headless browser is often already installed and its
// --print-to-pdf path produces the same document from the same HTML.
//
// LookPath is what makes "installed" a fact about the host rather than a guess
// about a path: the candidates are bare program names, so PATH -- and therefore
// the operator's own installation -- decides, and a plane with none of them
// answers 503 instead of failing at exec time.
func findPDFTool() (pdfTool, bool) {
	candidates := []struct {
		name string
		kind pdfToolKind
	}{
		{"wkhtmltopdf", pdfToolWKHTMLTOPDF},
		{"chromium", pdfToolChromium},
		{"chromium-browser", pdfToolChromium},
		{"google-chrome", pdfToolChromium},
		{"google-chrome-stable", pdfToolChromium},
	}

	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate.name); err == nil {
			return pdfTool{name: candidate.name, path: path, kind: candidate.kind}, true
		}
	}

	return pdfTool{}, false
}

// args renders the renderer's argument list for one HTML file and one output
// path. Every element is either a constant of this build or a path this process
// created; nothing a request carried can reach it.
func (t pdfTool) args(htmlPath, pdfPath string) []string {
	if t.kind == pdfToolWKHTMLTOPDF {
		// --quiet because the renderer's progress output is not a response
		// body's business; --print-media-type because the template's layout is
		// written for print (a report is not a screen).
		return []string{"--quiet", "--print-media-type", htmlPath, pdfPath}
	}

	// A profile directory inside the same temp directory, so the renderer never
	// touches -- or needs -- the invoking user's real profile, which would also
	// make two concurrent reports contend for one lock.
	profileDir := filepath.Join(filepath.Dir(pdfPath), "renderer-profile")

	return []string{
		"--headless",
		"--disable-gpu",
		// The template renders its own headings and footer; the browser's own
		// are a file path and a page number stamped over a compliance document.
		"--no-pdf-header-footer",
		"--user-data-dir=" + profileDir,
		"--print-to-pdf=" + pdfPath,
		// A file:// URL rather than a bare path: the input is a local file this
		// process just wrote, and saying so keeps the argument shape independent
		// of the working directory.
		fileURL(htmlPath),
	}
}

// fileURL renders an absolute local path as a file:// URL.
//
// url.URL escapes anything a path could contain, so an unusual temp directory name
// cannot turn into a second argument or a query string. The input is always this
// process's own temp path, so the escaping is hygiene rather than a defense.
func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// renderComplianceReportHTML executes the template at templatePath with the
// report and returns the HTML.
//
// The template is read and parsed per request rather than cached at boot: an
// operator fixing a typo in a compliance document should not have to restart a
// plane to see it, and the file is small. A parse error carries the path, which is
// deployment configuration rather than anything a caller supplied.
func renderComplianceReportHTML(report ComplianceReport, templatePath string) ([]byte, error) {
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return nil, fmt.Errorf("plane: parse compliance report template: %w", err)
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, report); err != nil {
		return nil, fmt.Errorf("plane: render compliance report template: %w", err)
	}

	return out.Bytes(), nil
}

// renderComplianceReportPDF turns the report into PDF bytes: render the HTML,
// write it to a 0600 file inside a fresh 0700 directory, run the renderer over
// that path, and read the result back.
//
// The directory is this process's own (os.MkdirTemp) and is removed on the way
// out whatever happens, so a failed render leaves no report on disk. The HTML file
// is 0600 because everything Synapse writes to disk is; the renderer's own output
// is chmod'd to 0600 for the same reason -- it is the tenant's compliance report,
// not a public artifact.
//
// The renderer's output is captured, not inherited: a renderer that succeeds
// should be silent, and one that fails should be readable in the plane's log
// without its noise reaching a response body. Nothing captured here is reflected
// to a client -- the response is the generic 500 -- and what is captured is the
// renderer's own stdout/stderr, which carries no report content.
func renderComplianceReportPDF(ctx context.Context, report ComplianceReport, templatePath string, tool pdfTool) ([]byte, error) {
	page, err := renderComplianceReportHTML(report, templatePath)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "synapse-compliance-report-")
	if err != nil {
		return nil, fmt.Errorf("plane: create report working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	htmlPath := filepath.Join(dir, "compliance-report.html")
	if err := os.WriteFile(htmlPath, page, 0o600); err != nil {
		return nil, fmt.Errorf("plane: write rendered report: %w", err)
	}

	pdfPath := filepath.Join(dir, "compliance-report.pdf")

	renderCtx, cancel := context.WithTimeout(ctx, compliancePDFTimeout)
	defer cancel()

	cmd := exec.CommandContext(renderCtx, tool.path, tool.args(htmlPath, pdfPath)...)

	output := &cappedBuffer{limit: pdfToolOutputLimit}
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plane: %s failed: %w: %s", tool.name, err, output.String())
	}

	info, err := os.Stat(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("plane: %s produced no report: %w", tool.name, err)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("plane: %s produced an empty report", tool.name)
	}

	if err := os.Chmod(pdfPath, 0o600); err != nil {
		return nil, fmt.Errorf("plane: secure rendered report: %w", err)
	}

	pdf, err := os.ReadFile(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("plane: read rendered report: %w", err)
	}

	return pdf, nil
}

// cappedBuffer accumulates at most limit bytes and discards the rest, so a
// renderer that floods its output cannot make the plane allocate without bound
// while the head of that output is still available for the log.
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
}

// Write appends what fits and reports the full length, so the process writing
// into it is never told its write failed.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			_, _ = c.buf.Write(p[:remaining])
		} else {
			_, _ = c.buf.Write(p)
		}
	}

	return len(p), nil
}

// String returns what was kept.
func (c *cappedBuffer) String() string {
	return c.buf.String()
}

// cappedBuffer is what the exec package's Stdout and Stderr fields need.
var _ io.Writer = (*cappedBuffer)(nil)
