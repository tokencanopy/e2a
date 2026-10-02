package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// readSendingPolicyFile reads one complete runtime-policy JSON document. This
// is a reviewed payload, not a server config: environment/config defaults must
// never alter it. Errors do not echo paths or possibly sensitive file contents.
func readSendingPolicyFile(path string) (sendingpolicy.RuntimePolicy, error) {
	const maxBytes = 64 * 1024
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return sendingpolicy.RuntimePolicy{}, errors.New("policy file must be a readable regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return sendingpolicy.RuntimePolicy{}, errors.New("cannot open policy file")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return sendingpolicy.RuntimePolicy{}, errors.New("cannot read policy file")
	}
	if len(raw) > maxBytes {
		return sendingpolicy.RuntimePolicy{}, errors.New("policy file exceeds 64 KiB")
	}
	if !json.Valid(raw) {
		return sendingpolicy.RuntimePolicy{}, errors.New("policy file must contain one valid JSON document")
	}
	policy, err := sendingpolicy.ParsePolicy(raw)
	if err != nil {
		return sendingpolicy.RuntimePolicy{}, errors.New("policy file does not match the runtime policy schema or validation rules")
	}
	return policy, nil
}
