//go:build windows

package platform

import "fmt"

func ReadCredentialFromKeystore() ([]byte, error) {
	return nil, fmt.Errorf("SA key fallback not supported on Windows\n" +
		"Set GOOGLE_APPLICATION_CREDENTIALS to a service account JSON file, or run: gcloud auth application-default login")
}
