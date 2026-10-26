// Package localereader is the SSH-only UTF-8 input adapter used by MioChat.
// SSH terminal data is already UTF-8; treating it as the server's Windows ACP
// corrupts Chinese input. This app never reads the host's console input.
package localereader

import "io"

func NewReader(input io.Reader) io.Reader { return input }
