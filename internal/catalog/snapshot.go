package catalog

import _ "embed"

// snapshot contains only public server targets extracted for this project.
//
//go:embed snapshot.json
var snapshot []byte

func Bundled() (Catalog, error) { return decode(snapshot) }
