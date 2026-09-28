package harvester

import (
	"fmt"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
)

func testResourceID(_ auth.Owner, sequence int, kind ...string) string {
	prefix := "ci-vm-"
	if len(kind) != 0 && kind[0] == "volume" {
		prefix = "ci-vol-"
	}
	return fmt.Sprintf("%s%08x", prefix, sequence)
}
