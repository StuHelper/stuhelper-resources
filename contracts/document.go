// Package contracts supplies the same OpenAPI document used to generate clients.
package contracts

import _ "embed"

//go:embed resources.openapi.json
var document []byte

func Document() []byte { return append([]byte{}, document...) }
