//go:build windows

package main

import (
	"fmt"
)

// readCredentialFromKeystore is not yet supported on Windows.
// Users should set GOOGLE_APPLICATION_CREDENTIALS instead.
func readCredentialFromKeystore() ([]byte, error) {
	return nil, fmt.Errorf("SA key fallback not supported on Windows\n" +
		"Set GOOGLE_APPLICATION_CREDENTIALS to a service account JSON file, or run: gcloud auth application-default login")
}
