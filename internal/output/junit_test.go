package output

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

// junitReasonXML is a <failure> or <skipped> element. A pointer to one is nil
// when the element is absent, which is how the tests below tell "this case
// passed" from "this case failed with an empty message".
type junitReasonXML struct {
	Message string `xml:"message,attr"`
}

type junitCaseXML struct {
	Name    string          `xml:"name,attr"`
	Time    string          `xml:"time,attr"`
	Failure *junitReasonXML `xml:"failure"`
	Skipped *junitReasonXML `xml:"skipped"`
}

// junitSuiteXML decodes what the renderer wrote. Decoding rather than matching
// strings is the point: a report a real CI parser cannot read is a failure of
// this task even if every substring is present.
type junitSuiteXML struct {
	XMLName  xml.Name       `xml:"testsuite"`
	Name     string         `xml:"name,attr"`
	Tests    int            `xml:"tests,attr"`
	Failures int            `xml:"failures,attr"`
	Skipped  int            `xml:"skipped,attr"`
	Time     string         `xml:"time,attr"`
	Cases    []junitCaseXML `xml:"testcase"`
}

// renderJUnit renders a suite and decodes it back.
func renderJUnit(t *testing.T, suite Suite) (junitSuiteXML, string) {
	t.Helper()

	var out bytes.Buffer
	if err := New(FormatJUnit, &out).Render(Payload{JUnit: suite}); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	var got junitSuiteXML
	if err := xml.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("junit output is not well-formed XML: %v\n%s", err, out.String())
	}

	return got, out.String()
}

// sampleSuite is one of each outcome, which is what makes the counts worth
// asserting on.
func sampleSuite() Suite {
	return Suite{
		Name: "talaria run",
		Cases: []Case{
			{Name: "listPets", Seconds: 0.012},
			{Name: "getBoom", Seconds: 0.5, Failure: "the server returned 500 Internal Server Error"},
			{Name: "deletePet", Skipped: "a DELETE request may change server state"},
		},
	}
}

func TestJUnitReportCountsEachOutcome(t *testing.T) {
	got, raw := renderJUnit(t, sampleSuite())

	if !strings.HasPrefix(raw, xml.Header) {
		t.Errorf("junit output has no XML declaration:\n%s", raw)
	}
	if got.Name != "talaria run" {
		t.Errorf("testsuite name = %q, want %q", got.Name, "talaria run")
	}
	if got.Tests != 3 {
		t.Errorf("tests = %d, want 3", got.Tests)
	}
	if got.Failures != 1 {
		t.Errorf("failures = %d, want 1", got.Failures)
	}
	if got.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", got.Skipped)
	}
	// The suite's own time is the sum of its cases: a CI dashboard shows it as
	// how long the smoke test took.
	if got.Time != "0.512" {
		t.Errorf("testsuite time = %q, want %q", got.Time, "0.512")
	}
}

func TestJUnitReportHasOneCasePerOperation(t *testing.T) {
	got, raw := renderJUnit(t, sampleSuite())

	if len(got.Cases) != 3 {
		t.Fatalf("got %d testcases, want one per operation:\n%s", len(got.Cases), raw)
	}

	want := []string{"listPets", "getBoom", "deletePet"}
	for i, name := range want {
		if got.Cases[i].Name != name {
			t.Errorf("testcase %d name = %q, want %q (report order must follow the run)",
				i, got.Cases[i].Name, name)
		}
	}

	// Seconds, not milliseconds: JUnit's time attribute is defined in seconds
	// and a CI dashboard will happily report 12s for a 12ms call.
	if got.Cases[0].Time != "0.012" {
		t.Errorf("listPets time = %q, want %q", got.Cases[0].Time, "0.012")
	}
	// An operation that never ran still carries a time, so the attribute is
	// present on every case rather than only on the ones that reached a server.
	if got.Cases[2].Time != "0.000" {
		t.Errorf("deletePet time = %q, want %q", got.Cases[2].Time, "0.000")
	}
}

