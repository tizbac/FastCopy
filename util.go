package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// parseSize accepts cp-ish sizes: 512, 4K, 1M, 8MiB, 2G
func parseSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "KiB"), strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult = 1024
		s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "KiB"), "K"), "k")
	case strings.HasSuffix(s, "MiB"), strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult = 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "MiB"), "M"), "m")
	case strings.HasSuffix(s, "GiB"), strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		mult = 1024 * 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "GiB"), "G"), "g")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

func atoi(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}

// cpErr formats an error the way GNU cp does: "cannot <verb> <path>: <reason>".
// The syscall errors already contain the path, so this keeps messages short and
// avoids printing the same path twice.
func cpErr(verb, path string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("cannot %s %s: %w", verb, path, pe.Err)
	}
	return fmt.Errorf("cannot %s %s: %w", verb, path, err)
}
