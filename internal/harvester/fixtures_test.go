package harvester

import (
	"fmt"
	"strings"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

var testMetadata = gateway.ResourceMetadata{
	IdentityHash: strings.Repeat("1", 64),
	RequestHash:  strings.Repeat("2", 64),
}

func testResourceID(owner auth.Owner, sequence int) string {
	return fmt.Sprintf("ci-%s-%s-a%s-%03d", owner.RepositoryID, owner.RunID, owner.RunAttempt, sequence)
}
