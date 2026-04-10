//go:build darwin

package platform

import (
	"fmt"
	"os/exec"
)

func ReadCredentialFromKeystore() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-a", "qai-local-dev", "-s", "gcp-sa-key-b64", "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("macOS Keychain: no SA key stored (add with: security add-generic-password -a qai-local-dev -s gcp-sa-key-b64 -w '<base64-key>')")
	}
	return out, nil
}
