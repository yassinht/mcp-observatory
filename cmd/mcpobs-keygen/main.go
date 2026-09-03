// Command mcpobs-keygen creates the ed25519 key that signs tree heads.
//
// It is a separate binary, run once by hand, for the same reason a log is
// append-only: replacing the signing key of a running log silently invalidates
// every head already published, so it must never be something a scheduled job
// can do by accident. GenerateKey refuses to overwrite an existing key.
//
//	mcpobs-keygen heads/key
//
// The private key is written 0600 and must stay on the server. The public key
// goes next to the heads in git, where anyone can fetch it.
package main

import (
	"encoding/base64"
	"fmt"
	"os"

	"github.com/yhouta/mcp-observatory/internal/mlog"
)

func main() {
	path := "heads/key"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	pub, err := mlog.GenerateKey(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpobs-keygen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("private key  %s      (keep on the server, never commit)\n", path)
	fmt.Printf("public key   %s.pub  (publish alongside the heads)\n", path)
	fmt.Printf("\n%s\n", base64.StdEncoding.EncodeToString(pub))
}