func TestJUnitReportMarksFailuresAndSkips(t *testing.T) {
	got, raw := renderJUnit(t, sampleSuite())

	passed := got.Cases[0]
	if passed.Failure != nil || passed.Skipped != nil {
		t.Errorf("a passing case must carry neither child element:\n%s", raw)
	}

	failed := got.Cases[1]
	if failed.Failure == nil {
		t.Fatalf("the failed case has no <failure> child:\n%s", raw)
	}
	if failed.Failure.Message != "the server returned 500 Internal Server Error" {
		t.Errorf("failure message = %q, want the run's reason", failed.Failure.Message)
	}
	if failed.Skipped != nil {
		t.Error("a failed case must not also be marked skipped")
	}

	skipped := got.Cases[2]
	if skipped.Skipped == nil {
		t.Fatalf("the skipped case has no <skipped> child:\n%s", raw)
	}
	if skipped.Skipped.Message != "a DELETE request may change server state" {
		t.Errorf("skipped message = %q, want the run's reason", skipped.Skipped.Message)
	}
	if skipped.Failure != nil {
		t.Error("a skipped case must not be reported as a failure")
	}
}

func TestJUnitReportEscapesReasonText(t *testing.T) {
	// Every character XML cares about, in a message shaped like one talaria
	// actually produces: a validation error quoting a JSON pointer and a value.
	reason := `body invalid: <root> & "id" must be > 0`

	got, raw := renderJUnit(t, Suite{
		Name:  `talaria run "pets" <v1> & co`,
		Cases: []Case{{Name: `get<Pet> & "friends"`, Failure: reason}},
	})

	if strings.Contains(raw, "<root>") {
		t.Errorf("the reason's angle brackets went out raw and corrupt the file:\n%s", raw)
	}
	if !strings.Contains(raw, "&amp;") {
		t.Errorf("the reason's ampersand was not escaped:\n%s", raw)
	}

	if got.Cases[0].Failure == nil {
		t.Fatalf("the failed case has no <failure> child:\n%s", raw)
	}
	// Escaped on the way out, identical on the way back: the reason survives a
	// round trip through a parser, which is all a consumer sees.
	if got.Cases[0].Failure.Message != reason {
		t.Errorf("failure message = %q, want %q", got.Cases[0].Failure.Message, reason)
	}
	if got.Cases[0].Name != `get<Pet> & "friends"` {
		t.Errorf("testcase name = %q, want the operationId verbatim", got.Cases[0].Name)
	}
	if got.Name != `talaria run "pets" <v1> & co` {
		t.Errorf("testsuite name = %q, want it verbatim", got.Name)
	}
}

func TestJUnitReportRendersAnEmptySuite(t *testing.T) {
	// Nothing selected is still a valid report: a CI job that parses the file
	// unconditionally must not fall over on a suite that tested nothing.
	got, raw := renderJUnit(t, Suite{Name: "talaria run"})

	if got.Tests != 0 || len(got.Cases) != 0 {
		t.Errorf("empty suite = %d tests, %d cases, want 0 and 0:\n%s", got.Tests, len(got.Cases), raw)
	}
}

func TestJUnitIsAReportFormatOnly(t *testing.T) {
	// --output junit is refused: no other command has test cases to report, and
	// a `describe` rendered as a test suite would mean nothing.
	if _, err := ParseFormat("junit"); err == nil {
		t.Error("ParseFormat accepted junit; it is a --report format, not an --output one")
	}
	for _, f := range Formats() {
		if f == string(FormatJUnit) {
			t.Errorf("Formats() lists %q, which --output does not accept", f)
		}
	}

	got, err := ParseReportFormat("junit")
	if err != nil {
		t.Fatalf("ParseReportFormat(junit) returned error: %v", err)
	}
	if got != FormatJUnit {
		t.Errorf("ParseReportFormat(junit) = %q, want %q", got, FormatJUnit)
	}

	// Every --output format is also a report format: --report is a narrowing of
	// where the report goes, not a different vocabulary.
	reports := ReportFormats()
	for _, f := range append(Formats(), string(FormatJUnit)) {
		if !contains(reports, f) {
			t.Errorf("ReportFormats() = %v, missing %q", reports, f)
		}
	}

	if _, err := ParseReportFormat("yaml"); err == nil {
		t.Error("ParseReportFormat accepted yaml")
	} else if !strings.Contains(err.Error(), "junit") {
		t.Errorf("error %q does not name junit among the valid values", err)
	}
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}

	return false
}
