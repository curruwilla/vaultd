package cli

import (
	"fmt"
	"os"

	"filippo.io/age"
)

// identityEnv names the file holding the age private key, for the commands
// that have to read a backup back.
const identityEnv = "VAULTD_AGE_IDENTITY_FILE"

// loadIdentities reads the age private keys that can decrypt a backup.
//
// The key is passed as a file, never as a flag value: a private key on the
// command line would be visible to every process on the host. vaultd itself
// never stores it — by design it lives away from the machine that takes the
// backups (SPEC §15), and is supplied for the run that needs to read one back.
func loadIdentities(path string) ([]age.Identity, error) {
	if path == "" {
		path = os.Getenv(identityEnv)
	}
	if path == "" {
		return nil, nil
	}

	// The path is the operator's own, given on the command line or in the
	// environment; opening it is the whole point of the flag.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("reading the age identity: %w", err)
	}
	defer file.Close()

	identities, err := age.ParseIdentities(file)
	if err != nil {
		return nil, fmt.Errorf("reading the age identity from %s: %w", path, err)
	}
	if len(identities) == 0 {
		return nil, fmt.Errorf("%s holds no age identity", path)
	}
	return identities, nil
}
