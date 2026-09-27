package smoke

import (
	"log/slog"
	"os"
	"testing"
)

func TestGatewaySmoke(t *testing.T) {
	if os.Getenv("GATEWAY_SMOKE") != "1" {
		t.Skip("set GATEWAY_SMOKE=1 to run the live gateway smoke test")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := Run(t, os.Getenv, logger); err != nil {
		t.Fatal(err)
	}
}
