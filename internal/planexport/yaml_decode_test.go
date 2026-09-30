package planexport

import (
	"gopkg.in/yaml.v3"
)

func decodeYAML(b []byte, into any) error {
	return yaml.Unmarshal(b, into)
}
