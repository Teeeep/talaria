package spec

import (
	"io"
	"log/slog"
	"os"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel"

	"github.com/Teeeep/talaria/internal/clierr"
)

// sourceBytes is the Source recorded for a spec that never had a path or URL.
const sourceBytes = "<bytes>"

// documentConfig is the configuration every parse runs under.
//
// Without one, libopenapi builds a default whose logger is a JSON slog handler
// writing to the real os.Stdout — past cmd.OutOrStdout, past the talaria/v1
// envelope and past every redaction path. A spec with an unresolvable $ref
// would put stray log documents where the envelope goes and nothing else, so
// the logger is discarded here; stdout belongs to the envelope.
//
// AllowFileReferences and AllowRemoteReferences stay at their false defaults on
// purpose: that default is what stops a hostile spec's $ref reading local files
// or fetching URLs, and naming the configuration explicitly is precisely where
// it could be lost.
func documentConfig() *datamodel.DocumentConfiguration {
	cfg := datamodel.NewDocumentConfiguration()
	cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))

	return cfg
}

// LoadBytes parses raw spec bytes, in either JSON or YAML, and builds the
// OpenAPI 3.x model. Every failure comes back as a clierr.SpecLoad (exit 3),
// including the ones libopenapi reports as a panic-free error slice, so no
// caller has to classify a parse failure itself.
func LoadBytes(data []byte) (*Document, error) {
	return loadBytes(data, sourceBytes)
}

// LoadFile reads a spec from disk and loads it. A missing or unreadable path is
// a spec-load failure too, not a usage error: the distinction that matters to an
// agent is "the spec did not come up", and the message names the path either
// way.
func LoadFile(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, clierr.SpecLoad("reading spec %s: %w", path, err)
	}

	return loadBytes(data, path)
}

// loadBytes is the single parse path both entry points share, so the error
// wrapping and the Source bookkeeping stay in one place.
func loadBytes(data []byte, source string) (*Document, error) {
	// Swagger 2.0 is converted here, before anything else sees the bytes, so
	// "any API doc" stops being a special case one command at a time.
	var convertedFrom string
	if isSwagger2(data) {
		converted, err := convertSwagger2(data, source)
		if err != nil {
			return nil, err
		}
		data, convertedFrom = converted, versionSwagger2
	}

	doc, err := libopenapi.NewDocumentWithConfiguration(data, documentConfig())
	if err != nil {
		return nil, clierr.SpecLoad("parsing spec %s: %w", source, err)
	}

	// The second return is a single error, not a slice; on a spec with several
	// build problems it is a join of all of them.
	model, err := doc.BuildV3Model()
	if err != nil {
		return nil, clierr.SpecLoad("building model for spec %s: %w", source, err)
	}

	return &Document{
		Version:       model.Model.Version,
		Source:        source,
		Model:         &model.Model,
		ConvertedFrom: convertedFrom,
	}, nil
}
