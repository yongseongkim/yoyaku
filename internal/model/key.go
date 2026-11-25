package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalKey는 로컬 모델에 요청을 보내는 서버 쪽 클라이언트끼리만 공유하는 키다.
func LocalKey(root string) (string, error) {
	path := filepath.Join(root, ".local-native/worker.key")
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return "", e
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	key := hex.EncodeToString(b)
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e == nil {
		_, e = f.WriteString(key + "\n")
		ce := f.Close()
		if e != nil {
			return "", e
		}
		return key, ce
	}
	if !os.IsExist(e) {
		return "", e
	}
	for i := 0; i < 20; i++ {
		data, e := os.ReadFile(path)
		if e != nil {
			return "", e
		}
		key = strings.TrimSpace(string(data))
		if len(key) == 64 {
			if _, e = hex.DecodeString(key); e == nil {
				return key, nil
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "", errors.New("Invalid local worker key")
}
