// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
)

func validateRuntimeConfig(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > 1<<20 {
		return "", errors.New("runtime config exceeds 1 MiB")
	}
	var object map[string]json.RawMessage
	if e := json.Unmarshal(data, &object); e != nil || object == nil {
		return "", errors.New("runtime config must be a valid JSON object")
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}
func writeRuntimeConfig(ctx context.Context, stage string, data []byte, expected string) error {
	root, e := openDirectoryRoot(stage)
	if e != nil {
		return e
	}
	defer root.Close()
	if old, e := openRegularDestination(ctx, root, "config.json"); e == nil {
		sum, size, err := hashOpenFile(ctx, old)
		old.Close()
		if err != nil {
			return err
		}
		if sum != expected || size != int64(len(data)) {
			return errors.New("staged runtime config differs from migration input")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	tmp := ".runtime-config-" + rand.Text()
	f, e := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer root.Remove(tmp)
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = root.Link(tmp, "config.json"); e != nil {
		return e
	}
	return root.Remove(tmp)
}
