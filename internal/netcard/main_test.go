package netcard

import (
	"io"
	"log"
	"os"
	"testing"
)

// The tests drop many price lists on purpose; Build logs each one.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}
