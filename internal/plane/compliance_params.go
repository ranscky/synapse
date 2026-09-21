// Query-parameter parsing and access-record redaction for
// GET /v2/compliance/audit.
//
// Split out of compliance.go for the 300-line ceiling this project holds every
// file to, and along a line that is real: everything here turns a request into
// values -- a filter to read with and a string to record -- and nothing here
// writes a response or touches a dependency.
package plane

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// auditParams is one request's query, parsed: the filter the read runs with, and
// the redacted rendering of what the caller asked for.
type auditParams struct {
	filter   AuditFilter
	redacted string
}

// formatJSON and formatPDF are the two values the report endpoint's format
// parameter accepts. They are constants rather than literals at the call sites so
// the validation, the recorded parameter, and the branch that acts on it cannot
// disagree about which strings are legal.
const (
	formatJSON = "json"
	formatPDF  = "pdf"
)

// reportParams is one report request's query, parsed: the window the report
// covers, the format it will be rendered in, and the redacted rendering of what
// the caller asked for.
type reportParams struct {
	window   ReportWindow
	format   string
	redacted string
}

// parseReportParams reads the three parameters GET /v2/compliance/report accepts
// into a window and a format, and returns alongside them the string an access
// record will carry. The second return value is a machine-readable reason for a
// 400 -- "" when the request is good -- for the same reason parseAuditParams
// returns one: the reason is the response body's own value, and nothing in this
// package should invent one twice.
//
// format defaults to formatJSON and is refused unless it is exactly one of the two
// literals. Case matters: "PDF" is a 400 rather than a courtesy, because the value
// selects which of two very different code paths runs, and a client that has to
// spell it correctly is a client that knows which one it asked for. Every other
// value -- including one carrying shell metacharacters -- never reaches the
// renderer, which is the point of validating it here rather than at exec time.
//
// The window is parsed by parseWindow, so the report and the audit endpoint accept
// exactly the same timestamps, with the same fractional-second fidelity, and
// neither has its own copy of the rule.
func parseReportParams(r *http.Request) (reportParams, string) {
	query := r.URL.Query()

	params := reportParams{format: formatJSON}

	since, until, valid, reason := parseWindow(query)

	params.window = ReportWindow{Since: since, Until: until}

	// finish renders the parameters that were valid and answers with reason. It
	// runs on the rejecting path too, so a refused request still records what it
	// legitimately asked for -- which is how a run of `format=xml` attempts is
	// visible in the access log as attempts rather than as blank rows.
	finish := func(reason string) (reportParams, string) {
		params.redacted = valid.Encode()
		return params, reason
	}

	if reason != "" {
		return finish(reason)
	}

	if raw := query.Get("format"); raw != "" {
		if raw != formatJSON && raw != formatPDF {
			return finish("invalid_format")
		}

		params.format = raw
	}

	valid.Set("format", params.format)

	return finish("")
}

// parseAuditParams reads the four parameters the audit endpoint accepts into a
// filter, and returns alongside it the string an access record will carry.
// The second return value is a machine-readable reason for a 400 -- "" when the
// request is good -- because the reason is the response body's own value and
// nothing in this package should invent one twice.
//
// since and until are RFC 3339, parsed by parseWindow so this endpoint and the
// report endpoint share one rule. times are UTC once parsed.
//
// limit defaults to defaultCompliancePageLimit and is refused outside
// 1..maxCompliancePageLimit. offset defaults to 0 and is refused when negative.
// A parameter sent with an empty value counts as absent, so ?limit= means the
// default rather than zero.
//
// The redaction rule is the reason this function builds a string instead of
// echoing r.URL.RawQuery, and it is a security property rather than tidiness: an
// access log records *what was asked for*, and only the four keys above are
// things this endpoint asks for. Unknown parameters are dropped, never copied --
// otherwise a caller could park arbitrary text in the tenant's own audit table
// through a query string, which is a storage and smuggling channel this endpoint
// has no reason to offer. Values are re-rendered from what was parsed (times in
// UTC, integers in decimal) and encoded by url.Values, so the recorded string is
// canonical: two requests that asked the same question produce the same row, and
// nothing a caller typed is reproduced verbatim.
func parseAuditParams(r *http.Request) (auditParams, string) {
	query := r.URL.Query()

	params := auditParams{filter: AuditFilter{Limit: defaultCompliancePageLimit}}

	since, until, valid, reason := parseWindow(query)

	params.filter.Since = since
	params.filter.Until = until

	// finish renders the parameters that were valid and answers with reason. It
	// runs on the rejecting path too, so a refused request still records what it
	// legitimately asked for -- which is how a run of invalid_limit attempts is
	// visible in the access log as attempts rather than as blank rows.
	finish := func(reason string) (auditParams, string) {
		params.redacted = valid.Encode()
		return params, reason
	}

	if reason != "" {
		return finish(reason)
	}

	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxCompliancePageLimit {
			return finish("invalid_limit")
		}

		params.filter.Limit = parsed
		valid.Set("limit", strconv.Itoa(parsed))
	}

	if raw := query.Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return finish("invalid_offset")
		}

		params.filter.Offset = parsed
		valid.Set("offset", strconv.Itoa(parsed))
	}

	return finish("")
}

// parseWindow reads the since and until parameters shared by the compliance
// endpoints: both RFC 3339, both optional, both UTC once parsed.
//
// Go's parser accepts a fractional second even though the layout does not require
// one, so a timestamp read back out of the database and sent as RFC 3339 Nano
// round-trips to the microsecond the row was written with -- which is what makes
// "since this very entry" a boundary a caller can name instead of a value that
// lands a second early. That is why a whole-second layout is not used: it would
// floor the boundary into the row before the one the caller named.
//
// It returns the parsed halves, a url.Values holding every parameter that was
// valid (which the caller renders into the access record), and a machine-readable
// reason for a 400. A parameter sent with an empty value counts as absent, so
// ?since= means "no bound" rather than the zero time.
//
// An inverted window (until before since) is not an error: it is a valid question
// whose answer is no entries, and both endpoints read it that way. Inventing a 400
// for it would make them refuse a query the caller can see is empty, for no gain.
func parseWindow(query url.Values) (since, until *time.Time, valid url.Values, reason string) {
	valid = make(url.Values, 3)

	if raw := query.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, nil, valid, "invalid_since"
		}

		parsed = parsed.UTC()
		since = &parsed
		valid.Set("since", parsed.Format(time.RFC3339Nano))
	}

	if raw := query.Get("until"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return since, nil, valid, "invalid_until"
		}

		parsed = parsed.UTC()
		until = &parsed
		valid.Set("until", parsed.Format(time.RFC3339Nano))
	}

	return since, until, valid, ""
}

// ipHash is the access record's view of the caller's address:
// hex(sha256(r.RemoteAddr)), host and port exactly as they arrived.
//
// It is a stable pseudonym and not an anonymization, and the difference is
// stated here rather than assumed: IPv4 has few enough addresses that the digest
// of one can be brute-forced, so this hides an address from someone reading the
// table on a screen without hiding it from someone determined. The same address
// always hashes to the same value, which is the property an auditor actually
// wants ("did the same client read this tenant's history twenty times?"), and it
// is why no salt is applied: a per-deployment salt would make the column
// unjoinable across deployments while changing nothing about the brute-force
// argument.
//
// What this does buy is that the access table never holds a raw address, so
// reading it is not reading a second copy of the caller's identity.
func ipHash(remoteAddr string) string {
	sum := sha256.Sum256([]byte(remoteAddr))

	return hex.EncodeToString(sum[:])
}
