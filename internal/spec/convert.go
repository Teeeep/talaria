package spec

import (
	"encoding/json"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"gopkg.in/yaml.v3"

	"github.com/Teeeep/talaria/internal/clierr"
)

// versionSwagger2 is the only Swagger 2.0 version string, and the value
// recorded in Document.ConvertedFrom for a converted document.
const versionSwagger2 = "2.0"

// versionProbe reads just the mutually exclusive version keys. Swagger 2.0
// declares "swagger", OpenAPI 3.x declares "openapi", and every other field
// differs enough that guessing from anything else is unreliable.
type versionProbe struct {
	Swagger string `json:"swagger" yaml:"swagger"`
	OpenAPI string `json:"openapi" yaml:"openapi"`
}

// isSwagger2 reports whether raw spec bytes, in either JSON or YAML, declare
// Swagger 2.0. Format is detected by trying JSON and falling back to YAML
// rather than by file extension, because specs also arrive from URLs that carry
// no extension at all.
func isSwagger2(data []byte) bool {
	var probe versionProbe

	if err := json.Unmarshal(data, &probe); err != nil {
		if err := yaml.Unmarshal(data, &probe); err != nil {
			return false
		}
	}

	return strings.HasPrefix(probe.Swagger, "2.")
}

// convertSwagger2 turns Swagger 2.0 bytes into OpenAPI 3.x JSON bytes that the
// normal load path can build. libopenapi cannot do this conversion, so
// kin-openapi's openapi2conv does it and hands the result back as bytes rather
// than as a second model type — one model reaches the rest of the program.
func convertSwagger2(data []byte, source string) ([]byte, error) {
	jsonData, err := toJSON(data, source)
	if err != nil {
		return nil, err
	}

	var v2 openapi2.T
	if err := json.Unmarshal(jsonData, &v2); err != nil {
		return nil, clierr.SpecLoad("reading Swagger 2.0 spec %s: %w", source, err)
	}

	v3, err := openapi2conv.ToV3(&v2)
	if err != nil {
		return nil, clierr.SpecLoad("converting Swagger 2.0 spec %s to OpenAPI 3: %w", source, err)
	}

	converted, err := json.Marshal(v3)
	if err != nil {
		return nil, clierr.SpecLoad("serialising converted spec %s: %w", source, err)
	}

	return converted, nil
}

// toJSON returns spec bytes as JSON, converting from YAML when needed, because
// openapi2.T only unmarshals JSON.
//
// The YAML decode goes through yaml.v3 deliberately: yaml.v2 decodes mappings
// as map[interface{}]interface{}, which json.Marshal rejects outright, so a
// downgrade there would break every YAML 2.0 spec.
func toJSON(data []byte, source string) ([]byte, error) {
	if json.Valid(data) {
		return data, nil
	}

	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, clierr.SpecLoad("parsing spec %s: %w", source, err)
	}

	jsonData, err := json.Marshal(doc)
	if err != nil {
		return nil, clierr.SpecLoad("converting spec %s from YAML to JSON: %w", source, err)
	}

	return jsonData, nil
}
