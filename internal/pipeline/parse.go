package pipeline

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSpec rebuilds a Spec from what a manifest recorded — "zstd:3",
// "age:x25519". Reading a backup back means reversing exactly the stages that
// produced it, and the manifest is the only record of what those were.
//
// Compression levels do not affect decoding, so the level is parsed for
// reporting and ignored by the reader.
func ParseSpec(compression, encryption string) (Spec, error) {
	spec := Spec{}

	algo, level, _ := strings.Cut(compression, ":")
	switch Algo(algo) {
	case AlgoZstd, AlgoGzip:
		spec.Compression.Algo = Algo(algo)
		if level != "" {
			parsed, err := strconv.Atoi(level)
			if err != nil {
				return Spec{}, fmt.Errorf("manifest records compression %q, whose level is not a number", compression)
			}
			spec.Compression.Level = parsed
		}
	case AlgoNone, "":
		spec.Compression.Algo = AlgoNone
	default:
		return Spec{}, fmt.Errorf("manifest records compression %q, which this build cannot read", compression)
	}

	switch encryption {
	case "age:x25519":
		spec.Encryption.Mode = ModeAge
	case "age:scrypt":
		spec.Encryption.Mode = ModePassphrase
	case string(ModeNone), "":
		spec.Encryption.Mode = ModeNone
	default:
		return Spec{}, fmt.Errorf("manifest records encryption %q, which this build cannot read", encryption)
	}

	return spec, nil
}
