// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// LoadAdminAliases reads a list of admin aliases from a file. Each non-empty,
// non-comment line is treated as one alias. Whitespace is trimmed. Lines
// beginning with '#' are skipped.
//
// If path is empty or the file does not exist, returns nil with no error so
// portal deployments without an admin allowlist still start.
func LoadAdminAliases(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var aliases []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		aliases = append(aliases, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return aliases, nil
}
