package main

import (
	"bytes"
	"os"
)

// Extractor supplies the in-page detection script. A file named extractor.js
// in the data folder overrides the built-in one, so detection fixes can be
// dropped in without rebuilding or re-downloading the .exe. It is re-read on
// every use, so a new file takes effect on the next lookup.
type Extractor struct{ OverridePath string }

func (e Extractor) JS() (js string, external bool) {
	if e.OverridePath != "" {
		if b, err := os.ReadFile(e.OverridePath); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return string(b), true
		}
	}
	b, _ := staticFS.ReadFile("static/extractor.js")
	return string(b), false
}
