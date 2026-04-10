//go:build linux

package platform

import (
	"fmt"
	"os/exec"
)

func ReadCredentialFromKeystore() ([]byte, error) {
	if _, err := exec.LookPath("secret-tool"); err == nil {
		out, err := exec.Command("secret-tool", "lookup",
			"service", "qai-local-dev", "key", "gcp-sa-key-b64").Output()
		if err == nil && len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("no SA key found in secret storage\n" +
		"Store with: secret-tool store --label='qai GCP SA key' service qai-local-dev key gcp-sa-key-b64\n" +
		"Or set GOOGLE_APPLICATION_CREDENTIALS to a service account JSON file")
}
