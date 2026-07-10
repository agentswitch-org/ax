package shell

import (
	"encoding/base64"
	"encoding/binary"
	"unicode/utf16"
)

// encodePwshCommand renders cmd for PowerShell's -EncodedCommand: base64 of the
// UTF-16LE (little-endian, no BOM) bytes of cmd. PowerShell decodes it verbatim,
// so a command is immune to command-line re-tokenization. That matters because
// `-Command` parses its string as a script: a newline-separated command whose
// line begins with a flag like `--task-file` is read as the `--` unary operator
// ("Missing expression after unary operator '--'") and the launch dies.
// -EncodedCommand sidesteps that entirely. The encoding is pure and deterministic.
func encodePwshCommand(cmd string) string {
	units := utf16.Encode([]rune(cmd))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[i*2:], u)
	}
	return base64.StdEncoding.EncodeToString(buf)
}
