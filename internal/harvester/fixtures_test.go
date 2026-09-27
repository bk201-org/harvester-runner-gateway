package harvester

import (
	"fmt"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
)

func testResourceID(owner auth.Owner, sequence int) string {
	return fmt.Sprintf("ci-%s-%s-a%s-%03d", owner.RepositoryID, owner.RunID, owner.RunAttempt, sequence)
}
