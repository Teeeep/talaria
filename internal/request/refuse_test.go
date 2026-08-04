package request

import (
	"strings"
	"testing"

	"github.com/Teeeep/talaria/internal/clierr"
	"github.com/Teeeep/talaria/internal/config"
	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/secret"
)

// hostileCred is an apiKey credential with a name a spec supplied, for the
// cases whose subject is the name rather than the operation.
func hostileCred(in, name string) config.Credential {
	return config.Credential{
		Scheme: "hostile", Kind: config.KindAPIKey, In: in, Name: name,
		Ref: secret.Env("TALARIA_AUTH_APIKEY_HOSTILE"), Supported: true,
	}
}

// Refuses is what lets `auth check` answer the question `call` answers: a
// document carrying a string this package refuses to put on the wire cannot be
// called at all, whatever the caller's flags say. The two commands may not
// disagree (DESIGN.md:329), so every defect Refuses reports has to be one Build
// refuses too — and every one it passes has to be one Build accepts, or the
// pre-flight refuses a spec that works.
func TestRefusesAgreesWithBuildOnADocumentDefect(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		creds []config.Credential
		// names is what the refusal has to identify, so a caller reading exit 2
		// learns which string to fix.
		names string
	}{
		{name: "hostile path key", path: "@evil.example.com/steal", names: "path"},
		{name: "path with no leading slash", path: "pets", names: "path"},
		{name: "path with a newline", path: "/pets\r\nX-Injected: 1", names: "path"},
		{
			name:  "credential name with a colon",
			creds: []config.Credential{hostileCred(config.InHeader, "X-Key: yes")},
			names: "hostile",
		},
		{
			name:  "credential name with a newline",
			creds: []config.Credential{hostileCred(config.InQuery, "api_key\r\nX-Injected: yes")},
			names: "hostile",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := inputs(t, "listPets")
			in.Params = []string{"limit=10"}
			in.Hosts = specHosts(t, in.Doc)
			if tc.path != "" {
				in.Op.Path = tc.path
			}
			in.Creds = tc.creds

			err := Refuses([]operation.Operation{in.Op}, in.Creds)
			if err == nil {
				t.Fatalf("Refuses accepted a document Build refuses")
			}
			if code := clierr.From(err).Code; code != clierr.CodeUsage {
				t.Errorf("Refuses error = %v (code %d), want a usage error (%d)", err, code, clierr.CodeUsage)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("Refuses error = %v, want it to name %q", err, tc.names)
			}

			// The agreement itself: the same document, through the builder.
			buildErr(t, in)
		})
	}
}

// The other half, and the one that decides where the line falls: Refuses
// answers about the *document*, not about the invocation. A required parameter
// nobody passed is a command line to correct, and a pre-flight that reported it
// would tell an agent its spec was broken when the spec is fine.
func TestRefusesAcceptsADocumentWhoseCallIsMerelyIncomplete(t *testing.T) {
	op, doc := fixture(t, "getPet")
	if !strings.Contains(op.Path, "{") {
		t.Fatalf("fixture path %q holds no placeholder, so this test proves nothing", op.Path)
	}

	creds, err := config.Resolve(op, doc, nil)
	if err != nil {
		t.Fatalf("config.Resolve: %v", err)
	}

	if err := Refuses([]operation.Operation{op}, creds); err != nil {
		t.Errorf("Refuses = %v, want nil: the document is sound and only the --param is missing", err)
	}

	// And Build does refuse it, so the two are answering different questions on
	// purpose rather than by accident.
	buildErr(t, Inputs{Op: op, Doc: doc, Creds: creds, Hosts: specHosts(t, doc)})
}

// Every operation is asked, because one hostile `paths:` key is enough: a spec
// whose other operations bind cleanly is still a spec no agent should be told
// is callable.
func TestRefusesAsksEveryOperation(t *testing.T) {
	clean, _ := fixture(t, "listPets")
	hostile := clean
	hostile.ID, hostile.Path = "steal", "@evil.example.com/steal"

	if err := Refuses([]operation.Operation{clean}, nil); err != nil {
		t.Fatalf("Refuses = %v on the clean operation alone, want nil", err)
	}

	err := Refuses([]operation.Operation{clean, hostile}, nil)
	if err == nil {
		t.Fatal("Refuses accepted a spec one of whose paths moves the request off its host")
	}
	if !strings.Contains(err.Error(), "steal") {
		t.Errorf("Refuses error = %v, want it to name the operation at fault", err)
	}
}
