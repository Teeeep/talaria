package output

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
)

// FormatJUnit renders a suite of test cases as JUnit XML, the one format CI
// systems read without glue.
//
// It is a report format rather than an --output format: only `talaria run`
// produces a suite of operations that each passed, failed or was skipped, and a
// `describe` rendered as a test suite would mean nothing. So --output rejects
// it and `run --report` accepts it.
const FormatJUnit Format = "junit"

// reportFormats lists every `run --report` value, in the order shown to users.
// Every --output format is one, plus JUnit: --report narrows where a report
// goes, it does not introduce a second vocabulary.
var reportFormats = append(append([]Format{}, formats...), FormatJUnit)

// ReportFormats returns every valid --report value, in the order shown to
// users. The returned slice is a copy.
func ReportFormats() []string { return names(reportFormats) }

// ParseReportFormat converts a --report value into a Format. As with
// ParseFormat, the error names every valid value so an agent can correct itself
// without reading the help text.
func ParseReportFormat(s string) (Format, error) { return parseFormat("report", s, reportFormats) }

// Case is one operation's line in a JUnit report.
//
// Failure and Skipped carry the reason rather than a bare flag because the
// reason is the whole value of the report: "getPet failed" sends someone to the
// logs, "the server returned 500 Internal Server Error" does not.
type Case struct {
	// Name is the operationId — what a CI dashboard shows and what someone
	// searches for when it goes red.
	Name string
	// Seconds is how long the call took. JUnit's time attribute is defined in
	// seconds, so a caller holding milliseconds converts before it gets here.
	Seconds float64
	// Failure explains why the case failed; empty means it did not.
	Failure string
	// Skipped explains why the case never ran; empty means it ran. A case
	// carrying both is reported as a failure: the stronger outcome wins.
	Skipped string
}

// Suite is a whole JUnit report: every operation a run covered, in the order it
// covered them.
type Suite struct {
	Name  string
	Cases []Case
}

type junitRenderer struct{ w io.Writer }

// Render writes the suite as JUnit XML.
//
// Marshalled from structs rather than assembled as strings, so every reason
// talaria puts in the file — a validation message quoting a JSON pointer, a
// server error carrying an ampersand — is escaped by the encoder instead of by
// whoever remembers to.
func (r junitRenderer) Render(p Payload) error {
	body, err := xml.MarshalIndent(newXMLSuite(p.JUnit), "", "  ")
	if err != nil {
		return fmt.Errorf("rendering junit output: %w", err)
	}

	_, err = fmt.Fprintf(r.w, "%s%s\n", xml.Header, body)

	return err
}

// xmlSuite is the <testsuite> element. The counts are attributes rather than
// something a consumer derives, because that is what the format says and what
// every dashboard reads.
type xmlSuite struct {
	XMLName  xml.Name  `xml:"testsuite"`
	Name     string    `xml:"name,attr"`
	Tests    int       `xml:"tests,attr"`
	Failures int       `xml:"failures,attr"`
	Skipped  int       `xml:"skipped,attr"`
	Errors   int       `xml:"errors,attr"`
	Time     string    `xml:"time,attr"`
	Cases    []xmlCase `xml:"testcase"`
}

// xmlCase is one <testcase>. Failure and Skipped are pointers so a passing case
// carries neither child element rather than an empty one, which is how a parser
// tells the outcomes apart.
type xmlCase struct {
	Name    string     `xml:"name,attr"`
	Time    string     `xml:"time,attr"`
	Failure *xmlReason `xml:"failure,omitempty"`
	Skipped *xmlReason `xml:"skipped,omitempty"`
}

// xmlReason is a <failure> or <skipped> element.
type xmlReason struct {
	Message string `xml:"message,attr"`
}

// newXMLSuite converts a Suite into the element tree, counting the outcomes on
// the way through.
//
// Errors is always zero and always present: JUnit distinguishes a test that
// failed an assertion from one whose harness broke, and talaria has no second
// category — an operation talaria could not execute is a skip with a reason.
// Emitting the attribute anyway keeps consumers that require it happy.
func newXMLSuite(suite Suite) xmlSuite {
	out := xmlSuite{
		Name:  suite.Name,
		Tests: len(suite.Cases),
		Cases: make([]xmlCase, 0, len(suite.Cases)),
	}

	var total float64
	for _, c := range suite.Cases {
		total += c.Seconds

		xc := xmlCase{Name: c.Name, Time: seconds(c.Seconds)}
		switch {
		case c.Failure != "":
			out.Failures++
			xc.Failure = &xmlReason{Message: c.Failure}
		case c.Skipped != "":
			out.Skipped++
			xc.Skipped = &xmlReason{Message: c.Skipped}
		}

		out.Cases = append(out.Cases, xc)
	}
	out.Time = seconds(total)

	return out
}

// seconds renders a duration the way JUnit consumers expect: fixed-point
// seconds, never scientific notation, which is what %v would produce for a call
// that took under a tenth of a millisecond.
func seconds(s float64) string { return strconv.FormatFloat(s, 'f', 3, 64) }
